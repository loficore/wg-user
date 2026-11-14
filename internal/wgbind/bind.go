// Package wgbind adapts a WebSocket transport to wireguard-go's conn.Bind.
//
// There is no UDP socket and no local port: every WireGuard datagram becomes
// exactly one WebSocket frame, and the WireGuard loopback hop that 0.2.0
// needed disappears. Open therefore ignores its port argument and reports an
// actual port of zero.
package wgbind

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// retryInterval is how long ReceiveFunc waits after a transient
// Transport.Recv failure before trying again. It is a variable so tests can
// shorten it; production never changes it.
var retryInterval = 50 * time.Millisecond

var _ conn.Bind = (*Bind)(nil)

// Transport carries datagrams to and from the peer. One Send or one
// successful Recv is exactly one datagram: the transport must not coalesce or
// split frames.
//
// Recv blocks until a datagram arrives, the transport fails, or the transport
// is closed. A transport that can be torn down should implement io.Closer as
// well: Bind.Close uses it to unblock a parked Recv.
type Transport interface {
	Send(pkt []byte) error
	Recv(buf []byte) (int, error)
}

// Bind implements conn.Bind over a Transport.
type Bind struct {
	transport Transport
	done      chan struct{}

	// remote is the endpoint ParseEndpoint last returned, handed back as the
	// source of received datagrams. wireguard-go stores it as the peer's
	// endpoint, and a nil there would make Send fail with "no known endpoint".
	remote atomic.Pointer[endpoint]

	closeOnce sync.Once
	closeErr  error
}

// New returns a Bind that exchanges datagrams over t.
func New(t Transport) *Bind {
	b := &Bind{
		transport: t,
		done:      make(chan struct{}),
	}
	b.remote.Store(&endpoint{})
	return b
}

// Open reports the single receive function this bind offers. Nothing is
// bound: the transport already owns the connection, so port is ignored and
// the reported port is zero.
func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	return []conn.ReceiveFunc{b.receive}, 0, nil
}

// Close shuts the bind down and is idempotent. It unblocks a ReceiveFunc
// parked in Transport.Recv by closing a transport that implements io.Closer;
// afterwards ReceiveFunc reports net.ErrClosed. The first transport close
// error is reported on every subsequent call.
func (b *Bind) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		if closer, ok := b.transport.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				b.closeErr = fmt.Errorf("wgbind: close transport: %w", err)
			}
		}
	})
	return b.closeErr
}

// SetMark is a no-op: datagrams leave through the transport, not a socket
// owned by this process (which runs as root and has no SO_MARK to set).
func (b *Bind) SetMark(mark uint32) error {
	return nil
}

// Send gives each buffer to the transport as its own frame. Buffers are never
// coalesced: the server forwards frames as individual UDP datagrams, so
// batching two WireGuard packets into one frame would corrupt the stream.
// ep is ignored; the transport already knows where the peer is.
func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	for _, buf := range bufs {
		if err := b.transport.Send(buf); err != nil {
			return fmt.Errorf("wgbind: send: %w", err)
		}
	}
	return nil
}

// ParseEndpoint keeps the string verbatim. The endpoint is a label for
// logging and IPC compatibility, not an address this bind will dial.
func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	ep := &endpoint{addr: s}
	b.remote.Store(ep)
	return ep, nil
}

// BatchSize is one: a WebSocket frame carries one datagram and Recv returns
// at most one packet per call.
func (b *Bind) BatchSize() int {
	return 1
}

// receive blocks until the transport delivers a datagram, the bind is closed,
// or the transport dies for good.
//
// Transient transport errors must never leave this loop. wireguard-go's
// receive routine (device/receive.go:72-127 in v0.0.0-20250521234502) counts
// non-closed errors: after ten consecutive ones, each followed by a 1/3 s
// sleep, the goroutine returns permanently and the device stops receiving
// until it is recreated. Client.Recv fails immediately and repeatedly while
// the WebSocket is reconnecting, so propagating those errors would kill
// receiving during a routine reconnect. Only net.ErrClosed (teardown) is
// returned.
func (b *Bind) receive(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	if len(packets) == 0 || len(sizes) == 0 {
		return 0, fmt.Errorf("wgbind: receive: %d packet buffers", len(packets))
	}

	for {
		select {
		case <-b.done:
			return 0, net.ErrClosed
		default:
		}

		n, err := b.transport.Recv(packets[0])
		if err == nil {
			sizes[0] = n
			if len(eps) > 0 {
				eps[0] = b.remote.Load()
			}
			return 1, nil
		}
		if errors.Is(err, net.ErrClosed) {
			return 0, net.ErrClosed
		}

		// Transient (reconnecting, write timeout, oversized frame): pause
		// briefly so a broken transport cannot spin this goroutine, then
		// retry. Do not return the error; see the comment above.
		select {
		case <-b.done:
			return 0, net.ErrClosed
		case <-time.After(retryInterval):
		}
	}
}

// endpoint is a named peer. Everything but the name is inert: no source
// address is cached and no destination is dialed.
type endpoint struct {
	addr string
}

var _ conn.Endpoint = (*endpoint)(nil)

func (e *endpoint) ClearSrc()           {}
func (e *endpoint) SrcToString() string { return "" }
func (e *endpoint) DstToString() string { return e.addr }
func (e *endpoint) DstToBytes() []byte  { return []byte(e.addr) }
func (e *endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

// DstIP reports the parsed address when the name is a host:port, otherwise
// the zero value. It only feeds wireguard-go's per-endpoint rate limiting,
// which needs a stable key rather than a routable address.
func (e *endpoint) DstIP() netip.Addr {
	ap, err := netip.ParseAddrPort(e.addr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr()
}
