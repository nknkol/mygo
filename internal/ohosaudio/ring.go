//go:build linux && (amd64 || arm64)

// Package ohosaudio connects package audio to the HarmonyOS host process over a
// shared-memory SPSC PCM ringbuffer. Keeping OH_AudioRenderer inside the
// Stage-model Ability process avoids cgo callbacks on real-time audio pthreads
// and ties the audio stream directly to the Ability's AVSession and
// AUDIO_PLAYBACK continuous background task.
package ohosaudio

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// HeaderBytes is the fixed byte size of the shared-memory ring header.
const HeaderBytes = 64

// Stream states stored in ShmHeader.State.
const (
	StateRunning uint32 = 0
	StateClosed  uint32 = 1
	StateError   uint32 = 2
)

// Latency modes matching OH_AudioStream_LatencyMode.
const (
	LatencyNormal uint32 = 0
	LatencyFast   uint32 = 1
)

const (
	mfdCloexec = 0x0001
	futexWait  = 0 // shared futex (no FUTEX_PRIVATE_FLAG)
	futexWake  = 1 // shared futex (no FUTEX_PRIVATE_FLAG)
)

// Bridge sends audio stream lifecycle commands (and the shared-memory fd) to
// the HarmonyOS host process.
type Bridge interface {
	AudioOpen(streamID, sampleRate, channels, latencyMode uint32, shmFD int, shmBytes uint32) error
	AudioClose(streamID uint32) error
}

var (
	bridgeMu  sync.RWMutex
	curBridge Bridge
	nextID    atomic.Uint32
)

// SetBridge installs the active HarmonyOS host bridge.
func SetBridge(b Bridge) {
	bridgeMu.Lock()
	curBridge = b
	bridgeMu.Unlock()
}

// ClearBridge removes b if it is still the active bridge.
func ClearBridge(b Bridge) {
	bridgeMu.Lock()
	if curBridge == b {
		curBridge = nil
	}
	bridgeMu.Unlock()
}

func loadBridge() Bridge {
	bridgeMu.RLock()
	b := curBridge
	bridgeMu.RUnlock()
	return b
}

// ShmHeader is the 64-byte header at offset 0 of the shared-memory segment,
// followed immediately by CapFrames*channels float32 samples at offset 64.
type ShmHeader struct {
	WriteFrames atomic.Uint64 // [0..7]   stereo frames written by Go producer
	ReadFrames  atomic.Uint64 // [8..15]  stereo frames read by C++ consumer
	CapFrames   uint32        // [16..19] ring capacity in frames (power of two)
	TargetAhead uint32        // [20..23] target buffered frames ahead of output
	State       atomic.Uint32 // [24..27] StateRunning / StateClosed / StateError
	ErrCode     atomic.Uint32 // [28..31] OH_AudioStream_Result when StateError
	HeartbeatNs atomic.Uint64 // [32..39] consumer monotonic timestamp in ns
	ReadSeq     atomic.Uint32 // [40..43] incremented + FUTEX_WAKE on consumer progress
	_           [20]byte      // [44..63] pad to 64 bytes
}

var (
	_ [HeaderBytes - unsafe.Sizeof(ShmHeader{})]byte
	_ [unsafe.Sizeof(ShmHeader{}) - HeaderBytes]byte
)

// Stream is an open shared-memory PCM output stream.
type Stream struct {
	id             uint32
	channels       int
	framesPerWrite int
	capFrames      uint32
	targetAhead    uint32
	mem            []byte
	hdr            *ShmHeader
	samples        []float32
	bridge         Bridge
	closeOnce      sync.Once
}

var startTime = time.Now()

// MonoNano returns a process-monotonic nanosecond timestamp.
func MonoNano() uint64 {
	d := time.Since(startTime)
	if d <= 0 {
		return 1
	}
	return uint64(d)
}

func nextPow2(v uint32) uint32 {
	if v <= 1 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return v + 1
}

func createSharedFD(size int) (int, error) {
	name := [...]byte{'m', 'y', 'g', 'o', '-', 'a', 'u', 'd', 'i', 'o', 0}
	r1, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(&name[0])), mfdCloexec, 0)
	if errno == 0 && int(r1) >= 0 {
		fd := int(r1)
		if err := syscall.Ftruncate(fd, int64(size)); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
		return fd, nil
	}

	f, err := os.CreateTemp("", "mygo-audio-*")
	if err != nil {
		return -1, fmt.Errorf("audio: ohos shm create failed (memfd errno=%d): %w", errno, err)
	}
	_ = os.Remove(f.Name())
	if err := f.Truncate(int64(size)); err != nil {
		_ = f.Close()
		return -1, err
	}
	dupFD, err := syscall.Dup(int(f.Fd()))
	_ = f.Close()
	if err != nil {
		return -1, err
	}
	syscall.CloseOnExec(dupFD)
	return dupFD, nil
}

// Open opens a new shared-memory PCM output stream through the active Bridge
// and returns the stream and the chunk size in frames per Write.
func Open(sampleRate, channels int, ahead time.Duration) (*Stream, int, error) {
	b := loadBridge()
	if b == nil {
		return nil, 0, errors.New("audio: ohos host bridge not initialized")
	}
	if sampleRate <= 0 || channels <= 0 {
		return nil, 0, errors.New("audio: invalid sample rate or channel count")
	}

	framesPerWrite := max(sampleRate/100, 64) // 10 ms per chunk (480 frames @ 48 kHz)
	aheadFrames := int(int64(ahead) * int64(sampleRate) / int64(time.Second))
	if aheadFrames < framesPerWrite*2 {
		aheadFrames = framesPerWrite * 2
	}
	capFrames := nextPow2(uint32(max(aheadFrames*2, framesPerWrite*8)))
	sampleCount := int(capFrames) * channels
	shmBytes := HeaderBytes + sampleCount*4

	fd, err := createSharedFD(shmBytes)
	if err != nil {
		return nil, 0, err
	}
	mem, err := syscall.Mmap(fd, 0, shmBytes, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		_ = syscall.Close(fd)
		return nil, 0, fmt.Errorf("audio: ohos mmap failed: %w", err)
	}

	hdr := (*ShmHeader)(unsafe.Pointer(&mem[0]))
	hdr.CapFrames = capFrames
	hdr.TargetAhead = uint32(aheadFrames)
	hdr.State.Store(StateRunning)
	hdr.HeartbeatNs.Store(MonoNano())

	latMode := LatencyNormal
	if ahead <= 35*time.Millisecond {
		latMode = LatencyFast
	}

	id := nextID.Add(1)
	openErr := b.AudioOpen(id, uint32(sampleRate), uint32(channels), latMode, fd, uint32(shmBytes))
	_ = syscall.Close(fd)
	if openErr != nil {
		_ = syscall.Munmap(mem)
		return nil, 0, fmt.Errorf("audio: ohos open stream: %w", openErr)
	}

	samples := unsafe.Slice((*float32)(unsafe.Pointer(&mem[HeaderBytes])), sampleCount)
	return &Stream{
		id:             id,
		channels:       channels,
		framesPerWrite: framesPerWrite,
		capFrames:      capFrames,
		targetAhead:    uint32(aheadFrames),
		mem:            mem,
		hdr:            hdr,
		samples:        samples,
		bridge:         b,
	}, framesPerWrite, nil
}

// MapForConsumer maps an existing shared-memory descriptor on the consumer side
// (used by tests simulating the C++ OH_AudioRenderer callback).
func MapForConsumer(fd int, shmBytes uint32, channels int) (*ShmHeader, []float32, func(), error) {
	if shmBytes < HeaderBytes || channels <= 0 {
		return nil, nil, nil, errors.New("ohosaudio: invalid consumer shm size")
	}
	mem, err := syscall.Mmap(fd, 0, int(shmBytes), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, nil, err
	}
	hdr := (*ShmHeader)(unsafe.Pointer(&mem[0]))
	sampleCount := int(shmBytes-HeaderBytes) / 4
	samples := unsafe.Slice((*float32)(unsafe.Pointer(&mem[HeaderBytes])), sampleCount)
	unmap := func() { _ = syscall.Munmap(mem) }
	return hdr, samples, unmap, nil
}

// WakeProducer increments ReadSeq and wakes a blocked Write via shared futex.
func WakeProducer(hdr *ShmHeader) {
	hdr.ReadSeq.Add(1)
	_, _, _ = syscall.Syscall6(
		sysFutex,
		uintptr(unsafe.Pointer(&hdr.ReadSeq)),
		futexWake,
		1,
		0, 0, 0,
	)
}

func waitConsumer(hdr *ShmHeader, seq uint32, timeout time.Duration) {
	ts := syscall.NsecToTimespec(timeout.Nanoseconds())
	_, _, _ = syscall.Syscall6(
		sysFutex,
		uintptr(unsafe.Pointer(&hdr.ReadSeq)),
		futexWait,
		uintptr(seq),
		uintptr(unsafe.Pointer(&ts)),
		0, 0,
	)
}

// Write appends interleaved float32 frames to the ring, blocking while at least
// targetAhead frames are already buffered ahead of the consumer.
func (s *Stream) Write(samples []float32) error {
	if s == nil || s.hdr == nil {
		return errors.New("audio: closed ohos stream")
	}
	ch := s.channels
	totalFrames := len(samples) / ch
	if totalFrames == 0 {
		return nil
	}
	mask := uint64(s.capFrames - 1)
	maxBuffered := uint64(s.targetAhead)
	if maxBuffered > uint64(s.capFrames) {
		maxBuffered = uint64(s.capFrames)
	}

	written := 0
	stallStart := time.Time{}
	for written < totalFrames {
		st := s.hdr.State.Load()
		if st == StateClosed {
			return errors.New("audio: ohos stream closed")
		}
		if st != StateRunning {
			return fmt.Errorf("audio: ohos renderer error (code %d)", s.hdr.ErrCode.Load())
		}

		w := s.hdr.WriteFrames.Load()
		r := s.hdr.ReadFrames.Load()
		var used uint64
		if w >= r {
			used = w - r
		}

		// Allow filling up to targetAhead + one write chunk, never exceeding ring capacity.
		limit := min(maxBuffered+uint64(s.framesPerWrite), uint64(s.capFrames))
		if used >= limit {
			seq := s.hdr.ReadSeq.Load()
			// Re-check after reading ReadSeq so we never miss a wakeup.
			if s.hdr.State.Load() != StateRunning {
				continue
			}
			r2 := s.hdr.ReadFrames.Load()
			if w >= r2 && (w-r2) < limit {
				stallStart = time.Time{}
				continue
			}
			if stallStart.IsZero() {
				stallStart = time.Now()
			} else if time.Since(stallStart) > 1500*time.Millisecond {
				return errors.New("audio: ohos renderer stalled")
			}
			waitConsumer(s.hdr, seq, 25*time.Millisecond)
			if s.hdr.ReadFrames.Load() != r {
				stallStart = time.Time{}
			}
			continue
		}
		stallStart = time.Time{}

		room := int(limit - used)
		rem := totalFrames - written
		n := min(rem, room)

		pos := int(w & mask)
		first := min(n, int(s.capFrames)-pos)
		src := samples[written*ch : (written+n)*ch]
		copy(s.samples[pos*ch:(pos+first)*ch], src[:first*ch])
		if second := n - first; second > 0 {
			copy(s.samples[:second*ch], src[first*ch:])
		}
		s.hdr.WriteFrames.Store(w + uint64(n))
		written += n
	}
	return nil
}

// Close stops the host OH_AudioRenderer and unmaps the shared ringbuffer.
func (s *Stream) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.hdr != nil {
			s.hdr.State.Store(StateClosed)
		}
		if s.bridge != nil {
			_ = s.bridge.AudioClose(s.id)
		}
		if len(s.mem) > 0 {
			_ = syscall.Munmap(s.mem)
			s.mem = nil
			s.hdr = nil
			s.samples = nil
		}
	})
}
