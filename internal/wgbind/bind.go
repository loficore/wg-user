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
// Recv blocks until a datagram arrives, stop is closed, or the transport is
// closed, and reports net.ErrClosed in the last two cases. stop lets
// Bind.Close interrupt a parked receive without tearing the transport down:
// wireguard-go closes and reopens a bind on every device.Up (BindUpdate), so
// the transport must outlive bind generations.
type Transport interface {
	Send(pkt []byte) error
	Recv(buf []byte, stop <-chan struct{}) (int, error)
}

// Bind implements conn.Bind over a Transport.
//
// Receives are grouped into generations: each Open creates one, and Close
// ends only the current one. wireguard-go's device.BindUpdate runs
// bind.Close() → stopping.Wait() → bind.Open(), and device.Up starts with
// BindUpdate, so the first Up closes a bind that was never opened. Close is
// therefore a no-op until there is a generation, and it never touches the
// transport (*wstunnel.Client is terminal on Close and must survive).
type Bind struct {
	transport Transport

	// remote is the endpoint ParseEndpoint last returned, handed back as the
	// source of received datagrams. wireguard-go stores it as the peer's
	// endpoint, and a nil there would make Send fail with "no known endpoint".
	remote atomic.Pointer[endpoint]

	mu  sync.Mutex
	gen *generation // current generation, nil before the first Open or after Close
}

// generation is one Open/Close cycle: its done channel is closed by Close to
// stop the ReceiveFuncs that belong to it.
type generation struct {
	done chan struct{}
}

// New returns a Bind that exchanges datagrams over t.
func New(t Transport) *Bind {
	b := &Bind{
		transport: t,
	}
	b.remote.Store(&endpoint{})
	return b
}

// Open reports the single receive function of a new generation. Nothing is
// bound: the transport already owns the connection, so port is ignored and
// the reported port is zero.
func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	g := &generation{done: make(chan struct{})}
	b.mu.Lock()
	b.gen = g
	b.mu.Unlock()

	return []conn.ReceiveFunc{func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		return b.receive(g, packets, sizes, eps)
	}}, 0, nil
}

// Close ends the current generation and is idempotent. Closing the
// generation's stop channel wakes a ReceiveFunc blocked in Transport.Recv
// with net.ErrClosed, which wireguard-go's stopping.Wait() is waiting for;
// the next Open starts a fresh generation. The transport is deliberately left
// alive, and Close before the first Open is a no-op.
func (b *Bind) Close() error {
	b.mu.Lock()
	g := b.gen
	b.gen = nil
	b.mu.Unlock()

	if g != nil {
		close(g.done)
	}
	return nil
}

// SetMark is a no-op: datagrams leave through the transport, not a socket
// owned by this process (which runs as root and has no SO_MARK to set).
func (b *Bind) SetMark(mark uint32) error {
	return nil
}

// Send gives each buffer to the transport as its own frame. Buffers are never
// coalesced: the server forwards frames as individual UDP datagrams, so
// batching two WireGuard packets into one frame would corrupt the stream.
// ep is ignored; the transport already knows where the peer is. Send keeps
// working after Close, since the transport is not closed with the bind.
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

// receive blocks until the transport delivers a datagram, the generation is
// closed, or the transport dies for good.
//
// Transient transport errors must never leave this loop. wireguard-go's
// receive routine (device/receive.go:72-127 in v0.0.0-20250521234502) counts
// non-closed errors: after ten consecutive ones, each followed by a 1/3 s
// sleep, the goroutine returns permanently and the device stops receiving
// until it is recreated. Client.Recv fails immediately and repeatedly while
// the WebSocket is reconnecting, so propagating those errors would kill
// receiving during a routine reconnect. Only net.ErrClosed (teardown) is
// returned.
func (b *Bind) receive(g *generation, packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	if len(packets) == 0 || len(sizes) == 0 {
		return 0, fmt.Errorf("wgbind: receive: %d packet buffers", len(packets))
	}

	for {
		select {
		case <-g.done:
			return 0, net.ErrClosed
		default:
		}

		n, err := b.transport.Recv(packets[0], g.done)
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
		// retry. Do not return the error; see the comment above. Selecting on
		// done means Close never waits out the full interval.
		select {
		case <-g.done:
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
