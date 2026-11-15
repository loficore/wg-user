package resolve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stub replaces the resolver and ping injection points for one test and clears
// the cache on the way in and out, so no test can observe another's entries.
func stub(t *testing.T, lookup func(context.Context, string) ([]string, error), ping func(context.Context, string) ([]byte, error)) {
	t.Helper()

	oldLookup, oldPing, oldNow := lookupHost, runPing, now
	lookupHost, runPing = lookup, ping
	now = time.Now
	resetCache()
	t.Cleanup(func() {
		lookupHost, runPing, now = oldLookup, oldPing, oldNow
		resetCache()
	})
}

// failLookup is a lookup that must not be called, for tests that assert a path
// skips DNS entirely.
func failLookup(t *testing.T) func(context.Context, string) ([]string, error) {
	t.Helper()
	return func(context.Context, string) ([]string, error) {
		t.Fatal("resolver was called, want no DNS")
		return nil, nil
	}
}

// failPing is a ping that must not be called.
func failPing(t *testing.T) func(context.Context, string) ([]byte, error) {
	t.Helper()
	return func(context.Context, string) ([]byte, error) {
		t.Fatal("ping was called, want no ping fallback")
		return nil, nil
	}
}

func TestLookup_IPLiteralPassesThrough(t *testing.T) {
	tests := []string{"203.0.113.10", "::1", "2001:db8::1"}

	for _, host := range tests {
		t.Run(host, func(t *testing.T) {
			stub(t, failLookup(t), failPing(t))

			got, err := Lookup(context.Background(), host)
			if err != nil {
				t.Fatalf("Lookup(%q) error: %v", host, err)
			}
			if len(got) != 1 || got[0] != host {
				t.Fatalf("Lookup(%q) = %v, want [%q]", host, got, host)
			}
		})
	}
}

func TestLookup_ResolverSuccess(t *testing.T) {
	calls := 0
	stub(t, func(_ context.Context, host string) ([]string, error) {
		calls++
		if host != "example.test" {
			t.Fatalf("resolver got host %q, want example.test", host)
		}
		return []string{"203.0.113.10", "2001:db8::1"}, nil
	}, failPing(t))

	got, err := Lookup(context.Background(), "example.test")
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	want := []string{"203.0.113.10", "2001:db8::1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Lookup() = %v, want %v", got, want)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
}

func TestLookup_PingIPv4Fallback(t *testing.T) {
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("lookup example.test on [::1]:53: read udp [::1]:12345->[::1]:53: connection refused")
	}, func(_ context.Context, host string) ([]byte, error) {
		if host != "example.test" {
			t.Fatalf("ping got host %q, want example.test", host)
		}
		// Real ping exits non-zero when the probe gets no reply; the PING line
		// is still printed first.
		return []byte("PING example.test (203.0.113.10) 56(84) bytes of data.\n"), errors.New("exit status 1")
	})

	got, err := Lookup(context.Background(), "example.test")
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if len(got) != 1 || got[0] != "203.0.113.10" {
		t.Fatalf("Lookup() = %v, want [203.0.113.10]", got)
	}
}

func TestLookup_PingIPv6Fallback(t *testing.T) {
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}, func(context.Context, string) ([]byte, error) {
		return []byte("PING example.test (2001:db8::1) 56 data bytes\n64 bytes from 2001:db8::1: icmp_seq=1 ttl=64 time=1.2 ms\n"), errors.New("exit status 1")
	})

	got, err := Lookup(context.Background(), "example.test")
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if len(got) != 1 || got[0] != "2001:db8::1" {
		t.Fatalf("Lookup() = %v, want [2001:db8::1]", got)
	}
}

func TestLookup_PingSuccessOnZeroExit(t *testing.T) {
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}, func(context.Context, string) ([]byte, error) {
		return []byte("PING example.test (203.0.113.10) 56(84) bytes of data.\n"), nil
	})

	if _, err := Lookup(context.Background(), "example.test"); err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
}

func TestLookup_BothFail(t *testing.T) {
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("lookup bad.invalid on [::1]:53: connection refused")
	}, func(context.Context, string) ([]byte, error) {
		return []byte("ping: bad address 'bad.invalid'\n"), errors.New("exit status 68")
	})

	_, err := Lookup(context.Background(), "bad.invalid")
	if err == nil {
		t.Fatal("Lookup() = nil error, want error")
	}
	// Both causes must be named: the Go resolver's own failure is not enough to
	// explain that the phone cannot resolve anything.
	for _, want := range []string{"bad.invalid", "connection refused", "ping"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLookup_EmptyOutputWithSuccessfulExit(t *testing.T) {
	stub(t, func(context.Context, string) ([]string, error) {
		return []string{}, nil
	}, func(context.Context, string) ([]byte, error) {
		return nil, nil
	})

	if _, err := Lookup(context.Background(), "example.test"); err == nil {
		t.Fatal("Lookup() = nil error, want error")
	}
}

func TestLookup_CacheHitSkipsSecondResolve(t *testing.T) {
	pingCalls := 0
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}, func(context.Context, string) ([]byte, error) {
		pingCalls++
		return []byte("PING example.test (203.0.113.10) 56(84) bytes of data.\n"), errors.New("exit status 1")
	})

	for i := 0; i < 3; i++ {
		got, err := Lookup(context.Background(), "example.test")
		if err != nil {
			t.Fatalf("Lookup() #%d error: %v", i, err)
		}
		if len(got) != 1 || got[0] != "203.0.113.10" {
			t.Fatalf("Lookup() #%d = %v", i, got)
		}
	}
	if pingCalls != 1 {
		t.Fatalf("ping calls = %d, want 1 (cache hit)", pingCalls)
	}
}

func TestLookup_ExpiredEntryResolvesAgain(t *testing.T) {
	base := time.Now()
	pingCalls := 0
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}, func(context.Context, string) ([]byte, error) {
		pingCalls++
		return []byte("PING example.test (203.0.113.10) 56(84) bytes of data.\n"), errors.New("exit status 1")
	})

	now = func() time.Time { return base }
	if _, err := Lookup(context.Background(), "example.test"); err != nil {
		t.Fatalf("first Lookup() error: %v", err)
	}

	// Just inside the TTL: still cached.
	now = func() time.Time { return base.Add(cacheTTL - time.Second) }
	if _, err := Lookup(context.Background(), "example.test"); err != nil {
		t.Fatalf("second Lookup() error: %v", err)
	}
	if pingCalls != 1 {
		t.Fatalf("ping calls before expiry = %d, want 1", pingCalls)
	}

	// Past the TTL: re-resolved.
	now = func() time.Time { return base.Add(cacheTTL + time.Second) }
	if _, err := Lookup(context.Background(), "example.test"); err != nil {
		t.Fatalf("third Lookup() error: %v", err)
	}
	if pingCalls != 2 {
		t.Fatalf("ping calls after expiry = %d, want 2", pingCalls)
	}
}

func TestLookup_FailureIsNotCached(t *testing.T) {
	pingCalls := 0
	stub(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	}, func(context.Context, string) ([]byte, error) {
		pingCalls++
		return []byte("ping: bad address\n"), errors.New("exit status 68")
	})

	if _, err := Lookup(context.Background(), "bad.invalid"); err == nil {
		t.Fatal("first Lookup() = nil error, want error")
	}
	if _, err := Lookup(context.Background(), "bad.invalid"); err == nil {
		t.Fatal("second Lookup() = nil error, want error")
	}
	if pingCalls != 2 {
		t.Fatalf("ping calls = %d, want 2 (failures must not be cached)", pingCalls)
	}
}

// TestParsePingOutput covers the output shapes of the ping implementations a
// phone or a developer machine may have. toybox is Android's; iputils prints a
// nested parenthesised form with its own translated text around it.
func TestLookup_RejectsOptionLikeHost(t *testing.T) {
	stub(t, failLookup(t), failPing(t))

	for _, host := range []string{"-c", "--help"} {
		_, err := Lookup(context.Background(), host)
		if err == nil {
			t.Fatalf("Lookup(%q) = nil error, want it rejected", host)
		}
		if !strings.Contains(err.Error(), host) {
			t.Fatalf("error = %q, want it to name %q", err, host)
		}
	}
}

// TestLookup_GoResolverIsBounded proves the ping fallback is reachable when the
// Go resolver hangs: a blackholed nameserver must not consume the caller's whole
// budget. Before the bound existed the stub below ran until its 2 s guard, so the
// elapsed-time assertion failed.
func TestLookup_GoResolverIsBounded(t *testing.T) {
	oldTimeout := lookupTimeout
	lookupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lookupTimeout = oldTimeout })

	deadlineSeen := false
	pingCalls := 0
	stub(t, func(ctx context.Context, _ string) ([]string, error) {
		if _, ok := ctx.Deadline(); ok {
			deadlineSeen = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return nil, errors.New("resolver call was not bounded")
		}
	}, func(context.Context, string) ([]byte, error) {
		pingCalls++
		return []byte("PING example.test (203.0.113.10) 56(84) bytes of data.\n"), errors.New("exit status 1")
	})

	start := time.Now()
	got, err := Lookup(context.Background(), "example.test")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if !deadlineSeen {
		t.Error("resolver got a context without a deadline")
	}
	if elapsed > time.Second {
		t.Fatalf("Lookup() took %v, want it bounded by lookupTimeout (50ms)", elapsed)
	}
	if pingCalls != 1 {
		t.Fatalf("ping calls = %d, want 1 (fallback must run)", pingCalls)
	}
	if len(got) != 1 || got[0] != "203.0.113.10" {
		t.Fatalf("Lookup() = %v, want [203.0.113.10]", got)
	}
}

func TestParsePingOutput(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "toybox ipv4",
			out:  "PING example.test (203.0.113.10) 56(84) bytes of data.\n",
			want: "203.0.113.10",
		},
		{
			name: "toybox ipv6",
			out:  "PING example.test (2001:db8::1) 56 data bytes\n64 bytes from 2001:db8::1: icmp_seq=1 ttl=64 time=1.2 ms\n",
			want: "2001:db8::1",
		},
		{
			name: "busybox",
			out:  "PING example.test (203.0.113.10): 56 data bytes\n",
			want: "203.0.113.10",
		},
		{
			name: "iputils nested",
			out:  "PING example.test(example.test (2001:db8::1)) 56 data bytes\n",
			want: "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePingOutput([]byte(tt.out))
			if err != nil {
				t.Fatalf("parsePingOutput() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parsePingOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParsePingOutput_NoAddress(t *testing.T) {
	for _, out := range []string{"", "ping: bad address 'bad.invalid'\n", "PING bad.invalid (unknown) 56 data bytes\n"} {
		if got, err := parsePingOutput([]byte(out)); err == nil {
			t.Fatalf("parsePingOutput(%q) = %q, nil error; want error", out, got)
		}
	}
}

// TestRunPingCommand_Args pins the command line Android's toybox ping needs:
// one probe, a short reply timeout, and the host last.
func TestRunPingCommand_Args(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + argsFile + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ping"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ping: %v", err)
	}
	t.Setenv("PATH", dir)

	out, err := runPingCommand(context.Background(), "example.test")
	if err == nil {
		t.Fatal("runPingCommand() = nil error, want the fake's exit 1")
	}
	if len(out) != 0 {
		t.Fatalf("output = %q, want empty", out)
	}

	args, readErr := os.ReadFile(argsFile)
	if readErr != nil {
		t.Fatalf("read args: %v", readErr)
	}
	if got, want := strings.TrimSpace(string(args)), "-c 1 -W 2 example.test"; got != want {
		t.Fatalf("ping args = %q, want %q", got, want)
	}
}
