package debug

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// FrameEndpoint is one side of the L2 relay: one Read returns exactly one
// whole Ethernet frame; Write consumes exactly one whole frame. *os.File of
// a TAP fd satisfies this natively (TAP read/write are frame-atomic).
type FrameEndpoint interface {
	ReadFrame(buf []byte) (int, error)
	WriteFrame(buf []byte) error
	Close() error
}

// TapEndpoint adapts a TAP *os.File. Blocking semantics are required: a full
// device queue must block the writer (natural backpressure), and a read must
// block until a whole frame arrives. A short write is a fatal error: the tail
// of a frame must never be treated as a new frame.
type TapEndpoint struct {
	F *os.File
}

func (t *TapEndpoint) ReadFrame(buf []byte) (int, error) { return t.F.Read(buf) }

func (t *TapEndpoint) WriteFrame(buf []byte) error {
	n, err := t.F.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

func (t *TapEndpoint) Close() error { return t.F.Close() }

// BridgeCounters reports per-direction frame/byte counters and errors.
type BridgeCounters struct {
	AtoBFrames, AtoBBytes uint64
	BtoAFrames, BtoABytes uint64
}

// Bridge relays whole Ethernet frames between two FrameEndpoints with one
// goroutine per direction. First error on either side tears the relay down
// (both endpoints closed); Wait returns that error.
type Bridge struct {
	a, b  FrameEndpoint
	count BridgeCounters
	once  sync.Once
	wg    sync.WaitGroup
	done  chan struct{}
	err   error
}

// NewBridge wires the two endpoints.
func NewBridge(a, b FrameEndpoint) *Bridge {
	return &Bridge{a: a, b: b, done: make(chan struct{})}
}

// Start launches the two forwarding goroutines.
func (br *Bridge) Start() {
	br.wg.Add(2)
	go br.pump(&br.count.AtoBFrames, &br.count.AtoBBytes, br.a, br.b)
	go br.pump(&br.count.BtoAFrames, &br.count.BtoABytes, br.b, br.a)
}

// pump moves frames src→dst until either side errors or closes; on exit it
// closes both endpoints so the peer direction unblocks.
func (br *Bridge) pump(frames, bytes *uint64, src, dst FrameEndpoint) {
	defer br.wg.Done()
	buf := make([]byte, 65536+64) // covers max tap MTU (65536) plus headroom
	for {
		n, err := src.ReadFrame(buf)
		if err != nil {
			br.fail(err)
			return
		}
		if n == 0 {
			continue
		}
		if err := dst.WriteFrame(buf[:n]); err != nil {
			br.fail(err)
			return
		}
		atomic.AddUint64(frames, 1)
		atomic.AddUint64(bytes, uint64(n))
	}
}

func (br *Bridge) fail(err error) {
	br.once.Do(func() {
		br.err = err
		_ = br.a.Close()
		_ = br.b.Close()
		close(br.done)
	})
}

// Done closes when the relay stops.
func (br *Bridge) Done() <-chan struct{} { return br.done }

// Err returns the relay error once one occurred (nil while healthy).
func (br *Bridge) Err() error {
	select {
	case <-br.done:
		return br.err
	default:
		return nil
	}
}

// Wait blocks until both directions ended.
func (br *Bridge) Wait() error {
	br.wg.Wait()
	return br.Err()
}

// Stop tears the bridge down (idempotent) and waits.
func (br *Bridge) Stop() error {
	br.fail(io.EOF)
	return br.Wait()
}

// Counters returns a snapshot of the per-direction counters.
func (br *Bridge) Counters() BridgeCounters {
	return BridgeCounters{
		AtoBFrames: atomic.LoadUint64(&br.count.AtoBFrames),
		AtoBBytes:  atomic.LoadUint64(&br.count.AtoBBytes),
		BtoAFrames: atomic.LoadUint64(&br.count.BtoAFrames),
		BtoABytes:  atomic.LoadUint64(&br.count.BtoABytes),
	}
}

// ErrBridgeStopped marks an operator-initiated stop (not a fault).
var ErrBridgeStopped = errors.New("debug bridge stopped")
