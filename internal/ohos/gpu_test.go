//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"image"
	"os"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

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

// Zero-copy GPU swapchain imports cross-process OH_NativeBuffers, Y-flip blits full and partial damage without CPU copies, waits on acquire fences, and recovers across Idle.
func TestGPUZeroCopySwapchainBlitsAndExportsReleaseFence(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !gpuAvailable() {
		t.Skip("HarmonyOS EGL/OH_NativeBuffer GPU unavailable")
	}
	drv, err := getGPUDriver()
	if err != nil || !drv.hasParcel {
		t.Skip("HarmonyOS OHIPCParcel NativeBuffer serialization unavailable")
	}

	b, s, hostConn, h := newTestSurface(t)
	defer s.ReleaseGL()

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
		w  = 32
		ht = 32
	)
	type hostSwapBuf struct {
		id     uint32
		nb     uintptr
		stride int
		fd     int
		pix    []byte
	}
	var ring [2]hostSwapBuf
	for i := range ring {
		cfg := ohNativeBufferConfig{
			width:  w,
			height: ht,
			format: nbFmtRGBA8888,
			usage:  nbUsageCPURead | nbUsageMemDMA | nbUsageHWRender | nbUsageHWTexture,
		}
		nb := drv.nbAlloc(&cfg)
		if nb == 0 {
			t.Fatalf("OH_NativeBuffer_Alloc(%d) failed", i)
		}
		nbCap := nb
		t.Cleanup(func() { drv.nbUnref(nbCap) })

		var vir uintptr
		if rc := drv.nbMap(nb, &vir); rc != 0 || vir == 0 {
			t.Fatalf("OH_NativeBuffer_Map(%d) rc=%d", i, rc)
		}
		t.Cleanup(func() { drv.nbUnmap(nbCap) })

		nwb := drv.nwFromNB(nb)
		if nwb == 0 {
			t.Fatalf("nwFromNB(%d) failed", i)
		}
		bhp := drv.nwGetBH(nwb)
		bh := (*ohBufferHandle)(*(*unsafe.Pointer)(unsafe.Pointer(&bhp)))
		stride := int(bh.stride)
		dmaFD, err := syscall.Dup(int(bh.fd))
		if err != nil {
			drv.nwFreeNB(nwb)
			t.Fatalf("Dup(bh.fd): %v", err)
		}
		t.Cleanup(func() { _ = syscall.Close(dmaFD) })
		pix := unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&vir))), int(bh.size))
		drv.nwFreeNB(nwb)

		bufID := uint32(500 + i)
		isFD, ints, fds, err := drv.serializeNativeBuffer(nb)
		if err != nil {
			t.Fatalf("serializeNativeBuffer(%d): %v", i, err)
		}
		if err := hostConn.writeWithFDs(kindGPUBuffer, encodeGPUBufferMessage(1, bufID, w, ht, isFD, ints), fds); err != nil {
			closeFDs(fds)
			t.Fatalf("writeWithFDs: %v", err)
		}
		closeFDs(fds)

		gotKind, gotPayload, gotFDs, err := b.conn.readMulti()
		if err != nil || gotKind != kindGPUBuffer {
			closeFDs(gotFDs)
			t.Fatalf("readMulti: kind=%d err=%v, want kindGPUBuffer", gotKind, err)
		}
		b.onGPUBuffer(gotPayload, gotFDs)
		ring[i] = hostSwapBuf{id: bufID, nb: nb, stride: stride, fd: dmaFD, pix: pix}
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

	makeScene := func(topRightGreen bool, bottomLeftAmber bool) *scene.Scene {
		sc := &scene.Scene{
			Width:  w,
			Height: ht,
			Clear:  scene.Color{R: 20, G: 30, B: 40, A: 255},
			Ops: []scene.Op{
				{Kind: scene.OpFill, Rect: scene.Rect{X: 0, Y: 0, W: 16, H: 16}, Color: scene.Color{R: 240, G: 50, B: 70, A: 255}},
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
		if bottomLeftAmber {
			sc.Ops = append(sc.Ops, scene.Op{
				Kind:  scene.OpFill,
				Rect:  scene.Rect{X: 4, Y: 20, W: 8, H: 8},
				Color: scene.Color{R: 230, G: 170, B: 30, A: 255},
			})
		}
		return sc
	}

	stepZeroCopy := func(seq uint32, hb hostSwapBuf, acquireFD int) (kind byte, payload []byte, releaseFD int) {
		t.Helper()
		b.onFrame(makeFramePayload(1, seq, hb.id, w, ht, uint32(hb.stride), 0), acquireFD)
		k, p, rfd, err := hostConn.read()
		if err != nil {
			t.Fatalf("hostConn.read: %v", err)
		}
		return k, p, rfd
	}

	pixelAt := func(hb hostSwapBuf, x, y int) [4]byte {
		dmaBufSync(hb.fd, dmaBufSyncStart|dmaBufSyncRead)
		off := y*hb.stride + x*4
		px := [4]byte{hb.pix[off], hb.pix[off+1], hb.pix[off+2], hb.pix[off+3]}
		dmaBufSync(hb.fd, dmaBufSyncEnd|dmaBufSyncRead)
		return px
	}

	// 1. Whole frame allocates pure-VRAM FBO and hardware-blits Y-flipped into ring[0].
	curScene = makeScene(false, false)
	kind, payload, relFD := stepZeroCopy(1, ring[0], -1)
	pollAndCloseFD(relFD)
	if kind != kindPresent || !reallocatedSeen {
		t.Fatalf("frame 1: kind=%d reallocated=%v, want kindPresent & true", kind, reallocatedSeen)
	}
	if s.gl == nil || !s.gl.pureVRAM {
		t.Fatalf("expected pure-VRAM scene FBO in zero-copy mode, got %+v", s.gl)
	}
	if _, _, _, perf, ok := decodePresentPerf(payload); !ok || perf.copyUs != 0 || perf.meta&presentPerfZeroCopy == 0 {
		t.Fatalf("expected zero-copy perf trailer, got ok=%v perf=%+v", ok, perf)
	}
	if got := pixelAt(ring[0], 4, 4); got != [4]byte{240, 50, 70, 255} {
		t.Fatalf("ring[0] top-left (4,4) = %v, want coral [240 50 70 255]", got)
	}
	if got := pixelAt(ring[0], 24, 24); got != [4]byte{30, 100, 230, 255} {
		t.Fatalf("ring[0] bottom-right (24,24) = %v, want blue [30 100 230 255]", got)
	}

	// 2. Unchanged frame with an acquireFD closes it cleanly and aborts.
	var pfd [2]int
	if err := syscall.Pipe(pfd[:]); err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	_ = syscall.Close(pfd[1])
	kind, payload, relFD = stepZeroCopy(2, ring[1], pfd[0])
	pollAndCloseFD(relFD)
	if kind != kindAbort {
		t.Fatalf("unchanged frame 2: got kind=%d, want kindAbort", kind)
	}

	// 3. Warm ring[1] then apply two consecutive small updates so ring[1] blits partial damage (testing scissor disable + acquireFence handoff).
	curScene = makeScene(true, false)
	kind, _, relFD = stepZeroCopy(3, ring[1], -1)
	if kind != kindPresent {
		pollAndCloseFD(relFD)
		t.Fatalf("frame 3: got kind=%d, want kindPresent", kind)
	}
	// Pass relFD from frame 3 back as acquireFD for frame 4!
	curScene = makeScene(true, true)
	kind, payload, relFD2 := stepZeroCopy(4, ring[1], relFD)
	pollAndCloseFD(relFD2)
	if kind != kindPresent {
		t.Fatalf("frame 4: got kind=%d, want kindPresent", kind)
	}
	if nRects := binary.LittleEndian.Uint32(payload[8:]); nRects == 0 {
		t.Fatalf("frame 4 expected partial damage blit, got whole-frame nRects=0")
	}
	if got := pixelAt(ring[1], 6, 22); got != [4]byte{230, 170, 30, 255} {
		t.Fatalf("ring[1] partial blit bottom-left (6,22) = %v, want amber [230 170 30 255]", got)
	}
	if got := pixelAt(ring[1], 22, 6); got != [4]byte{40, 210, 90, 255} {
		t.Fatalf("ring[1] preserved top-right (22,6) = %v, want green [40 210 90 255]", got)
	}

	// 4. Idle drops GL wrappers while keeping imported NativeBuffers warm for immediate recovery without resending kindGPUBuffer.
	s.Idle()
	if s.gl != nil {
		t.Fatalf("expected s.gl == nil after Idle")
	}
	curScene = makeScene(false, true)
	kind, _, relFD = stepZeroCopy(5, ring[0], -1)
	pollAndCloseFD(relFD)
	if kind != kindPresent || !reallocatedSeen {
		t.Fatalf("frame 5 after Idle: kind=%d reallocated=%v, want kindPresent & true", kind, reallocatedSeen)
	}
	if got := pixelAt(ring[0], 6, 22); got != [4]byte{230, 170, 30, 255} {
		t.Fatalf("ring[0] after Idle (6,22) = %v, want amber [230 170 30 255]", got)
	}
	if got := pixelAt(ring[0], 22, 6); got != [4]byte{20, 30, 40, 255} {
		t.Fatalf("ring[0] after Idle (22,6) = %v, want clear [20 30 40 255]", got)
	}
}
