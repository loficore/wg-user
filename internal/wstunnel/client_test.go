package wstunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeServer is an httptest server that upgrades every request to a WebSocket
// and records the handshake details this package is responsible for. By
// default it echoes every binary frame back unchanged.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	protocols []string
	auths     []string
	paths     []string
	conns     int
	closed    int
	frames    [][]byte
}

// newFakeServer starts the fake server. handle, when non-nil, replaces the
// default echo loop for every accepted connection.
func newFakeServer(t *testing.T, handle func(*websocket.Conn)) *fakeServer {
	t.Helper()

	fs := &fakeServer{t: t}
	upgrader := websocket.Upgrader{}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.protocols = append(fs.protocols, r.Header.Get("Sec-WebSocket-Protocol"))
		fs.auths = append(fs.auths, r.Header.Get("Authorization"))
		fs.paths = append(fs.paths, r.URL.Path)
		fs.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("server: upgrade: %v", err)
			return
		}

		fs.mu.Lock()
		fs.conns++
		fs.mu.Unlock()
		defer func() {
			fs.mu.Lock()
			fs.closed++
			fs.mu.Unlock()
		}()

		if handle != nil {
			handle(conn)
			return
		}
		fs.echo(conn)
	}))
	t.Cleanup(fs.srv.Close)

	return fs
}

// echo returns every frame it reads, unchanged. One frame in, one frame out:
// the server must never coalesce datagrams, so neither must the test helper.
func (fs *fakeServer) echo(conn *websocket.Conn) {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.frames = append(fs.frames, data)
		fs.mu.Unlock()
		if err := conn.WriteMessage(mt, data); err != nil {
			return
		}
	}
}

// url is the ws:// form of the server address, optionally with a trailing
// slash so tests can pin the double-slash behaviour.
func (fs *fakeServer) url(trailingSlash bool) string {
	u := "ws" + strings.TrimPrefix(fs.srv.URL, "http")
	if trailingSlash {
		u += "/"
	}
	return u
}

func (fs *fakeServer) protocol(t *testing.T) string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.protocols) == 0 {
		t.Fatal("server: no handshake recorded")
	}
	return fs.protocols[0]
}

func (fs *fakeServer) auth(t *testing.T) string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.auths) == 0 {
		t.Fatal("server: no handshake recorded")
	}
	return fs.auths[0]
}

func (fs *fakeServer) path(t *testing.T) string {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.paths) == 0 {
		t.Fatal("server: no handshake recorded")
	}
	return fs.paths[0]
}

func (fs *fakeServer) connections() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.conns
}

func (fs *fakeServer) closedConnections() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.closed
}

func (fs *fakeServer) received() [][]byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([][]byte(nil), fs.frames...)
}

// waitFor polls cond until it is true or the deadline passes, so tests never
// depend on a fixed sleep for a condition that may settle earlier.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// dialTestClient dials a client against the fake server once and fails the
// test if the handshake does not succeed.
func dialTestClient(t *testing.T, fs *fakeServer, cfg Config) *Client {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = fs.url(false)
	}
	if cfg.Target == "" {
		cfg.Target = "10.0.0.2:51820"
	}
	c := New(cfg)
	t.Cleanup(func() { c.Close() })
	if err := c.dialOnce(context.Background()); err != nil {
		t.Fatalf("dialOnce: %v", err)
	}
	return c
}

func TestDialOnce_SendsTunnelJWT(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{URL: fs.url(true), Path: "v1"})

	if got := fs.path(t); got != "/v1/events" {
		t.Errorf("request path = %q, want %q", got, "/v1/events")
	}

	const prefix = "v1, authorization.bearer."
	header := fs.protocol(t)
	if !strings.HasPrefix(header, prefix) {
		t.Fatalf("Sec-WebSocket-Protocol = %q, want prefix %q", header, prefix)
	}

	tok := strings.TrimPrefix(header, prefix)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
	}

	var claim struct {
		R  string `json:"r"`
		RP uint16 `json:"rp"`
	}
	if err := json.Unmarshal(segment(t, "payload", parts[1]), &claim); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	if claim.R != "10.0.0.2" || claim.RP != 51820 {
		t.Errorf("token target = %s:%d, want 10.0.0.2:51820", claim.R, claim.RP)
	}

	if got := c.State(); got != "connected" {
		t.Errorf("State = %q, want %q", got, "connected")
	}
}

func TestDialOnce_DefaultsToV1Path(t *testing.T) {
	fs := newFakeServer(t, nil)
	dialTestClient(t, fs, Config{Path: ""})

	if got := fs.path(t); got != "/v1/events" {
		t.Errorf("request path = %q, want %q", got, "/v1/events")
	}
}

func TestDialOnce_SendsBasicAuthOnlyWhenBothSet(t *testing.T) {
	tests := []struct {
		name, user, pass string
	}{
		{"both", "alice", "s3cret"},
		{"user only", "alice", ""},
		{"password only", "", "s3cret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeServer(t, nil)
			dialTestClient(t, fs, Config{Username: tt.user, Password: tt.pass})

			want := ""
			if tt.user != "" && tt.pass != "" {
				want = "Basic " + base64.StdEncoding.EncodeToString([]byte(tt.user+":"+tt.pass))
			}
			if got := fs.auth(t); got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}
		})
	}
}

func TestSendRecv_PreservesDatagramBoundaries(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	for _, size := range []int{1, 100, 1300} {
		pkt := make([]byte, size)
		for i := range pkt {
			pkt[i] = byte(i)
		}
		if err := c.Send(pkt); err != nil {
			t.Fatalf("Send(%d bytes): %v", size, err)
		}

		buf := make([]byte, 2000)
		n, err := c.Recv(buf)
		if err != nil {
			t.Fatalf("Recv(%d bytes): %v", size, err)
		}
		if n != size {
			t.Fatalf("Recv returned %d bytes, want %d", n, size)
		}
		if string(buf[:n]) != string(pkt) {
			t.Fatalf("Recv payload mismatch for %d-byte datagram", size)
		}
	}

	waitFor(t, "server to record three frames", func() bool {
		return len(fs.received()) == 3
	})
	for i, size := range []int{1, 100, 1300} {
		if got := len(fs.received()[i]); got != size {
			t.Errorf("server frame %d has %d bytes, want %d", i, got, size)
		}
	}
}

func TestSend_WhenNotConnectedDropsWithoutError(t *testing.T) {
	fs := newFakeServer(t, nil)

	c := New(Config{URL: fs.url(false), Target: "10.0.0.2:51820"})
	t.Cleanup(func() { c.Close() })

	if err := c.Send([]byte("dropped")); err != nil {
		t.Fatalf("Send before dial returned %v, want nil", err)
	}
	if n := fs.connections(); n != 0 {
		t.Errorf("server saw %d connections, want 0", n)
	}

	// After a close, Send must keep its drop-without-error contract too, and
	// must not reach the server.
	if err := c.dialOnce(context.Background()); err != nil {
		t.Fatalf("dialOnce: %v", err)
	}
	if err := c.Send([]byte("before close")); err != nil {
		t.Fatalf("Send while connected: %v", err)
	}
	waitFor(t, "server to record the connected frame", func() bool {
		return len(fs.received()) == 1
	})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Send([]byte("after close")); err != nil {
		t.Fatalf("Send after Close returned %v, want nil", err)
	}
	if n := len(fs.received()); n != 1 {
		t.Errorf("server recorded %d frames, want 1 (Send after Close must drop)", n)
	}
}

func TestRecv_AfterCloseReturnsErrClosed(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err := c.Recv(make([]byte, 2000))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Recv after Close = %v, want net.ErrClosed", err)
	}
}

func TestDialOnce_FailsOnNonUpgradeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a websocket", http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := New(Config{
		URL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		Target: "10.0.0.2:51820",
	})
	t.Cleanup(func() { c.Close() })

	err := c.dialOnce(context.Background())
	if err == nil {
		t.Fatal("dialOnce returned nil for a non-101 response")
	}
	if !strings.HasPrefix(err.Error(), "wstunnel:") {
		t.Errorf("error %q is not prefixed with %q", err, "wstunnel:")
	}
	if got := c.State(); got != "reconnecting" {
		t.Errorf("State after failed dial = %q, want %q", got, "reconnecting")
	}
}

func TestRecv_RejectsOversizeFrame(t *testing.T) {
	const frameSize = 70000
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		if err := conn.WriteMessage(websocket.BinaryMessage, make([]byte, frameSize)); err != nil {
			return
		}
		// Keep the handler alive until the client reacts.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	c := dialTestClient(t, fs, Config{})

	_, err := c.Recv(make([]byte, 2000))
	if err == nil {
		t.Fatal("Recv accepted a 70000-byte frame past the 65536-byte read limit")
	}
	if !strings.HasPrefix(err.Error(), "wstunnel:") {
		t.Errorf("error %q is not prefixed with %q", err, "wstunnel:")
	}

	waitFor(t, "the oversize connection to be torn down", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn == nil
	})
}

func TestRecv_RejectsFrameLargerThanBuffer(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	if err := c.Send(make([]byte, 1300)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, err := c.Recv(make([]byte, 100))
	if err == nil {
		t.Fatal("Recv truncated a frame larger than the caller's buffer")
	}
	const want = "wstunnel: frame of 1300 bytes exceeds buffer of 100"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// newStalledServer starts a fake server that upgrades but never reads, so the
// client's TCP send buffer eventually fills and writes block until their
// deadline.
func newStalledServer(t *testing.T) *fakeServer {
	t.Helper()
	stop := make(chan struct{})
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		<-stop
	})
	// Registered after the server's own cleanup, so it runs first (LIFO) and
	// lets the blocked handler return before httptest.Server.Close.
	t.Cleanup(func() { close(stop) })
	return fs
}

func TestSend_BoundsWriteWhenPeerStalls(t *testing.T) {
	fs := newStalledServer(t)
	c := dialTestClient(t, fs, Config{})
	c.writeTimeout = 100 * time.Millisecond

	pkt := make([]byte, 1<<20)
	var err error
	start := time.Now()
	for i := 0; i < 200; i++ {
		if err = c.Send(pkt); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("Send never failed against a peer that never reads")
	}
	if !strings.HasPrefix(err.Error(), "wstunnel:") {
		t.Errorf("error %q is not prefixed with %q", err, "wstunnel:")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send took %v to give up on a stalled peer, want the write deadline to bound it", elapsed)
	}

	waitFor(t, "the stalled connection to be torn down", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.conn == nil
	})
	if err := c.Send([]byte("dropped")); err != nil {
		t.Errorf("Send after teardown = %v, want nil", err)
	}
}

func TestClose_UnblocksStalledWrite(t *testing.T) {
	fs := newStalledServer(t)
	c := dialTestClient(t, fs, Config{})
	// Long enough that only Close can release the writer.
	c.writeTimeout = 30 * time.Second

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		pkt := make([]byte, 1<<20)
		for i := 0; i < 500; i++ {
			if err := c.Send(pkt); err != nil {
				return
			}
		}
	}()

	// Let the writer fill the socket and block inside WriteMessage.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close blocked for %v behind a stalled write", elapsed)
	}

	select {
	case <-sendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Send stayed blocked after Close")
	}
}

func TestReadLoop_StaleFailureIgnoredAfterRedial(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	c.mu.Lock()
	oldConn, oldGen := c.conn, c.gen
	c.mu.Unlock()

	if err := c.dialOnce(context.Background()); err != nil {
		t.Fatalf("second dialOnce: %v", err)
	}
	if n := fs.connections(); n != 2 {
		t.Fatalf("server saw %d connections, want 2", n)
	}

	// Simulate the previous connection's reader failing only now. It must be
	// ignored: the newer connection owns the error channel and the state.
	if c.failConn(oldConn, oldGen, errors.New("stale reader")) {
		t.Fatal("failConn accepted a retired generation")
	}
	if got := c.State(); got != "connected" {
		t.Errorf("State = %q after a stale failure, want %q", got, "connected")
	}

	if err := c.Send([]byte("live")); err != nil {
		t.Fatalf("Send on the new connection: %v", err)
	}
	buf := make([]byte, 16)
	n, err := c.Recv(buf)
	if err != nil {
		t.Fatalf("Recv on the new connection was poisoned by the stale reader: %v", err)
	}
	if n != len("live") || string(buf[:n]) != "live" {
		t.Errorf("Recv = %q, want %q", buf[:n], "live")
	}
}

func TestDialOnce_ClosesPreviousConnection(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	c.mu.Lock()
	oldConn := c.conn
	c.mu.Unlock()

	if err := c.dialOnce(context.Background()); err != nil {
		t.Fatalf("second dialOnce: %v", err)
	}

	waitFor(t, "the previous connection's handler to finish", func() bool {
		return fs.closedConnections() >= 1
	})

	if err := oldConn.WriteMessage(websocket.BinaryMessage, []byte("old")); err == nil {
		t.Error("previous connection is still writable after the redial")
	}
}

func TestRecv_SurvivesRedialWithoutNilError(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := dialTestClient(t, fs, Config{})

	type result struct {
		n   int
		err error
	}
	res := make(chan result, 1)
	go func() {
		n, err := c.Recv(make([]byte, 16))
		res <- result{n: n, err: err}
	}()

	// Give Recv time to park on the first generation's error channel.
	time.Sleep(20 * time.Millisecond)

	// Retiring the generation closes that channel with readErr still nil; Recv
	// must re-snapshot instead of wrapping a nil error.
	if err := c.dialOnce(context.Background()); err != nil {
		t.Fatalf("second dialOnce: %v", err)
	}

	if err := c.Send([]byte("after")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("Recv returned %v after a redial, want the echoed frame", r.err)
		}
		if r.n != len("after") {
			t.Errorf("Recv returned %d bytes, want %d", r.n, len("after"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv stayed parked after a redial")
	}
}

func TestDialOnce_AfterCloseReturnsErrClosed(t *testing.T) {
	fs := newFakeServer(t, nil)

	c := New(Config{URL: fs.url(false), Target: "10.0.0.2:51820"})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := c.dialOnce(context.Background())
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dialOnce after Close = %v, want net.ErrClosed", err)
	}
	if n := fs.connections(); n != 0 {
		t.Errorf("server saw %d connections, want 0 (dialOnce must not dial after Close)", n)
	}
}

func TestDialOnce_CloseDuringPublishDoesNotLeak(t *testing.T) {
	fs := newFakeServer(t, nil)
	c := New(Config{URL: fs.url(false), Target: "10.0.0.2:51820"})

	// Close lands after the handshake, in the window between the done check
	// and the publish critical section. The fresh connection must not be
	// published; it must be closed and dialOnce must report ErrClosed.
	c.beforePublish = func() { _ = c.Close() }

	err := c.dialOnce(context.Background())
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dialOnce = %v, want net.ErrClosed", err)
	}
	if got := c.State(); got == "connected" {
		t.Errorf("State = %q after a Close during publish, want not connected", got)
	}
	if n := fs.connections(); n != 1 {
		t.Fatalf("server saw %d connections, want 1 (the handshake did happen)", n)
	}

	waitFor(t, "the unpublished connection to be closed", func() bool {
		return fs.closedConnections() >= 1
	})

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		t.Error("dialOnce published a connection after Close")
	}
}
