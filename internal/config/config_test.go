package config

import (
	"strings"
	"testing"
	"time"
)

// wantErrContains fails the test unless Load returned an error whose text
// contains want. Callers print single-line errors, so the whole message
// (including the "config <path>:" prefix) is part of the contract.
func wantErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Load() = nil error, want error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) unexpected error: %v", path, err)
	}
	if c == nil {
		t.Fatalf("Load(%q) = nil config, nil error", path)
	}
	return c
}

func TestLoad_MissingTransportSectionDefaultsToOff(t *testing.T) {
	const path = "testdata/minimal.conf"

	c := mustLoad(t, path)

	if c.Path != path {
		t.Fatalf("Config.Path = %q, want %q", c.Path, path)
	}
	if c.Transport.Mode != ModeOff {
		t.Fatalf("mode = %q, want %q", c.Transport.Mode, ModeOff)
	}
	if c.Transport.URL != "" {
		t.Fatalf("url = %q, want empty", c.Transport.URL)
	}
	if c.Transport.Path != "v1" {
		t.Fatalf("path = %q, want default v1", c.Transport.Path)
	}
	if c.Transport.Insecure {
		t.Fatal("insecure = true, want false")
	}
	if c.Transport.Keepalive != 25*time.Second {
		t.Fatalf("keepalive = %v, want 25s", c.Transport.Keepalive)
	}
	if c.WireProxy == nil || c.WireProxy.Device == nil || len(c.WireProxy.Device.Peers) != 1 {
		t.Fatal("wireproxy config not parsed")
	}
	if len(c.WireProxy.Routines) == 0 {
		t.Fatal("wireproxy routines not parsed (expected the [Socks5] spawner)")
	}
}

func TestLoad_WstunnelParsesEveryKey(t *testing.T) {
	c := mustLoad(t, "testdata/transport-wstunnel.conf")

	tr := c.Transport
	if tr.Mode != ModeWstunnel {
		t.Errorf("mode = %q, want %q", tr.Mode, ModeWstunnel)
	}
	if tr.URL != "wss://example.com:443" {
		t.Errorf("url = %q, want wss://example.com:443", tr.URL)
	}
	if tr.Path != "wstunnel" {
		t.Errorf("path = %q, want wstunnel", tr.Path)
	}
	if !tr.Insecure {
		t.Error("insecure = false, want true")
	}
	if tr.Username != "u" {
		t.Errorf("username = %q, want u", tr.Username)
	}
	if tr.Password != "p" {
		t.Errorf("password = %q, want p", tr.Password)
	}
	if tr.Keepalive != 30*time.Second {
		t.Errorf("keepalive = %v, want 30s", tr.Keepalive)
	}
	if len(c.WireProxy.Device.Peers) != 1 || c.WireProxy.Device.Peers[0].Endpoint == nil {
		t.Fatal("peer endpoint not parsed; wstunnel mode needs it as the target")
	}
}

func TestLoad_RejectsUnknownMode(t *testing.T) {
	_, err := Load("testdata/transport-bad-mode.conf")
	wantErrContains(t, err, "unsupported transport mode")
	if err != nil && !strings.HasPrefix(err.Error(), "config testdata/transport-bad-mode.conf:") {
		t.Fatalf("error = %q, want it prefixed with the config path", err)
	}
}

func TestLoad_WstunnelRequiresURL(t *testing.T) {
	_, err := Load("testdata/transport-no-url.conf")
	wantErrContains(t, err, "[Transport] URL is required")
}

func TestLoad_WstunnelRequiresEndpoint(t *testing.T) {
	_, err := Load("testdata/transport-no-endpoint.conf")
	wantErrContains(t, err, "[Peer] Endpoint is required")
}

func TestLoad_WstunnelRejectsMultiplePeers(t *testing.T) {
	_, err := Load("testdata/transport-two-peers.conf")
	wantErrContains(t, err, "exactly one [Peer]")
}

func TestLoad_IgnoresLegacyTunnelKeys(t *testing.T) {
	c := mustLoad(t, "testdata/transport-legacy-keys.conf")
	if c.Transport.Mode != ModeWstunnel {
		t.Fatalf("mode = %q, want %q", c.Transport.Mode, ModeWstunnel)
	}
	if c.Transport.URL != "wss://example.com:443" {
		t.Fatalf("url = %q, want wss://example.com:443", c.Transport.URL)
	}
	if c.Transport.Path != "v1" {
		t.Fatalf("path = %q, want default v1", c.Transport.Path)
	}
}

// The single-peer rule belongs to wstunnel mode only; off mode must keep the
// upstream tolerance for multi-peer configs.
func TestLoad_OffModeAllowsMultiplePeers(t *testing.T) {
	c := mustLoad(t, "testdata/off-two-peers.conf")
	if c.Transport.Mode != ModeOff {
		t.Fatalf("mode = %q, want %q", c.Transport.Mode, ModeOff)
	}
	if got := len(c.WireProxy.Device.Peers); got != 2 {
		t.Fatalf("peers = %d, want 2", got)
	}
}

func TestLoad_RejectsNonWebSocketURLScheme(t *testing.T) {
	_, err := Load("testdata/transport-bad-scheme.conf")
	wantErrContains(t, err, "scheme")
}

func TestLoad_RejectsNegativeKeepalive(t *testing.T) {
	_, err := Load("testdata/transport-negative-keepalive.conf")
	wantErrContains(t, err, "Keepalive")
}

func TestLoad_KeepaliveZeroDisablesPings(t *testing.T) {
	c := mustLoad(t, "testdata/transport-keepalive-zero.conf")
	if c.Transport.Keepalive != 0 {
		t.Fatalf("keepalive = %v, want 0 (disabled)", c.Transport.Keepalive)
	}
}

func TestLoad_PathBecomesABareURLSegment(t *testing.T) {
	c := mustLoad(t, "testdata/transport-path-slashes.conf")
	if c.Transport.Path != "events" {
		t.Fatalf("path = %q, want events", c.Transport.Path)
	}
}

func TestLoad_RejectsInvalidInsecureValue(t *testing.T) {
	_, err := Load("testdata/transport-bad-insecure.conf")
	wantErrContains(t, err, "Insecure")
}

func TestLoad_WrapsMissingFileError(t *testing.T) {
	const path = "testdata/does-not-exist.conf"

	_, err := Load(path)
	wantErrContains(t, err, "config "+path+":")
}
