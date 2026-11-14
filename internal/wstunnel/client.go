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
type Client struct {
	cfg Config

	// mu guards conn and serialises every write. gorilla forbids concurrent
	// writers, so data frames (and, later, pings) all take this lock.
	mu   sync.Mutex
	conn *websocket.Conn

	// inbox carries frame payloads from the reader goroutine to Recv.
	inbox chan []byte

	// done is closed by Close and unblocks Recv.
	done      chan struct{}
	closeOnce sync.Once

	// readErr / readErrCh report a reader failure to Recv. readErrCh is
	// replaced by each successful dial so a reconnect can resume receiving;
	// readErr is written before readErrCh is closed.
	readErrMu sync.Mutex
	readErr   error
	readErrCh chan struct{}

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
		cfg:       cfg,
		inbox:     make(chan []byte, inboxCapacity),
		done:      make(chan struct{}),
		readErrCh: make(chan struct{}),
		state:     stateConnecting,
	}
}

// Send writes pkt as one binary frame, i.e. one UDP datagram. When no
// connection is established the datagram is dropped and Send returns nil:
// WireGuard generates traffic faster than a reconnect can complete, and
// blocking there would wedge the tunnel.
func (c *Client) Send(pkt []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, pkt); err != nil {
		return fmt.Errorf("wstunnel: send: %w", err)
	}
	return nil
}

// Recv copies the next received datagram into buf and returns its length. If
// the frame is larger than buf the frame is discarded and an error returned
// rather than truncating a datagram. After Close, Recv returns an error
// wrapping net.ErrClosed.
func (c *Client) Recv(buf []byte) (int, error) {
	// Prefer the closed signal over whatever happens to be buffered: once
	// Close returns, callers must observe net.ErrClosed deterministically.
	select {
	case <-c.done:
		return 0, fmt.Errorf("wstunnel: receive: %w", net.ErrClosed)
	default:
	}

	c.readErrMu.Lock()
	errCh, readErr := c.readErrCh, c.readErr
	c.readErrMu.Unlock()
	if readErr != nil {
		return 0, fmt.Errorf("wstunnel: receive: %w", readErr)
	}

	select {
	case <-c.done:
		return 0, fmt.Errorf("wstunnel: receive: %w", net.ErrClosed)
	case <-errCh:
		c.readErrMu.Lock()
		readErr := c.readErr
		c.readErrMu.Unlock()
		return 0, fmt.Errorf("wstunnel: receive: %w", readErr)
	case frame := <-c.inbox:
		if len(frame) > len(buf) {
			return 0, fmt.Errorf("wstunnel: frame of %d bytes exceeds buffer of %d", len(frame), len(buf))
		}
		return copy(buf, frame), nil
	}
}

// Close tears down the connection. It is idempotent, and Recv afterwards
// reports net.ErrClosed. A close while a dial is in flight is the
// supervisor's concern; the dial either fails or its connection is closed by
// the next Close.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)

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

// dialOnce performs a single WebSocket handshake and starts the reader. It
// does not retry; the supervisor owns backoff. Every error is prefixed with
// "wstunnel:" so callers can print one line.
func (c *Client) dialOnce(ctx context.Context) error {
	endpoint, err := c.endpoint()
	if err != nil {
		c.setState(stateReconnecting)
		return err
	}

	token, err := BuildToken(c.cfg.Target)
	if err != nil {
		c.setState(stateReconnecting)
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
		c.setState(stateReconnecting)
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

	// A fresh connection may follow a failed one, so clear the previous
	// reader error before exposing the connection.
	c.readErrMu.Lock()
	c.readErr = nil
	c.readErrCh = make(chan struct{})
	c.readErrMu.Unlock()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	c.setState(stateConnected)
	go c.readLoop(conn)

	return nil
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

// readLoop moves frames from conn into inbox until conn fails. It is the only
// reader of conn: gorilla allows one concurrent reader and one concurrent
// writer, and Send holds the writer side.
func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			c.readFailed(conn, err)
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

// readFailed records a terminal reader error, detaches the connection and
// wakes Recv.
func (c *Client) readFailed(conn *websocket.Conn, err error) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()

	// The read error already leaves the connection unusable; closing it
	// releases the socket and tells the peer.
	_ = conn.Close()

	select {
	case <-c.done:
		// Close already happened; Recv reports net.ErrClosed, not this error.
		return
	default:
	}

	c.readErrMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.readErrMu.Unlock()

	select {
	case <-c.readErrCh:
	default:
		close(c.readErrCh)
	}

	c.setState(stateReconnecting)
}
