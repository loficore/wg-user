//go:build interop

package wstunnel

// Interoperability tests against the real wstunnel v11 server.
//
// Unlike client_test.go, these drive the official Rust server binary (see
// WSTUNNEL_SERVER_BIN) over real TLS and real UDP, so they prove the wire
// format rather than our own reading of it. They are gated behind the
// "interop" build tag because they need that binary and bind local ports:
//
//	make interop
//
// Every test skips (never fails) when WSTUNNEL_SERVER_BIN is unset, so an
// accidental `go test -tags interop ./...` on a machine without the binary
// does not turn into a red build.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// interopReadyTimeout bounds how long the server may take to listen, and how
// long the client may take to complete its handshake.
const interopReadyTimeout = 10 * time.Second

// requireInteropServerBin returns the path to the official server binary, or
// skips the test when it is not configured.
func requireInteropServerBin(t *testing.T) string {
	t.Helper()

	bin := os.Getenv("WSTUNNEL_SERVER_BIN")
	if bin == "" {
		t.Skip("WSTUNNEL_SERVER_BIN not set")
	}
	return bin
}

// logBuffer collects a child process's combined output. The process writes
// from its own goroutine while the test may read it on failure, so access is
// serialised.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// interopServer is one running `wstunnel server` process.
type interopServer struct {
	// addr is the "127.0.0.1:<port>" the server listens on.
	addr string
	logs *logBuffer
}

// startInteropServer launches the real server with the given --restrict-to
// destination and returns once it accepts TCP connections on its port.
//
// --websocket-mask-frame is mandatory for this client, not cosmetic: gorilla
// masks every client frame as RFC 6455 requires, and wstunnel 11 only unmasks
// incoming frames when this flag is set (fastwebsockets' ReadHalf unmasks iff
// role == Server && auto_apply_mask). Without it the server forwards the
// still-masked payload to UDP and every datagram arrives XORed with the 4-byte
// WebSocket mask key. The flag only changes the client-to-server direction;
// the server never masks its own frames, so gorilla accepts the replies.
//
// The port is picked by binding 127.0.0.1:0 and closing the listener, which
// leaves a small window in which somebody else could take it; if the server
// cannot bind, it is started once more on a fresh port.
func startInteropServer(t *testing.T, restrictTo string) *interopServer {
	t.Helper()

	bin := requireInteropServerBin(t)

	for attempt := 0; attempt < 2; attempt++ {
		port := freeTCPPort(t)
		addr := fmt.Sprintf("127.0.0.1:%d", port)

		logs := &logBuffer{}
		cmd := exec.Command(bin, "server", "wss://"+addr,
			"--restrict-to", restrictTo,
			"--websocket-mask-frame",
		)
		cmd.Stdout = logs
		cmd.Stderr = logs

		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", bin, err)
		}

		// Kill and reap the child whatever happens next, and surface its
		// output when the test failed: without the server's log, a protocol
		// mismatch looks like a bare timeout.
		srv := &interopServer{addr: addr, logs: logs}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if t.Failed() {
				t.Logf("wstunnel server output (%s):\n%s", addr, logs.String())
			}
		})

		if waitTCP(addr, interopReadyTimeout) {
			return srv
		}

		// Not listening: either the port was stolen or the server rejected
		// its arguments. Retry once on a fresh port.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if attempt == 1 {
			t.Fatalf("wstunnel server did not listen on %s within %s; output:\n%s",
				addr, interopReadyTimeout, logs.String())
		}
		t.Logf("wstunnel server failed to listen on %s, retrying:\n%s", addr, logs.String())
	}

	panic("unreachable")
}

// freeTCPPort returns a port that was free a moment ago on 127.0.0.1.
func freeTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release TCP port %d: %v", port, err)
	}
	return port
}

// waitTCP returns whether addr accepts a TCP connection before the deadline,
// polling every 50 ms. A TCP accept is enough: the server binds after its TLS
// setup and before it can serve an upgrade.
func waitTCP(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// udpEcho is a datagram echo server on 127.0.0.1: one packet in, the same
// bytes out to the sender.
type udpEcho struct {
	addr string

	mu       sync.Mutex
	received int
}

// startUDPEcho starts the echo server and stops it in t.Cleanup.
func startUDPEcho(t *testing.T) *udpEcho {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP echo: %v", err)
	}
	echo := &udpEcho{addr: pc.LocalAddr().String()}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, maxFrameSize)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				// Closed by cleanup.
				return
			}

			echo.mu.Lock()
			echo.received++
			echo.mu.Unlock()

			// One datagram in, one datagram out. WriteTo copies the payload
			// before returning, so reusing buf is safe.
			if _, err := pc.WriteTo(buf[:n], from); err != nil {
				return
			}
		}
	}()

	return echo
}

func (e *udpEcho) datagrams() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.received
}

// startInteropClient runs a Client against the real server and stops it in
// t.Cleanup. runDone is closed when Run returns; runErr must only be read
// after that.
func startInteropClient(t *testing.T, serverAddr, target string) (c *Client, runDone <-chan struct{}, runErr func() error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	client := New(Config{
		URL:       "wss://" + serverAddr,
		Target:    target,
		Insecure:  true,
		Keepalive: 25 * time.Second,
	})

	done := make(chan struct{})
	var err error
	go func() {
		err = client.Run(ctx)
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("client Run did not return within 5s of cancellation")
		}
	})

	return client, done, func() error { return err }
}

// waitConnected blocks until the client reports "connected", failing the test
// if Run returns first or the deadline passes.
func waitConnected(t *testing.T, c *Client, runDone <-chan struct{}, runErr func() error) {
	t.Helper()

	deadline := time.Now().Add(interopReadyTimeout)
	for {
		if c.State() == stateConnected {
			return
		}

		select {
		case <-runDone:
			t.Fatalf("client Run returned before reaching %q: %v", stateConnected, runErr())
		default:
		}

		if !time.Now().Before(deadline) {
			t.Fatalf("client State = %q after %s, want %q", c.State(), interopReadyTimeout, stateConnected)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recvWithin calls Recv and gives up after timeout. The third result reports
// whether a frame or an error arrived; false means the channel stayed silent.
// Closing stop on the way out releases the Recv goroutine either way.
func recvWithin(c *Client, buf []byte, timeout time.Duration) (n int, err error, arrived bool) {
	stop := make(chan struct{})
	defer close(stop)

	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := c.Recv(buf, stop)
		ch <- result{n: n, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.n, r.err, true
	case <-timer.C:
		return 0, nil, false
	}
}

// waitLogContains polls the server's output until it contains want. It is how
// the restrict test proves the real server saw and judged the tunnel, instead
// of inferring it from silence.
func waitLogContains(logs *logBuffer, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// firstMismatch returns the index of the first differing byte, or -1.
func firstMismatch(got, want []byte) int {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return i
		}
	}
	if len(got) != len(want) {
		return min(len(got), len(want))
	}
	return -1
}

// TestInterop_UDPRoundTripThroughRealServer sends ten random-length datagrams
// through the real server to a real UDP echo server and checks that every
// byte comes back unchanged. One WebSocket frame must be one datagram: a
// frame boundary bug shows up as a wrong length or mismatched payload.
//
// The server is started with --websocket-mask-frame (see startInteropServer),
// and that flag is load-bearing, not noise: gorilla masks every client frame
// as RFC 6455 requires, wstunnel 11 unmasks incoming frames only when that
// flag is set, and without it the handshake still succeeds while every
// datagram is forwarded XORed with the 4-byte WebSocket mask key — a silent
// data-plane failure, not a visible handshake error. Do not drop the flag.
func TestInterop_UDPRoundTripThroughRealServer(t *testing.T) {
	echo := startUDPEcho(t)
	srv := startInteropServer(t, echo.addr)
	c, runDone, runErr := startInteropClient(t, srv.addr, echo.addr)

	waitConnected(t, c, runDone, runErr)

	for i := 0; i < 10; i++ {
		size := 1 + rand.Intn(1200)
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("payload %d: read random bytes: %v", i, err)
		}

		if err := c.Send(payload); err != nil {
			t.Fatalf("payload %d (%d bytes): send: %v\nserver output:\n%s", i, size, err, srv.logs.String())
		}

		// Spare room so a coalesced or duplicated frame fails the byte
		// comparison instead of looking like a short read.
		buf := make([]byte, maxFrameSize)
		n, err, arrived := recvWithin(c, buf, 5*time.Second)
		if !arrived {
			t.Fatalf("payload %d (%d bytes): no echo within 5s\nserver output:\n%s", i, size, srv.logs.String())
		}
		if err != nil {
			t.Fatalf("payload %d (%d bytes): receive: %v\nserver output:\n%s", i, size, err, srv.logs.String())
		}
		if n != size || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("payload %d: echoed %d bytes, want %d identical bytes; first difference at byte %d\nserver output:\n%s",
				i, n, size, firstMismatch(buf[:n], payload), srv.logs.String())
		}
	}
}

// TestInterop_TargetOutsideRestrictIsDropped asks the real server to forward
// to an echo server it was not told to allow. --restrict-to must refuse: the
// datagram may be answered with a closed connection or by silence, but it
// must never be echoed.
func TestInterop_TargetOutsideRestrictIsDropped(t *testing.T) {
	allowed := startUDPEcho(t)
	outside := startUDPEcho(t)
	srv := startInteropServer(t, allowed.addr)

	c, _, _ := startInteropClient(t, srv.addr, outside.addr)

	// wstunnel 11 answers a forbidden destination by closing the tunnel right
	// after the upgrade, so the client can flip to "connected" and back before
	// a poll sees it. Wait for the server's own verdict instead: it proves the
	// handshake, path and JWT were accepted and that the refusal came from
	// --restrict-to, not from a broken client.
	const rejection = "Rejecting connection with not allowed destination"
	if !waitLogContains(srv.logs, rejection, interopReadyTimeout) {
		t.Fatalf("server never logged %q for a tunnel to %s within %s; --restrict-to was not exercised\nserver output:\n%s",
			rejection, outside.addr, interopReadyTimeout, srv.logs.String())
	}

	// Send while the tunnel may or may not be up: Send drops the datagram when
	// no connection is published, and a closed tunnel makes it fail. Both are
	// acceptable; only an echo would be a failure.
	if err := c.Send([]byte("restrict-to must drop this datagram")); err != nil {
		t.Logf("send after rejection: %v (acceptable: the server closed the tunnel)", err)
	}

	// Watch the whole 2s window rather than one Recv: a close frame can arrive
	// first, and the assertion is about what never arrives.
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		buf := make([]byte, maxFrameSize)
		n, err, arrived := recvWithin(c, buf, remaining)
		if !arrived {
			// Silence for the rest of the window.
			break
		}
		if err == nil {
			t.Fatalf("server relayed %d bytes to %s, which is outside --restrict-to %s", n, outside.addr, allowed.addr)
		}
		t.Logf("tunnel closed as expected: %v", err)
		break
	}

	if got := outside.datagrams(); got != 0 {
		t.Fatalf("echo server outside --restrict-to received %d datagram(s), want 0", got)
	}

	if got := allowed.datagrams(); got != 0 {
		t.Fatalf("allowed echo server received %d datagram(s) addressed elsewhere, want 0", got)
	}
}
