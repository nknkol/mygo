//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"image"
	"math"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/platform"
)

type testWindowHandler struct {
	platform.WindowHandler
	events          []platform.SurfaceEvent
	take            func(platform.SurfaceEvent) bool
	shouldClose     func() bool
	focused         int
	blurred         int
	minimized       int
	maximized       int
	unmaximized     int
	restored        int
	enteredFS       int
	leftFS          int
	resized         int
	moved           int
	closed          int
	titleBarChanged int
	menuClicks      []int
}

func (h *testWindowHandler) SurfaceEvent(ev platform.SurfaceEvent) bool {
	h.events = append(h.events, ev)
	if h.take != nil {
		return h.take(ev)
	}
	return false
}

func (h *testWindowHandler) ShouldClose() bool {
	if h.shouldClose != nil {
		return h.shouldClose()
	}
	return true
}

func (h *testWindowHandler) Closed()                { h.closed++ }
func (h *testWindowHandler) Focused()               { h.focused++ }
func (h *testWindowHandler) Blurred()               { h.blurred++ }
func (h *testWindowHandler) Minimized()             { h.minimized++ }
func (h *testWindowHandler) Maximized()             { h.maximized++ }
func (h *testWindowHandler) Unmaximized()           { h.unmaximized++ }
func (h *testWindowHandler) Restored()              { h.restored++ }
func (h *testWindowHandler) EnteredFullScreen()     { h.enteredFS++ }
func (h *testWindowHandler) LeftFullScreen()        { h.leftFS++ }
func (h *testWindowHandler) Resized()               { h.resized++ }
func (h *testWindowHandler) Moved()                 { h.moved++ }
func (h *testWindowHandler) TitleBarChanged()       { h.titleBarChanged++ }
func (h *testWindowHandler) MenuItemClicked(id int) { h.menuClicks = append(h.menuClicks, id) }

func newTestSurface(t *testing.T) (*Backend, *Surface, *conn, *testWindowHandler) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	b := NewWithFD(fds[0])
	b.initW, b.initH, b.initScale = 1600, 1000, 2
	if err := b.Init(nil, platform.AppOptions{}); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		t.Fatalf("Init: %v", err)
	}
	hostConn := &conn{fd: fds[1]}
	t.Cleanup(func() {
		b.conn.close()
		hostConn.close()
	})
	h := &testWindowHandler{}
	w, err := b.NewWindow(&platform.WindowOptions{Surface: true}, h)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinCreate {
		t.Fatalf("initial NewWindow message = (kind=%d, err=%v), want kindWinCreate", kind, err)
	}
	if cfg, ok := decodeWinConfigMessage(payload); !ok || cfg.winID != 1 {
		t.Fatalf("initial winCreate = %+v (ok=%v), want winID=1", cfg, ok)
	}
	return b, w.(*Window).surface, hostConn, h
}

// Mouse presses count double clicks, translate side buttons to history keys, and forward pen pressure and cancel.
func TestMouseAndPenInput(t *testing.T) {
	b, _, _, h := newTestSurface(t)

	b.onInput(encodeMouseInput(1, mouseDown, 0, 0, 0, 100, 40, 50, 0))
	b.onInput(encodeMouseInput(1, mouseUp, 0, 0, 0, 140, 40, 50, 0))
	b.onInput(encodeMouseInput(1, mouseDown, 0, 0, 0, 260, 42, 51, 0))
	b.onInput(encodeMouseInput(1, mouseUp, 0, 0, 0, 300, 42, 51, 0))
	b.onInput(encodeMouseInput(1, mouseDown, 3, 0, 0, 400, 42, 51, 0))
	b.onInput(encodeMouseInput(1, mouseMove, 255, 1, platform.ModShift, 420, 80, 90, 0.75))
	b.onInput(encodeMouseInput(1, mouseCancel, 0, 0, 0, 450, 80, 90, 0))

	if len(h.events) != 7 {
		t.Fatalf("got %d events, want 7: %+v", len(h.events), h.events)
	}
	if h.events[0].Clicks != 1 || h.events[2].Clicks != 2 {
		t.Fatalf("clicks = (%d, %d), want (1, 2)", h.events[0].Clicks, h.events[2].Clicks)
	}
	if h.events[4].Kind != platform.KeyPressed || h.events[4].Key != platform.KeyBack {
		t.Fatalf("side button = %+v, want KeyPressed KeyBack", h.events[4])
	}
	if h.events[5].Pointer != platform.PointerPen || math.Abs(h.events[5].Pressure-0.75) > 1e-5 {
		t.Fatalf("pen move = %+v, want PointerPen pressure 0.75", h.events[5])
	}
	if h.events[6].Kind != platform.PointerCancel {
		t.Fatalf("cancel = %+v, want PointerCancel", h.events[6])
	}
}

// Touchscreen gestures turn taps into clicks, long presses into secondary clicks, fast drags into momentum scroll, and two-finger pinches into zoom.
func TestTouchTapLongPressFlingAndPinch(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)

	// Tap -> primary click + PointerLeave.
	b.onInput(encodeTouchInput(1, touchDown, true, 0, 100, []touchPoint{{id: 0, x: 50, y: 60, pressure: 0.5}}))
	b.onInput(encodeTouchInput(1, touchUp, false, 0, 180, []touchPoint{{id: 0, x: 52, y: 61, pressure: 0.5}}))
	if len(h.events) != 3 || h.events[0].Kind != platform.PointerDown || h.events[0].Button != 0 ||
		h.events[1].Kind != platform.PointerUp || h.events[2].Kind != platform.PointerLeave {
		t.Fatalf("tap events = %+v", h.events)
	}

	// Long press (>= 500ms within slop) -> secondary click.
	h.events = nil
	b.onInput(encodeTouchInput(1, touchDown, true, 0, 1000, []touchPoint{{id: 1, x: 100, y: 120}}))
	b.onInput(encodeTouchInput(1, touchUp, false, 0, 1550, []touchPoint{{id: 1, x: 103, y: 122}}))
	if len(h.events) != 3 || h.events[0].Button != 1 || h.events[1].Button != 1 {
		t.Fatalf("long-press events = %+v", h.events)
	}

	// Fast upward swipe -> precise scroll followed by momentum steps that decay and request frames.
	h.events = nil
	b.onInput(encodeTouchInput(1, touchDown, true, 0, 2000, []touchPoint{{id: 2, x: 200, y: 400}}))
	b.onInput(encodeTouchInput(1, touchMove, false, 0, 2016, []touchPoint{{id: 2, x: 200, y: 360}}))
	b.onInput(encodeTouchInput(1, touchMove, false, 0, 2032, []touchPoint{{id: 2, x: 200, y: 300}}))
	b.onInput(encodeTouchInput(1, touchUp, false, 0, 2048, []touchPoint{{id: 2, x: 200, y: 240}}))
	if !s.touch.fling.active {
		t.Fatalf("expected active fling after fast touch swipe")
	}
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindRequest || len(payload) != 4 || binary.LittleEndian.Uint32(payload) != 1 {
		t.Fatalf("expected kindRequest(winID=1) for fling frame, got kind=%d payload=%v err=%v", kind, payload, err)
	}
	ev1, ok1 := s.touch.stepFling(1.0 / 120.0)
	ev2, ok2 := s.touch.stepFling(1.0 / 120.0)
	if !ok1 || !ok2 || !ev1.Momentum || !ev1.Precise || ev1.DY <= 0 || ev2.DY >= ev1.DY {
		t.Fatalf("unexpected fling decay: ev1=%+v ev2=%+v", ev1, ev2)
	}

	// Two-finger spread -> PointerZoom with incremental scale > 1; touchCancel -> PointerCancel.
	h.events = nil
	b.onInput(encodeTouchInput(1, touchDown, true, 0, 3000, []touchPoint{{id: 0, x: 100, y: 200}}))
	b.onInput(encodeTouchInput(1, touchDown, false, 0, 3010, []touchPoint{{id: 1, x: 200, y: 200}}))
	b.onInput(encodeTouchInput(1, touchMove, false, 0, 3020, []touchPoint{{id: 0, x: 50, y: 200}, {id: 1, x: 250, y: 200}}))
	b.onInput(encodeTouchInput(1, touchCancel, false, 0, 3030, []touchPoint{{id: 0, x: 50, y: 200}, {id: 1, x: 250, y: 200}}))
	var sawZoom, sawCancel bool
	for _, ev := range h.events {
		if ev.Kind == platform.PointerZoom && math.Abs(ev.Scale-2.0) < 1e-4 {
			sawZoom = true
		}
		if ev.Kind == platform.PointerCancel {
			sawCancel = true
		}
	}
	if !sawZoom || !sawCancel {
		t.Fatalf("pinch/cancel events = %+v (sawZoom=%v sawCancel=%v)", h.events, sawZoom, sawCancel)
	}
}

// Touchpad scroll, coasting momentum, and cumulative pinch scale convert to incremental SurfaceEvents.
func TestScrollCoastingAndPinchZoom(t *testing.T) {
	b, _, _, h := newTestSurface(t)

	b.onInput(encodeScrollInput(1, scrollPrecise, 0, 120, 80, 0, 18.5))
	b.onInput(encodeScrollInput(1, scrollPrecise|scrollMomentum, 0, 120, 80, 0, 9.25))
	b.onInput(encodeZoomInput(1, zoomBegin, 0, 300, 200, 1.0))
	b.onInput(encodeZoomInput(1, zoomUpdate, 0, 300, 200, 1.5))
	b.onInput(encodeZoomInput(1, zoomUpdate, 0, 300, 200, 3.0))
	b.onInput(encodeZoomInput(1, zoomEnd, 0, 300, 200, 3.0))

	if len(h.events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(h.events), h.events)
	}
	if !h.events[0].Precise || h.events[0].Momentum || h.events[1].Momentum != true {
		t.Fatalf("scroll flags wrong: %+v %+v", h.events[0], h.events[1])
	}
	if math.Abs(h.events[2].Scale-1.5) > 1e-5 || math.Abs(h.events[3].Scale-2.0) > 1e-5 {
		t.Fatalf("zoom scales = (%v, %v), want (1.5, 2.0)", h.events[2].Scale, h.events[3].Scale)
	}
}

// Modifier keys keep the bit set while either side stays held, repeats mark Repeat, and unconsumed printable keys insert text when active.
func TestKeyboardModifiersRepeatAndDirectTyping(t *testing.T) {
	b, s, _, h := newTestSurface(t)
	s.SetTextInput(platform.TextInputState{Active: true, Caret: platform.RectF{X: 10, Y: 20, W: 2, H: 18}})

	// Left Shift down, Right Shift down, Left Shift up (ModShift stays set!), Right Shift up (ModShift clears).
	b.onInput(encodeKeyInput(1, keyDown, 0, arkKeyShiftLeft, 0))
	b.onInput(encodeKeyInput(1, keyDown, platform.ModShift, arkKeyShiftRight, 0))
	b.onInput(encodeKeyInput(1, keyUp, platform.ModShift, arkKeyShiftLeft, 0))
	b.onInput(encodeKeyInput(1, keyUp, 0, arkKeyShiftRight, 0))

	if h.events[4].Kind != platform.ModifiersChanged || h.events[4].Mods != platform.ModShift {
		t.Fatalf("left shift release while right shift held = %+v, want ModShift", h.events[4])
	}
	if h.events[6].Kind != platform.ModifiersChanged || h.events[6].Mods != 0 {
		t.Fatalf("right shift release = %+v, want 0", h.events[6])
	}

	// Printable key press + repeat commits text when unhandled, while Ctrl+A does not commit text.
	h.events = nil
	b.onInput(encodeKeyInput(1, keyDown, 0, arkKeyA, 'a'))
	b.onInput(encodeKeyInput(1, keyDown, 0, arkKeyA, 'a'))
	b.onInput(encodeKeyInput(1, keyUp, 0, arkKeyA, 'a'))
	b.onInput(encodeKeyInput(1, keyDown, platform.ModCtrl, arkKeyA, 'a'))
	b.onInput(encodeKeyInput(1, keyUp, platform.ModCtrl, arkKeyA, 'a'))

	var commits []string
	var repeats int
	for _, ev := range h.events {
		if ev.Kind == platform.KeyPressed && ev.Repeat {
			repeats++
		}
		if ev.Kind == platform.TextInput {
			commits = append(commits, ev.Text)
		}
	}
	if repeats != 1 || len(commits) != 2 || commits[0] != "a" || commits[1] != "a" {
		t.Fatalf("repeats=%d commits=%v, want 1 and [a a]; events=%+v", repeats, commits, h.events)
	}
}

type fakeTextClient struct {
	buf    string
	sel    platform.TextSelection
	mark   platform.TextRange
	marked bool
}

func (c *fakeTextClient) u16Len() int { return platform.UTF16Len(c.buf) }

func (c *fakeTextClient) TextForRange(r platform.TextRange) (string, platform.TextRange) {
	r = platform.NormalizeTextRange(r)
	n := c.u16Len()
	r.Start, r.End = min(r.Start, n), min(r.End, n)
	a, z := platform.UTF16ByteOffset(c.buf, r.Start), platform.UTF16ByteOffset(c.buf, r.End)
	return c.buf[a:z], r
}

func (c *fakeTextClient) Selection() platform.TextSelection       { return c.sel }
func (c *fakeTextClient) MarkedRange() (platform.TextRange, bool) { return c.mark, c.marked }
func (c *fakeTextClient) BoundsForRange(r platform.TextRange) (platform.RectF, platform.TextRange, bool) {
	return platform.RectF{X: 12, Y: 24, W: 2, H: 16}, r, true
}
func (c *fakeTextClient) IndexForPoint(x, y float64) (int, bool) { return 0, false }

func (c *fakeTextClient) ReplaceText(r *platform.TextRange, text string) {
	var target platform.TextRange
	switch {
	case r != nil:
		target = platform.NormalizeTextRange(*r)
	case c.marked:
		target = c.mark
	default:
		target = platform.NormalizeTextRange(c.sel.Range)
	}
	a, z := platform.UTF16ByteOffset(c.buf, target.Start), platform.UTF16ByteOffset(c.buf, target.End)
	c.buf = c.buf[:a] + text + c.buf[z:]
	caret := target.Start + platform.UTF16Len(text)
	c.sel = platform.TextSelection{Range: platform.TextRange{Start: caret, End: caret}}
	c.marked = false
	c.mark = platform.TextRange{}
}

func (c *fakeTextClient) SetMarkedText(r *platform.TextRange, text string, sel platform.TextRange) {
	var target platform.TextRange
	switch {
	case r != nil:
		target = platform.NormalizeTextRange(*r)
	case c.marked:
		target = c.mark
	default:
		target = platform.NormalizeTextRange(c.sel.Range)
	}
	a, z := platform.UTF16ByteOffset(c.buf, target.Start), platform.UTF16ByteOffset(c.buf, target.End)
	c.buf = c.buf[:a] + text + c.buf[z:]
	u := platform.UTF16Len(text)
	if u == 0 {
		c.marked = false
		c.mark = platform.TextRange{}
		c.sel = platform.TextSelection{Range: platform.TextRange{Start: target.Start, End: target.Start}}
		return
	}
	c.marked = true
	c.mark = platform.TextRange{Start: target.Start, End: target.Start + u}
	c.sel = platform.TextSelection{Range: platform.TextRange{
		Start: target.Start + sel.Start,
		End:   target.Start + sel.End,
	}}
}

func (c *fakeTextClient) UnmarkText() {
	c.marked = false
	c.mark = platform.TextRange{}
}

// IME preview, finish-preview, commit, and surrounding-character deletion preserve UTF-16 offsets across emoji.
func TestIMEPreviewCommitAndDeleteSurrounding(t *testing.T) {
	b, s, hostConn, _ := newTestSurface(t)
	client := &fakeTextClient{
		buf: "Hi😀",
		sel: platform.TextSelection{Range: platform.TextRange{Start: 4, End: 4}}, // after surrogate pair
	}
	s.SetTextInput(platform.TextInputState{
		Active: true,
		Caret:  platform.RectF{X: 30, Y: 40, W: 2, H: 20},
		Client: client,
	})

	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindTextInput {
		t.Fatalf("read kindTextInput: kind=%d err=%v", kind, err)
	}
	wire, ok := decodeTextInputMessage(payload)
	if !ok || wire.winID != 1 || !wire.active || wire.selStart != 4 || wire.selEnd != 4 || wire.text != "Hi😀" {
		t.Fatalf("decoded wire = %+v (ok=%v)", wire, ok)
	}

	// Duplicate SetTextInput and SetCursor do not re-send identical state.
	s.SetTextInput(platform.TextInputState{
		Active: true,
		Caret:  platform.RectF{X: 30, Y: 40, W: 2, H: 20},
		Client: client,
	})
	s.SetCursor(platform.CursorText)
	s.SetCursor(platform.CursorText)
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindCursor || len(payload) != 8 || binary.LittleEndian.Uint32(payload[0:]) != 1 || platform.Cursor(payload[4]) != platform.CursorText {
		t.Fatalf("expected single kindCursor(winID=1, CursorText), got kind=%d payload=%v err=%v", kind, payload, err)
	}

	// Preview "ni" -> FinishTextPreview -> Commit "你好" replaces "ni" cleanly regardless of ordering.
	b.onInput(encodeIMEInput(1, imePreedit, -1, -1, "ni"))
	if client.buf != "Hi😀ni" || !client.marked {
		t.Fatalf("after preedit: buf=%q marked=%v", client.buf, client.marked)
	}
	b.onInput(encodeIMEInput(1, imePreeditEnd, 0, 0, ""))
	b.onInput(encodeIMEInput(1, imeCommit, 0, 0, "你好"))
	if client.buf != "Hi😀你好" || client.marked {
		t.Fatalf("after finish+commit: buf=%q marked=%v", client.buf, client.marked)
	}

	// DeleteBackward 3 runes ("😀你好") deletes the emoji as a single rune rather than splitting its surrogate pair.
	b.onInput(encodeIMEInput(1, imeDeleteSurrounding, -3, 3, ""))
	if client.buf != "Hi" {
		t.Fatalf("after deleteSurrounding(-3, 3): buf=%q, want %q", client.buf, "Hi")
	}
}

type recordedMediaCmd struct {
	cmd platform.MediaCommand
	pos time.Duration
}

type testAppHandler struct {
	platform.AppHandler
	ready           func()
	dispatched      int
	themeChanged    int
	becameActive    int
	resigned        int
	displaysChanged int
	localeChanged   int
	openURLs        []string
	openFiles       []string
	activated       []bool
	mediaCmds       []recordedMediaCmd
	notifClicks     []string
	powerEvents     []string
	netChanges      []platform.NetworkStatus
	quitRequested   func() bool
	quitCalls       int
}

func (h *testAppHandler) QuitRequested() bool {
	h.quitCalls++
	if h.quitRequested != nil {
		return h.quitRequested()
	}
	return true
}

func (h *testAppHandler) Ready() {
	if h.ready != nil {
		h.ready()
	}
}
func (h *testAppHandler) Dispatch()                                 { h.dispatched++ }
func (h *testAppHandler) ThemeChanged()                             { h.themeChanged++ }
func (h *testAppHandler) DidBecomeActive()                          { h.becameActive++ }
func (h *testAppHandler) DidResignActive()                          { h.resigned++ }
func (h *testAppHandler) DisplaysChanged()                          { h.displaysChanged++ }
func (h *testAppHandler) LocaleChanged()                            { h.localeChanged++ }
func (h *testAppHandler) OpenURLs(urls []string)                    { h.openURLs = append(h.openURLs, urls...) }
func (h *testAppHandler) OpenFiles(paths []string)                  { h.openFiles = append(h.openFiles, paths...) }
func (h *testAppHandler) Activated(vis bool)                        { h.activated = append(h.activated, vis) }
func (h *testAppHandler) NotificationClicked(id string)             { h.notifClicks = append(h.notifClicks, id) }
func (h *testAppHandler) PowerEvent(ev string)                      { h.powerEvents = append(h.powerEvents, ev) }
func (h *testAppHandler) NetworkChanged(st platform.NetworkStatus)  { h.netChanges = append(h.netChanges, st) }
func (h *testAppHandler) MediaCommand(cmd platform.MediaCommand, pos time.Duration) {
	h.mediaCmds = append(h.mediaCmds, recordedMediaCmd{cmd: cmd, pos: pos})
}

// Signaling the backend before Init (such as Application.SetLanguages posting before Run) does not panic on a nil sync.Cond and dispatches on the first step.
func TestSignalBeforeInitDispatchesOnFirstStep(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	b := NewWithFD(fds[0])
	b.Signal() // previously panicked with nil b.cond before Init
	app := &testAppHandler{}
	if err := b.Init(app, platform.AppOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	b.Step()
	if app.dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1", app.dispatched)
	}
}

func makeFramePayload(winID, seq, id, w, h, stride, size uint32) []byte {
	p := make([]byte, 28)
	binary.LittleEndian.PutUint32(p[0:], winID)
	binary.LittleEndian.PutUint32(p[4:], seq)
	binary.LittleEndian.PutUint32(p[8:], id)
	binary.LittleEndian.PutUint32(p[12:], w)
	binary.LittleEndian.PutUint32(p[16:], h)
	binary.LittleEndian.PutUint32(p[20:], stride)
	binary.LittleEndian.PutUint32(p[24:], size)
	return p
}

// Rotating across a 3-buffer swapchain with incremental PresentDamage brings returning buffers fully up to date instead of leaving ghost highlights from older frames.
// Previously Surface.present only painted whole on a buffer's first visit and then copied only the newest frame's damage, leaving intermediate frames missing.
func TestTripleBufferDamageCatchesUpStaleBuffers(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)

	const (
		w      = 16
		ht     = 16
		stride = w * 4
		size   = stride * ht
	)
	type ringBuf struct {
		id  uint32
		f   *os.File
		mem []byte
	}
	var ring [3]ringBuf
	for i := range ring {
		f, err := os.CreateTemp(t.TempDir(), "buf-*")
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
		ring[i] = ringBuf{id: uint32(100 + i), f: f, mem: mem}
	}

	pix := make([]byte, size)
	fillRect := func(r image.Rectangle, val byte) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				off := y*stride + x*4
				pix[off+0], pix[off+1], pix[off+2], pix[off+3] = val, val, val, 0xff
			}
		}
	}

	// Reuse a single backing slice for damage just as raster.Renderer does.
	reusedDamage := make([]image.Rectangle, 0, 4)
	var curDamage []image.Rectangle
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			s.PresentDamage(pix, stride, w, ht, curDamage)
		}
		return false
	}

	sendAndCheck := func(step int, rb ringBuf, dmg []image.Rectangle) {
		t.Helper()
		reusedDamage = append(reusedDamage[:0], dmg...)
		curDamage = reusedDamage
		dupFD, err := syscall.Dup(int(rb.f.Fd()))
		if err != nil {
			t.Fatalf("Dup: %v", err)
		}
		b.onFrame(makeFramePayload(1, uint32(step), rb.id, w, ht, stride, size), dupFD)
		kind, payload, _, err := hostConn.read()
		if err != nil || kind != kindPresent || len(payload) < 12 || binary.LittleEndian.Uint32(payload[0:]) != 1 {
			t.Fatalf("step %d: got kind=%d payload=%v err=%v, want kindPresent(winID=1)", step, kind, payload, err)
		}
		if !bytes.Equal(rb.mem, pix) {
			t.Fatalf("step %d (buf %d): mapped buffer diverged from framebuffer", step, rb.id)
		}
	}

	// Frame 0..2: initial fill + moving a 4-row hover highlight across rows 0, 4, 8.
	fillRect(image.Rect(0, 0, w, ht), 0x10)
	fillRect(image.Rect(0, 0, w, 4), 0xa0)
	sendAndCheck(0, ring[0], []image.Rectangle{image.Rect(0, 0, w, ht)})

	fillRect(image.Rect(0, 0, w, 4), 0x10)
	fillRect(image.Rect(0, 4, w, 8), 0xb0)
	sendAndCheck(1, ring[1], []image.Rectangle{image.Rect(0, 0, w, 4), image.Rect(0, 4, w, 8)})

	fillRect(image.Rect(0, 4, w, 8), 0x10)
	fillRect(image.Rect(0, 8, w, 12), 0xc0)
	sendAndCheck(2, ring[2], []image.Rectangle{image.Rect(0, 4, w, 8), image.Rect(0, 8, w, 12)})

	// Frame 3..5: buffers 0, 1, 2 return after holding frames 0, 1, 2; each must catch up on all intermediate hover unhighlights.
	fillRect(image.Rect(0, 8, w, 12), 0x10)
	fillRect(image.Rect(0, 12, w, 16), 0xd0)
	sendAndCheck(3, ring[0], []image.Rectangle{image.Rect(0, 8, w, 12), image.Rect(0, 12, w, 16)})

	// Frame 4: empty damage (nothing changed since frame 3) still brings ring[1] up to frame 3's pixels.
	sendAndCheck(4, ring[1], []image.Rectangle{})

	// Frame 5: full-screen page switch followed by a tiny hover on frame 6 into ring[0].
	fillRect(image.Rect(0, 0, w, ht), 0x42)
	sendAndCheck(5, ring[2], []image.Rectangle{image.Rect(0, 0, w, ht)})

	fillRect(image.Rect(2, 2, 6, 6), 0xee)
	sendAndCheck(6, ring[0], []image.Rectangle{image.Rect(2, 2, 6, 6)})
}

// Skipping presentation during SurfaceFrame (such as 60fps pacing on a 120Hz display) aborts the unused buffer and forwards RequestFrame afterwards.
// Previously cur stayed non-nil so RequestFrame was dropped and the host timed out after 2s and killed the engine.
func TestSkippedFrameAbortsBufferAndRequestsNext(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)

	const (
		w      = 8
		ht     = 8
		stride = w * 4
		size   = stride * ht
	)
	f, err := os.CreateTemp(t.TempDir(), "skip-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			// Simulate runtime.surfaceFrame paceSkips(): ask for next refresh without presenting.
			s.RequestFrame()
		}
		return false
	}

	dupFD, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("Dup: %v", err)
	}
	b.onFrame(makeFramePayload(1, 7, 200, w, ht, stride, size), dupFD)

	kind1, payload1, _, err := hostConn.read()
	if err != nil || kind1 != kindAbort || len(payload1) != 8 ||
		binary.LittleEndian.Uint32(payload1[0:]) != 1 || binary.LittleEndian.Uint32(payload1[4:]) != 7 {
		t.Fatalf("first message = (kind=%d, payload=%v, err=%v), want kindAbort(winID=1, seq=7)", kind1, payload1, err)
	}
	kind2, payload2, _, err := hostConn.read()
	if err != nil || kind2 != kindRequest || len(payload2) != 4 || binary.LittleEndian.Uint32(payload2) != 1 {
		t.Fatalf("second message = (kind=%d, payload=%v, err=%v), want kindRequest(winID=1)", kind2, payload2, err)
	}
}

// Signal is never dropped when socket events are already queued, Wake does not spuriously invoke Dispatch, and pending inputs coalesce ahead of a waiting frame.
// Previously Backend.step cleared woken whenever len(b.events) > 0, wedging mainLoop timers such as ui.Spinner and Window.Update until the user clicked.
func TestSignalNotDroppedBehindSocketEventsAndCoalescesBeforeFrame(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	// Wake unblocks Step without invoking AppHandler.Dispatch.
	b.Wake()
	b.Step()
	if app.dispatched != 0 {
		t.Fatalf("Wake invoked Dispatch %d times, want 0", app.dispatched)
	}

	const (
		w      = 8
		ht     = 8
		stride = w * 4
		size   = stride * ht
	)
	f, err := os.CreateTemp(t.TempDir(), "frame-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	dupFD, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("Dup: %v", err)
	}

	pix := make([]byte, size)
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			s.PresentPixels(pix, stride, w, ht)
		}
		return false
	}

	// Queue a waiting frame followed by three consecutive mouse moves, and Signal concurrently.
	b.mu.Lock()
	b.events = append(b.events,
		event{kind: kindFrame, payload: makeFramePayload(1, 1, 300, w, ht, stride, size), fd: dupFD},
		event{kind: kindInput, payload: encodeMouseInput(1, mouseMove, 255, 0, 0, 10, 10, 20, 0)},
		event{kind: kindInput, payload: encodeMouseInput(1, mouseMove, 255, 0, 0, 12, 30, 40, 0)},
		event{kind: kindInput, payload: encodeMouseInput(1, mouseMove, 255, 0, 0, 14, 55, 65, 0)},
	)
	b.mu.Unlock()
	b.Signal()

	// First Step runs Dispatch AND drains/coalesces the three PointerMoves ahead of the waiting frame.
	b.Step()
	if app.dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1", app.dispatched)
	}
	if len(h.events) != 1 || h.events[0].Kind != platform.PointerMove || h.events[0].X != 55 || h.events[0].Y != 65 {
		t.Fatalf("after first step events = %+v, want single coalesced PointerMove(55, 65)", h.events)
	}

	// Second Step runs the waiting paced frame.
	b.Step()
	if len(h.events) != 2 || h.events[1].Kind != platform.SurfaceFrame || !h.events[1].Paced {
		t.Fatalf("after second step events = %+v, want Paced SurfaceFrame", h.events)
	}
	kind, _, _, err := hostConn.read()
	if err != nil || kind != kindPresent {
		t.Fatalf("expected kindPresent, got kind=%d err=%v", kind, err)
	}
}

// SetFrameRate, Idle, duplicate onSurface suppression, window focus/occlusion, and system dark mode follow native platform contracts.
func TestSurfacePacingIdleLifecycleAndTheme(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	// Matching initial geometry updates refresh rate without firing a redundant SurfaceResize.
	initSurf := make([]byte, 20)
	binary.LittleEndian.PutUint32(initSurf[0:], 1)
	binary.LittleEndian.PutUint32(initSurf[4:], 1600)
	binary.LittleEndian.PutUint32(initSurf[8:], 1000)
	binary.LittleEndian.PutUint32(initSurf[12:], 2000)
	binary.LittleEndian.PutUint32(initSurf[16:], 120000)
	b.onSurface(initSurf)
	if len(h.events) != 0 || h.resized != 0 || s.RefreshRate() != 120 {
		t.Fatalf("identical onSurface: events=%+v resized=%d hz=%v", h.events, h.resized, s.RefreshRate())
	}

	// Real resize updates bounds and notifies both SurfaceResize and WindowHandler.Resized.
	binary.LittleEndian.PutUint32(initSurf[4:], 2000)
	binary.LittleEndian.PutUint32(initSurf[8:], 1200)
	b.onSurface(initSurf)
	if len(h.events) != 1 || h.events[0].Kind != platform.SurfaceResize || h.resized != 1 || s.w.Bounds().Width != 1000 {
		t.Fatalf("real resize: events=%+v resized=%d bounds=%+v", h.events, h.resized, s.w.Bounds())
	}

	// SetFrameRate deduplicates identical rates and sends kindFrameRate on change.
	s.SetFrameRate(60)
	s.SetFrameRate(60)
	s.SetFrameRate(0)
	for i, want := range []uint32{60, 0} {
		kind, payload, _, err := hostConn.read()
		if err != nil || kind != kindFrameRate || len(payload) != 8 ||
			binary.LittleEndian.Uint32(payload[0:]) != 1 || binary.LittleEndian.Uint32(payload[4:]) != want {
			t.Fatalf("rate msg %d = (kind=%d, payload=%v, err=%v), want kindFrameRate(winID=1, %d)", i, kind, payload, err, want)
		}
	}

	// Window focus, blur, minimize/hide, show, and dark mode propagate to WindowHandler, OccludableSurface, and AppHandler.
	s.w.Show()
	_, _, _, _ = hostConn.read() // drain winActShow
	h.events = nil
	b.onInput(encodeWindowInput(1, winFocus))
	b.onInput(encodeWindowInput(1, winFocus)) // duplicate ignored
	b.onInput(encodeWindowInput(1, winBlur))
	b.onInput(encodeWindowInput(1, winMinimize))
	if !s.Occluded() || !s.w.IsMinimized() || !s.w.IsVisible() ||
		h.focused != 1 || h.blurred != 1 || h.minimized != 1 || app.becameActive != 1 || app.resigned != 1 {
		t.Fatalf("focus/minimize state mismatch: occluded=%v min=%v vis=%v focused=%d blurred=%d minCount=%d active=%d resigned=%d",
			s.Occluded(), s.w.IsMinimized(), s.w.IsVisible(), h.focused, h.blurred, h.minimized, app.becameActive, app.resigned)
	}
	b.onInput(encodeWindowInput(1, winRestore))
	if s.Occluded() || s.w.IsMinimized() || h.restored != 1 {
		t.Fatalf("after winRestore: occluded=%v min=%v restored=%d", s.Occluded(), s.w.IsMinimized(), h.restored)
	}

	// Explicit Hide() sets IsVisible=false and ignores the host's echoed winHide without marking IsMinimized.
	s.w.Hide()
	_, _, _, _ = hostConn.read() // drain winActHide
	b.onInput(encodeWindowInput(1, winHide))
	if s.w.IsVisible() || s.w.IsMinimized() || !s.Occluded() {
		t.Fatalf("after explicit Hide()+winHide echo: vis=%v min=%v occluded=%v", s.w.IsVisible(), s.w.IsMinimized(), s.Occluded())
	}

	b.onInput(encodeWindowInput(1, winThemeDark))
	if !b.Theme().IsDark() || app.themeChanged != 1 {
		t.Fatalf("after winThemeDark: isDark=%v themeChanged=%d", b.Theme().IsDark(), app.themeChanged)
	}
	b.Theme().SetSource("light")
	if b.Theme().IsDark() {
		t.Fatalf("expected light override to report IsDark=false")
	}

	// Idle unmaps idle swapchain buffers and clears history.
	f, err := os.CreateTemp(t.TempDir(), "idle-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	_ = f.Truncate(64)
	dupFD, _ := syscall.Dup(int(f.Fd()))
	pix := make([]byte, 64)
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			s.PresentPixels(pix, 16, 4, 4)
		}
		return false
	}
	b.onFrame(makeFramePayload(1, 1, 500, 4, 4, 16, 64), dupFD)
	_, _, _, _ = hostConn.read()
	if len(s.buffers) != 1 {
		t.Fatalf("buffers before Idle = %d, want 1", len(s.buffers))
	}
	s.Idle()
	if len(s.buffers) != 0 || len(s.history) != 0 {
		t.Fatalf("after Idle: buffers=%d history=%d, want 0 and 0", len(s.buffers), len(s.history))
	}
}

// Multiple windows route input and resize events independently, keep the app active across focus handoffs, and honor ShouldClose interception.
func TestMultiWindowLifecycleFocusAndShouldClose(t *testing.T) {
	b, s1, hostConn, h1 := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	h2 := &testWindowHandler{}
	pw2, err := b.NewWindow(&platform.WindowOptions{
		Surface:     true,
		Parent:      s1.w,
		Title:       "Preferences",
		Width:       480,
		Height:      360,
		AlwaysOnTop: true,
		Modal:       true,
		Resizable:   false,
	}, h2)
	if err != nil {
		t.Fatalf("NewWindow(2): %v", err)
	}
	w2 := pw2.(*Window)
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinCreate {
		t.Fatalf("NewWindow(2) kind=%d err=%v, want kindWinCreate", kind, err)
	}
	cfg, ok := decodeWinConfigMessage(payload)
	if !ok || cfg.winID != 2 || cfg.parentOrMask != 1 || cfg.title != "Preferences" ||
		cfg.flags&winFlagModal == 0 || cfg.flags&winFlagAlwaysOnTop == 0 || cfg.flags&winFlagResizable != 0 {
		t.Fatalf("win2 config = %+v (ok=%v)", cfg, ok)
	}

	// Independent pointer events hit only the targeted window.
	b.onInput(encodeMouseInput(1, mouseDown, 0, 0, 0, 100, 15, 25, 0))
	b.onInput(encodeMouseInput(2, mouseDown, 0, 0, 0, 110, 95, 105, 0))
	if len(h1.events) != 1 || h1.events[0].X != 15 || len(h2.events) != 1 || h2.events[0].X != 95 {
		t.Fatalf("independent mouse routing failed: h1=%+v h2=%+v", h1.events, h2.events)
	}

	// Focus handoff from window 1 -> window 2 keeps the application active without firing DidResignActive.
	b.onInput(encodeWindowInput(1, winFocus))
	b.onInput(encodeWindowInput(2, winFocus))
	b.onInput(encodeWindowInput(1, winBlur))
	if !b.App().IsActive() || app.becameActive != 1 || app.resigned != 0 || !w2.IsFocused() || s1.w.IsFocused() {
		t.Fatalf("handoff state: active=%v became=%d resigned=%d w1Focus=%v w2Focus=%v",
			b.App().IsActive(), app.becameActive, app.resigned, s1.w.IsFocused(), w2.IsFocused())
	}
	b.onInput(encodeWindowInput(2, winBlur))
	if b.App().IsActive() || app.resigned != 1 {
		t.Fatalf("after blurring last window: active=%v resigned=%d, want false and 1", b.App().IsActive(), app.resigned)
	}

	// ShouldClose returning false denies windowWillClose; returning true allows closing and cleans up the window.
	allowClose := false
	h2.shouldClose = func() bool { return allowClose }
	b.onInput(encodeWindowInput(2, winCloseRequest))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActCloseDeny || w2.closed || h2.closed != 0 {
		t.Fatalf("denied close: kind=%d payload=%v closed=%v h2.closed=%d", kind, payload, w2.closed, h2.closed)
	}

	allowClose = true
	b.onInput(encodeWindowInput(2, winCloseRequest))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActCloseAllow || !w2.closed || h2.closed != 1 || b.windowByID(2) != nil {
		t.Fatalf("allowed close: kind=%d payload=%v closed=%v h2.closed=%d", kind, payload, w2.closed, h2.closed)
	}
}

// Hidden title bar windows report caption button insets, clear them in full screen, forward StartDrag, and toggle zoom on double click.
func TestHiddenTitleBarInsetsDragAndDoubleClickZoom(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	b := NewWithFD(fds[0])
	if err := b.Init(nil, platform.AppOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	hostConn := &conn{fd: fds[1]}
	h := &testWindowHandler{}
	pw, err := b.NewWindow(&platform.WindowOptions{
		Surface:        true,
		Width:          960,
		Height:         640,
		TitleBarStyle:  "hidden",
		TitleBarHeight: 40,
		Resizable:      true,
		Maximizable:    true,
		Movable:        true,
	}, h)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	w := pw.(*Window)
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinCreate {
		t.Fatalf("NewWindow msg = (%d, %v)", kind, err)
	}
	cfg, ok := decodeWinConfigMessage(payload)
	if !ok || cfg.flags&winFlagHiddenTitleBar == 0 || cfg.titleBarHeight != 40 {
		t.Fatalf("hidden titlebar cfg = %+v (ok=%v)", cfg, ok)
	}
	if tb := w.TitleBar(); tb.Height != 40 || tb.Right <= 0 {
		t.Fatalf("initial TitleBar = %+v, want Height=40 Right>0", tb)
	}

	// Host reports exact TitleButtonRect in vp -> updates TitleBar and fires TitleBarChanged.
	b.onInput(encodeWindowTitleBarInput(1, 36, 0, 138))
	if tb := w.TitleBar(); tb != (platform.TitleBar{Height: 40, Left: 0, Right: 138}) || h.titleBarChanged != 1 {
		t.Fatalf("updated TitleBar = %+v changed=%d, want {40 0 138} and 1", tb, h.titleBarChanged)
	}

	// StartDrag sends winActStartDrag while movable and non-fullscreen.
	w.StartDrag()
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActStartDrag {
		t.Fatalf("StartDrag msg = (%d, %v, %v), want winActStartDrag", kind, payload, err)
	}

	// Double clicking titlebar toggles Maximize then Unmaximize.
	w.TitleBarDoubleClicked()
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActMaximize || !w.IsMaximized() {
		t.Fatalf("first double click = (%d, %v, max=%v), want winActMaximize", kind, payload, w.IsMaximized())
	}
	w.TitleBarDoubleClicked()
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActUnmaximize || w.IsMaximized() {
		t.Fatalf("second double click = (%d, %v, max=%v), want winActUnmaximize", kind, payload, w.IsMaximized())
	}

	// Fullscreen zeroes TitleBar insets and restores them on exit.
	w.SetFullScreen(true)
	_, _, _, _ = hostConn.read()
	if tb := w.TitleBar(); tb != (platform.TitleBar{}) || h.titleBarChanged != 2 {
		t.Fatalf("fullscreen TitleBar = %+v changed=%d", tb, h.titleBarChanged)
	}
	w.SetFullScreen(false)
	_, _, _, _ = hostConn.read()
	if tb := w.TitleBar(); tb != (platform.TitleBar{Height: 40, Left: 0, Right: 138}) || h.titleBarChanged != 3 {
		t.Fatalf("restored TitleBar = %+v changed=%d", tb, h.titleBarChanged)
	}
}

// PopupMenu serializes nested menus, pumps concurrent host events while waiting, and fires MenuItemClicked before returning.
func TestPopupMenuNestedLoopAndConcurrency(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)
	go b.readLoop()

	menu := &platform.Menu{
		Items: []*platform.MenuItem{
			{Type: platform.MenuItemSeparator, Visible: true}, // leading separator trimmed
			{ID: 10, Type: platform.MenuItemNormal, Label: "&Copy", Accelerator: "CmdOrCtrl+c", Enabled: true, Visible: true},
			{Type: platform.MenuItemSeparator, Visible: true},
			{Type: platform.MenuItemSeparator, Visible: true}, // consecutive separator collapsed
			{ID: 20, Type: platform.MenuItemCheckbox, Label: "Shuffle", Enabled: true, Checked: true, Visible: true},
			{
				ID: 30, Type: platform.MenuItemSubmenu, Label: "Output", Enabled: true, Visible: true,
				Submenu: &platform.Menu{
					Items: []*platform.MenuItem{
						{ID: 31, Type: platform.MenuItemRadio, Label: "Speakers", Enabled: true, Checked: true, Visible: true},
					},
				},
			},
		},
	}

	doneHost := make(chan error, 1)
	go func() {
		kind, payload, _, err := hostConn.read()
		if err != nil {
			doneHost <- err
			return
		}
		if kind != kindPopupMenu {
			doneHost <- os.ErrInvalid
			return
		}
		reqID, winID, x, y, _, items, ok := decodePopupMenuMessage(payload)
		if !ok || winID != 1 || x != 120 || y != 180 || len(items) != 4 ||
			items[0].kind != 0 || items[0].label != "Copy" || items[0].accel != "Ctrl+C" ||
			items[1].kind != 1 ||
			items[2].kind != 3 || !items[2].checked ||
			items[3].kind != 2 || len(items[3].items) != 1 ||
			items[3].items[0].kind != 4 || items[3].items[0].id != 31 {
			doneHost <- os.ErrInvalid
			return
		}
		// Send a concurrent window move while PopupMenu's nested Step() loop is waiting.
		if err := hostConn.write(kindInput, encodeWindowRectInput(1, 200, 100, 800, 500)); err != nil {
			doneHost <- err
			return
		}
		doneHost <- hostConn.write(kindPopupResult, encodePopupResultMessage(reqID, 31))
	}()

	b.PopupMenu(menu, s.w, &platform.Point{X: 120, Y: 180})
	if err := <-doneHost; err != nil {
		t.Fatalf("host popup handler failed: %v", err)
	}
	if h.moved != 1 || s.w.Bounds().X != 200 || s.w.Bounds().Y != 100 {
		t.Fatalf("concurrent winRect not pumped during PopupMenu: moved=%d bounds=%+v", h.moved, s.w.Bounds())
	}
	if !slices.Equal(h.menuClicks, []int{31}) {
		t.Fatalf("menuClicks = %v, want [31]", h.menuClicks)
	}
}

// Pre-Init environment variables populate App and Screen metadata immediately, dynamic kindEnvInfo updates notify AppHandler, and CursorPoint tracks screen DIPs.
func TestAppEnvPreInitDisplaysAndCursorPoint(t *testing.T) {
	t.Setenv("MYGO_OHOS_PKG_ID", "dev.egoist.music")
	t.Setenv("MYGO_OHOS_PKG_NAME", "MyGo Music")
	t.Setenv("MYGO_OHOS_PKG_VER", "0.1.28")
	t.Setenv("MYGO_OHOS_LANGS", "zh-Hans-CN,en-US")
	t.Setenv("MYGO_OHOS_LOCALE", "zh-Hans-CN")
	t.Setenv("MYGO_OHOS_HOUR_CYCLE", "24")
	t.Setenv("MYGO_OHOS_FIRST_WEEKDAY", "1")
	t.Setenv("MYGO_OHOS_DISPLAYS", "0:0,0,1920,1280:0,0,1920,1224:1500:0:3:Built-in Display")

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	b := NewWithFD(fds[0])
	// Safe before Init!
	pkg, ok := b.App().Package()
	if !ok || pkg.Identifier != "dev.egoist.music" || pkg.Name != "MyGo Music" || pkg.Version != "0.1.28" {
		t.Fatalf("pre-Init Package() = (%+v, %v)", pkg, ok)
	}
	if got := b.App().PreferredLanguages(); !slices.Equal(got, []string{"zh-Hans-CN", "en-US"}) {
		t.Fatalf("pre-Init PreferredLanguages() = %v", got)
	}
	if reg := b.App().Region(); reg.Locale != "zh-Hans-CN" || reg.HourCycle != 24 || reg.FirstWeekday != 1 {
		t.Fatalf("pre-Init Region() = %+v", reg)
	}
	disps := b.Screen().Displays()
	if len(disps) != 1 || disps[0].Bounds.Width != 1920 || disps[0].WorkArea.Height != 1224 ||
		math.Abs(disps[0].ScaleFactor-1.5) > 1e-6 || !disps[0].Primary || !disps[0].Internal {
		t.Fatalf("pre-Init Displays() = %+v", disps)
	}

	app := &testAppHandler{}
	if err := b.Init(app, platform.AppOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err = b.NewWindow(&platform.WindowOptions{Surface: true, X: 100, Y: 50, Width: 800, Height: 600}, &testWindowHandler{})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	// Pointer move updates Screen.CursorPoint in screen DIPs (window origin + content offset).
	b.onInput(encodeMouseInput(1, mouseMove, 255, 0, 0, 10, 45, 75, 0))
	if pt := b.Screen().CursorPoint(); pt != (platform.Point{X: 145, Y: 125}) {
		t.Fatalf("CursorPoint = %+v, want {145, 125}", pt)
	}

	// Dynamic kindEnvInfo triggers DisplaysChanged and LocaleChanged.
	b.onEnvInfo(encodeEnvInfoMessage(envInfoWire{
		langs:  []string{"en-US"},
		region: platform.Region{Locale: "en-US", FirstWeekday: 0, HourCycle: 12},
		displays: []platform.Display{
			disps[0],
			{
				ID:          2,
				Label:       "External 4K",
				Bounds:      platform.Rect{X: 1920, Y: 0, Width: 1920, Height: 1080},
				WorkArea:    platform.Rect{X: 1920, Y: 0, Width: 1920, Height: 1080},
				ScaleFactor: 2.0,
			},
		},
	}))
	if app.displaysChanged != 1 || app.localeChanged != 1 || len(b.Screen().Displays()) != 2 {
		t.Fatalf("after onEnvInfo: displaysChanged=%d localeChanged=%d count=%d",
			app.displaysChanged, app.localeChanged, len(b.Screen().Displays()))
	}
}

// Fresh surface notifications re-emit SurfaceResize and reset swapchain damage history even when geometry matches, and coalescing preserves the fresh flag.
func TestFreshSurfaceReexposeAndCoalescing(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)

	// Populate swapchain buffer seq + damage history and leave waiting=true.
	f, err := os.CreateTemp(t.TempDir(), "fresh-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	_ = f.Truncate(64)
	dupFD, _ := syscall.Dup(int(f.Fd()))
	pix := make([]byte, 64)
	h.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.SurfaceFrame {
			s.PresentPixels(pix, 16, 4, 4)
		}
		return false
	}
	b.onFrame(makeFramePayload(1, 1, 100, 4, 4, 16, 64), dupFD)
	_, _, _, _ = hostConn.read() // drain kindPresent
	s.RequestFrame()
	_, _, _, _ = hostConn.read() // drain kindRequest
	h.take = nil
	h.events = nil
	h.resized = 0

	if !s.waiting || len(s.history) == 0 || s.buffers[100].lastSeq == 0 {
		t.Fatalf("precondition failed: waiting=%v history=%d lastSeq=%d", s.waiting, len(s.history), s.buffers[100].lastSeq)
	}

	// Enqueue fresh kindSurface(1600x1000@2x) followed by non-fresh kindSurface(1600x1000@2x) before Step().
	freshMsg := make([]byte, 21)
	binary.LittleEndian.PutUint32(freshMsg[0:], 1)
	binary.LittleEndian.PutUint32(freshMsg[4:], 1600)
	binary.LittleEndian.PutUint32(freshMsg[8:], 1000)
	binary.LittleEndian.PutUint32(freshMsg[12:], 2000)
	binary.LittleEndian.PutUint32(freshMsg[16:], 60000)
	freshMsg[20] = 1

	staleMsg := bytes.Clone(freshMsg)
	staleMsg[20] = 0

	b.mu.Lock()
	b.events = append(b.events,
		event{kind: kindSurface, payload: freshMsg, fd: -1},
		event{kind: kindSurface, payload: staleMsg, fd: -1},
	)
	b.mu.Unlock()
	b.Step()

	if s.waiting || len(s.history) != 0 || s.buffers[100].lastSeq != 0 {
		t.Fatalf("after fresh re-expose: waiting=%v history=%d lastSeq=%d, want false, 0, 0",
			s.waiting, len(s.history), s.buffers[100].lastSeq)
	}
	if h.resized != 1 || len(h.events) != 1 || h.events[0].Kind != platform.SurfaceResize {
		t.Fatalf("fresh same-geometry surface did not emit single SurfaceResize: resized=%d events=%+v", h.resized, h.events)
	}
}

// Hiding a hovered window clears hover before SurfaceHidden, Show restores visibility, and winQuitRequest honors AppHandler.QuitRequested.
func TestHidePointerLeaveShowAndQuitRequest(t *testing.T) {
	b, s, hostConn, h := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	s.w.Show()
	_, _, _, _ = hostConn.read() // drain winActShow
	b.onInput(encodeMouseInput(1, mouseMove, 255, 0, 0, 100, 40, 30, 0))
	h.events = nil

	// Hide while hovered emits PointerLeave then SurfaceHidden once; echoed winHide is a no-op.
	s.w.Hide()
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActHide {
		t.Fatalf("Hide msg = (%d, %v, %v), want winActHide", kind, payload, err)
	}
	b.onInput(encodeWindowInput(1, winHide))
	if s.w.IsVisible() || len(h.events) != 2 ||
		h.events[0].Kind != platform.PointerLeave || h.events[1].Kind != platform.SurfaceHidden {
		t.Fatalf("after Hide+winHide: vis=%v events=%+v", s.w.IsVisible(), h.events)
	}

	// Show restores visibility and emits SurfaceShown once even if host echoes winShow.
	h.events = nil
	s.w.Show()
	_, _, _, _ = hostConn.read() // drain winActShow
	b.onInput(encodeWindowInput(1, winShow))
	if !s.w.IsVisible() || s.Occluded() || len(h.events) != 1 || h.events[0].Kind != platform.SurfaceShown {
		t.Fatalf("after Show+winShow: vis=%v occluded=%v events=%+v", s.w.IsVisible(), s.Occluded(), h.events)
	}

	// QuitRequested veto sends winActQuitDeny without quitting; allowing sends kindQuit.
	allowQuit := false
	app.quitRequested = func() bool { return allowQuit }
	b.onInput(encodeWindowInput(1, winQuitRequest))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActQuitDeny || app.quitCalls != 1 || b.quit {
		t.Fatalf("vetoed quit: kind=%d payload=%v calls=%d quit=%v", kind, payload, app.quitCalls, b.quit)
	}

	allowQuit = true
	b.onInput(encodeWindowInput(1, winQuitRequest))
	kind, _, _, err = hostConn.read()
	if err != nil || kind != kindQuit || app.quitCalls != 2 || !b.quit {
		t.Fatalf("allowed quit: kind=%d err=%v calls=%d quit=%v", kind, err, app.quitCalls, b.quit)
	}
}

// PlaceFlyout positions window-anchored and tray-anchored flyouts inside display work areas, and blurring a focusable flyout closes it unless a modal child is open.
func TestFlyoutPlacementAndAutoDismissOnBlur(t *testing.T) {
	t.Setenv("MYGO_OHOS_DISPLAYS", "0:0,0,1920,1080:0,0,1920,1040:1000:0:3:Primary")
	b, s, hostConn, _ := newTestSurface(t)
	s.w.bounds = platform.Rect{X: 200, Y: 100, Width: 400, Height: 300}

	fh := &testWindowHandler{}
	initFlyout := platform.Flyout{
		Anchor:    platform.Rect{X: 20, Y: 30, Width: 80, Height: 24},
		Side:      platform.SideBottom,
		Align:     platform.AlignStart,
		Gap:       6,
		Focusable: true,
	}
	pfw, err := b.NewWindow(&platform.WindowOptions{
		Surface: true,
		Parent:  s.w,
		Width:   240,
		Height:  160,
		Flyout:  &initFlyout,
	}, fh)
	if err != nil {
		t.Fatalf("NewWindow(flyout): %v", err)
	}
	fw := pfw.(*Window)
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinCreate {
		t.Fatalf("flyout create = (%d, %v)", kind, err)
	}
	cfg, ok := decodeWinConfigMessage(payload)
	if !ok || cfg.flags&winFlagFrameless == 0 || cfg.flags&winFlagAlwaysOnTop == 0 || cfg.flags&winFlagResizable != 0 ||
		fw.Bounds() != (platform.Rect{X: 220, Y: 160, Width: 240, Height: 160}) {
		t.Fatalf("initial flyout cfg=%+v bounds=%+v", cfg, fw.Bounds())
	}

	// PlaceFlyout with updated size repositions once and deduplicates identical calls.
	fw.Show()
	_, _, _, _ = hostConn.read() // drain winActShow
	fw.PlaceFlyout(initFlyout, platform.Size{Width: 260, Height: 180})
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinConfig || fw.Bounds() != (platform.Rect{X: 220, Y: 160, Width: 260, Height: 180}) {
		t.Fatalf("PlaceFlyout msg=(%d, %v) bounds=%+v", kind, payload, fw.Bounds())
	}
	fw.PlaceFlyout(initFlyout, platform.Size{Width: 260, Height: 180}) // duplicate -> no wire message

	// Tray-anchored flyout without tray bounds centers inside primary display work area.
	tray, err := b.NewTray(&testTrayHandler{})
	if err != nil {
		t.Fatalf("NewTray: %v", err)
	}
	tf := platform.Flyout{Tray: tray, Focusable: true}
	ptw, err := b.NewWindow(&platform.WindowOptions{
		Surface: true,
		Width:   300,
		Height:  200,
		Flyout:  &tf,
	}, &testWindowHandler{})
	if err != nil {
		t.Fatalf("NewWindow(tray flyout): %v", err)
	}
	_, _, _, _ = hostConn.read() // drain winCreate
	wantCentered := platform.Centered(platform.Size{Width: 300, Height: 200}, platform.Rect{X: 0, Y: 0, Width: 1920, Height: 1040})
	if ptw.Bounds() != wantCentered {
		t.Fatalf("tray flyout bounds = %+v, want %+v", ptw.Bounds(), wantCentered)
	}

	// Open modal child prevents auto-dismiss on blur; closing the modal child allows blur to close the flyout.
	mw, err := b.NewWindow(&platform.WindowOptions{
		Surface: true,
		Parent:  fw,
		Modal:   true,
		Width:   200,
		Height:  120,
	}, &testWindowHandler{})
	if err != nil {
		t.Fatalf("NewWindow(modal): %v", err)
	}
	_, _, _, _ = hostConn.read() // drain modal winCreate
	mw.Show()
	_, _, _, _ = hostConn.read() // drain modal winActShow

	b.onInput(encodeWindowInput(fw.id, winFocus))
	b.onInput(encodeWindowInput(fw.id, winBlur))
	if fw.closed {
		t.Fatal("flyout closed on blur while modal child was open")
	}

	mw.Close()
	_, _, _, _ = hostConn.read() // drain modal winActClose
	b.onInput(encodeWindowInput(fw.id, winFocus))
	b.onInput(encodeWindowInput(fw.id, winBlur))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActClose || !fw.closed || fh.closed != 1 {
		t.Fatalf("after modal closed + blur: kind=%d payload=%v err=%v closed=%v fh.closed=%d", kind, payload, err, fw.closed, fh.closed)
	}
}
