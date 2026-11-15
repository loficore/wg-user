package wstunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// stubLookup replaces the name resolver for one test.
func stubLookup(t *testing.T, lookup func(context.Context, string) ([]string, error)) {
	t.Helper()

	old := lookupHost
	lookupHost = lookup
	t.Cleanup(func() { lookupHost = old })
}

// startListener accepts background connections and reports each one. The
// listener address is always 127.0.0.1, so a test can pretend a hostname
// resolved to it.
func startListener(t *testing.T) (net.Listener, <-chan net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()
	return ln, accepted
}

// listenerPort returns the port of ln's 127.0.0.1 address.
func listenerPort(t *testing.T, ln net.Listener) string {
	t.Helper()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener address %q: %v", ln.Addr(), err)
	}
	return port
}

func TestDialResolved_ConnectsToResolvedIPForHostname(t *testing.T) {
	ln, accepted := startListener(t)

	var lookedUp string
	stubLookup(t, func(_ context.Context, host string) ([]string, error) {
		lookedUp = host
		return []string{"127.0.0.1"}, nil
	})

	conn, err := dialResolved(context.Background(), "tcp", "example.test:"+listenerPort(t, ln))
	if err != nil {
		t.Fatalf("dialResolved() error: %v", err)
	}
	defer conn.Close()

	if lookedUp != "example.test" {
		t.Fatalf("resolved host = %q, want example.test", lookedUp)
	}
	if got, want := conn.RemoteAddr().String(), ln.Addr().String(); got != want {
		t.Fatalf("remote address = %q, want %q (the listener)", got, want)
	}

	select {
	case serverConn := <-accepted:
		_ = serverConn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("listener accepted no connection")
	}
}

func TestDialResolved_IPLiteralSkipsResolver(t *testing.T) {
	ln, accepted := startListener(t)
	stubLookup(t, func(context.Context, string) ([]string, error) {
		t.Fatal("resolver called for an IP literal")
		return nil, nil
	})

	conn, err := dialResolved(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dialResolved() error: %v", err)
	}
	defer conn.Close()

	select {
	case serverConn := <-accepted:
		_ = serverConn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("listener accepted no connection")
	}
}

func TestDialResolved_ResolutionErrorIsReturnedUnchanged(t *testing.T) {
	wantErr := errors.New("lookup example.test on [::1]:53: connection refused")
	stubLookup(t, func(context.Context, string) ([]string, error) {
		return nil, wantErr
	})

	if _, err := dialResolved(context.Background(), "tcp", "example.test:443"); !errors.Is(err, wantErr) {
		t.Fatalf("dialResolved() error = %v, want %v", err, wantErr)
	}
}

// TestDialResolved_AggregatesDialErrors: every resolved address is tried, and a
// failure names all of them instead of only the last one, so a log line shows
// what was attempted.
func TestDialResolved_AggregatesDialErrors(t *testing.T) {
	// Grab a free port, then close the listener: both loopback addresses now
	// refuse the connection immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listenerPort(t, ln)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	stubLookup(t, func(context.Context, string) ([]string, error) {
		return []string{"127.0.0.1", "127.0.0.2"}, nil
	})

	_, err = dialResolved(context.Background(), "tcp", "example.test:"+port)
	if err == nil {
		t.Fatal("dialResolved() = nil error, want a dial failure")
	}
	for _, want := range []string{"127.0.0.1:" + port, "127.0.0.2:" + port} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestDialOnce_UsesResolvedIPForHostname pins the wiring in dialOnce: a URL
// with a hostname must reach the server through the resolved IP. Without the
// NetDialContext hook this dial fails, because nothing on this host can resolve
// example.test.
func TestDialOnce_UsesResolvedIPForHostname(t *testing.T) {
	fs := newFakeServer(t, nil)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(fs.srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split fake server address: %v", err)
	}

	stubLookup(t, func(_ context.Context, host string) ([]string, error) {
		if host != "example.test" {
			t.Errorf("resolved host = %q, want example.test", host)
		}
		return []string{"127.0.0.1"}, nil
	})

	dialTestClient(t, fs, Config{URL: "ws://example.test:" + port})
	if got := fs.connections(); got != 1 {
		t.Fatalf("server connections = %d, want 1", got)
	}
}

// TestDialResolved_KeepsHostnameForSNI is the reason the helper only replaces
// the TCP destination: the certificate is issued for the name, so the TLS
// handshake must still carry it. A TLS listener captures the ClientHello.
func TestDialResolved_KeepsHostnameForSNI(t *testing.T) {
	cert := selfSignedCert(t)

	sni := make(chan string, 1)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case sni <- hello.ServerName:
			default:
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
			}(c)
		}
	}()

	stubLookup(t, func(_ context.Context, host string) ([]string, error) {
		if host != "example.test" {
			t.Errorf("resolved host = %q, want example.test", host)
		}
		return []string{"127.0.0.1"}, nil
	})

	dialer := websocket.Dialer{
		NetDialContext: dialResolved,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // the test certificate is self-signed
		},
		HandshakeTimeout: 5 * time.Second,
	}
	// The listener speaks TLS but not WebSocket: the handshake reaching the
	// server is what this test asserts, so the upgrade error is expected.
	if _, _, err := dialer.Dial("wss://example.test:"+listenerPort(t, ln)+"/", nil); err == nil {
		t.Fatal("Dial() = nil error, want an upgrade error from the non-WebSocket listener")
	}

	select {
	case got := <-sni:
		if got != "example.test" {
			t.Fatalf("SNI = %q, want example.test", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server saw no TLS handshake")
	}
}

// selfSignedCert returns a certificate for example.test. Only the handshake
// shape matters here, so it is not added to any trust store.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"example.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
