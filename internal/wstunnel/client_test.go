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
