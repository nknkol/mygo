//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"image"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/egoist/mygo/internal/gpu/gl"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/scene"
)

// Software presentation swizzles premultiplied BGRA pixels into OHNativeWindow's RGBA_8888 order across full-row and odd-width sub-rectangle updates.
func TestSoftwarePresentSwizzlesBGRAtoRGBA(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)

	const (
		w      = 7 // odd width exercises both 8-byte word and 4-byte tail swizzle
		ht     = 4
		stride = w * 4
		size   = stride * ht
	)
	f, err := os.CreateTemp(t.TempDir(), "swizzle-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap: %v", err)
	}
	defer syscall.Munmap(mem)

	pix := make([]byte, size)
	for i := 0; i < size; i += 4 {
		// Premultiplied BGRA coral: B=40, G=60, R=220, A=255.
		pix[i+0], pix[i+1], pix[i+2], pix[i+3] = 40, 60, 220, 255
	}

	var dmg []image.Rectangle
	whole := true
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			if whole {
				s.PresentPixels(pix, stride, w, ht)
			} else {
				s.PresentDamage(pix, stride, w, ht, dmg)
			}
		}
		return false
	}

	sendFrame := func(seq uint32) {
		t.Helper()
		dupFD, err := syscall.Dup(int(f.Fd()))
		if err != nil {
			t.Fatalf("Dup: %v", err)
		}
		b.onFrame(makeFramePayload(1, seq, 77, w, ht, stride, size), dupFD)
		kind, _, _, err := hostConn.read()
		if err != nil || kind != kindPresent {
			t.Fatalf("seq %d: got kind=%d err=%v, want kindPresent", seq, kind, err)
		}
	}

	sendFrame(1)
	for i := 0; i < size; i += 4 {
		got := [4]byte{mem[i], mem[i+1], mem[i+2], mem[i+3]}
		want := [4]byte{220, 60, 40, 255}
		if got != want {
			t.Fatalf("whole pixel %d = %v, want RGBA %v", i/4, got, want)
		}
	}

	// Update a 3x2 sub-rectangle at (2, 1) with premultiplied BGRA sky-blue (B=235, G=99, R=37, A=200).
	whole = false
	sub := image.Rect(2, 1, 5, 3)
	dmg = []image.Rectangle{sub}
	for y := sub.Min.Y; y < sub.Max.Y; y++ {
		for x := sub.Min.X; x < sub.Max.X; x++ {
			off := y*stride + x*4
			pix[off+0], pix[off+1], pix[off+2], pix[off+3] = 235, 99, 37, 200
		}
	}
	sendFrame(2)

	for y := 0; y < ht; y++ {
		for x := 0; x < w; x++ {
			off := y*stride + x*4
			got := [4]byte{mem[off], mem[off+1], mem[off+2], mem[off+3]}
			want := [4]byte{220, 60, 40, 255}
			if image.Pt(x, y).In(sub) {
				want = [4]byte{37, 99, 235, 200}
			}
			if got != want {
				t.Fatalf("after damage pixel (%d,%d) = %v, want RGBA %v", x, y, got, want)
			}
		}
	}
}

// Hardware OpenGL ES rendering into OH_NativeBuffer flips rows top-down into RGBA_8888 swapchain buffers, aborts unchanged frames, and invalidates cleanly across Idle.
func TestGPURenderTargetFlipsRowsAndTracksPartialDamage(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !gpuAvailable() {
		t.Skip("HarmonyOS EGL/OH_NativeBuffer GPU unavailable")
	}

	b, s, hostConn, h := newTestSurface(t)
	defer s.ReleaseGL()

	n := s.Native()
	if n.OHOSSurface != s {
		t.Fatalf("Surface.Native().OHOSSurface = %v, want %v", n.OHOSSurface, s)
	}
	if _, err := s.BeginGL(0, 0); err != nil {
		t.Fatalf("BeginGL(0,0): %v", err)
	}
	r, err := gl.New()
	if err != nil {
		t.Fatalf("gl.New: %v", err)
	}
	defer func() {
		_, _ = s.BeginGL(0, 0)
		r.Release()
	}()
	r.Partial = true

	const (
		w      = 32
		ht     = 32
		stride = w * 4
		size   = stride * ht
	)
	type ringBuf struct {
		id  uint32
		f   *os.File
		mem []byte
	}
	var ring [2]ringBuf
	for i := range ring {
		f, err := os.CreateTemp(t.TempDir(), "glring-*")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		if err := f.Truncate(size); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		t.Cleanup(func() { syscall.Munmap(mem) })
		ring[i] = ringBuf{id: uint32(200 + i), f: f, mem: mem}
	}

	var curScene *scene.Scene
	var reallocatedSeen bool
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind != platform.SurfaceFrame {
			return false
		}
		realloc, err := s.BeginGL(curScene.Width, curScene.Height)
		if err != nil {
			t.Errorf("BeginGL: %v", err)
			return false
		}
		reallocatedSeen = realloc
		if realloc {
			r.Invalidate()
		}
		if err := r.Render(curScene); err != nil {
			t.Errorf("Render: %v", err)
			return false
		}
		whole, unchanged, damage := r.LastRedraw()
		if err := s.EndGL(curScene.Width, curScene.Height, damage, whole, unchanged); err != nil {
			t.Errorf("EndGL: %v", err)
		}
		return false
	}

	makeScene := func(topRightGreen bool) *scene.Scene {
		sc := &scene.Scene{
			Width:  w,
			Height: ht,
			Clear:  scene.Color{R: 20, G: 30, B: 40, A: 255},
			Ops: []scene.Op{
				// Top-left coral red (0..16, 0..16): verifies RGBA order and top-down Y flip.
				{Kind: scene.OpFill, Rect: scene.Rect{X: 0, Y: 0, W: 16, H: 16}, Color: scene.Color{R: 240, G: 50, B: 70, A: 255}},
				// Bottom-right blue (16..32, 16..32).
				{Kind: scene.OpFill, Rect: scene.Rect{X: 16, Y: 16, W: 16, H: 16}, Color: scene.Color{R: 30, G: 100, B: 230, A: 255}},
			},
		}
		if topRightGreen {
			sc.Ops = append(sc.Ops, scene.Op{
				Kind:  scene.OpFill,
				Rect:  scene.Rect{X: 20, Y: 4, W: 8, H: 8},
				Color: scene.Color{R: 40, G: 210, B: 90, A: 255},
			})
		}
		return sc
	}

	stepFrame := func(seq uint32, rb ringBuf) (kind byte, payload []byte) {
		t.Helper()
		dupFD, err := syscall.Dup(int(rb.f.Fd()))
		if err != nil {
			t.Fatalf("Dup: %v", err)
		}
		b.onFrame(makeFramePayload(1, seq, rb.id, w, ht, stride, size), dupFD)
		k, p, _, err := hostConn.read()
		if err != nil {
			t.Fatalf("hostConn.read: %v", err)
		}
		return k, p
	}

	pixelAt := func(mem []byte, x, y int) [4]byte {
		off := y*stride + x*4
		return [4]byte{mem[off], mem[off+1], mem[off+2], mem[off+3]}
	}

	// 1. Initial frame allocates OH_NativeBuffer FBO and presents top-down RGBA.
	curScene = makeScene(false)
	kind, _ := stepFrame(1, ring[0])
	if kind != kindPresent || !reallocatedSeen {
		t.Fatalf("frame 1: kind=%d reallocated=%v, want kindPresent & true", kind, reallocatedSeen)
	}
	if got := pixelAt(ring[0].mem, 4, 4); got != [4]byte{240, 50, 70, 255} {
		t.Fatalf("top-left (4,4) = %v, want coral RGBA [240 50 70 255]", got)
	}
	if got := pixelAt(ring[0].mem, 24, 24); got != [4]byte{30, 100, 230, 255} {
		t.Fatalf("bottom-right (24,24) = %v, want blue RGBA [30 100 230 255]", got)
	}
	if got := pixelAt(ring[0].mem, 4, 24); got != [4]byte{20, 30, 40, 255} {
		t.Fatalf("bottom-left (4,24) = %v, want clear RGBA [20 30 40 255]", got)
	}

	// 2. Identical scene reports unchanged and returns the swapchain buffer via kindAbort.
	kind, payload := stepFrame(2, ring[1])
	if kind != kindAbort || binary.LittleEndian.Uint32(payload[4:]) != 2 {
		t.Fatalf("unchanged frame 2: got kind=%d payload=%v, want kindAbort(seq=2)", kind, payload)
	}

	// 3. Partial damage updates top-right green in ring[1] (first visit -> catches up whole) and then ring[0] (partial damage!).
	curScene = makeScene(true)
	if kind, _ = stepFrame(3, ring[1]); kind != kindPresent {
		t.Fatalf("frame 3: got kind=%d, want kindPresent", kind)
	}
	curScene = makeScene(false)
	kind, payload = stepFrame(4, ring[1])
	if kind != kindPresent {
		t.Fatalf("frame 4: got kind=%d, want kindPresent", kind)
	}
	if nRects := binary.LittleEndian.Uint32(payload[8:]); nRects == 0 {
		t.Fatalf("frame 4 on warm buffer expected partial damage rects, got whole-frame nRects=0")
	}
	if got := pixelAt(ring[1].mem, 22, 6); got != [4]byte{20, 30, 40, 255} {
		t.Fatalf("after partial clear (22,6) = %v, want clear RGBA [20 30 40 255]", got)
	}

	// 4. Idle releases the OH_NativeBuffer target, and the next frame reallocates + invalidates cleanly.
	s.Idle()
	if s.gl != nil {
		t.Fatalf("expected s.gl == nil after Idle")
	}
	curScene = makeScene(true)
	if kind, _ = stepFrame(5, ring[0]); kind != kindPresent || !reallocatedSeen {
		t.Fatalf("frame 5 after Idle: kind=%d reallocated=%v, want kindPresent & true", kind, reallocatedSeen)
	}
	if got := pixelAt(ring[0].mem, 22, 6); got != [4]byte{40, 210, 90, 255} {
		t.Fatalf("frame 5 after Idle (22,6) = %v, want green RGBA [40 210 90 255]", got)
	}
}
