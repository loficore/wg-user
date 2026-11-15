// Package resolve resolves hostnames to IP literals, with a ping fallback for
// Android.
//
// wg-user is built with CGO_ENABLED=0, so hostname lookups go through Go's pure
// resolver. Android 10+ ships no /etc/resolv.conf and leaves the net.dns1…4
// system properties empty, so the pure resolver has no nameserver to ask and
// falls back to [::1]:53, where every query is refused. The result was that
// every hostname in the config failed on a device while the same config with a
// numeric IP worked.
//
// /system/bin/ping is the one resolver tool present on those devices (getent,
// nslookup, host and dig are not), and it resolves names through libc's
// getaddrinfo, which reads the OS's own configuration. Lookup therefore tries
// the Go resolver first — correct and fast on Linux/CI and older Android — and
// otherwise parses ping's "PING <host> (<addr>)" line. That line is printed
// before the probe is sent, so it is available even when ICMP is blocked and
// ping exits non-zero.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// pingTimeout caps one whole ping fallback, so a wedged ping cannot pin a
	// dial or a config load.
	pingTimeout = 4 * time.Second

	// cacheTTL is how long a successful answer is reused. Android's DNS answers
	// change rarely; 5 minutes keeps a reconnect storm from re-resolving.
	cacheTTL = 5 * time.Minute

	// androidPing is where ping lives when it is not in PATH (a daemon started
	// by init can have a minimal environment).
	androidPing = "/system/bin/ping"
)

// lookupTimeout bounds the Go resolver attempt. Without its own bound the
// caller's context decides: Load uses a 10 s context and a dial uses the
// supervisor's 10 s budget, which a blackholed nameserver consumes entirely, so
// the ping fallback would never run exactly when it is needed. It is a variable
// so tests can shorten it.
var lookupTimeout = 2500 * time.Millisecond

// pingHostLine matches the line where ping prints the host and the resolved
// address. Every flavour prints it before sending, so it is available even when
// ICMP is blocked or the probe is killed by the timeout:
//
//	PING example.com (203.0.113.10) 56(84) bytes of data.   Android/toybox
//	PING example.com (203.0.113.10): 56 data bytes          busybox
//	PING example.com(example.com (::1)) 56 data bytes       iputils
var pingHostLine = regexp.MustCompile(`(?m)^PING[^\n]*`)

// pingAddrInParens matches a parenthesised token on that line. Several ping
// implementations print a nested or additional parenthesised value, so the
// caller picks the first token that is a valid IP literal.
var pingAddrInParens = regexp.MustCompile(`\(([^()\s]+)\)`)

// Injection points. They are package variables so tests can run without a
// resolver or a real ping binary.
var (
	lookupHost = net.DefaultResolver.LookupHost
	runPing    = runPingCommand
	now        = time.Now
)

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

type cacheEntry struct {
	addrs   []string
	expires time.Time
}

// Lookup resolves host to one or more IP literals.
//
// An IP literal — with or without a port stripped by the caller — is returned
// unchanged and never cached. Otherwise the Go resolver is tried first, then
// ping. Successful answers are cached for cacheTTL; failures are not cached, so
// a transient network drop does not poison the next attempt.
func Lookup(ctx context.Context, host string) ([]string, error) {
	if host == "" {
		return nil, errors.New("resolve: empty host")
	}
	if host[0] == '-' {
		// ping would read the host as an option token.
		return nil, fmt.Errorf("resolve: refusing host %q: a name cannot start with '-'", host)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{host}, nil
	}

	if addrs, ok := cacheGet(host); ok {
		return addrs, nil
	}

	addrs, err := resolveUncached(ctx, host)
	if err != nil {
		return nil, err
	}
	cachePut(host, addrs)
	return addrs, nil
}

// resolveUncached tries the Go resolver, then ping. The error, when both fail,
// carries both causes: the Go resolver's failure alone ("no such host") does not
// tell a user that the phone cannot resolve anything at all.
func resolveUncached(ctx context.Context, host string) ([]string, error) {
	// The Go attempt gets its own bound: a nameserver that silently drops
	// packets would otherwise consume the caller's whole budget (Load's 10 s, a
	// dial's supervisor budget) and the ping fallback below would never run.
	lookupCtx, cancelLookup := context.WithTimeout(ctx, lookupTimeout)
	addrs, lookupErr := lookupHost(lookupCtx, host)
	cancelLookup()
	if lookupErr == nil && len(addrs) > 0 {
		return addrs, nil
	}
	if lookupErr == nil {
		lookupErr = errors.New("resolver returned no addresses")
	}

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	out, pingErr := runPing(pingCtx, host)
	if addr, parseErr := parsePingOutput(out); parseErr == nil {
		// ping exits non-zero whenever the probe gets no reply, which is normal
		// on networks that drop ICMP. The address was printed before the probe,
		// so its exit status says nothing about the resolution.
		return []string{addr}, nil
	} else if pingErr != nil {
		return nil, fmt.Errorf("resolve %s: %v; ping fallback: %w", host, lookupErr, pingErr)
	} else {
		return nil, fmt.Errorf("resolve %s: %v; ping fallback: %w", host, lookupErr, parseErr)
	}
}

// runPingCommand runs "ping -c 1 -W 2 <host>" and returns its combined output.
// The exit status is deliberately returned as-is: the caller parses the output
// and only treats an exec failure as fatal when it has no address to fall back
// on.
func runPingCommand(ctx context.Context, host string) ([]byte, error) {
	path, err := exec.LookPath("ping")
	if err != nil {
		if _, statErr := os.Stat(androidPing); statErr != nil {
			return nil, err
		}
		path = androidPing
	}
	// -W 2 bounds the wait for a reply to two seconds; pingTimeout bounds the
	// whole exec in case the binary itself hangs.
	return exec.CommandContext(ctx, path, "-c", "1", "-W", "2", host).CombinedOutput()
}

// parsePingOutput extracts the first IP literal from the PING line. Output may
// be partial when ping was killed by the timeout; the line is printed first, so
// it is still usable.
func parsePingOutput(out []byte) (string, error) {
	line := pingHostLine.Find(out)
	if line == nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return "", errors.New("ping printed nothing")
		}
		return "", fmt.Errorf("no PING line in ping output %q", trimmed)
	}
	for _, m := range pingAddrInParens.FindAllSubmatch(line, -1) {
		addr := string(m[1])
		if _, err := netip.ParseAddr(addr); err == nil {
			return addr, nil
		}
	}
	return "", fmt.Errorf("no IP address in PING line %q", strings.TrimSpace(string(line)))
}

func cacheGet(host string) ([]string, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	e, ok := cache[host]
	if !ok || !now().Before(e.expires) {
		return nil, false
	}
	// Copy so a caller cannot mutate what the next lookup returns.
	return append([]string(nil), e.addrs...), true
}

func cachePut(host string, addrs []string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	cache[host] = cacheEntry{
		addrs:   append([]string(nil), addrs...),
		expires: now().Add(cacheTTL),
	}
}

// resetCache drops every entry. Tests use it to start from a clean slate.
func resetCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	cache = map[string]cacheEntry{}
}
