//go:build linux && (amd64 || arm64) && mygo_ohos

package audio

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/ohosaudio"
)

type testOHOSAudioBridge struct {
	mu       sync.Mutex
	dupFD    int
	shmBytes uint32
	closed   int
}

func (b *testOHOSAudioBridge) AudioOpen(_, _, _, _ uint32, shmFD int, shmBytes uint32) error {
	dup, err := syscall.Dup(shmFD)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.dupFD = dup
	b.shmBytes = shmBytes
	b.mu.Unlock()
	return nil
}

func (b *testOHOSAudioBridge) AudioClose(_ uint32) error {
	b.mu.Lock()
	b.closed++
	b.mu.Unlock()
	return nil
}

// openDevice streams mixed float32 stereo frames through the ohosaudio shared-memory ringbuffer and closes cleanly.
func TestOHOSDeviceStreamsPCMToConsumer(t *testing.T) {
	setup(t)
	tb := &testOHOSAudioBridge{dupFD: -1}
	ohosaudio.SetBridge(tb)
	t.Cleanup(func() {
		ohosaudio.ClearBridge(tb)
		tb.mu.Lock()
		fd := tb.dupFD
		tb.dupFD = -1
		tb.mu.Unlock()
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
	})

	dev, chunkFrames, err := openDevice(60 * time.Millisecond)
	if err != nil {
		t.Fatalf("openDevice: %v", err)
	}
	hdr, ring, unmap, err := ohosaudio.MapForConsumer(tb.dupFD, tb.shmBytes, channels)
	if err != nil {
		dev.close()
		t.Fatalf("MapForConsumer: %v", err)
	}
	defer unmap()

	NewSound(sampleRate, 2, []float32{0.25, -0.25, 0.5, -0.5}).Play()
	chunk := render(chunkFrames)
	if err := dev.write(chunk); err != nil {
		dev.close()
		t.Fatalf("dev.write: %v", err)
	}
	if got := hdr.WriteFrames.Load(); got != uint64(chunkFrames) {
		dev.close()
		t.Fatalf("WriteFrames = %d, want %d", got, chunkFrames)
	}
	if !near(ring[0], 0.25) || !near(ring[1], -0.25) || !near(ring[2], 0.5) || !near(ring[3], -0.5) {
		dev.close()
		t.Fatalf("ring head = %v, want [0.25 -0.25 0.5 -0.5]", ring[:4])
	}

	dev.close()
	tb.mu.Lock()
	closed := tb.closed
	tb.mu.Unlock()
	if closed != 1 || hdr.State.Load() != ohosaudio.StateClosed {
		t.Fatalf("after close: closed=%d state=%d", closed, hdr.State.Load())
	}
}
