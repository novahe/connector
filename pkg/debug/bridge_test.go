package debug

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// chanEndpoint is an in-memory FrameEndpoint that preserves whole-frame
// boundaries (one Write → one Read), mirroring TAP read/write semantics.
type chanEndpoint struct {
	in  chan []byte
	out chan []byte
	err error

	mu     sync.Mutex
	closed bool
}

// newChanEndpoint builds an independent endpoint: the bridge reads frames the
// "kernel" transmitted (in) and injects frames toward it (out).
func newChanEndpoint() *chanEndpoint {
	return &chanEndpoint{in: make(chan []byte, 8), out: make(chan []byte, 8)}
}

func (c *chanEndpoint) ReadFrame(buf []byte) (int, error) {
	frame, ok := <-c.in
	if !ok {
		return 0, io.EOF
	}
	copy(buf, frame)
	return len(frame), nil
}

func (c *chanEndpoint) WriteFrame(buf []byte) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	c.out <- append([]byte(nil), buf...)
	return nil
}

func (c *chanEndpoint) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.in)
		close(c.out)
	}
	c.mu.Unlock()
	return nil
}

// failingWriteEndpoint accepts reads but fails on first write.
type failingWriteEndpoint struct {
	*chanEndpoint
	failErr error
}

func (f *failingWriteEndpoint) WriteFrame(buf []byte) error { return f.failErr }

func waitFrame(t *testing.T, ch chan []byte, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if string(got) != want {
			t.Fatalf("frame = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for frame %q", want)
	}
}

func TestBridgeRelaysBothDirections(t *testing.T) {
	a, b := newChanEndpoint(), newChanEndpoint()
	br := NewBridge(a, b)
	br.Start()
	defer br.Stop()

	// env transmits into A → bridge → env observes on B
	a.in <- []byte("frame-a")
	waitFrame(t, b.out, "frame-a")
	b.in <- []byte("frame-b")
	waitFrame(t, a.out, "frame-b")

	// Counters must reflect one frame each direction after a settle.
	time.Sleep(100 * time.Millisecond)
	c := br.Counters()
	if c.AtoBFrames != 1 || c.BtoAFrames != 1 || c.AtoBBytes != 7 || c.BtoABytes != 7 {
		t.Fatalf("counters = %+v", c)
	}
}

func TestBridgeWriteFailureTearsDownBothSides(t *testing.T) {
	a, b := newChanEndpoint(), newChanEndpoint()
	fwd := &failingWriteEndpoint{chanEndpoint: b, failErr: errors.New("tap write failed")}
	br := NewBridge(a, fwd)
	br.Start()

	a.in <- []byte("x")
	select {
	case err := <-errDone(br):
		if err == nil || err.Error() != "tap write failed" {
			t.Fatalf("relay err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not report the write failure")
	}
	// The healthy endpoint must be closed too (peer direction unblocked).
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if !closed {
		t.Fatal("peer endpoint was not closed on relay failure")
	}
}

func TestBridgeStopIsIdempotent(t *testing.T) {
	a, b := newChanEndpoint(), newChanEndpoint()
	br := NewBridge(a, b)
	br.Start()
	if err := br.Stop(); err != nil && err != io.EOF {
		t.Fatalf("first stop: %v", err)
	}
	if err := br.Stop(); err != nil && err != io.EOF {
		t.Fatalf("second stop: %v", err)
	}
}

func TestBridgeErrorCanBeObservedRepeatedly(t *testing.T) {
	a, b := newChanEndpoint(), newChanEndpoint()
	br := NewBridge(a, &failingWriteEndpoint{chanEndpoint: b, failErr: errors.New("write failed")})
	br.Start()
	a.in <- []byte("x")
	select {
	case <-br.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not stop")
	}
	for i := 0; i < 2; i++ {
		if err := br.Err(); err == nil || err.Error() != "write failed" {
			t.Fatalf("Err call %d: %v", i, err)
		}
	}
	if err := br.Wait(); err == nil || err.Error() != "write failed" {
		t.Fatalf("Wait: %v", err)
	}
}

func errDone(br *Bridge) <-chan error {
	ch := make(chan error, 1)
	go func() {
		br.wg.Wait()
		ch <- br.Err()
	}()
	return ch
}
