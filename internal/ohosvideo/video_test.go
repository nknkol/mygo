//go:build linux && (amd64 || arm64)

package ohosvideo

import (
	"bytes"
	"errors"
	"image/color"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/scene"
	"github.com/egoist/mygo/internal/wait"
	"github.com/egoist/mygo/ui"
)

type recordedCmd struct {
	op   byte
	arg  int64
	farg float64
}

type fakeHostBridge struct {
	mu       sync.Mutex
	lastID   uint32
	lastURL  string
	lastSize int64
	lastData []byte
	cmds     []recordedCmd
}

func (f *fakeHostBridge) VideoOpen(playerID uint32, rawURL string, fd int, size int64) error {
	var data []byte
	if fd >= 0 {
		dupFD, err := syscall.Dup(fd)
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(dupFD), "video-src")
		_, _ = file.Seek(0, io.SeekStart)
		b, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return err
		}
		data = b
	}
	f.mu.Lock()
	f.lastID = playerID
	f.lastURL = rawURL
	f.lastSize = size
	f.lastData = data
	f.mu.Unlock()
	return nil
}

func (f *fakeHostBridge) VideoCmd(playerID uint32, op byte, arg int64, farg float64) error {
	f.mu.Lock()
	if playerID == f.lastID {
		f.cmds = append(f.cmds, recordedCmd{op: op, arg: arg, farg: farg})
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeHostBridge) takeCmds() []recordedCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]recordedCmd(nil), f.cmds...)
	f.cmds = nil
	return out
}

type hostShmWriter struct {
	playerID  uint32
	slotBytes int
	mem       []byte
	hdr       *ShmHeader
	seq       uint32
}

func newHostShm(t *testing.T, playerID uint32, w, h int, format uint8) *hostShmWriter {
	t.Helper()
	slotBytes := SlotNeededBytes(w, h, format)
	total := HeaderBytes + slotBytes*NumSlots
	f, err := os.CreateTemp(t.TempDir(), "video-shm-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(int64(total)); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, total, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(mem) })

	hdr := (*ShmHeader)(unsafe.Pointer(&mem[0]))
	hdr.BusySlot.Store(-1)
	hdr.SlotBytes = uint32(slotBytes)

	dupFD, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("Dup: %v", err)
	}
	OnVideoShm(playerID, uint32(slotBytes), uint32(w), uint32(h), dupFD)
	return &hostShmWriter{
		playerID:  playerID,
		slotBytes: slotBytes,
		mem:       mem,
		hdr:       hdr,
	}
}

func (w *hostShmWriter) publishNV12Solid(width, height int, yVal, cbVal, crVal byte, matrix scene.YUVMatrix, posMs, durMs int64) {
	slot := int(w.seq % NumSlots)
	if int(w.hdr.BusySlot.Load()) == slot {
		slot = (slot + 1) % NumSlots
	}
	off := HeaderBytes + slot*w.slotBytes
	yLen := width * height
	cw, ch := (width+1)/2, (height+1)/2
	uvLen := 2 * cw * ch
	yPlane := w.mem[off : off+yLen]
	for i := range yPlane {
		yPlane[i] = yVal
	}
	uvPlane := w.mem[off+yLen : off+yLen+uvLen]
	for i := 0; i < len(uvPlane); i += 2 {
		uvPlane[i] = cbVal
		uvPlane[i+1] = crVal
	}
	w.hdr.Format = FormatNV12
	w.hdr.Matrix = uint8(matrix)
	w.hdr.Width.Store(uint32(width))
	w.hdr.Height.Store(uint32(height))
	w.hdr.PosMs.Store(posMs)
	if durMs > 0 {
		w.hdr.DurMs.Store(durMs)
	}
	w.seq++
	w.hdr.LatestSlot.Store(uint32(slot))
	w.hdr.WriteSeq.Store(w.seq)
	OnVideoEvent(w.playerID, EvFrame, 0, posMs, durMs, uint32(width), uint32(height), "")
}

func (w *hostShmWriter) publishBGRASolid(width, height int, c color.RGBA, posMs, durMs int64) {
	slot := int(w.seq % NumSlots)
	if int(w.hdr.BusySlot.Load()) == slot {
		slot = (slot + 1) % NumSlots
	}
	off := HeaderBytes + slot*w.slotBytes
	dst := w.mem[off : off+4*width*height]
	for i := 0; i < len(dst); i += 4 {
		dst[i+0] = c.B
		dst[i+1] = c.G
		dst[i+2] = c.R
		dst[i+3] = c.A
	}
	w.hdr.Format = FormatBGRA
	w.hdr.Matrix = 0
	w.hdr.Width.Store(uint32(width))
	w.hdr.Height.Store(uint32(height))
	w.hdr.PosMs.Store(posMs)
	if durMs > 0 {
		w.hdr.DurMs.Store(durMs)
	}
	w.seq++
	w.hdr.LatestSlot.Store(uint32(slot))
	w.hdr.WriteSeq.Store(w.seq)
}

func nearRGBA(c, want color.RGBA) bool {
	diff := func(a, b uint8) uint8 { return max(a, b) - min(a, b) }
	return diff(c.R, want.R) <= 20 && diff(c.G, want.G) <= 20 && diff(c.B, want.B) <= 20 && c.A == 255
}

// OpenURL and OpenData return ErrNoBridge when no HarmonyOS host bridge is registered.
func TestOpenWithoutBridge(t *testing.T) {
	SetBridge(nil)
	if _, err := OpenURL("https://example.com/test.mp4"); !errors.Is(err, ErrNoBridge) {
		t.Fatalf("OpenURL err = %v, want ErrNoBridge", err)
	}
	if _, err := OpenData([]byte{1, 2, 3}); !errors.Is(err, ErrNoBridge) {
		t.Fatalf("OpenData err = %v, want ErrNoBridge", err)
	}
}

// OpenURL decodes percent-encoded file URLs into open file descriptors while forwarding remote URLs directly.
func TestOpenURLFileAndRemoteSources(t *testing.T) {
	bridge := &fakeHostBridge{}
	SetBridge(bridge)
	t.Cleanup(func() { ClearBridge(bridge) })

	dir := t.TempDir()
	localPath := filepath.Join(dir, "a video clip.mp4")
	payload := []byte("fake-mp4-bytes-for-fd-check")
	if err := os.WriteFile(localPath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fileURL := (&url.URL{Scheme: "file", Path: localPath}).String()
	eng, err := OpenURL(fileURL)
	if err != nil {
		t.Fatalf("OpenURL(file): %v", err)
	}
	_ = eng.Close()
	if bridge.lastURL != "" || bridge.lastSize != int64(len(payload)) || !bytes.Equal(bridge.lastData, payload) {
		t.Fatalf("file OpenURL got url=%q size=%d data=%q", bridge.lastURL, bridge.lastSize, bridge.lastData)
	}

	remoteEng, err := OpenURL("https://cdn.example.com/live.mp4")
	if err != nil {
		t.Fatalf("OpenURL(remote): %v", err)
	}
	_ = remoteEng.Close()
	if bridge.lastURL != "https://cdn.example.com/live.mp4" || bridge.lastSize != 0 || bridge.lastData != nil {
		t.Fatalf("remote OpenURL got url=%q size=%d data=%v", bridge.lastURL, bridge.lastSize, bridge.lastData)
	}
}

// Shared-memory NV12 and BGRA frames render through ui.Video while seek, buffering, loop, and replay-after-end drive host commands.
func TestVideoPlaybackSeekLoopAndFormatSwitch(t *testing.T) {
	bridge := &fakeHostBridge{}
	SetBridge(bridge)
	t.Cleanup(func() { ClearBridge(bridge) })

	srcBytes := []byte("in-memory-mp4-stream")
	eng, err := OpenData(srcBytes)
	if err != nil {
		t.Fatalf("OpenData: %v", err)
	}
	if !bytes.Equal(bridge.lastData, srcBytes) {
		t.Fatalf("OpenData fd bytes = %q, want %q", bridge.lastData, srcBytes)
	}

	p := ui.NewEngineVideoPlayer(eng)
	t.Cleanup(p.Close)
	tt := ui.NewTester(func(c *ui.Context) { ui.Video(c, p).Size(80, 60) }, 80, 60)
	center := func() color.RGBA { return tt.Image().RGBAAt(40, 30) }

	playerID := bridge.lastID
	shmNV12 := newHostShm(t, playerID, 64, 48, FormatNV12)
	// Full-range BT.601 pure red: Y=76, Cb=85, Cr=255.
	shmNV12.publishNV12Solid(64, 48, 76, 85, 255, scene.BT601+scene.FullRange, 0, 1200)

	wait.Until(t, "first NV12 red frame", func() bool {
		tt.Frame()
		return p.Ready() && nearRGBA(center(), color.RGBA{R: 255, G: 0, B: 0, A: 255})
	})
	if w, h := p.Size(); w != 64 || h != 48 {
		t.Fatalf("Size() = %dx%d, want 64x48", w, h)
	}
	if d := p.Duration(); d != 1200*time.Millisecond {
		t.Fatalf("Duration() = %v, want 1.2s", d)
	}

	// Buffering toggles on and off without losing Ready state.
	OnVideoEvent(playerID, EvBuffering, 1, 100, 1200, 64, 48, "")
	tt.Frame()
	if !p.Buffering() {
		t.Fatal("expected p.Buffering() == true after EvBuffering=1")
	}
	OnVideoEvent(playerID, EvBuffering, 0, 100, 1200, 64, 48, "")
	tt.Frame()
	if p.Buffering() || !p.Ready() {
		t.Fatalf("after EvBuffering=0: Buffering=%v Ready=%v", p.Buffering(), p.Ready())
	}

	// Pause and seek while replacing the shm segment with an 80x60 BGRA green frame.
	p.Pause()
	bridge.takeCmds()
	p.Seek(500 * time.Millisecond)
	shmBGRA := newHostShm(t, playerID, 80, 60, FormatBGRA)
	shmBGRA.publishBGRASolid(80, 60, color.RGBA{R: 0, G: 255, B: 0, A: 255}, 500, 1200)
	OnVideoEvent(playerID, EvSeekDone, 0, 500, 1200, 80, 60, "")

	wait.Until(t, "paused seek BGRA green frame", func() bool {
		tt.Frame()
		return nearRGBA(center(), color.RGBA{R: 0, G: 255, B: 0, A: 255})
	})
	if p.Position() != 500*time.Millisecond {
		t.Fatalf("Position() = %v, want 500ms", p.Position())
	}
	if w, h := p.Size(); w != 80 || h != 60 {
		t.Fatalf("Size() after shm replace = %dx%d, want 80x60", w, h)
	}
	if p.Playing() {
		t.Fatal("Seek unexpectedly started playback while paused")
	}

	// Enabling loop while playing rewinds to 0 and keeps playing on EvEnded.
	p.SetLoop(true)
	p.Play()
	bridge.takeCmds()
	OnVideoEvent(playerID, EvEnded, 0, 1200, 1200, 80, 60, "")
	tt.Frame()
	if p.Ended() {
		t.Fatal("looping player reported Ended() == true")
	}
	cmds := bridge.takeCmds()
	if len(cmds) < 2 || cmds[0].op != CmdSeek || cmds[0].arg != 0 || cmds[1].op != CmdPlay {
		t.Fatalf("loop EvEnded commands = %+v, want [CmdSeek(0), CmdPlay]", cmds)
	}

	// Disabling loop transitions to Ended on EvEnded, and Play() seeks back to 0.
	p.SetLoop(false)
	bridge.takeCmds()
	OnVideoEvent(playerID, EvEnded, 0, 1200, 1200, 80, 60, "")
	tt.Frame()
	if !p.Ended() || p.Playing() {
		t.Fatalf("non-loop EvEnded: Ended=%v Playing=%v", p.Ended(), p.Playing())
	}
	p.Play()
	cmds = bridge.takeCmds()
	if len(cmds) < 2 || cmds[0].op != CmdSeek || cmds[0].arg != 0 || cmds[1].op != CmdPlay {
		t.Fatalf("Play after Ended commands = %+v, want [CmdSeek(0), CmdPlay]", cmds)
	}
}

// Host EvError transitions the player to Failed and exposes the error message on Err().
func TestVideoErrorReportsFailure(t *testing.T) {
	bridge := &fakeHostBridge{}
	SetBridge(bridge)
	t.Cleanup(func() { ClearBridge(bridge) })

	eng, err := OpenURL("https://example.com/broken.mp4")
	if err != nil {
		t.Fatalf("OpenURL: %v", err)
	}
	p := ui.NewEngineVideoPlayer(eng)
	t.Cleanup(p.Close)
	tt := ui.NewTester(func(c *ui.Context) { ui.Video(c, p).Size(64, 48) }, 64, 48)

	OnVideoEvent(bridge.lastID, EvError, 0, 0, 0, 0, 0, "unsupported codec")
	tt.Frame()
	if p.Err() == nil || p.Err().Error() != "video: unsupported codec" {
		t.Fatalf("p.Err() = %v, want 'video: unsupported codec'", p.Err())
	}
}
