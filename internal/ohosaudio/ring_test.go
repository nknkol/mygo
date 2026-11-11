//go:build linux && (amd64 || arm64)

package ohosaudio

import (
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeHostBridge struct {
	mu       sync.Mutex
	openID   uint32
	rate     uint32
	channels uint32
	latency  uint32
	shmBytes uint32
	dupFD    int
	closes   []uint32
	openErr  error
}

func (f *fakeHostBridge) AudioOpen(streamID, sampleRate, channels, latencyMode uint32, shmFD int, shmBytes uint32) error {
	if f.openErr != nil {
		return f.openErr
	}
	dup, err := syscall.Dup(shmFD)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.openID = streamID
	f.rate = sampleRate
	f.channels = channels
	f.latency = latencyMode
	f.shmBytes = shmBytes
	f.dupFD = dup
	f.mu.Unlock()
	return nil
}

func (f *fakeHostBridge) AudioClose(streamID uint32) error {
	f.mu.Lock()
	f.closes = append(f.closes, streamID)
	f.mu.Unlock()
	return nil
}

// Write wraps around the shared-memory ringbuffer without corruption and blocks once targetAhead is buffered until the consumer advances ReadFrames and wakes the futex.
func TestRingWrapBackpressureAndWake(t *testing.T) {
	fb := &fakeHostBridge{dupFD: -1}
	SetBridge(fb)
	t.Cleanup(func() {
		ClearBridge(fb)
		fb.mu.Lock()
		fd := fb.dupFD
		fb.dupFD = -1
		fb.mu.Unlock()
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
	})

	s, chunkFrames, err := Open(48000, 2, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if chunkFrames != 480 {
		t.Fatalf("chunkFrames = %d, want 480", chunkFrames)
	}
	if fb.latency != LatencyFast {
		t.Fatalf("latency = %d, want LatencyFast (%d)", fb.latency, LatencyFast)
	}

	hdr, ring, unmap, err := MapForConsumer(fb.dupFD, fb.shmBytes, 2)
	if err != nil {
		t.Fatalf("MapForConsumer: %v", err)
	}
	defer unmap()

	totalFrames := int(hdr.CapFrames)*3 + 333
	want := make([]float32, totalFrames*2)
	for i := range want {
		want[i] = float32((i%997)-498) / 512.0
	}

	got := make([]float32, 0, len(want))
	stopConsumer := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		mask := uint64(hdr.CapFrames - 1)
		for len(got) < len(want) {
			select {
			case <-stopConsumer:
				return
			default:
			}
			w := hdr.WriteFrames.Load()
			r := hdr.ReadFrames.Load()
			if w <= r {
				time.Sleep(time.Millisecond)
				continue
			}
			avail := int(w - r)
			take := min(avail, 257)
			pos := int(r & mask)
			first := min(take, int(hdr.CapFrames)-pos)
			got = append(got, ring[pos*2:(pos+first)*2]...)
			if second := take - first; second > 0 {
				got = append(got, ring[:second*2]...)
			}
			hdr.HeartbeatNs.Store(MonoNano())
			hdr.ReadFrames.Store(r + uint64(take))
			WakeProducer(hdr)
		}
	}()
	defer func() {
		close(stopConsumer)
		<-consumerDone
	}()

	for off := 0; off < len(want); off += chunkFrames * 2 {
		end := min(off+chunkFrames*2, len(want))
		if err := s.Write(want[off:end]); err != nil {
			t.Fatalf("Write at %d: %v", off, err)
		}
	}

	deadline := time.After(2 * time.Second)
	for hdr.ReadFrames.Load() < uint64(totalFrames) {
		select {
		case <-deadline:
			t.Fatalf("consumer read %d/%d frames before timeout", hdr.ReadFrames.Load(), totalFrames)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	<-consumerDone

	if len(got) != len(want) {
		t.Fatalf("consumed %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// Write unblocks with an error when the host renderer reports StateError, and Close releases the host stream at most once.
func TestRingRendererErrorAndIdempotentClose(t *testing.T) {
	fb := &fakeHostBridge{dupFD: -1}
	SetBridge(fb)
	t.Cleanup(func() {
		ClearBridge(fb)
		fb.mu.Lock()
		fd := fb.dupFD
		fb.dupFD = -1
		fb.mu.Unlock()
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
	})

	s, chunkFrames, err := Open(48000, 2, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	hdr, _, unmap, err := MapForConsumer(fb.dupFD, fb.shmBytes, 2)
	if err != nil {
		t.Fatalf("MapForConsumer: %v", err)
	}
	defer unmap()

	var blocked atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		// Write enough frames to exceed targetAhead + chunkFrames so Write parks in futex wait.
		big := make([]float32, (int(hdr.CapFrames)+chunkFrames*4)*2)
		blocked.Store(true)
		errCh <- s.Write(big)
	}()

	for !blocked.Load() || hdr.WriteFrames.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	hdr.ErrCode.Store(42)
	hdr.State.Store(StateError)
	WakeProducer(hdr)

	select {
	case werr := <-errCh:
		if werr == nil || !strings.Contains(werr.Error(), "code 42") {
			t.Fatalf("Write error = %v, want renderer error code 42", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked Write to observe StateError")
	}

	s.Close()
	s.Close()
	fb.mu.Lock()
	closes := append([]uint32(nil), fb.closes...)
	fb.mu.Unlock()
	if len(closes) != 1 || closes[0] != fb.openID {
		t.Fatalf("closes = %v, want [%d]", closes, fb.openID)
	}
}
