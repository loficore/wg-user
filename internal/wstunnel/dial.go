package wstunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/loficore/wg-user/internal/resolve"
)

// lookupHost is internal/resolve.Lookup. It is a package variable so tests can
// stub name resolution without a resolver or a network.
var lookupHost = resolve.Lookup

// dialResolved is the WebSocket dialer's NetDialContext.
//
// gorilla passes "host:port" from the URL. A name is resolved with
// internal/resolve — the Go resolver, then Android's ping fallback — and the TCP
// connection is made to the resulting IP. The URL host is left alone, so gorilla
// still uses the name as ServerName (SNI) and as the Host header: the
// certificate must keep matching the name, and a server behind the IP still
// routes the virtual host correctly.
//
// A resolution error is returned as-is; the supervisor retries the whole
// attempt. There is deliberately no fallback to dialling the unresolved name:
// that is exactly the path that fails on Android, and retrying it in a loop
// hides the cause.
func dialResolved(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{}
	if _, err := netip.ParseAddr(host); err == nil {
		return dialer.DialContext(ctx, network, addr)
	}

	addrs, err := lookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}

	// Try every answer in order: a dual-stack name whose first address is
	// unreachable should not fail the attempt while another one works. Every
	// failure is kept, so the returned error names each address that was tried
	// instead of only the last one.
	var errs []error
	for _, ip := range addrs {
		target := net.JoinHostPort(ip, port)
		conn, dialErr := dialer.DialContext(ctx, network, target)
		if dialErr == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", target, dialErr))
	}
	return nil, fmt.Errorf("dial %s: %w", net.JoinHostPort(host, port), errors.Join(errs...))
}
