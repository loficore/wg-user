package device

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/loficore/wg-user/internal/config"
	"github.com/windtf/wireproxy"
	"golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
)

// mustLoadConfig loads a configuration file for a test.
func mustLoadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q) unexpected error: %v", path, err)
	}
	return cfg
}

// silentLogger discards wireguard-go's output. Tests that need to observe it
// build their own logger.
func silentLogger() *wgdevice.Logger {
	return &wgdevice.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(string, ...any) {},
	}
}

// create builds a VirtualTun from path with the default (kernel UDP) bind and
// closes the device when the test ends. The device opens a real UDP socket on
// Up; no datagram is ever sent because nothing answers the peer.
func create(t *testing.T, path string) *wireproxy.VirtualTun {
	t.Helper()
	cfg := mustLoadConfig(t, path)

	vt, err := Create(cfg.WireProxy, conn.NewDefaultBind(), silentLogger())
	if err != nil {
		t.Fatalf("Create(%q) unexpected error: %v", path, err)
	}
	if vt == nil {
		t.Fatalf("Create(%q) = nil VirtualTun, nil error", path)
	}
	t.Cleanup(func() { vt.Dev.Close() })
	return vt
}

// The stack must come up whole: a netstack TUN, a wireguard-go device, and the
// wireproxy VirtualTun that carries the SOCKS5 routines.
func TestCreate_ReturnsUsableVirtualTun(t *testing.T) {
	cfg := mustLoadConfig(t, "../config/testdata/minimal.conf")

	vt, err := Create(cfg.WireProxy, conn.NewDefaultBind(), silentLogger())
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	if vt == nil {
		t.Fatal("Create() = nil VirtualTun, nil error")
	}
	t.Cleanup(func() { vt.Dev.Close() })

	if vt.Tnet == nil {
		t.Error("VirtualTun.Tnet = nil, want the netstack")
	}
	if vt.Dev == nil {
		t.Error("VirtualTun.Dev = nil, want the wireguard-go device")
	}
	if vt.Conf != cfg.WireProxy.Device {
		t.Error("VirtualTun.Conf does not point at the parsed device config")
	}
	if vt.ResolveConfig != cfg.WireProxy.Resolve {
		t.Error("VirtualTun.ResolveConfig does not point at the parsed resolve config")
	}
	if vt.PingRecord == nil {
		t.Error("VirtualTun.PingRecord = nil, want an empty map")
	} else if len(vt.PingRecord) != 0 {
		t.Errorf("VirtualTun.PingRecord has %d entries, want 0", len(vt.PingRecord))
	}
	if vt.PingRecordLock == nil {
		t.Error("VirtualTun.PingRecordLock = nil, want a mutex")
	}
}

// SystemDNS is derived from the [Interface] DNS list: the fixture has none,
// so the system resolver has to be used.
func TestCreate_SystemDNSWhenConfigHasNoDNS(t *testing.T) {
	vt := create(t, "../config/testdata/minimal.conf")

	if !vt.SystemDNS {
		t.Error("VirtualTun.SystemDNS = false, want true (the fixture has no DNS)")
	}
}

// With a DNS server in the config the system resolver must not be used.
func TestCreate_NotSystemDNSWhenConfigHasDNS(t *testing.T) {
	vt := create(t, "testdata/dns.conf")

	if vt.SystemDNS {
		t.Error("VirtualTun.SystemDNS = true, want false (the fixture has a DNS server)")
	}
}

// A v4-only interface must resolve over IPv4, not IPv6.
func TestCreate_SetsResolveStrategyForV4Only(t *testing.T) {
	cfg := mustLoadConfig(t, "../config/testdata/minimal.conf")

	vt, err := Create(cfg.WireProxy, conn.NewDefaultBind(), silentLogger())
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	t.Cleanup(func() { vt.Dev.Close() })

	if got := vt.ResolveConfig.ResolveStrategy; got != "ipv4" {
		t.Errorf("ResolveStrategy = %q, want %q", got, "ipv4")
	}
}

// failingBind is a conn.Bind that refuses to open, used to exercise the error
// path of Create without touching the network.
type failingBind struct {
	err error
}

func (b failingBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	return nil, 0, b.err
}

func (b failingBind) Close() error                       { return nil }
func (b failingBind) SetMark(uint32) error               { return nil }
func (b failingBind) Send([][]byte, conn.Endpoint) error { return nil }
func (b failingBind) ParseEndpoint(string) (conn.Endpoint, error) {
	return nil, b.err
}
func (b failingBind) BatchSize() int { return 1 }

// A bind that cannot open must fail the whole assembly, with an error the
// caller can print on one line.
func TestCreate_BindFailureIsReported(t *testing.T) {
	cfg := mustLoadConfig(t, "../config/testdata/minimal.conf")
	bindErr := errors.New("bind refused")

	vt, err := Create(cfg.WireProxy, failingBind{err: bindErr}, silentLogger())
	if err == nil {
		if vt != nil {
			vt.Dev.Close()
		}
		t.Fatal("Create() = nil error, want the bind failure")
	}
	if vt != nil {
		vt.Dev.Close()
		t.Error("Create() returned a VirtualTun together with an error")
	}
	if !strings.HasPrefix(err.Error(), "device:") {
		t.Errorf("error = %q, want it prefixed with %q", err, "device:")
	}
	if !strings.Contains(err.Error(), bindErr.Error()) {
		t.Errorf("error = %q, want it to contain %q", err, bindErr.Error())
	}
}

// The injected logger must receive wireguard-go's own lines: Create must wire
// the caller's logger into the device instead of building wireguard-go's
// NewLogger, which writes to the process's real stdout.
func TestCreate_UsesInjectedLogger(t *testing.T) {
	cfg := mustLoadConfig(t, "../config/testdata/minimal.conf")

	// wireguard-go logs from several goroutines, so the recorder needs a lock.
	var (
		mu    sync.Mutex
		lines []string
	)
	logger := &wgdevice.Logger{
		Verbosef: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
		Errorf: func(string, ...any) {},
	}

	vt, err := Create(cfg.WireProxy, conn.NewDefaultBind(), logger)
	if err != nil {
		t.Fatalf("Create() unexpected error: %v", err)
	}
	t.Cleanup(func() { vt.Dev.Close() })

	mu.Lock()
	defer mu.Unlock()

	for _, line := range lines {
		if strings.Contains(line, "UAPI: Updating private key") {
			return
		}
	}
	t.Errorf("logger received %q, want wireguard-go's startup lines", lines)
}
