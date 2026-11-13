//go:build linux && (amd64 || arm64)

// Package ohosvideo connects package ui/video to the HarmonyOS host process's
// native OH_AVPlayer and OH_ImageReceiverNative pipeline over shared memory.
// Keeping OH_AVPlayer inside the Stage-model Ability process avoids C callbacks
// on foreign multimedia/Binder threads in pure-Go binaries while delivering
// hardware-decoded NV12 frames with zero socket payload copying.
package ohosvideo

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/media"
	"github.com/egoist/mygo/internal/scene"
)

// ErrNoBridge reports that the HarmonyOS host IPC bridge has not been initialized
// (for example when running standalone unit tests outside a packaged .hap).
var ErrNoBridge = errors.New("ohosvideo: host bridge not initialized")

const (
	// HeaderBytes is the byte size of ShmHeader at offset 0 of a video shm segment.
	HeaderBytes = 64
	// NumSlots is the number of frame slots following ShmHeader in shared memory.
	NumSlots = 3

	mfdCloexec = 0x0001
)

// Pixel formats stored in ShmHeader.Format.
const (
	FormatNV12 uint8 = 0
	FormatBGRA uint8 = 1
)

// Engine -> host command opcodes (kindVideoCmd).
const (
	CmdPlay      byte = 1
	CmdPause     byte = 2
	CmdSeek      byte = 3 // arg = target position in milliseconds
	CmdSetLoop   byte = 4 // arg = 0 or 1
	CmdSetRate   byte = 5 // farg = playback rate
	CmdSetVolume byte = 6 // farg = linear volume [0, 1]
	CmdSetMuted  byte = 7 // arg = 0 or 1
	CmdClose     byte = 8
)

// Host -> engine event codes (kindVideoEvent).
const (
	EvFrame     byte = 1
	EvMeta      byte = 2
	EvBuffering byte = 3
	EvSeekDone  byte = 4
	EvEnded     byte = 5
	EvError     byte = 6
)

// Bridge sends video player lifecycle and playback commands to the HarmonyOS host.
type Bridge interface {
	VideoOpen(playerID uint32, rawURL string, fd int, size int64) error
	VideoCmd(playerID uint32, op byte, arg int64, farg float64) error
}

var (
	bridgeMu  sync.RWMutex
	curBridge Bridge
	nextID    atomic.Uint32

	playersMu sync.RWMutex
	players   = make(map[uint32]*Engine)
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
// followed by NumSlots tightly-packed frame slots of SlotBytes each.
type ShmHeader struct {
	WriteSeq   atomic.Uint32 // [0..3]   incremented after publishing LatestSlot
	LatestSlot atomic.Uint32 // [4..7]   most recently written slot index (0..NumSlots-1)
	BusySlot   atomic.Int32  // [8..11]  slot index currently copied by Go, or -1
	Format     uint8         // [12]     FormatNV12 or FormatBGRA
	Matrix     uint8         // [13]     scene.YUVMatrix value
	_          [2]byte       // [14..15] pad
	PosMs      atomic.Int64  // [16..23] playback position in ms
	DurMs      atomic.Int64  // [24..31] media duration in ms (0 until known)
	SlotBytes  uint32        // [32..35] byte capacity per slot
	Width      atomic.Uint32 // [36..39] decoded frame width in pixels
	Height     atomic.Uint32 // [40..43] decoded frame height in pixels
	_          [20]byte      // [44..63] pad to 64 bytes
}

var (
	_ [HeaderBytes - unsafe.Sizeof(ShmHeader{})]byte
	_ [unsafe.Sizeof(ShmHeader{}) - HeaderBytes]byte
)

// SlotNeededBytes returns the byte size required for one frame slot of w×h in format.
func SlotNeededBytes(w, h int, format uint8) int {
	if w <= 0 || h <= 0 {
		return 0
	}
	if format == FormatBGRA {
		return 4 * w * h
	}
	cw, ch := (w+1)/2, (h+1)/2
	return w*h + 2*cw*ch
}

type videoFrame struct {
	surface scene.Surface
	w, h    int
}

// Engine implements internal/media.Engine over the HarmonyOS host bridge.
type Engine struct {
	id     uint32
	bridge Bridge

	mu        sync.Mutex
	shmMem    []byte
	shmHdr    *ShmHeader
	slotBytes int
	lastSeq   uint32

	ring          [3]videoFrame
	shown, latest int
	changed       bool

	w, h      int
	state     media.State
	err       error
	buffering bool
	loop      bool
	wantPlay  bool
	rate      float64
	volume    float64
	muted     bool
	posMs     int64
	durMs     int64
	seekPend  bool
	seekMs    int64
	wake      func()
	closed    bool
}

// OpenURL opens a local file: URL (via an open file descriptor) or a remote
// stream URL on the HarmonyOS host's OH_AVPlayer.
func OpenURL(rawURL string) (media.Engine, error) {
	b := loadBridge()
	if b == nil {
		return nil, ErrNoBridge
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("video: empty URL")
	}

	if u, err := url.Parse(rawURL); err == nil && strings.EqualFold(u.Scheme, "file") {
		localPath := u.Path
		if localPath == "" {
			localPath = u.Opaque
		}
		f, err := os.Open(localPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.IsDir() || info.Size() <= 0 {
			return nil, fmt.Errorf("video: invalid file %s", localPath)
		}
		return openEngine(b, "", int(f.Fd()), info.Size())
	}

	return openEngine(b, rawURL, -1, 0)
}

// OpenData opens an in-memory video buffer by writing it to an anonymous memfd
// and passing the descriptor to OH_AVPlayer_SetFDSource in the host process.
func OpenData(data []byte) (media.Engine, error) {
	b := loadBridge()
	if b == nil {
		return nil, ErrNoBridge
	}
	if len(data) == 0 {
		return nil, errors.New("video: no data")
	}
	fd, err := createDataFD(data)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	return openEngine(b, "", fd, int64(len(data)))
}

func openEngine(b Bridge, rawURL string, fd int, size int64) (*Engine, error) {
	id := nextID.Add(1)
	e := &Engine{
		id:     id,
		bridge: b,
		shown:  -1,
		latest: -1,
		rate:   1,
		volume: 1,
		state:  media.Loading,
	}
	playersMu.Lock()
	players[id] = e
	playersMu.Unlock()

	if err := b.VideoOpen(id, rawURL, fd, size); err != nil {
		playersMu.Lock()
		delete(players, id)
		playersMu.Unlock()
		return nil, err
	}
	return e, nil
}

func createDataFD(data []byte) (int, error) {
	name := [...]byte{'m', 'y', 'g', 'o', '-', 'v', 'i', 'd', 'e', 'o', 0}
	r1, _, errno := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(&name[0])), mfdCloexec, 0)
	runtime.KeepAlive(name)
	if errno == 0 && int(r1) >= 0 {
		fd := int(r1)
		if err := writeAllFD(fd, data); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
		_, _ = syscall.Seek(fd, 0, io.SeekStart)
		return fd, nil
	}

	f, err := os.CreateTemp("", "mygo-video-*")
	if err != nil {
		return -1, fmt.Errorf("video: memfd create failed (errno=%d): %w", errno, err)
	}
	_ = os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return -1, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
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

func writeAllFD(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := syscall.Write(fd, data)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// OnVideoShm installs or replaces the shared-memory frame ring for playerID.
// Ownership of fd transfers to OnVideoShm, which closes it after mmap.
func OnVideoShm(playerID, slotBytes, w, h uint32, fd int) {
	if fd < 0 {
		return
	}
	defer syscall.Close(fd)
	if slotBytes == 0 {
		return
	}
	total := HeaderBytes + int(slotBytes)*NumSlots
	if total <= HeaderBytes {
		return
	}

	playersMu.RLock()
	e := players[playerID]
	playersMu.RUnlock()
	if e == nil {
		return
	}

	mem, err := syscall.Mmap(fd, 0, total, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return
	}

	var wake func()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		_ = syscall.Munmap(mem)
		return
	}
	oldMem := e.shmMem
	e.shmMem = mem
	e.shmHdr = (*ShmHeader)(unsafe.Pointer(&mem[0]))
	e.slotBytes = int(slotBytes)
	e.lastSeq = 0
	if w > 0 && h > 0 && e.w == 0 && e.h == 0 {
		e.w, e.h = int(w), int(h)
	}
	if e.pullFrameLocked() {
		wake = e.wake
	}
	e.mu.Unlock()

	if oldMem != nil {
		_ = syscall.Munmap(oldMem)
	}
	if wake != nil {
		wake()
	}
}

// OnVideoEvent updates playback metadata/state for playerID and wakes its window.
func OnVideoEvent(playerID uint32, ev, buffering byte, posMs, durMs int64, w, h uint32, errText string) {
	playersMu.RLock()
	e := players[playerID]
	playersMu.RUnlock()
	if e == nil {
		return
	}

	var (
		wake    func()
		loopCmd bool
	)
	e.mu.Lock()
	if !e.closed {
		if durMs > 0 {
			e.durMs = durMs
		}
		if w > 0 && h > 0 {
			e.w, e.h = int(w), int(h)
		}
		switch ev {
		case EvFrame:
			if posMs >= 0 && !e.seekPend {
				e.posMs = posMs
			}
			if e.pullFrameLocked() {
				wake = e.wake
			}
		case EvMeta:
			if posMs >= 0 && !e.seekPend {
				e.posMs = posMs
			}
		case EvBuffering:
			e.buffering = buffering != 0
			switch {
			case e.buffering && e.state == media.Ready:
				e.state = media.Buffering
				wake = e.wake
			case !e.buffering && e.state == media.Buffering:
				e.state = media.Ready
				wake = e.wake
			}
		case EvSeekDone:
			e.seekPend = false
			if posMs >= 0 {
				e.posMs = posMs
			}
			_ = e.pullFrameLocked()
			wake = e.wake
		case EvEnded:
			_ = e.pullFrameLocked()
			if e.loop {
				e.posMs = 0
				loopCmd = true
			} else {
				if e.durMs > 0 {
					e.posMs = e.durMs
				}
				e.state = media.Ended
			}
			wake = e.wake
		case EvError:
			msg := strings.TrimSpace(errText)
			if msg == "" {
				msg = "OH_AVPlayer playback error"
			}
			e.state = media.Failed
			e.err = errors.New("video: " + msg)
			wake = e.wake
		}
	}
	e.mu.Unlock()

	if loopCmd {
		_ = e.bridge.VideoCmd(e.id, CmdSeek, 0, 0)
		if e.isPlaying() {
			_ = e.bridge.VideoCmd(e.id, CmdPlay, 0, 0)
		}
	}
	if wake != nil {
		wake()
	}
}

func (e *Engine) isPlaying() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && e.wantPlay
}

// pullFrameLocked copies the newest frame from shared memory into e.ring when
// ShmHeader.WriteSeq has advanced. Must be called with e.mu held.
func (e *Engine) pullFrameLocked() bool {
	hdr := e.shmHdr
	if hdr == nil || len(e.shmMem) < HeaderBytes {
		return false
	}
	seq := hdr.WriteSeq.Load()
	if seq == 0 || seq == e.lastSeq {
		return false
	}
	slot := int(hdr.LatestSlot.Load())
	if slot < 0 || slot >= NumSlots {
		return false
	}
	hdr.BusySlot.Store(int32(slot))
	// Re-read LatestSlot after publishing BusySlot so the producer skips it.
	if slot2 := int(hdr.LatestSlot.Load()); slot2 >= 0 && slot2 < NumSlots && slot2 != slot {
		slot = slot2
		hdr.BusySlot.Store(int32(slot))
	}
	defer hdr.BusySlot.Store(-1)

	w := int(hdr.Width.Load())
	h := int(hdr.Height.Load())
	fmtKind := hdr.Format
	matrix := scene.YUVMatrix(hdr.Matrix)
	needed := SlotNeededBytes(w, h, fmtKind)
	if needed <= 0 || needed > e.slotBytes {
		return false
	}
	slotOff := HeaderBytes + slot*e.slotBytes
	if slotOff+needed > len(e.shmMem) {
		return false
	}
	src := e.shmMem[slotOff : slotOff+needed]

	dstIdx := 0
	for dstIdx == e.shown || dstIdx == e.latest {
		dstIdx++
	}
	f := &e.ring[dstIdx]
	f.w, f.h = w, h
	s := &f.surface

	if fmtKind == FormatBGRA {
		s.Format = scene.SurfaceBGRA
		s.Matrix = 0
		if cap(s.Planes[0].Pix) < needed {
			s.Planes[0].Pix = make([]byte, needed)
		}
		dst := s.Planes[0].Pix[:needed]
		copy(dst, src)
		s.Planes[0] = scene.Plane{Pix: dst, Stride: 4 * w}
		s.Planes[1] = scene.Plane{}
	} else {
		s.Format = scene.SurfaceNV12
		s.Matrix = matrix
		yLen := w * h
		cw, ch := (w+1)/2, (h+1)/2
		uvLen := 2 * cw * ch
		if cap(s.Planes[0].Pix) < yLen {
			s.Planes[0].Pix = make([]byte, yLen)
		}
		if cap(s.Planes[1].Pix) < uvLen {
			s.Planes[1].Pix = make([]byte, uvLen)
		}
		yDst := s.Planes[0].Pix[:yLen]
		uvDst := s.Planes[1].Pix[:uvLen]
		copy(yDst, src[:yLen])
		copy(uvDst, src[yLen:yLen+uvLen])
		s.Planes[0] = scene.Plane{Pix: yDst, Stride: w}
		s.Planes[1] = scene.Plane{Pix: uvDst, Stride: 2 * cw}
	}

	e.lastSeq = hdr.WriteSeq.Load()
	e.latest = dstIdx
	e.changed = true
	e.w, e.h = w, h
	if d := hdr.DurMs.Load(); d > 0 {
		e.durMs = d
	}
	if p := hdr.PosMs.Load(); p >= 0 && !e.seekPend {
		e.posMs = p
	}
	if e.state == media.Loading {
		if e.buffering {
			e.state = media.Buffering
		} else {
			e.state = media.Ready
		}
	}
	return true
}

func (e *Engine) SetWake(wake func()) {
	e.mu.Lock()
	if !e.closed {
		e.wake = wake
	}
	e.mu.Unlock()
}

func (e *Engine) Frame(time.Time) (media.Picture, bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		_ = e.pullFrameLocked()
	}
	if e.latest < 0 {
		return media.Picture{}, false, false
	}
	changed := e.changed
	e.shown, e.changed = e.latest, false
	f := &e.ring[e.shown]
	s := &f.surface
	w, h := f.w, f.h
	return media.Picture{
		Surface: s,
		W:       w,
		H:       h,
		RGBA:    func(dst []byte) []byte { return s.RGBA(dst, w, h) },
	}, changed, true
}

func (e *Engine) Interval() time.Duration { return 0 }

func (e *Engine) State() (media.State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed && e.state == media.Loading {
		_ = e.pullFrameLocked()
	}
	return e.state, e.err
}

func (e *Engine) Size() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w, e.h
}

func (e *Engine) Duration() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shmHdr != nil {
		if d := e.shmHdr.DurMs.Load(); d > 0 {
			e.durMs = d
		}
	}
	return time.Duration(max(e.durMs, 0)) * time.Millisecond
}

func (e *Engine) Position() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.seekPend {
		return time.Duration(max(e.seekMs, 0)) * time.Millisecond
	}
	if !e.closed && e.shmHdr != nil && e.state != media.Ended {
		if p := e.shmHdr.PosMs.Load(); p >= 0 {
			e.posMs = p
		}
	}
	return time.Duration(max(e.posMs, 0)) * time.Millisecond
}

func (e *Engine) Play() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.wantPlay = true
	restartFromZero := false
	if e.state == media.Ended {
		e.state = media.Ready
		e.posMs = 0
		e.seekMs = 0
		e.seekPend = true
		restartFromZero = true
	}
	e.mu.Unlock()

	if restartFromZero {
		_ = e.bridge.VideoCmd(e.id, CmdSeek, 0, 0)
	}
	_ = e.bridge.VideoCmd(e.id, CmdPlay, 0, 0)
}

func (e *Engine) Pause() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.wantPlay = false
	e.mu.Unlock()
	_ = e.bridge.VideoCmd(e.id, CmdPause, 0, 0)
}

func (e *Engine) Seek(d time.Duration) {
	ms := max(d.Milliseconds(), 0)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if e.state == media.Ended {
		e.state = media.Ready
	}
	e.posMs = ms
	e.seekMs = ms
	e.seekPend = true
	e.mu.Unlock()
	_ = e.bridge.VideoCmd(e.id, CmdSeek, ms, 0)
}

func (e *Engine) SetLoop(loop bool) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.loop = loop
	e.mu.Unlock()
	var arg int64
	if loop {
		arg = 1
	}
	_ = e.bridge.VideoCmd(e.id, CmdSetLoop, arg, 0)
}

func (e *Engine) SetRate(r float64) {
	if r <= 0 {
		return
	}
	e.mu.Lock()
	if e.closed || e.rate == r {
		e.mu.Unlock()
		return
	}
	e.rate = r
	e.mu.Unlock()
	_ = e.bridge.VideoCmd(e.id, CmdSetRate, 0, r)
}

func (e *Engine) SetVolume(v float64) {
	v = min(max(v, 0), 1)
	e.mu.Lock()
	if e.closed || e.volume == v {
		e.mu.Unlock()
		return
	}
	e.volume = v
	e.mu.Unlock()
	_ = e.bridge.VideoCmd(e.id, CmdSetVolume, 0, v)
}

func (e *Engine) SetMuted(m bool) {
	e.mu.Lock()
	if e.closed || e.muted == m {
		e.mu.Unlock()
		return
	}
	e.muted = m
	e.mu.Unlock()
	var arg int64
	if m {
		arg = 1
	}
	_ = e.bridge.VideoCmd(e.id, CmdSetMuted, arg, 0)
}

func (e *Engine) Close() error {
	playersMu.Lock()
	delete(players, e.id)
	playersMu.Unlock()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.wake = nil
	oldMem := e.shmMem
	e.shmMem = nil
	e.shmHdr = nil
	e.mu.Unlock()

	_ = e.bridge.VideoCmd(e.id, CmdClose, 0, 0)
	if oldMem != nil {
		_ = syscall.Munmap(oldMem)
	}
	return nil
}
