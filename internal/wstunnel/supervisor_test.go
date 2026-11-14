package wstunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// runHandle is a Client.Run executing in the background. stop cancels the
// supervisor and waits for it at most once, so both the test and the cleanup
// can call it without racing on the result.
type runHandle struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
	err    error
}

// startRun starts c.Run on its own goroutine and guarantees it is stopped and
// the client closed when the test ends.
func startRun(t *testing.T, c *Client) *runHandle {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	h := &runHandle{cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- c.Run(ctx) }()

	t.Cleanup(func() {
		if err := h.stop(); err != nil {
			t.Errorf("Run returned %v after cancellation, want nil", err)
		}
		c.Close()
	})
	return h
}

// stop cancels the supervisor's context and waits for Run to return.
func (h *runHandle) stop() error {
	h.once.Do(func() {
		h.cancel()
		select {
		case h.err = <-h.done:
		case <-time.After(3 * time.Second):
			h.err = errors.New("Run did not return within 3s of cancellation")
		}
	})
	return h.err
}

// pingCountingServer starts a fake server that counts WebSocket pings and
// counts pongs back, keeping every connection open until the client leaves.
func pingCountingServer(t *testing.T) (*fakeServer, *atomic.Int32) {
	t.Helper()

	var pings atomic.Int32
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(appData string) error {
			pings.Add(1)
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	return fs, &pings
}

func TestRun_ReconnectsAfterServerClose(t *testing.T) {
	var upgrades atomic.Int32
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		if upgrades.Add(1) == 1 {
			// First connection: echo exactly one datagram, then hang up. The
			// supervisor must notice and redial.
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		// Later connections stay up until the client closes them.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	c := New(Config{
		URL:        fs.url(false),
		Target:     "10.0.0.2:51820",
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	// The first connection cannot end until a packet flows, so this also pins
	// that Run dials eagerly and publishes "connected".
	waitFor(t, "the client to connect", func() bool {
		return c.State() == "connected"
	})
	if err := c.Send([]byte("hello")); err != nil {
		t.Fatalf("Send on the first connection: %v", err)
	}

	waitFor(t, "the server to see a redial", func() bool {
		return fs.connections() >= 2
	})
	waitFor(t, "the client to be connected again", func() bool {
		return c.State() == "connected"
	})

	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if got := c.State(); got != "reconnecting" && got != "connecting" {
		t.Errorf("State after Run = %q, want reconnecting or connecting", got)
	}
}

func TestRun_SendsPingWhenKeepaliveElapses(t *testing.T) {
	fs, pings := pingCountingServer(t)

	c := New(Config{
		URL:        fs.url(false),
		Target:     "10.0.0.2:51820",
		Keepalive:  50 * time.Millisecond,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	waitFor(t, "the client to connect", func() bool {
		return c.State() == "connected"
	})

	start := time.Now()
	waitFor(t, "the client to send a keepalive ping", func() bool {
		return pings.Load() >= 1
	})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("first ping took %v with Keepalive=50ms", elapsed)
	}

	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestRun_KeepaliveZeroSendsNoPings(t *testing.T) {
	fs, pings := pingCountingServer(t)

	c := New(Config{
		URL:        fs.url(false),
		Target:     "10.0.0.2:51820",
		Keepalive:  0,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	waitFor(t, "the client to connect", func() bool {
		return c.State() == "connected"
	})
	time.Sleep(150 * time.Millisecond)

	if n := pings.Load(); n != 0 {
		t.Errorf("server saw %d pings with Keepalive=0, want 0", n)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestRun_DropsPacketsWhileReconnecting(t *testing.T) {
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		// Hang up immediately: the supervisor spends the test backing off.
		_ = conn.Close()
	})

	c := New(Config{
		URL:    fs.url(false),
		Target: "10.0.0.2:51820",
		// A long, fixed backoff leaves a wide window to Send inside.
		BackoffMin: 500 * time.Millisecond,
		BackoffMax: 500 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	waitFor(t, "the first connection loss", func() bool {
		return fs.connections() >= 1 && c.State() == "reconnecting"
	})

	start := time.Now()
	for i := 0; i < 20; i++ {
		if err := c.Send([]byte("dropped")); err != nil {
			t.Fatalf("Send while reconnecting = %v, want nil", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("20 Sends while reconnecting blocked for %v", elapsed)
	}

	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestRun_RedialsWhenConnectionDropsBeforeWait(t *testing.T) {
	// Every connection is closed immediately after the upgrade, so the
	// generation's error channel can already be closed by the time Run
	// snapshots it. Run must still treat that as "connection ended".
	fs := newFakeServer(t, func(conn *websocket.Conn) {
		_ = conn.Close()
	})

	c := New(Config{
		URL:        fs.url(false),
		Target:     "10.0.0.2:51820",
		BackoffMin: 5 * time.Millisecond,
		BackoffMax: 10 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	waitFor(t, "three upgrades", func() bool {
		return fs.connections() >= 3
	})

	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	fs := newFakeServer(t, nil)

	c := New(Config{
		URL:        fs.url(false),
		Target:     "10.0.0.2:51820",
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
	})
	t.Cleanup(func() { c.Close() })
	h := startRun(t, c)

	waitFor(t, "the client to connect", func() bool {
		return c.State() == "connected"
	})

	start := time.Now()
	if err := h.stop(); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Run took %v to return after cancellation", elapsed)
	}
	if got := c.State(); got != "reconnecting" && got != "connecting" {
		t.Errorf("State after Run = %q, want reconnecting or connecting", got)
	}

	// A cancelled Run closes the client, so its Close contract holds without
	// the caller having to close it again.
	if err := c.Send([]byte("late")); err != nil {
		t.Errorf("Send after Run returned %v, want nil", err)
	}
	if _, err := c.Recv(make([]byte, 8)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Recv after Run = %v, want net.ErrClosed", err)
	}
}

func TestRun_ReturnsFatalOnCertificateError(t *testing.T) {
	var handlers atomic.Int32
	upgrader := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	// The expected handshake failure would otherwise be logged to stderr.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	c := New(Config{
		URL:        "wss" + strings.TrimPrefix(srv.URL, "https"),
		Target:     "10.0.0.2:51820",
		BackoffMin: 10 * time.Second,
		BackoffMax: 30 * time.Second,
	})
	t.Cleanup(func() { c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A certificate error is permanent. With a 10s minimum backoff, returning
	// well inside the 5s deadline proves Run did not schedule a retry.
	start := time.Now()
	err := c.Run(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Run returned nil for an untrusted server certificate")
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &certErr) && !errors.As(err, &unknownAuthority) {
		t.Fatalf("Run error %v is not a certificate verification error", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v to fail on a certificate error, want an immediate fatal return", elapsed)
	}
	if n := handlers.Load(); n != 0 {
		t.Errorf("TLS server handlers ran %d times, want 0 (verification must fail first)", n)
	}
	if got := c.State(); got != "reconnecting" && got != "connecting" {
		t.Errorf("State after a fatal dial error = %q, want reconnecting or connecting", got)
	}
}

func TestBackoff_JittersWithinBoundsAndCaps(t *testing.T) {
	c := New(Config{BackoffMin: 100 * time.Millisecond, BackoffMax: 400 * time.Millisecond})

	tests := []struct {
		current, next time.Duration
	}{
		{100 * time.Millisecond, 200 * time.Millisecond},
		{200 * time.Millisecond, 400 * time.Millisecond},
		{400 * time.Millisecond, 400 * time.Millisecond}, // capped
	}
	for _, tt := range tests {
		for i := 0; i < 50; i++ {
			delay, next := c.backoff(tt.current)
			if next != tt.next {
				t.Fatalf("backoff(%v) next = %v, want %v", tt.current, next, tt.next)
			}
			// ±20% jitter, with a little slack for duration truncation.
			lo := time.Duration(float64(tt.current) * 0.79)
			hi := time.Duration(float64(tt.current) * 1.21)
			if delay < lo || delay > hi {
				t.Fatalf("backoff(%v) delay = %v, want within [%v, %v]", tt.current, delay, lo, hi)
			}
		}
	}
}
