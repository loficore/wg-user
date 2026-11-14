package wstunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// inboxCapacity bounds how many received datagrams may wait for Recv.
	// WireGuard's receive path drains promptly, so a full inbox means the
	// consumer is wedged; dropping is preferable to stalling the reader.
	inboxCapacity = 256

	// maxFrameSize is the largest WebSocket frame accepted. A WireGuard
	// datagram is at most ~1.5 KB, so 64 KiB leaves ample headroom while
	// still refusing an obviously broken peer.
	maxFrameSize = 65536

	// defaultPath is the upgrade prefix used when Config.Path is empty,
	// matching the config layer's default.
	defaultPath = "v1"

	// defaultBackoffMin and defaultBackoffMax are the reconnect bounds used
	// when Config leaves them zero. The reconnect loop itself lives in the
	// supervisor (Run); these are declared here so Config is complete.
	defaultBackoffMin = 500 * time.Millisecond
	defaultBackoffMax = 30 * time.Second

	// defaultWriteTimeout bounds every WebSocket write. Without it a peer
	// that stops reading TCP would pin the writer forever.
	defaultWriteTimeout = 10 * time.Second

	// State values reported by State. Before the first successful dial the
	// client is "connecting"; a failed dial or a dropped connection is
	// "reconnecting"; a live WebSocket is "connected".
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
)

// Config configures a Client. It is a copy of the keys the [Transport]
// section carries, plus the [Peer] Endpoint the server is asked to deliver to.
type Config struct {
	// URL is the wstunnel server base URL (ws:// or wss://).
	URL string
	// Path is the WS upgrade prefix as a bare segment; empty means "v1".
	Path string
	// Target is the "host:port" the server forwards datagrams to: the
	// [Peer] Endpoint.
	Target string
	// Username and Password are optional HTTP basic-auth credentials. They
	// are sent only when both are non-empty.
	Username string
	Password string
	// Insecure skips TLS certificate verification for wss:// URLs.
	Insecure bool
	// Keepalive is the WS ping interval; zero disables pings.
	Keepalive time.Duration
	// BackoffMin and BackoffMax bound the reconnect delay. Zero means
	// 500ms and 30s. They are read by the supervisor, not by a single dial.
	BackoffMin, BackoffMax time.Duration
}

// Client carries WireGuard datagrams over one WebSocket connection to a
// wstunnel v11 server. One WebSocket binary frame is exactly one UDP
// datagram: datagrams are never coalesced or split across frames.
//
// A Client is safe for concurrent use. Send may be called while the reader is
// active; the reconnect supervisor may replace the connection underneath it.
//
// gorilla permits exactly one reader and one writer per connection. dialOnce
// is the only code that starts a reader, and Clients serialise all writes
// under writeMu; the supervisor must not start a reader of its own (see
// dialOnce).
type Client struct {
	cfg Config

	// writeTimeout bounds every write so a peer that stops reading cannot pin
	// the writer forever. Tests shorten it.
	writeTimeout time.Duration

	// writeMu serialises writes: gorilla allows one concurrent writer per
	// connection, and every write must set a deadline via writeFrame. Lock
	// order is writeMu → mu; nothing takes them the other way.
	writeMu sync.Mutex

	// mu guards the fields below. Critical sections are short and never do
	// I/O, so teardown cannot get stuck behind a stalled write (a write holds
	// only writeMu).
	mu   sync.Mutex
	conn *websocket.Conn
	// gen identifies the published connection. A reader or writer captures
	// it with conn and teardown only acts when both still match, so a stale
	// reader can never poison a newer connection.
	gen uint64
	// readErr is the terminal error of the published connection; nil while
	// it is healthy. readErrCh is closed when readErr is published or when
	// the connection is retired by a redial, waking a parked Recv.
	readErr   error
	readErrCh chan struct{}

	// inbox carries frame payloads from the reader goroutine to Recv.
	inbox chan []byte

	// done is closed by Close and unblocks Recv.
	done      chan struct{}
	closeOnce sync.Once

	stateMu sync.RWMutex
	state   string
}

// New returns an idle Client. It does not dial; the supervisor calls dialOnce
// and owns the reconnect policy.
func New(cfg Config) *Client {
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = defaultBackoffMax
	}

	return &Client{
		cfg:          cfg,
		writeTimeout: defaultWriteTimeout,
		inbox:        make(chan []byte, inboxCapacity),
		done:         make(chan struct{}),
		readErrCh:    make(chan struct{}),
		state:        stateConnecting,
	}
}

// Send writes pkt as one binary frame, i.e. one UDP datagram. When no
// connection is established the datagram is dropped and Send returns nil:
// WireGuard generates traffic faster than a reconnect can complete, and
// blocking there would wedge the tunnel.
//
// A write that fails or exceeds writeTimeout tears the connection down (so the
// reader exits instead of waiting on a dead socket) and returns an error.
func (c *Client) Send(pkt []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	conn, gen := c.conn, c.gen
	c.mu.Unlock()
	if conn == nil {
		return nil
	}

	if err := writeFrame(conn, websocket.BinaryMessage, pkt, c.writeTimeout); err != nil {
		c.failConn(conn, gen, err)
		return fmt.Errorf("wstunnel: send: %w", err)
	}
	return nil
}

// Recv copies the next received datagram into buf and returns its length. If
// the frame is larger than buf the frame is discarded and an error returned
// rather than truncating a datagram. After Close, Recv returns an error
// wrapping net.ErrClosed.
func (c *Client) Recv(buf []byte) (int, error) {
	for {
		// Prefer the closed signal over whatever happens to be buffered: once
		// Close returns, callers must observe net.ErrClosed deterministically.
		select {
		case <-c.done:
			return 0, fmt.Errorf("wstunnel: receive: %w", net.ErrClosed)
		default:
		}

		c.mu.Lock()
		errCh, readErr := c.readErrCh, c.readErr
		c.mu.Unlock()
		if readErr != nil {
			return 0, fmt.Errorf("wstunnel: receive: %w", readErr)
		}

		select {
		case <-c.done:
			return 0, fmt.Errorf("wstunnel: receive: %w", net.ErrClosed)
		case <-errCh:
			// A redial may have retired this generation between the close and
			// this wake-up, clearing readErr. Loop and re-snapshot rather than
			// wrapping a nil error; a live generation is open again.
			continue
		case frame := <-c.inbox:
			if len(frame) > len(buf) {
				return 0, fmt.Errorf("wstunnel: frame of %d bytes exceeds buffer of %d", len(frame), len(buf))
			}
			return copy(buf, frame), nil
		}
	}
}

// Close tears down the connection. It is idempotent, and Recv afterwards
// reports net.ErrClosed. It never blocks on an in-flight write: the write
// deadline and the socket close both release the writer.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)

		// Snapshot under mu, then close outside it: a writer may be blocked
		// inside WriteMessage, and this close is what unblocks it.
		c.mu.Lock()
		conn := c.conn
		c.conn = nil
		c.mu.Unlock()

		if conn != nil {
			// The error is always "use of closed network connection" by the
			// time anyone sees it, and Close's contract here is best-effort.
			_ = conn.Close()
		}
	})
	return nil
}

// State returns the connection state: "connecting" before the first
// successful dial, "connected" while the WebSocket is live, and
// "reconnecting" after a dial failure or a dropped connection.
func (c *Client) State() string {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *Client) setState(state string) {
	c.stateMu.Lock()
	c.state = state
	c.stateMu.Unlock()
}

// dialOnce performs a single WebSocket handshake, publishes the connection and
// starts its one and only reader. It does not retry; the supervisor owns
// backoff. Every error is prefixed with "wstunnel:".
//
// Reader ownership: dialOnce starts the readLoop goroutine for each connection
// it publishes. The supervisor (Run) must only wait for the connection to end
// and call dialOnce again; it must never start a read goroutine of its own.
// gorilla supports exactly one concurrent reader per connection.
//
// If a previous connection is still published (an overlapping dial, or a
// supervisor bug), it is closed so its socket and reader do not leak and two
// readers never run against one conn. The reader's teardown is ignored
// because its generation no longer matches.
func (c *Client) dialOnce(ctx context.Context) error {
	// Never dial during or after shutdown.
	select {
	case <-c.done:
		return fmt.Errorf("wstunnel: dial: %w", net.ErrClosed)
	default:
	}

	endpoint, err := c.endpoint()
	if err != nil {
		c.dialFailed()
		return err
	}

	token, err := BuildToken(c.cfg.Target)
	if err != nil {
		c.dialFailed()
		return err
	}

	header := http.Header{}
	// The server expects the raw value "v1, authorization.bearer.<jwt>",
	// which is not a valid subprotocol list, so it is set as a plain header
	// instead of Dialer.Subprotocols.
	header.Set("Sec-WebSocket-Protocol", "v1, authorization.bearer."+token)
	if c.cfg.Username != "" && c.cfg.Password != "" {
		req := &http.Request{Header: header}
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}

	dialer := websocket.Dialer{
		// Insecure is the user's explicit choice for self-signed servers.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.cfg.Insecure}, //nolint:gosec
	}

	conn, _, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		c.dialFailed()
		return fmt.Errorf("wstunnel: dial %s: %w", endpoint, err)
	}

	conn.SetReadLimit(maxFrameSize)

	// A Close that raced the handshake must win: never publish or read from a
	// connection nobody will close.
	select {
	case <-c.done:
		_ = conn.Close()
		return fmt.Errorf("wstunnel: dial %s: %w", endpoint, net.ErrClosed)
	default:
	}

	// Publish the new generation atomically: conn, generation, a fresh error
	// channel and the connected state all become visible together.
	c.mu.Lock()
	prev, prevErrCh := c.conn, c.readErrCh
	c.conn = conn
	c.gen++
	gen := c.gen
	c.readErr = nil
	c.readErrCh = make(chan struct{})
	c.stateMu.Lock()
	c.state = stateConnected
	c.stateMu.Unlock()
	c.mu.Unlock()

	// Retire the previous connection outside mu: close its socket (its reader
	// then exits) and its error channel (a parked Recv re-snapshots the new
	// generation). The old reader's failConn finds a generation mismatch and
	// does not touch the new connection's state.
	if prev != nil {
		_ = prev.Close()
		close(prevErrCh)
	}

	go c.readLoop(conn, gen)

	return nil
}

// dialFailed reports a dial failure as "reconnecting", but only when no
// connection is published (a concurrent dial may have succeeded) and the
// client is not shutting down.
func (c *Client) dialFailed() {
	select {
	case <-c.done:
		return
	default:
	}

	// The idle check and the state change are atomic with respect to a
	// concurrent successful dial, which publishes conn and "connected" under
	// the same lock.
	c.mu.Lock()
	if c.conn == nil {
		c.stateMu.Lock()
		c.state = stateReconnecting
		c.stateMu.Unlock()
	}
	c.mu.Unlock()
}

// endpoint builds the upgrade URL: the server's path is always
// "/<Path>/events". Parsing and assigning Path (rather than concatenating
// strings) keeps a trailing slash on Config.URL from producing "//".
func (c *Client) endpoint() (string, error) {
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return "", fmt.Errorf("wstunnel: parse URL %q: %w", c.cfg.URL, err)
	}

	path := strings.Trim(c.cfg.Path, "/")
	if path == "" {
		path = defaultPath
	}
	u.Path = "/" + path + "/events"
	u.RawPath = ""

	return u.String(), nil
}

// writeFrame writes one WebSocket message under the caller's writeMu, always
// with a deadline so a peer that stops reading cannot block the writer
// forever. Every write path (data frames here, pings later) must go through
// it.
func writeFrame(conn *websocket.Conn, messageType int, pkt []byte, timeout time.Duration) error {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	return conn.WriteMessage(messageType, pkt)
}

// readLoop moves frames from conn into inbox until conn fails. It is the only
// reader of conn; dialOnce starts exactly one per published connection.
func (c *Client) readLoop(conn *websocket.Conn, gen uint64) {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			c.failConn(conn, gen, err)
			return
		}
		if mt != websocket.BinaryMessage {
			// wstunnel only carries datagrams; ignore text/control frames
			// that gorilla has already handled or that a broken peer sent.
			continue
		}

		select {
		case <-c.done:
			return
		default:
		}

		select {
		case c.inbox <- data:
		default:
			// inbox is full: the consumer is not draining. Dropping this
			// datagram keeps the reader alive, which matters more than any
			// single UDP packet; WireGuard retransmits what it needs.
		}
	}
}

// failConn tears down conn and publishes cause to Recv, but only when
// (conn, gen) is still the published connection. A stale reader or writer
// therefore returns false without touching readErr, readErrCh or state, so it
// cannot poison a newer connection. It reports whether this call owned the
// teardown.
func (c *Client) failConn(conn *websocket.Conn, gen uint64, cause error) bool {
	c.mu.Lock()
	if c.conn != conn || c.gen != gen {
		c.mu.Unlock()
		return false
	}

	// Detaching under mu is the atomic decision: only this goroutine may now
	// close the generation's error channel. Publish the state here too, so a
	// redial cannot publish "connected" in between and then be overwritten.
	c.conn = nil
	errCh := c.readErrCh
	if c.readErr == nil {
		c.readErr = cause
	}
	c.stateMu.Lock()
	select {
	case <-c.done:
		// Shutdown owns the terminal state; Recv reports net.ErrClosed.
	default:
		c.state = stateReconnecting
	}
	c.stateMu.Unlock()
	c.mu.Unlock()

	// Close outside mu: a concurrent Send may be blocked in WriteMessage, and
	// this close is what releases it.
	_ = conn.Close()

	// The failure won the detach, so this channel is still open: a redial
	// that retired the generation would have cleared c.conn first, in which
	// case the check above returned false.
	close(errCh)

	return true
}
