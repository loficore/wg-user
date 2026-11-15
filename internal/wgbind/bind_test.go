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

// fakeTransport is a scripted Transport. It records every frame it is handed,
// returns queued results from Recv, and then parks until either the caller's
// stop channel closes or the transport itself is closed (the latter only ever
// happens because the test decides to, never because the bind did).
type fakeTransport struct {
	mu        sync.Mutex
	sends     [][]byte
	results   []fakeRecv
	recvCalls int
	sendErr   error

	closed    chan struct{}
	closeOnce sync.Once

	// parked is closed the first time Recv has to wait; observed receives a
	// token for every scripted result handed out.
	parked   chan struct{}
	parkOnce sync.Once
	observed chan struct{}
}

func newFakeTransport(payloads ...string) *fakeTransport {
	f := &fakeTransport{
		closed:   make(chan struct{}),
		parked:   make(chan struct{}),
		observed: make(chan struct{}, 8),
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

func (f *fakeTransport) Recv(buf []byte, stop <-chan struct{}) (int, error) {
	f.mu.Lock()
	f.recvCalls++
	if len(f.results) > 0 {
		r := f.results[0]
		f.results = f.results[1:]
		f.mu.Unlock()
		select {
		case f.observed <- struct{}{}:
		default:
		}
		if r.err != nil {
			return 0, r.err
		}
		return copy(buf, r.pkt), nil
	}
	f.mu.Unlock()

	// Nothing scripted: park like a live transport waiting for a frame.
	f.parkOnce.Do(func() { close(f.parked) })
	select {
	case <-stop:
		return 0, fmt.Errorf("fake: receive: %w", net.ErrClosed)
	case <-f.closed:
		return 0, fmt.Errorf("fake: receive: %w", net.ErrClosed)
	}
}

// Close closes the fake transport itself. The bind must never call it: the
// transport is owned by the process that created it, not by a bind
// generation.
func (f *fakeTransport) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTransport) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
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

func TestBind_TransientErrorsAreRetried(t *testing.T) {
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

	if tr.isClosed() {
		t.Fatal("Close closed the transport; the transport outlives bind generations")
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

// TestBind_CloseThenOpenWorksAgain models wireguard-go's BindUpdate, which
// closes the bind, waits for the receive routines to stop, and only then
// opens it again. device.Up() starts with BindUpdate, so the very first Up
// closes before it opens.
func TestBind_CloseThenOpenWorksAgain(t *testing.T) {
	tr := newFakeTransport("first", "second")
	b := New(tr)

	// device.Up() closes before the first Open: there is no generation yet.
	if err := b.Close(); err != nil {
		t.Fatalf("Close before first Open: %v", err)
	}

	recv1 := openBind(t, b)
	res := recvOnce(recv1, 64)
	if res.err != nil || string(res.bufs[0][:res.sizes[0]]) != "first" {
		t.Fatalf("first generation received (%q, %v), want %q", res.bufs[0][:res.sizes[0]], res.err, "first")
	}

	// BindUpdate for a reconfiguration: Close, wait, Open.
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if res := recvOnce(recv1, 64); !errors.Is(res.err, net.ErrClosed) {
		t.Fatalf("retired generation's ReceiveFunc = %v, want net.ErrClosed", res.err)
	}

	recv2 := openBind(t, b)
	res = recvOnce(recv2, 64)
	if res.err != nil {
		t.Fatalf("ReceiveFunc from the re-opened bind = %v, want the next datagram", res.err)
	}
	if res.n != 1 || res.sizes[0] != 6 {
		t.Fatalf("ReceiveFunc = (%d, sizes=%v), want (1, [6])", res.n, res.sizes)
	}
	if got := string(res.bufs[0][:res.sizes[0]]); got != "second" {
		t.Fatalf("re-opened bind received %q, want %q", got, "second")
	}
}

func TestBind_CloseDoesNotCloseTransport(t *testing.T) {
	tr := newFakeTransport()
	b := New(tr)
	recv := openBind(t, b)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = recv

	if tr.isClosed() {
		t.Fatal("Close closed the transport; its lifetime belongs to the CLI")
	}
	if err := b.Send([][]byte{[]byte("after")}, nil); err != nil {
		t.Fatalf("Send after Close: %v", err)
	}
	got := tr.sent()
	if len(got) != 1 || string(got[0]) != "after" {
		t.Fatalf("transport sent %q, want one %q frame", got, "after")
	}
}

func TestBind_CloseDuringRetryWaitIsPrompt(t *testing.T) {
	old := retryInterval
	retryInterval = time.Second
	t.Cleanup(func() { retryInterval = old })

	tr := newFakeTransport()
	tr.results = []fakeRecv{{err: errors.New("transient")}}
	b := New(tr)
	recv := openBind(t, b)

	done := make(chan recvResult, 1)
	go func() { done <- recvOnce(recv, 64) }()

	select {
	case <-tr.observed:
		// Recv returned the transient error; the loop is about to wait.
	case <-time.After(2 * time.Second):
		t.Fatal("Transport.Recv was never asked for a datagram")
	}

	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case res := <-done:
		if !errors.Is(res.err, net.ErrClosed) {
			t.Fatalf("ReceiveFunc = %v, want net.ErrClosed", res.err)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Fatalf("Close took %v to interrupt a %v retry wait", elapsed, retryInterval)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not interrupt the retry wait")
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
