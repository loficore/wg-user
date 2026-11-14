package wgbind

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// fakeRecv is one scripted result returned by fakeTransport.Recv.
type fakeRecv struct {
	pkt []byte
	err error
}

// fakeTransport is a scripted Transport: Send records every frame it is
// handed, Recv returns queued results and then blocks until the transport is
// closed (after which it reports net.ErrClosed).
type fakeTransport struct {
	mu        sync.Mutex
	sends     [][]byte
	results   []fakeRecv
	recvCalls int
	sendErr   error

	closed    chan struct{}
	closeOnce sync.Once
	parked    chan struct{}
	parkOnce  sync.Once
}

func newFakeTransport(payloads ...string) *fakeTransport {
	f := &fakeTransport{
		closed: make(chan struct{}),
		parked: make(chan struct{}),
	}
	for _, p := range payloads {
		f.results = append(f.results, fakeRecv{pkt: []byte(p)})
	}
	return f
}

func (f *fakeTransport) Send(pkt []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sends = append(f.sends, append([]byte(nil), pkt...))
	return nil
}

func (f *fakeTransport) Recv(buf []byte) (int, error) {
	f.mu.Lock()
	f.recvCalls++
	if len(f.results) > 0 {
		r := f.results[0]
		f.results = f.results[1:]
		f.mu.Unlock()
		if r.err != nil {
			return 0, r.err
		}
		return copy(buf, r.pkt), nil
	}
	f.mu.Unlock()

	// Nothing scripted: park like a live transport waiting for a frame.
	f.parkOnce.Do(func() { close(f.parked) })
	<-f.closed
	return 0, net.ErrClosed
}

func (f *fakeTransport) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTransport) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.sends))
	copy(out, f.sends)
	return out
}

func (f *fakeTransport) recvCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recvCalls
}

func (f *fakeTransport) waitParked(t *testing.T) {
	t.Helper()
	select {
	case <-f.parked:
	case <-time.After(2 * time.Second):
		t.Fatal("ReceiveFunc did not park inside Transport.Recv")
	}
}

// openBind opens b and returns its single ReceiveFunc.
func openBind(t *testing.T, b *Bind) conn.ReceiveFunc {
	t.Helper()
	fns, port, err := b.Open(0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if port != 0 {
		t.Fatalf("Open returned port %d, want 0 (no socket is bound)", port)
	}
	if len(fns) != 1 {
		t.Fatalf("Open returned %d ReceiveFuncs, want exactly 1", len(fns))
	}
	return fns[0]
}

type recvResult struct {
	n     int
	err   error
	bufs  [][]byte
	sizes []int
	eps   []conn.Endpoint
}

// recvOnce calls recv with one full-size message buffer, as wireguard-go does
// with BatchSize() == 1.
func recvOnce(recv conn.ReceiveFunc, bufLen int) recvResult {
	res := recvResult{
		bufs:  [][]byte{make([]byte, bufLen)},
		sizes: []int{0},
		eps:   []conn.Endpoint{nil},
	}
	res.n, res.err = recv(res.bufs, res.sizes, res.eps)
	return res
}

func TestBind_SendOneFramePerBuffer(t *testing.T) {
	tr := newFakeTransport()
	b := New(tr)

	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	if err := b.Send(payloads, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := tr.sent()
	if len(got) != len(payloads) {
		t.Fatalf("Transport.Send called %d times, want %d (one frame per datagram)", len(got), len(payloads))
	}
	for i := range payloads {
		if string(got[i]) != string(payloads[i]) {
			t.Errorf("frame %d = %q, want %q", i, got[i], payloads[i])
		}
	}
}

func TestBind_SendZeroBuffersIsNil(t *testing.T) {
	tr := newFakeTransport()
	b := New(tr)

	if err := b.Send(nil, nil); err != nil {
		t.Fatalf("Send(nil): %v", err)
	}
	if n := len(tr.sent()); n != 0 {
		t.Fatalf("Transport.Send called %d times for zero buffers", n)
	}
}

func TestBind_SendWrapsTransportError(t *testing.T) {
	tr := newFakeTransport()
	sentinel := errors.New("write failed")
	tr.sendErr = sentinel
	b := New(tr)

	err := b.Send([][]byte{[]byte("pkt")}, nil)
	if err == nil {
		t.Fatal("Send returned nil, want error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Send error = %v, want it to wrap %v", err, sentinel)
	}
	if got := fmt.Sprint(err); len(got) < len("wgbind:") || got[:len("wgbind:")] != "wgbind:" {
		t.Fatalf("Send error = %q, want a wgbind: prefix", got)
	}
}

func TestBind_BatchSizeIsOne(t *testing.T) {
	b := New(newFakeTransport())
	if got := b.BatchSize(); got != 1 {
		t.Fatalf("BatchSize() = %d, want 1", got)
	}
}

func TestBind_SetMarkIsNoop(t *testing.T) {
	b := New(newFakeTransport())
	if err := b.SetMark(0x1234); err != nil {
		t.Fatalf("SetMark: %v", err)
	}
}

// TestBind_ReceiveCopiesIntoBuffer covers the brief's "receive at offset"
// requirement. The wireguard-go version pinned by go.mod
// (v0.0.0-20250521234502) removed ReceiveFunc's offset argument: the caller
// hands over full message buffers and receive implementations write at index
// 0 (see conn.StdNetBind.receiveIP). WireGuard's own buffers carry the
// 16-byte transport header from the wire, so there is nothing to reserve.
func TestBind_ReceiveCopiesIntoBuffer(t *testing.T) {
	tr := newFakeTransport("hello")
	b := New(tr)

	ep, err := b.ParseEndpoint("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}

	recv := openBind(t, b)
	res := recvOnce(recv, 64)
	if res.err != nil {
		t.Fatalf("ReceiveFunc: %v", res.err)
	}
	if res.n != 1 {
		t.Fatalf("ReceiveFunc returned %d packets, want 1", res.n)
	}
	if res.sizes[0] != 5 {
		t.Fatalf("sizes[0] = %d, want 5", res.sizes[0])
	}
	if got := string(res.bufs[0][:res.sizes[0]]); got != "hello" {
		t.Fatalf("payload = %q, want %q", got, "hello")
	}
	if res.eps[0] == nil {
		t.Fatal("eps[0] is nil; wireguard-go would overwrite the peer endpoint with nil and stop sending")
	}
	if got := res.eps[0].DstToString(); got != ep.DstToString() {
		t.Fatalf("received endpoint %q, want %q", got, ep.DstToString())
	}
}

func TestBind_ReceiveRetriesTransientErrors(t *testing.T) {
	old := retryInterval
	retryInterval = time.Millisecond
	t.Cleanup(func() { retryInterval = old })

	tr := newFakeTransport()
	tr.results = []fakeRecv{
		{err: errors.New("wstunnel: receive: reconnect in progress")},
		{err: errors.New("wstunnel: receive: connection reset")},
		{pkt: []byte("hello")},
	}
	b := New(tr)
	recv := openBind(t, b)

	done := make(chan recvResult, 1)
	go func() { done <- recvOnce(recv, 64) }()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ReceiveFunc returned transient error to wireguard-go: %v", res.err)
		}
		if res.n != 1 || res.sizes[0] != 5 {
			t.Fatalf("ReceiveFunc = (%d, sizes=%v), want (1, [5])", res.n, res.sizes)
		}
		if got := string(res.bufs[0][:res.sizes[0]]); got != "hello" {
			t.Fatalf("payload = %q, want %q", got, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReceiveFunc did not recover from transient errors")
	}
	if got := tr.recvCount(); got != 3 {
		t.Fatalf("Transport.Recv called %d times, want 3 (two retries then a frame)", got)
	}
}

func TestBind_CloseUnblocksReceive(t *testing.T) {
	tr := newFakeTransport()
	b := New(tr)
	recv := openBind(t, b)

	done := make(chan recvResult, 1)
	go func() { done <- recvOnce(recv, 64) }()

	tr.waitParked(t)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case res := <-done:
		if !errors.Is(res.err, net.ErrClosed) {
			t.Fatalf("ReceiveFunc after Close = %v, want net.ErrClosed", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the parked ReceiveFunc")
	}

	// Close is idempotent.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestBind_ReceiveAfterCloseReturnsImmediately(t *testing.T) {
	tr := newFakeTransport()
	b := New(tr)
	recv := openBind(t, b)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	start := time.Now()
	res := recvOnce(recv, 64)
	if !errors.Is(res.err, net.ErrClosed) {
		t.Fatalf("ReceiveFunc after Close = %v, want net.ErrClosed", res.err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Fatalf("ReceiveFunc after Close took %v, want an immediate return", elapsed)
	}
	if got := tr.recvCount(); got != 0 {
		t.Fatalf("ReceiveFunc touched Transport.Recv %d times after Close, want 0", got)
	}
}

func TestBind_ParseEndpointKeepsString(t *testing.T) {
	b := New(newFakeTransport())

	ep, err := b.ParseEndpoint("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if got := ep.DstToString(); got != "127.0.0.1:51820" {
		t.Fatalf("DstToString() = %q, want the parsed string", got)
	}
}

func TestBind_CloseReportsTransportError(t *testing.T) {
	sentinel := errors.New("boom")
	b := New(closingErrTransport{err: sentinel})

	if err := b.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close = %v, want an error wrapping %v", err, sentinel)
	}
}

type closingErrTransport struct {
	err error
}

func (c closingErrTransport) Send([]byte) error        { return nil }
func (c closingErrTransport) Recv([]byte) (int, error) { return 0, net.ErrClosed }
func (c closingErrTransport) Close() error             { return c.err }
