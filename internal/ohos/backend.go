//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/egoist/mygo/internal/ohosaudio"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/unsupported"
)

// Backend is the HarmonyOS one. The app's loop runs here, in the engine's
// own process, and its events arrive as messages from the process that
// owns the windows.
type Backend struct {
	unsupported.Backend

	fd   int
	conn *conn

	// The surface the host starts the process with, on the command line so
	// that the first layout of the main window does not race the first message.
	initW, initH int
	initScale    float64

	mainTID atomic.Int64

	mu           sync.Mutex
	cond         *sync.Cond
	events       []event
	signaled     bool
	woken        bool
	quit         bool
	quitSent     bool
	systemDark   bool
	themeSource  string
	app          platform.AppHandler
	appActive    bool
	nextWinID    uint32
	windows      map[uint32]*Window
	winOrder     []uint32
	hasPkg       bool
	pkg          platform.PackageInfo
	langs        []string
	region       platform.Region
	displays     []platform.Display
	cursorPoint     platform.Point
	nextPopupReq    uint32
	popupResults    map[uint32]int32
	clipOwned       *clipboardSource
	clipGen         uint64
	clipExtMimes    []string
	nextClipReq     uint32
	clipResults     map[uint32]clipReadResult
	nextDialogReq   uint32
	pendingDialogs  map[uint32]pendingDialog
	nextShellReq    uint32
	shellResults    map[uint32]string
	readyDone          bool
	pendingAppOpens    []appOpenEvent
	pendingNotifClicks []string
	schemes            map[string]bool

	npActive  bool
	npHasMeta bool
	npTitle   string
	npArtist  string
	npAlbum   string
	npArtLen  int
	npArtSum  [32]byte

	nextNotifReq  uint32
	pendingNotifs map[uint32]func(error)

	powerWatching bool
	nextPowerHold uint64
	wakeHolds     map[uint64]wakeHold
	runHolds      map[uint64]string
	lastPowerReq  powerReqWire
	onBattery     bool
	lastInputAt   time.Time

	netWatching bool
	netStatus   platform.NetworkStatus

	nextCookieReq  uint32
	pendingCookies map[uint32]pendingCookieCall

	nextWebAsyncReq uint32
	pendingWebAsync map[uint32]pendingWebAsyncCall
	schemeTasks     map[uint32]*schemeResponder

	readLoopOnce  sync.Once
	prefs         platform.Preferences
	nextSecretReq uint32
	secretWaiters map[uint32]chan secretWireRes
	nextHotkeyReq uint32
	hotkeyWaiters map[uint32]chan string
	nextTrayID    uint32
	trays         map[uint32]*trayItem
	dragSessions  map[string]*Surface
}

// New returns the backend from the arguments and environment the host started
// the process with: the descriptor frames come on, the initial main surface's
// size and scale, and packaged app / display / locale metadata.
func New() *Backend {
	b := &Backend{
		fd:          3,
		initScale:   1,
		windows:     make(map[uint32]*Window),
		lastInputAt: time.Now(),
	}
	b.mainTID.Store(int64(syscall.Getpid()))
	b.cond = sync.NewCond(&b.mu)
	b.loadInitialEnv()
	for _, arg := range os.Args[1:] {
		value := arg[strings.IndexByte(arg, '=')+1:]
		switch {
		case strings.HasPrefix(arg, "--fd="):
			if n, err := strconv.Atoi(value); err == nil {
				b.fd = n
			}
		case strings.HasPrefix(arg, "--size="):
			if w, h, ok := strings.Cut(value, "x"); ok {
				bw, err1 := strconv.Atoi(w)
				bh, err2 := strconv.Atoi(h)
				if err1 == nil && err2 == nil && bw > 0 && bh > 0 {
					b.initW, b.initH = bw, bh
				}
			}
		case strings.HasPrefix(arg, "--scale="):
			if f, err := strconv.ParseFloat(value, 64); err == nil && f > 0 {
				b.initScale = f
			}
		case strings.HasPrefix(arg, "--dark="):
			b.systemDark = value == "1" || value == "true"
		}
	}
	return b
}

// NewWithFD is New for a process or test the host gave another descriptor.
func NewWithFD(fd int) *Backend {
	b := &Backend{
		fd:          fd,
		initScale:   1,
		windows:     make(map[uint32]*Window),
		lastInputAt: time.Now(),
	}
	b.mainTID.Store(int64(syscall.Getpid()))
	b.cond = sync.NewCond(&b.mu)
	b.loadInitialEnv()
	return b
}

func (b *Backend) Name() string { return "ohos/surface" }

func (b *Backend) WebViewAvailable() bool { return true }

func (b *Backend) Theme() platform.Theme { return themeBackend{b} }

type themeBackend struct{ b *Backend }

func (t themeBackend) IsDark() bool {
	if t.b == nil {
		return false
	}
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	switch t.b.themeSource {
	case "dark":
		return true
	case "light":
		return false
	default:
		return t.b.systemDark
	}
}

func (t themeBackend) SetSource(src string) {
	if t.b == nil {
		return
	}
	t.b.mu.Lock()
	t.b.themeSource = src
	t.b.mu.Unlock()
}

func (themeBackend) UIFont() string { return "HarmonyOS Sans" }
func (t themeBackend) Preferences() platform.Preferences {
	if t.b == nil {
		return platform.Preferences{}
	}
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	return t.b.prefs
}
func (themeBackend) FontRendering() platform.FontRendering {
	return platform.FontRendering{Antialias: "gray", Hinting: "slight"}
}

func (b *Backend) setSystemDark(dark bool) {
	b.mu.Lock()
	changed := b.systemDark != dark
	b.systemDark = dark
	app := b.app
	var trays []*trayItem
	if changed && len(b.trays) > 0 {
		trays = make([]*trayItem, 0, len(b.trays))
		for _, tr := range b.trays {
			trays = append(trays, tr)
		}
	}
	b.mu.Unlock()
	if changed {
		if app != nil {
			app.ThemeChanged()
		}
		for _, tr := range trays {
			if tr != nil && !tr.destroyed && tr.h != nil {
				tr.h.AppearanceChanged()
			}
		}
	}
}

func (b *Backend) Init(h platform.AppHandler, opts platform.AppOptions) error {
	b.mu.Lock()
	b.app = h
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
	if b.windows == nil {
		b.windows = make(map[uint32]*Window)
	}
	b.mu.Unlock()
	b.conn = &conn{fd: b.fd}
	ohosaudio.SetBridge(b)
	return nil
}

func (b *Backend) IsMainThread() bool { return int64(syscall.Gettid()) == b.mainTID.Load() }

func (b *Backend) ensureReadLoop() {
	if b == nil {
		return
	}
	b.readLoopOnce.Do(func() {
		b.mu.Lock()
		if b.conn == nil && b.fd >= 0 {
			b.conn = &conn{fd: b.fd}
		}
		hasConn := b.conn != nil
		b.mu.Unlock()
		if hasConn {
			go b.readLoop()
		}
	})
}

// Run runs the app's loop: one message at a time, on a thread of its own,
// which the backend pins so that IsMainThread stays true.
func (b *Backend) Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	b.mainTID.Store(int64(syscall.Gettid()))
	b.mu.Lock()
	if b.cond == nil {
		b.cond = sync.NewCond(&b.mu)
	}
	app := b.app
	b.mu.Unlock()
	b.ensureReadLoop()
	if app != nil {
		app.Ready()
	}
	b.flushPendingAppOpens()
	for b.step() {
	}
	return nil
}

// isMouseMove reports whether ev is a pointer move carrying window ID, button, and pointer kind.
func isMouseMove(ev event) (winID uint32, button, pointer byte, ok bool) {
	if ev.kind != kindInput || len(ev.payload) < 28 {
		return 0, 0, 0, false
	}
	if ev.payload[4] != inputMouse || ev.payload[5] != mouseMove {
		return 0, 0, 0, false
	}
	return binary.LittleEndian.Uint32(ev.payload[0:]), ev.payload[6], ev.payload[7], true
}

func surfaceWinID(ev event) (uint32, bool) {
	if ev.kind != kindSurface || len(ev.payload) < 16 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(ev.payload[0:]), true
}

// popNextLocked picks the next event to process under b.mu, draining pending
// input/popup/env messages ahead of a waiting frame buffer and coalescing
// consecutive pointer moves or resize notifications for the same window.
func (b *Backend) popNextLocked() (event, bool) {
	if len(b.events) == 0 {
		return event{}, false
	}
	idx := 0
	if b.events[0].kind == kindFrame {
		for i := 1; i < len(b.events); i++ {
			k := b.events[i].kind
			switch k {
			case kindInput, kindPopupResult, kindEnvInfo,
				kindClipChanged, kindClipData, kindDialogResult, kindShellResult, kindAppOpen,
				kindMediaCmd, kindNotifResult, kindNotifClicked, kindPowerState, kindNetState,
				kindCookieResult, kindWebEvent, kindWebAsyncRes, kindSchemeReq,
				kindHotkeyTrig, kindTrayEvent, kindDragEvent, kindAccessEvent, kindInternal:
				idx = i
			case kindQuit, kindSurface:
				i = len(b.events)
			}
			if idx != 0 {
				break
			}
		}
	}
	ev := b.events[idx]
	copy(b.events[idx:], b.events[idx+1:])
	b.events[len(b.events)-1] = event{}
	b.events = b.events[:len(b.events)-1]

	if wid, ok := surfaceWinID(ev); ok {
		for idx < len(b.events) {
			nwid, nok := surfaceWinID(b.events[idx])
			if !nok || nwid != wid {
				break
			}
			ev = b.events[idx]
			copy(b.events[idx:], b.events[idx+1:])
			b.events[len(b.events)-1] = event{}
			b.events = b.events[:len(b.events)-1]
		}
	} else if wid, btn, pk, ok := isMouseMove(ev); ok {
		for idx < len(b.events) {
			nwid, nbtn, npk, nok := isMouseMove(b.events[idx])
			if !nok || nwid != wid || nbtn != btn || npk != pk {
				break
			}
			ev = b.events[idx]
			copy(b.events[idx:], b.events[idx+1:])
			b.events[len(b.events)-1] = event{}
			b.events = b.events[:len(b.events)-1]
		}
	}
	return ev, true
}

// step waits for pending main-thread work or host messages and handles them,
// reporting whether the loop goes on.
func (b *Backend) step() bool {
	b.mu.Lock()
	for len(b.events) == 0 && !b.signaled && !b.woken && !b.quit {
		b.cond.Wait()
	}
	if b.quit {
		b.mu.Unlock()
		return false
	}
	doDispatch := b.signaled
	b.signaled = false
	b.woken = false
	ev, has := b.popNextLocked()
	app := b.app
	b.mu.Unlock()

	if doDispatch && app != nil {
		app.Dispatch()
	}
	if has {
		b.dispatch(ev)
	}
	return true
}

// Step handles one message, for the waits nested inside handlers.
func (b *Backend) Step() { b.step() }

// Wake unblocks a pending Step without scheduling AppHandler.Dispatch.
func (b *Backend) Wake() {
	b.mu.Lock()
	b.woken = true
	if b.cond != nil {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}

// Signal asks for AppHandler.Dispatch on the thread that runs the loop.
func (b *Backend) Signal() {
	b.mu.Lock()
	b.signaled = true
	if b.cond != nil {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}

func (b *Backend) Quit() {
	ohosaudio.ClearBridge(b)
	if b.IsMainThread() {
		b.syncOwnedClipboardToHost()
	}
	b.mu.Lock()
	sendQuit := !b.quitSent && b.conn != nil
	b.quitSent = true
	b.quit = true
	for id, ch := range b.secretWaiters {
		delete(b.secretWaiters, id)
		close(ch)
	}
	for id, ch := range b.hotkeyWaiters {
		delete(b.hotkeyWaiters, id)
		close(ch)
	}
	if b.cond != nil {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
	if sendQuit {
		_ = b.conn.write(kindQuit, nil)
	}
	b.conn.close()
}

func (b *Backend) NewWindow(opts *platform.WindowOptions, h platform.WindowHandler) (platform.Window, error) {
	b.mu.Lock()
	b.nextWinID++
	id := b.nextWinID
	scale := b.primaryScaleLocked()
	b.mu.Unlock()

	w := newWindow(b, id, scale, opts, h)

	b.mu.Lock()
	if b.windows == nil {
		b.windows = make(map[uint32]*Window)
	}
	b.windows[id] = w
	b.winOrder = append(b.winOrder, id)
	b.mu.Unlock()

	b.send(kindWinCreate, winConfigMessage(w.wireConfig(w.parentID)))
	if w.isWeb {
		if w.needsWebSetup() {
			w.sendWebSetup(false)
		}
		if w.ua != "" {
			w.sendWebReq(webOpSetUA, w.ua, "")
		}
		if w.zoom > 0 && w.zoom != 1 {
			w.sendWebReq(webOpSetZoom, strconv.FormatFloat(w.zoom, 'f', -1, 64), "")
		}
	}
	return w, nil
}

func (b *Backend) primaryScaleLocked() float64 {
	for _, d := range b.displays {
		if d.Primary && d.ScaleFactor > 0 {
			return d.ScaleFactor
		}
	}
	if len(b.displays) > 0 && b.displays[0].ScaleFactor > 0 {
		return b.displays[0].ScaleFactor
	}
	if b.initScale > 0 {
		return b.initScale
	}
	return 1
}

func (b *Backend) windowByID(id uint32) *Window {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == 0 {
		return b.focusedOrFirstWindowLocked()
	}
	return b.windows[id]
}

func (b *Backend) removeWindow(id uint32) {
	b.mu.Lock()
	delete(b.windows, id)
	b.winOrder = slices.DeleteFunc(b.winOrder, func(x uint32) bool { return x == id })
	if b.cond != nil {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}

func (b *Backend) hasFocusedWindowLocked() bool {
	for _, id := range b.winOrder {
		if w := b.windows[id]; w != nil && w.host == nil && !w.closed && w.focused {
			return true
		}
	}
	return false
}

func (b *Backend) focusedOrFirstWindowLocked() *Window {
	for i := len(b.winOrder) - 1; i >= 0; i-- {
		if w := b.windows[b.winOrder[i]]; w != nil && w.host == nil && !w.closed && w.focused {
			return w
		}
	}
	for i := len(b.winOrder) - 1; i >= 0; i-- {
		if w := b.windows[b.winOrder[i]]; w != nil && w.host == nil && !w.closed {
			return w
		}
	}
	return nil
}

func (b *Backend) send(kind byte, payload []byte) {
	if b.conn == nil {
		return
	}
	_ = b.conn.write(kind, payload)
}

type event struct {
	kind    byte
	payload []byte
	fd      int
	fn      func()
}

func (b *Backend) post(fn func()) {
	if b == nil || fn == nil {
		return
	}
	b.mu.Lock()
	b.events = append(b.events, event{kind: kindInternal, fd: -1, fn: fn})
	if b.cond != nil {
		b.cond.Broadcast()
	}
	b.mu.Unlock()
}

func (b *Backend) readLoop() {
	for {
		kind, payload, fd, err := b.conn.read()
		if err != nil {
			b.mu.Lock()
			b.quitSent = true
			b.mu.Unlock()
			b.Quit()
			return
		}
		if kind == kindSecretRes {
			b.handleSecretRes(payload)
			continue
		}
		if kind == kindHotkeyRes {
			b.handleHotkeyRes(payload)
			continue
		}
		b.mu.Lock()
		b.events = append(b.events, event{kind: kind, payload: payload, fd: fd})
		b.cond.Broadcast()
		b.mu.Unlock()
	}
}

func (b *Backend) dispatch(ev event) {
	switch ev.kind {
	case kindQuit:
		b.mu.Lock()
		b.quitSent = true
		b.mu.Unlock()
		b.Quit()
	case kindSurface:
		b.onSurface(ev.payload)
	case kindFrame:
		b.onFrame(ev.payload, ev.fd)
	case kindInput:
		b.onInput(ev.payload)
	case kindEnvInfo:
		b.onEnvInfo(ev.payload)
	case kindPopupResult:
		b.onPopupResult(ev.payload)
	case kindClipChanged:
		b.onClipChanged(ev.payload)
	case kindClipData:
		b.onClipData(ev.payload)
	case kindDialogResult:
		b.onDialogResult(ev.payload)
	case kindShellResult:
		b.onShellResult(ev.payload)
	case kindAppOpen:
		b.onAppOpen(ev.payload)
	case kindMediaCmd:
		b.onMediaCmd(ev.payload)
	case kindNotifResult:
		b.onNotifResult(ev.payload)
	case kindNotifClicked:
		b.onNotifClicked(ev.payload)
	case kindPowerState:
		b.onPowerState(ev.payload)
	case kindNetState:
		b.onNetState(ev.payload)
	case kindCookieResult:
		b.onCookieResult(ev.payload)
	case kindWebEvent:
		b.onWebEvent(ev.payload)
	case kindWebAsyncRes:
		b.onWebAsyncRes(ev.payload)
	case kindSchemeReq:
		b.onSchemeReq(ev.payload)
	case kindHotkeyTrig:
		b.onHotkeyTrig(ev.payload)
	case kindTrayEvent:
		b.onTrayEvent(ev.payload)
	case kindDragEvent:
		b.onDragEvent(ev.payload)
	case kindAccessEvent:
		b.onAccessEvent(ev.payload)
	case kindInternal:
		if ev.fn != nil {
			ev.fn()
		}
	}
}

// onSurface takes the size the host reports for winID, in device pixels and DIPs.
// Identical geometry updates the refresh rate without discarding swapchain
// damage history or firing a redundant SurfaceResize.
func (b *Backend) onSurface(payload []byte) {
	if len(payload) < 16 {
		return
	}
	winID := binary.LittleEndian.Uint32(payload[0:])
	w := int(binary.LittleEndian.Uint32(payload[4:]))
	h := int(binary.LittleEndian.Uint32(payload[8:]))
	scale := float64(binary.LittleEndian.Uint32(payload[12:])) / 1000
	var hz float64
	if len(payload) >= 20 {
		hz = float64(binary.LittleEndian.Uint32(payload[16:])) / 1000
	}

	win := b.windowByID(winID)
	if win == nil || win.closed || win.surface == nil {
		return
	}
	s := win.surface
	s.mu.Lock()
	if hz > 0 {
		s.hz = hz
	}
	same := s.width == w && s.height == h && math.Abs(s.scale-scale) < 1e-6
	if !same {
		s.width, s.height, s.scale = w, h, scale
		s.history = s.history[:0]
		for _, buf := range s.buffers {
			buf.lastSeq = 0
		}
	}
	s.mu.Unlock()
	if same {
		return
	}
	win.updateBoundsFromPixels(w, h, scale)
	s.refreshPlacedWebViews()
	s.send(platform.SurfaceEvent{Kind: platform.SurfaceResize})
	if win.h != nil {
		win.h.Resized()
	}
}

// onFrame maps the buffer the host handed over for winID and asks the content
// to draw into it. Unused buffers are returned via kindAbort so the host never
// stalls waiting on them.
func (b *Backend) onFrame(payload []byte, fd int) {
	if fd < 0 || len(payload) < 28 {
		if fd >= 0 {
			syscall.Close(fd)
		}
		return
	}
	winID := binary.LittleEndian.Uint32(payload[0:])
	seq := binary.LittleEndian.Uint32(payload[4:])
	id := binary.LittleEndian.Uint32(payload[8:])
	w := int(binary.LittleEndian.Uint32(payload[12:]))
	h := int(binary.LittleEndian.Uint32(payload[16:]))
	stride := int(binary.LittleEndian.Uint32(payload[20:]))
	size := int(binary.LittleEndian.Uint32(payload[24:]))

	win := b.windowByID(winID)
	if win == nil || win.closed || win.surface == nil {
		syscall.Close(fd)
		b.send(kindAbort, abortMessage(winID, seq))
		return
	}

	bufs := win.surface
	bufs.mu.Lock()
	buf, err := bufs.bufferFor(id, w, h, stride, size, fd)
	hz := bufs.hz
	if err != nil {
		bufs.waiting = false
	}
	bufs.mu.Unlock()
	syscall.Close(fd)
	if err != nil {
		b.send(kindAbort, abortMessage(winID, seq))
		return
	}

	bufs.mu.Lock()
	bufs.cur = buf
	bufs.curSeq = seq
	bufs.waiting = false
	bufs.inFrame = true
	bufs.wantNext = false
	bufs.mu.Unlock()

	if bufs.touch.fling.active {
		dt := 1.0 / 120.0
		if hz > 0 {
			dt = 1.0 / hz
		}
		if ev, ok := bufs.touch.stepFling(dt); ok {
			bufs.send(ev)
		}
	}
	bufs.send(platform.SurfaceEvent{Kind: platform.SurfaceFrame, Paced: true})

	bufs.mu.Lock()
	bufs.inFrame = false
	abort := bufs.cur != nil
	bufs.cur = nil
	askNext := bufs.wantNext
	bufs.wantNext = false
	bufs.mu.Unlock()

	if abort {
		b.send(kindAbort, abortMessage(winID, seq))
	}
	if askNext || bufs.touch.fling.active {
		bufs.RequestFrame()
	}
}

// bufferFor returns the mapping of a buffer of the ring, making it the
// first time the host hands that one over and evicting stale mappings when
// the swapchain reallocates.
func (s *Surface) bufferFor(id uint32, w, h, stride, size, fd int) (*buffer, error) {
	if b, ok := s.buffers[id]; ok && b.size == size && b.w == w && b.h == h && b.stride == stride {
		return b, nil
	}
	if b, ok := s.buffers[id]; ok {
		syscall.Munmap(b.mem)
		delete(s.buffers, id)
	}
	for len(s.buffers) >= 8 {
		var victimID uint32
		var victim *buffer
		for k, cand := range s.buffers {
			if cand == s.cur {
				continue
			}
			if victim == nil || cand.lastUsed < victim.lastUsed {
				victimID, victim = k, cand
			}
		}
		if victim == nil {
			break
		}
		syscall.Munmap(victim.mem)
		delete(s.buffers, victimID)
	}
	mem, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("ohos: mmap %d bytes: %w", size, err)
	}
	b := &buffer{id: id, mem: mem, stride: stride, w: w, h: h, size: size}
	if s.buffers == nil {
		s.buffers = make(map[uint32]*buffer)
	}
	s.buffers[id] = b
	return b, nil
}
