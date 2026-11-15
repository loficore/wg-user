package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// stubResolver replaces Endpoint name resolution for one test and returns a
// counter of how many hosts were resolved.
func stubResolver(t *testing.T, lookup func(context.Context, string) ([]string, error)) *int {
	t.Helper()

	calls := 0
	old := resolveHost
	resolveHost = func(ctx context.Context, host string) ([]string, error) {
		calls++
		return lookup(ctx, host)
	}
	t.Cleanup(func() { resolveHost = old })
	return &calls
}

// tempConfig is a temporary config Load created: its path and the permission
// bits it had when its bytes were written, i.e. after writeTempConfig set them.
type tempConfig struct {
	path string
	mode os.FileMode
}

// tempFiles records every temporary config Load creates and writes, including
// the mode it had at write time, so a test can assert both its lifetime and its
// permissions. The mode is sampled in writeTemp (not createTemp), because the
// 0600 chmod happens between the two.
func tempFiles(t *testing.T) *[]tempConfig {
	t.Helper()

	created := &[]tempConfig{}
	oldWrite := writeTemp
	writeTemp = func(f *os.File, data []byte) error {
		info, statErr := f.Stat()
		if statErr != nil {
			t.Fatalf("stat temporary config: %v", statErr)
		}
		*created = append(*created, tempConfig{path: f.Name(), mode: info.Mode().Perm()})
		return oldWrite(f, data)
	}
	t.Cleanup(func() { writeTemp = oldWrite })
	return created
}

// dirEntries returns the sorted base names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// writeConfig writes a config with the given Endpoint values (one per [Peer])
// into a fresh directory and returns its path.
func writeConfig(t *testing.T, endpoints ...string) string {
	t.Helper()

	var b strings.Builder
	b.WriteString(`# Test config: throwaway keys, never real credentials.

[Interface]
Address    = 10.66.0.11/32
PrivateKey = yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
MTU        = 1280

`)
	for i, endpoint := range endpoints {
		b.WriteString("[Peer]\n")
		b.WriteString("PublicKey  = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=\n")
		b.WriteString("Endpoint   = " + endpoint + "\n")
		if i == 0 {
			b.WriteString("AllowedIPs = 10.66.0.0/24\n")
		} else {
			b.WriteString("AllowedIPs = 10.67.0.0/24\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("[Socks5]\nBindAddress = 127.0.0.1:1080\n")

	path := filepath.Join(t.TempDir(), "wireproxy.conf")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// peerEndpoint returns peer i's parsed Endpoint.
func peerEndpoint(t *testing.T, c *Config, i int) string {
	t.Helper()

	if c.WireProxy == nil || c.WireProxy.Device == nil || len(c.WireProxy.Device.Peers) <= i {
		t.Fatalf("config has %d peers, want more than %d", len(c.WireProxy.Device.Peers), i)
	}
	ep := c.WireProxy.Device.Peers[i].Endpoint
	if ep == nil {
		t.Fatalf("peer %d has no Endpoint", i)
	}
	return *ep
}

func TestLoad_HostnameEndpointIsResolvedBeforeParse(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")
	before := dirEntries(t, filepath.Dir(path))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	calls := stubResolver(t, func(_ context.Context, host string) ([]string, error) {
		if host != "vpn.example.com" {
			t.Fatalf("resolved host = %q, want vpn.example.com", host)
		}
		return []string{"203.0.113.10"}, nil
	})
	created := tempFiles(t)

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q (hostname must be rewritten)", got, want)
	}
	if *calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", *calls)
	}
	if c.Path != path {
		t.Fatalf("Config.Path = %q, want the original %q", c.Path, path)
	}

	// The temporary file must be in the same directory (a relative WGConfig
	// depends on it), mode 0600 while it existed, and gone afterwards.
	if len(*created) != 1 {
		t.Fatalf("temporary configs created = %d, want 1", len(*created))
	}
	temp := (*created)[0]
	if dir := filepath.Dir(temp.path); dir != filepath.Dir(path) {
		t.Fatalf("temporary config directory = %q, want %q", dir, filepath.Dir(path))
	}
	if temp.mode != 0o600 {
		t.Fatalf("temporary config mode = %04o, want 0600 (it holds the private key)", temp.mode)
	}
	if _, statErr := os.Stat(temp.path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary config %s still exists after Load (stat err = %v)", temp.path, statErr)
	}
	if got := dirEntries(t, filepath.Dir(path)); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("directory changed: %v, want %v", got, before)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original after Load: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("original config was modified, want it byte-identical")
	}
}

func TestLoad_IPEndpointCreatesNoTempFile(t *testing.T) {
	path := writeConfig(t, "203.0.113.10:51820")
	before := dirEntries(t, filepath.Dir(path))

	calls := stubResolver(t, func(context.Context, string) ([]string, error) {
		t.Fatal("resolver called for an IP Endpoint")
		return nil, nil
	})
	created := tempFiles(t)

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q", got, want)
	}
	if *calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", *calls)
	}
	if len(*created) != 0 {
		t.Fatalf("temporary configs created = %d, want 0", len(*created))
	}
	if got := dirEntries(t, filepath.Dir(path)); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("directory changed: %v, want %v", got, before)
	}
}

func TestLoad_TwoHostnameEndpointsAreBothResolved(t *testing.T) {
	path := writeConfig(t, "one.example.com:51820", "two.example.com:51821")

	hosts := map[string]string{
		"one.example.com": "203.0.113.10",
		"two.example.com": "203.0.113.11",
	}
	stubResolver(t, func(_ context.Context, host string) ([]string, error) {
		ip, ok := hosts[host]
		if !ok {
			return nil, errors.New("unexpected host " + host)
		}
		return []string{ip}, nil
	})

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q", got, want)
	}
	if got, want := peerEndpoint(t, c, 1), "203.0.113.11:51821"; got != want {
		t.Fatalf("peer 1 Endpoint = %q, want %q", got, want)
	}
}

// TestLoad_QuotedEndpointIsResolvedOnce guards the offsets parseEndpointValue
// reports: the quote is re-emitted by the rewriter, so start/end must span the
// quoted value. Advancing past the quote doubled it and produced
// 'Endpoint = ""203.0.113.10:51820""', which wireproxy then rejected.
func TestLoad_QuotedEndpointIsResolvedOnce(t *testing.T) {
	path := writeConfig(t, `"vpn.example.com:51820"`)

	deadlineSeen := false
	stubResolver(t, func(ctx context.Context, host string) ([]string, error) {
		if host != "vpn.example.com" {
			t.Fatalf("resolved host = %q, want vpn.example.com", host)
		}
		_, deadlineSeen = ctx.Deadline()
		return []string{"203.0.113.10"}, nil
	})

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q", got, want)
	}
	if !deadlineSeen {
		t.Error("Load passed an unbounded context to the resolver")
	}

	// The splice keeps exactly one pair of quotes around the address.
	out, rewrote, err := rewritePeerEndpoints(context.Background(), []byte("[Peer]\nEndpoint   = \"vpn.example.com:51820\"  # keep\n"))
	if err != nil {
		t.Fatalf("rewritePeerEndpoints() error: %v", err)
	}
	if !rewrote {
		t.Fatal("rewrote = false, want true")
	}
	if want := "[Peer]\nEndpoint   = \"203.0.113.10:51820\"  # keep\n"; string(out) != want {
		t.Fatalf("rewritten config =\n%q\nwant\n%q", out, want)
	}
}

// TestLoad_BacktickEndpointIsKeptQuoted: go-ini also strips a backtick pair, so
// the scanner must treat it as a quote. Recognising only '"' and '\” left a
// backtick inside the hostname, which became a garbage resolver argument.
func TestLoad_BacktickEndpointIsKeptQuoted(t *testing.T) {
	path := writeConfig(t, "`vpn.example.com:51820`")
	stubResolver(t, func(_ context.Context, host string) ([]string, error) {
		if host != "vpn.example.com" {
			t.Fatalf("resolved host = %q, want vpn.example.com", host)
		}
		return []string{"203.0.113.10"}, nil
	})

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q", got, want)
	}
	out, _, err := rewritePeerEndpoints(context.Background(), []byte("[Peer]\nEndpoint = `vpn.example.com:51820`\n"))
	if err != nil {
		t.Fatalf("rewritePeerEndpoints() error: %v", err)
	}
	if want := "[Peer]\nEndpoint = `203.0.113.10:51820`\n"; string(out) != want {
		t.Fatalf("rewritten config =\n%q\nwant\n%q", out, want)
	}
}

// TestLoad_ColonDelimitedEndpointIsResolved: go-ini's default key delimiters are
// "=:", so wireproxy accepts "Endpoint: host:port". Scanning only for '=' left
// such an endpoint for wireproxy to resolve, i.e. broken on Android.
func TestLoad_ColonDelimitedEndpointIsResolved(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	const key = "Endpoint   = vpn.example.com:51820"
	if !strings.Contains(string(data), key) {
		t.Fatalf("test bug: config does not contain %q", key)
	}
	replaced := strings.Replace(string(data), key, "Endpoint: vpn.example.com:51820", 1)
	if err := os.WriteFile(path, []byte(replaced), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	calls := stubResolver(t, func(_ context.Context, host string) ([]string, error) {
		if host != "vpn.example.com" {
			t.Fatalf("resolved host = %q, want vpn.example.com", host)
		}
		return []string{"203.0.113.10"}, nil
	})

	c := mustLoad(t, path)

	if got, want := peerEndpoint(t, c, 0), "203.0.113.10:51820"; got != want {
		t.Fatalf("peer 0 Endpoint = %q, want %q", got, want)
	}
	if *calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", *calls)
	}
}

func TestLoad_HostnameEndpointResolutionFailure(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")
	created := tempFiles(t)
	stubResolver(t, func(_ context.Context, host string) ([]string, error) {
		return nil, errors.New("lookup " + host + " on [::1]:53: connection refused")
	})

	_, err := Load(path)
	wantErrContains(t, err, "vpn.example.com")
	wantErrContains(t, err, "connection refused")
	wantErrContains(t, err, "[Peer] 1")

	if len(*created) != 0 {
		t.Fatalf("temporary configs created = %d, want 0 on the failure path", len(*created))
	}
}

// TestLoad_HostnameEndpointTempRemovedOnParseError proves the temp file does
// not survive a wireproxy parse failure: it holds the private key.
func TestLoad_HostnameEndpointTempRemovedOnParseError(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")

	// Corrupt a key after the file is written; wireproxy rejects it only when
	// parsing, i.e. after the temporary copy exists.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	broken := strings.Replace(string(data), "PrivateKey = yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=", "PrivateKey = not-a-key", 1)
	if broken == string(data) {
		t.Fatal("test bug: PrivateKey replacement did not apply")
	}
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	stubResolver(t, func(context.Context, string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	})
	created := tempFiles(t)

	_, err = Load(path)
	wantErrContains(t, err, "config "+path+":")
	if strings.Contains(err.Error(), ".resolved-") {
		t.Fatalf("parse error names the temporary file: %v (it is gone, so that path is not actionable)", err)
	}
	if len(*created) != 1 {
		t.Fatalf("temporary configs created = %d, want 1", len(*created))
	}
	if _, statErr := os.Stat((*created)[0].path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary config %s still exists after a failed Load (stat err = %v)", (*created)[0].path, statErr)
	}
}

// TestLoad_TempChmodFailureRemovesFile covers the first cleanup branch of
// writeTempConfig: when the mode cannot be set, the file is already created and
// holds (or will hold) the private key, so it must be removed and the error
// wrapped with the reason.
func TestLoad_TempChmodFailureRemovesFile(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")
	stubResolver(t, func(context.Context, string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	})

	dir := filepath.Dir(path)
	closed, err := os.CreateTemp(dir, ".closed-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	name := closed.Name()
	if err := closed.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	old := createTemp
	// A closed *os.File makes Chmod fail with os.ErrClosed: the branch is
	// reachable without a filesystem fault.
	createTemp = func(string, string) (*os.File, error) { return closed, nil }
	t.Cleanup(func() {
		createTemp = old
		_ = os.Remove(name)
	})

	_, err = Load(path)
	wantErrContains(t, err, "set mode on temporary config")
	if _, statErr := os.Stat(name); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary config %s survived a chmod failure", name)
	}
}

// TestLoad_TempWriteFailureRemovesFile covers the write branch: the file exists
// on disk after a short write, so it must be removed and the error wrapped.
func TestLoad_TempWriteFailureRemovesFile(t *testing.T) {
	path := writeConfig(t, "vpn.example.com:51820")
	stubResolver(t, func(context.Context, string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	})

	// Record the created name here rather than through tempFiles: that helper
	// samples the write seam, which this test replaces to fail.
	oldCreate := createTemp
	var name string
	createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := oldCreate(dir, pattern)
		if err == nil {
			name = f.Name()
		}
		return f, err
	}
	t.Cleanup(func() { createTemp = oldCreate })

	old := writeTemp
	writeTemp = func(*os.File, []byte) error { return errors.New("no space left on device") }
	t.Cleanup(func() { writeTemp = old })

	_, err := Load(path)
	wantErrContains(t, err, "write temporary config")
	wantErrContains(t, err, "no space left on device")
	if name == "" {
		t.Fatal("Load created no temporary config, want one")
	}
	if _, statErr := os.Stat(name); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary config %s survived a write failure", name)
	}
}

// TestScanPeerEndpoints_IgnoresNonPeerSections pins the textual scanner: an
// Endpoint-looking key outside [Peer] is not an endpoint, section names and
// keys are matched case-insensitively, and comments and quoting are understood.
func TestScanPeerEndpoints_IgnoresNonPeerSections(t *testing.T) {
	data := []byte(`[wstunnel]
Endpoint = ignore.example.com:1

[ Peer ]
Endpoint = spaced.example.com:51819

[peer]
endpoint = quoted.example.com:51820

[Peer] ; trailing text after the section header
Endpoint = "commented.example.com:51821" ; 51821
Endpoint = 203.0.113.10:51822
`)

	got := scanPeerEndpoints(data)
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %+v", len(got), got)
	}
	// "[wstunnel]" is not a peer and neither is "[ Peer ]" — go-ini takes the
	// name verbatim, so it is a section named " Peer ". Both peer spellings
	// count, numbered in file order.
	if got[0].peer != 1 || got[1].peer != 2 {
		t.Errorf("peers = %d,%d, want 1,2", got[0].peer, got[1].peer)
	}
	if got[0].host != "quoted.example.com" || got[0].port != "51820" {
		t.Errorf("candidate 0 = %q:%q", got[0].host, got[0].port)
	}
	if got[1].host != "commented.example.com" || got[1].port != "51821" || got[1].quote != '"' {
		t.Errorf("candidate 1 = %q:%q quote %q", got[1].host, got[1].port, got[1].quote)
	}
}

// TestScanPeerEndpoints_SkipsValuesWithQuotesOrSpaces pins the defensive skip:
// a value the scanner cannot parse cleanly is left to wireproxy's validation,
// so it must never reach the resolver as a nonsense hostname.
func TestScanPeerEndpoints_SkipsValuesWithQuotesOrSpaces(t *testing.T) {
	data := []byte("[Peer]\n" +
		"Endpoint = vpn.example.com:51820 \"extra\"\n" +
		"Endpoint = `vpn.example.com:51820\"\n" +
		"Endpoint = \"unterminated:51820\n" +
		"Endpoint = vpn.example.com:51820 # trailing comment is fine\n")

	got := scanPeerEndpoints(data)
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1 (only the last line): %+v", len(got), got)
	}
	if got[0].host != "vpn.example.com" || got[0].port != "51820" {
		t.Fatalf("candidate = %q:%q, want vpn.example.com:51820", got[0].host, got[0].port)
	}
}

func TestRewritePeerEndpoints_PreservesEverythingElse(t *testing.T) {
	in := []byte("[Peer]\n# keep this comment\nEndpoint   = vpn.example.com:51820   # and this\nAllowedIPs = 10.66.0.0/24\n")
	old := resolveHost
	resolveHost = func(context.Context, string) ([]string, error) { return []string{"203.0.113.10"}, nil }
	t.Cleanup(func() { resolveHost = old })

	out, rewrote, err := rewritePeerEndpoints(context.Background(), in)
	if err != nil {
		t.Fatalf("rewritePeerEndpoints() error: %v", err)
	}
	if !rewrote {
		t.Fatal("rewrote = false, want true")
	}
	want := "[Peer]\n# keep this comment\nEndpoint   = 203.0.113.10:51820   # and this\nAllowedIPs = 10.66.0.0/24\n"
	if string(out) != want {
		t.Fatalf("rewritten config =\n%q\nwant\n%q", out, want)
	}
}
