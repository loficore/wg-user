package wstunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
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

	// defaultDialTimeout bounds one handshake. dialOnce has no timeout of its
	// own, so the supervisor wraps every attempt.
	defaultDialTimeout = 10 * time.Second

	// State values reported by State: "connecting" while an attempt is in
	// progress, "connected" while a live WebSocket is published,
	// "reconnecting" while waiting to retry after a failure, and "closed"
	// once Close has been called (nothing will reconnect).
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateClosed       = "closed"
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
// State reports one of "connecting" (an attempt is in progress), "connected"
// (a live WebSocket is published), "reconnecting" (waiting to retry after a
// failure) or "closed" (Close was called; nothing will reconnect).
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

	// beforePublish is a test-only seam invoked by dialOnce between the
	// post-handshake shutdown check and the publish critical section. It lets
	// tests exercise a Close landing exactly in that window. Production leaves
	// it nil.
	beforePublish func()

	// beforeDialFailed is a test-only seam invoked by dialFailed after its
	// fast-path shutdown check and before it takes mu, so tests can land a
	// Close exactly in that window. Production leaves it nil.
	beforeDialFailed func()

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

// Run supervises the connection until ctx is cancelled: it dials, keeps the
// published WebSocket alive with pings, and redials with exponential backoff
// whenever the connection ends. It returns nil after ctx cancellation — a
// signal is a clean exit for the CLI — but returns a permanent dial error,
// such as a TLS certificate verification failure, without retrying.
//
// Run owns the reconnect policy only. dialOnce owns each connection's single
// reader, so Run waits for the current generation's error channel to close and
// never starts a read goroutine of its own.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.backoffFloor()

	// A cancelled context must also release a write stuck on a peer that
	// stopped reading: without this, Run could sit inside a ping for the whole
	// write timeout. Close is idempotent, so racing stop is harmless.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-finished:
		}
	}()

	// stop ends the supervisor: every exit is terminal, so it tears the client
	// down before returning. Close publishes "closed", which is what State
	// reports from then on.
	stop := func() error {
		c.Close()
		return nil
	}

	for {
		if ctx.Err() != nil {
			return stop()
		}

		// No connection is published while dialing. A failed dial publishes
		// "reconnecting" from dialFailed; a successful one publishes
		// "connected" from dialOnce. setOpenState keeps "closed" terminal if a
		// concurrent Close wins this race.
		c.setOpenState(stateConnecting)

		// dialOnce has no handshake timeout of its own, so bound each attempt:
		// a peer that accepts TCP and then stalls must not wedge the
		// supervisor.
		dialCtx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
		err := c.dialOnce(dialCtx)
		cancel()

		if err != nil {
			switch {
			case ctx.Err() != nil, errors.Is(err, net.ErrClosed):
				// The attempt was cancelled, or the client was closed from
				// outside Run; neither is an error.
				return stop()
			case isPermanentDialError(err):
				// Certificate problems are configuration errors: retrying can
				// never succeed. Tear the client down like any other exit —
				// Send/Recv/State must not keep behaving as if a supervisor
				// were still reconnecting — then hand the error to the caller.
				_ = stop()
				return err
			}
			// Transient failure: back off and retry.
		} else {
			// A successful connection resets the backoff so the next drop
			// retries promptly.
			backoff = c.backoffFloor()

			// The reader belongs to dialOnce; Run only waits for this
			// generation to retire. failConn closes the channel, and a
			// connection that died before this snapshot leaves it already
			// closed, so both cases read as "the connection ended".
			c.mu.Lock()
			errCh := c.readErrCh
			c.mu.Unlock()

			var ticker *time.Ticker
			var ping <-chan time.Time
			if c.cfg.Keepalive > 0 {
				ticker = time.NewTicker(c.cfg.Keepalive)
				ping = ticker.C
			}

			c.waitConn(ctx, errCh, ping)

			if ticker != nil {
				ticker.Stop()
			}

			if ctx.Err() != nil {
				return stop()
			}
			select {
			case <-c.done:
				// Close was called from outside Run.
				return stop()
			default:
			}
		}

		delay, next := c.backoff(backoff)
		backoff = next

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stop()
		case <-timer.C:
		}
	}
}

// waitConn parks until the connection behind errCh ends, ctx is cancelled, or
// the client is closed, sending one keepalive ping per ping tick. It never
// reads from the WebSocket: dialOnce owns the reader.
func (c *Client) waitConn(ctx context.Context, errCh <-chan struct{}, ping <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-errCh:
			return
		case <-ping:
			c.ping()
		}
	}
}

// backoffFloor is the first reconnect delay: BackoffMin, clamped to BackoffMax
// so a misconfigured pair cannot delay a retry past the cap.
func (c *Client) backoffFloor() time.Duration {
	if c.cfg.BackoffMin > c.cfg.BackoffMax {
		return c.cfg.BackoffMax
	}
	return c.cfg.BackoffMin
}

// backoff returns the jittered delay before the next dial and the step after
// it. The delay is ±20% around current so a fleet of clients does not retry in
// lockstep; next doubles current, capped at BackoffMax.
func (c *Client) backoff(current time.Duration) (delay, next time.Duration) {
	delay = current
	if jitter := 0.2 * (2*rand.Float64() - 1); jitter != 0 {
		delay = time.Duration(float64(current) * (1 + jitter))
	}

	next = c.cfg.BackoffMax
	if current < c.cfg.BackoffMax/2 {
		next = current * 2
	}
	return delay, next
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

// ping sends one keepalive ping. It shares writeMu and writeFrame's deadline
// with data frames, so a stalled peer cannot pin it, and gorilla's one-writer
// rule holds. A failed ping retires the connection exactly like a failed
// write, which wakes Run to reconnect.
func (c *Client) ping() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	conn, gen := c.conn, c.gen
	c.mu.Unlock()
	if conn == nil {
		return
	}

	if err := writeFrame(conn, websocket.PingMessage, nil, c.writeTimeout); err != nil {
		c.failConn(conn, gen, err)
	}
}

// Recv copies the next received datagram into buf and returns its length. If
// the frame is larger than buf the frame is discarded and an error returned
// rather than truncating a datagram. After Close, Recv returns an error
// wrapping net.ErrClosed. Closing stop interrupts the call the same way: it
// returns net.ErrClosed without closing the client, so a caller can abandon a
// wait (wgbind uses this to make conn.Bind.Close prompt) and keep the
// connection usable.
func (c *Client) Recv(buf []byte, stop <-chan struct{}) (int, error) {
	for {
		// Prefer the closed signal over whatever happens to be buffered: once
		// Close returns, callers must observe net.ErrClosed deterministically.
		select {
		case <-c.done:
			return 0, fmt.Errorf("wstunnel: receive: %w", net.ErrClosed)
		case <-stop:
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
		case <-stop:
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

// Close tears down the connection and is terminal: afterwards State reports
// "closed" and nothing will reconnect. It is idempotent, and Recv afterwards
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

	// Outside the once so a repeated Close still ends in the terminal state:
	// a supervisor that raced this call may have published "connecting".
	c.setState(stateClosed)
	return nil
}

// State returns the connection state: "connecting" while an attempt is in
// progress, "connected" while a live WebSocket is published, "reconnecting"
// while waiting to retry after a failure, and "closed" once Close has been
// called (nothing will reconnect).
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

// setOpenState publishes state unless the client is already closed, so
// "closed" stays terminal even when the supervisor's state write races Close.
func (c *Client) setOpenState(state string) {
	c.stateMu.Lock()
	select {
	case <-c.done:
	default:
		c.state = state
	}
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
	//
	// A Close that raced the handshake passed the check above only because
	// nothing was published yet; recheck under mu, which Close takes after
	// closing done. Publishing here without the recheck would leak a live
	// socket and reader that Close never sees.
	if c.beforePublish != nil {
		c.beforePublish()
	}

	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("wstunnel: dial %s: %w", endpoint, net.ErrClosed)
	default:
	}

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

	if c.beforeDialFailed != nil {
		c.beforeDialFailed()
	}

	// The idle check and the state change are atomic with respect to a
	// concurrent successful dial, which publishes conn and "connected" under
	// the same lock. setOpenState re-checks done under stateMu, so a Close that
	// completed between the fast-path check above and this lock still leaves
	// "closed" terminal.
	c.mu.Lock()
	if c.conn == nil {
		c.setOpenState(stateReconnecting)
	}
	c.mu.Unlock()
}

// isPermanentDialError reports whether a dial error is a certificate problem
// that no amount of retrying can fix. Run returns such an error to the caller
// verbatim instead of backing off forever.
func isPermanentDialError(err error) bool {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}

	// Older and lower-level paths surface the x509 error directly. The x509
	// types implement error on the value receiver, hence value targets.
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}
	var invalid x509.CertificateInvalidError
	return errors.As(err, &invalid)
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
