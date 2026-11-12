//go:build linux && (amd64 || arm64)

package ohos

import (
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/internal/platform"
)

// Window is a HarmonyOS window backed by either an OHNativeWindow surface or
// a standalone ArkWeb view in the host process: Window 1 is the UIAbility's
// main window, and subsequent windows are freeform or modal subwindows on
// WindowStage. When host != nil, Window stands for an embedded ArkWeb view
// created via Surface.NewWebView under host.surface.
type Window struct {
	b            *Backend
	id           uint32
	parentID     uint32
	host         *Window
	webViews     []*Window
	surface      *Surface
	isWeb        bool
	webIncognito bool
	h            platform.WindowHandler
	opts         platform.WindowOptions

	title        string
	visible      bool
	minimized    bool
	maximized    bool
	fullscreen   bool
	focused      bool
	alwaysOnTop  bool
	hasShadow    bool
	closed       bool
	zoom         float64
	ua           string
	currentURL   string
	loading      bool
	canBack      bool
	canForward   bool
	audible      bool
	muted        bool
	devToolsOpen bool
	bounds       platform.Rect
	minSize      platform.Size
	maxSize      platform.Size
	bgColor      platform.Color
	hasBg        bool
	rawTitleBar  platform.TitleBar
	hasTitleBar  bool
	flyout       platform.Flyout
	flyoutSize   platform.Size
	flyoutPlaced bool
	dismissing   bool
}

func newWindow(b *Backend, id uint32, scale float64, opts *platform.WindowOptions, h platform.WindowHandler) *Window {
	if scale <= 0 {
		scale = 1
	}
	w := &Window{
		b:         b,
		id:        id,
		h:         h,
		focused:   id == 1,
		hasShadow: true,
		zoom:      1,
		opts: platform.WindowOptions{
			Resizable:   true,
			Movable:     true,
			Minimizable: true,
			Maximizable: true,
			Closable:    true,
			HasShadow:   true,
			Surface:     true,
		},
	}
	if opts != nil {
		w.opts = *opts
		w.isWeb = !opts.Surface
		if ds, ok := opts.DataStore.(*dataStore); ok && ds != nil && ds.ephemeral {
			w.webIncognito = true
		}
		w.ua = strings.TrimSpace(opts.UserAgent)
		if suf := strings.TrimSpace(opts.UserAgentSuffix); suf != "" {
			if w.ua != "" {
				w.ua += " " + suf
			} else {
				w.ua = suf
			}
		}
		if opts.Zoom > 0 {
			w.zoom = opts.Zoom
		}
		w.devToolsOpen = opts.DevTools
		w.title = opts.Title
		w.minSize = opts.MinSize
		w.maxSize = opts.MaxSize
		w.alwaysOnTop = opts.AlwaysOnTop
		w.maximized = opts.Maximized
		w.fullscreen = opts.FullScreen
		w.hasShadow = opts.HasShadow
		if opts.BackgroundColor != nil {
			w.bgColor = *opts.BackgroundColor
			w.hasBg = true
		}
		if pw, ok := opts.Parent.(*Window); ok && pw != nil {
			w.parentID = pw.id
		}
		w.bounds = platform.Rect{
			X:      opts.X,
			Y:      opts.Y,
			Width:  opts.Width,
			Height: opts.Height,
		}
		if opts.Flyout != nil {
			w.opts.Frameless = true
			w.opts.Resizable = false
			w.opts.Minimizable = false
			w.opts.Maximizable = false
			w.opts.Center = false
			w.alwaysOnTop = true
			sz := platform.Size{
				Width:  max(opts.Width, 1),
				Height: max(opts.Height, 1),
			}
			w.flyout = *opts.Flyout
			w.flyoutSize = sz
			if r, ok := w.resolveFlyoutBounds(*opts.Flyout, sz); ok {
				w.bounds = r
				w.flyoutPlaced = true
			}
		}
	}

	pxW, pxH := 0, 0
	if id == 1 && b.initW > 0 && b.initH > 0 {
		pxW, pxH = b.initW, b.initH
		if b.initScale > 0 {
			scale = b.initScale
		}
		w.updateBoundsFromPixels(pxW, pxH, scale)
	} else {
		dipW := max(w.bounds.Width, 1)
		dipH := max(w.bounds.Height, 1)
		if w.bounds.Width <= 0 {
			w.bounds.Width = dipW
		}
		if w.bounds.Height <= 0 {
			w.bounds.Height = dipH
		}
		pxW = max(1, int(math.Round(float64(dipW)*scale)))
		pxH = max(1, int(math.Round(float64(dipH)*scale)))
	}

	if !w.isWeb {
		w.surface = &Surface{
			w:      w,
			width:  pxW,
			height: pxH,
			scale:  scale,
			hz:     120,
		}
	}
	return w
}

func (w *Window) hiddenTitleBar() bool {
	return !w.opts.Frameless && (w.opts.TitleBarStyle == "hidden" || w.opts.TitleBarStyle == "hiddenInset")
}

func (w *Window) styleFlags() uint32 {
	var f uint32
	if w.opts.Resizable {
		f |= winFlagResizable
	}
	if w.opts.Minimizable {
		f |= winFlagMinimizable
	}
	if w.opts.Maximizable {
		f |= winFlagMaximizable
	}
	if w.opts.Closable {
		f |= winFlagClosable
	}
	if w.opts.Modal {
		f |= winFlagModal
	}
	if w.hiddenTitleBar() {
		f |= winFlagHiddenTitleBar
	}
	if w.opts.HideWindowButtons {
		f |= winFlagHideWinButtons
	}
	if w.alwaysOnTop {
		f |= winFlagAlwaysOnTop
	}
	if w.maximized {
		f |= winFlagMaximized
	}
	if w.fullscreen {
		f |= winFlagFullScreen
	}
	if w.opts.Center {
		f |= winFlagCenter
	}
	if w.opts.Frameless {
		f |= winFlagFrameless
	}
	if w.hasBg {
		f |= winFlagHasBg
	}
	if w.opts.Movable {
		f |= winFlagMovable
	}
	if w.isWeb {
		f |= winFlagWebView
		if w.webIncognito {
			f |= winFlagWebIncognito
		}
	}
	if w.opts.Transparent {
		f |= winFlagTransparent
	}
	return f
}

func (w *Window) packedBgRGBA() uint32 {
	return uint32(w.bgColor.R)<<24 | uint32(w.bgColor.G)<<16 | uint32(w.bgColor.B)<<8 | uint32(w.bgColor.A)
}

func (w *Window) wireConfig(parentOrMask uint32) winConfigWire {
	return winConfigWire{
		winID:          w.id,
		parentOrMask:   parentOrMask,
		x:              int32(w.bounds.X),
		y:              int32(w.bounds.Y),
		w:              int32(w.bounds.Width),
		h:              int32(w.bounds.Height),
		minW:           int32(w.minSize.Width),
		minH:           int32(w.minSize.Height),
		maxW:           int32(w.maxSize.Width),
		maxH:           int32(w.maxSize.Height),
		flags:          w.styleFlags(),
		titleBarHeight: int32(w.opts.TitleBarHeight),
		bgRGBA:         w.packedBgRGBA(),
		title:          w.title,
	}
}

func (w *Window) sendConfig(mask uint32) {
	if w.closed || w.b == nil {
		return
	}
	w.b.send(kindWinConfig, winConfigMessage(w.wireConfig(mask)))
}

func (w *Window) sendAction(act byte) {
	if w.closed || w.b == nil {
		return
	}
	w.b.send(kindWinAction, winActionMessage(w.id, act))
}

func (w *Window) updateBoundsFromPixels(pxW, pxH int, scale float64) {
	if pxW <= 0 || pxH <= 0 {
		return
	}
	if scale <= 0 {
		scale = 1
	}
	w.bounds.Width = int(math.Round(float64(pxW) / scale))
	w.bounds.Height = int(math.Round(float64(pxH) / scale))
}

func (w *Window) Handle() uintptr { return uintptr(w.id) }
func (w *Window) Surface() platform.Surface {
	if w.surface == nil {
		return nil
	}
	return w.surface
}

func (w *Window) SetTitle(title string) {
	if w.title == title {
		return
	}
	w.title = title
	w.sendConfig(cfgMaskTitle)
}

func (w *Window) Title() string             { return w.title }
func (w *Window) SetRepresentedFile(string) {}
func (w *Window) SetDocumentEdited(bool)    {}
func (w *Window) Bounds() platform.Rect     { return w.bounds }

func (w *Window) SetBounds(r platform.Rect) {
	w.bounds = r
	w.opts.Center = false
	w.sendConfig(cfgMaskBounds)
}

func (w *Window) ContentBounds() platform.Rect { return w.bounds }

func (w *Window) SetContentBounds(r platform.Rect) {
	w.SetBounds(r)
}

func (w *Window) SetMinimumSize(s platform.Size) {
	if w.minSize == s {
		return
	}
	w.minSize = s
	w.sendConfig(cfgMaskMinSize)
}

func (w *Window) SetMaximumSize(s platform.Size) {
	if w.maxSize == s {
		return
	}
	w.maxSize = s
	w.sendConfig(cfgMaskMaxSize)
}

func (w *Window) SetResizable(v bool) {
	if w.opts.Resizable == v {
		return
	}
	w.opts.Resizable = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsResizable() bool { return w.opts.Resizable }

func (w *Window) SetMovable(v bool) {
	if w.opts.Movable == v {
		return
	}
	w.opts.Movable = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsMovable() bool { return w.opts.Movable }

func (w *Window) SetMinimizable(v bool) {
	if w.opts.Minimizable == v {
		return
	}
	w.opts.Minimizable = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsMinimizable() bool { return w.opts.Minimizable }

func (w *Window) SetMaximizable(v bool) {
	if w.opts.Maximizable == v {
		return
	}
	w.opts.Maximizable = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsMaximizable() bool { return w.opts.Maximizable }

func (w *Window) SetClosable(v bool) {
	if w.opts.Closable == v {
		return
	}
	w.opts.Closable = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsClosable() bool { return w.opts.Closable }

func (w *Window) SetAlwaysOnTop(v bool) {
	if w.alwaysOnTop == v {
		return
	}
	w.alwaysOnTop = v
	w.sendConfig(cfgMaskFlags)
}

func (w *Window) IsAlwaysOnTop() bool { return w.alwaysOnTop }

func (w *Window) showSurface() {
	s := w.surface
	if s == nil {
		return
	}
	s.mu.Lock()
	wasHidden := s.hostHidden
	s.hostHidden = false
	s.mu.Unlock()
	if wasHidden {
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceShown})
	}
}

func (w *Window) hideSurface() {
	s := w.surface
	if s == nil {
		return
	}
	s.mu.Lock()
	wasHidden := s.hostHidden
	s.hostHidden = true
	s.mu.Unlock()
	if !wasHidden {
		s.touch.cancelFling()
		if s.pointerIn {
			s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
		}
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceHidden})
	}
}

func (w *Window) Show() {
	if w.closed {
		return
	}
	w.visible = true
	w.minimized = false
	w.showSurface()
	w.sendAction(winActShow)
}

func (w *Window) ShowInactive() {
	if w.closed {
		return
	}
	w.visible = true
	w.minimized = false
	w.showSurface()
	w.sendAction(winActShowInactive)
}

func (w *Window) Hide() {
	if w.closed {
		return
	}
	w.visible = false
	w.hideSurface()
	w.sendAction(winActHide)
}

func (w *Window) IsVisible() bool { return w.visible }

func (w *Window) Focus() {
	if w.closed {
		return
	}
	w.focused = true
	if w.host != nil {
		w.sendWebReq(webOpFocus, "", "")
		return
	}
	w.sendAction(winActFocus)
}

func (w *Window) Blur() {
	if w.closed {
		return
	}
	w.focused = false
	w.sendAction(winActBlur)
}

func (w *Window) IsFocused() bool { return w.focused }

func (w *Window) Minimize() {
	if w.closed {
		return
	}
	w.minimized = true
	w.sendAction(winActMinimize)
}

func (w *Window) IsMinimized() bool { return w.minimized }

func (w *Window) Maximize() {
	if w.closed {
		return
	}
	w.maximized = true
	w.minimized = false
	w.sendAction(winActMaximize)
}

func (w *Window) Unmaximize() {
	if w.closed {
		return
	}
	w.maximized = false
	w.sendAction(winActUnmaximize)
}

func (w *Window) IsMaximized() bool { return w.maximized }

func (w *Window) Restore() {
	if w.closed {
		return
	}
	w.visible = true
	w.minimized = false
	w.maximized = false
	w.showSurface()
	w.sendAction(winActRestore)
}

func (w *Window) SetFullScreen(v bool) {
	if w.closed || w.fullscreen == v {
		return
	}
	prevTB := w.TitleBar()
	w.fullscreen = v
	if v {
		w.sendAction(winActEnterFullScreen)
	} else {
		w.sendAction(winActExitFullScreen)
	}
	if w.TitleBar() != prevTB && w.h != nil {
		w.h.TitleBarChanged()
	}
}

func (w *Window) IsFullScreen() bool { return w.fullscreen }

func (w *Window) Center() {
	w.sendAction(winActCenter)
}

func (w *Window) SetBackgroundColor(c platform.Color) {
	if w.hasBg && w.bgColor == c {
		return
	}
	w.bgColor = c
	w.hasBg = true
	if w.host != nil {
		w.sendWebReq(webOpSetBg, strconv.FormatUint(uint64(w.packedBgRGBA()), 10), "")
		return
	}
	w.sendConfig(cfgMaskBg)
}

func (w *Window) SetOpacity(float64)              {}
func (w *Window) Opacity() float64                { return 1 }
func (w *Window) SetHasShadow(v bool)             { w.hasShadow = v }
func (w *Window) HasShadow() bool                 { return w.hasShadow }
func (w *Window) SetIgnoreMouseEvents(bool)       {}
func (w *Window) SetContentProtection(bool)       {}
func (w *Window) SetVibrancy(string)              {}
func (w *Window) SetVibrancyTint(*platform.Color) {}
func (w *Window) SetActiveAppearance(bool)        {}
func (w *Window) SetWindowButtonsTheme(string)    {}
func (w *Window) SetProgressBar(string, float64)  {}
func (w *Window) FlashFrame(bool)                 {}
func (w *Window) SetSkipTaskbar(bool)             {}
func (w *Window) SetVisibleOnAllWorkspaces(bool)  {}
func (w *Window) SetIcon([]byte) error            { return platform.ErrUnsupported }
func (w *Window) SetMenu(*platform.Menu)          {}
func (w *Window) SetAutoHideMenu(bool)            {}

func (w *Window) TitleBar() platform.TitleBar {
	if !w.hiddenTitleBar() || w.opts.HideWindowButtons || w.fullscreen {
		return platform.TitleBar{}
	}
	if w.hasTitleBar {
		tb := w.rawTitleBar
		if w.opts.TitleBarHeight > tb.Height {
			tb.Height = w.opts.TitleBarHeight
		}
		return tb
	}
	return platform.TitleBar{
		Height: max(36, w.opts.TitleBarHeight),
		Right:  128,
	}
}

func (w *Window) StartDrag() {
	if !w.opts.Movable || w.fullscreen {
		return
	}
	w.sendAction(winActStartDrag)
}

func (w *Window) TitleBarDoubleClicked() {
	if w.IsMaximized() {
		w.Unmaximize()
	} else if w.IsMaximizable() {
		w.Maximize()
	}
}

func (w *Window) hasModalChild() bool {
	if w.b == nil {
		return false
	}
	w.b.mu.Lock()
	defer w.b.mu.Unlock()
	for _, x := range w.b.windows {
		if !x.closed && x.visible && x.parentID == w.id && x.opts.Modal {
			return true
		}
	}
	return false
}

func (w *Window) resolveFlyoutBounds(f platform.Flyout, size platform.Size) (platform.Rect, bool) {
	size.Width = max(size.Width, 1)
	size.Height = max(size.Height, 1)
	var displays []platform.Display
	if w.b != nil {
		displays = w.b.Screen().Displays()
	}
	if f.Tray != nil {
		if tb := f.Tray.Bounds(); tb.Width > 0 && tb.Height > 0 {
			work := platform.WorkAreaFor(tb, displays)
			if work.Width <= 0 || work.Height <= 0 {
				work = platform.Rect{X: -100000, Y: -100000, Width: 200000, Height: 200000}
			}
			r, _, _ := f.Place(tb, size, work)
			return r, true
		}
		work := platform.PrimaryWorkArea(displays)
		if work.Width <= 0 || work.Height <= 0 {
			return platform.Rect{Width: size.Width, Height: size.Height}, true
		}
		return platform.Centered(size, work), true
	}
	if w.parentID == 0 || w.b == nil {
		return platform.Rect{}, false
	}
	pw := w.b.windowByID(w.parentID)
	if pw == nil || pw.closed {
		return platform.Rect{}, false
	}
	anchor := f.Anchor
	anchor.X += pw.bounds.X
	anchor.Y += pw.bounds.Y
	work := platform.WorkAreaFor(anchor, displays)
	if work.Width <= 0 || work.Height <= 0 {
		work = platform.Rect{X: -100000, Y: -100000, Width: 200000, Height: 200000}
	}
	r, _, _ := f.Place(anchor, size, work)
	return r, true
}

func (w *Window) PlaceFlyout(f platform.Flyout, size platform.Size) {
	if w.closed || w.opts.Flyout == nil {
		return
	}
	r, ok := w.resolveFlyoutBounds(f, size)
	if !ok {
		return
	}
	same := w.flyoutPlaced && f == w.flyout && size == w.flyoutSize && r == w.bounds
	w.flyout = f
	w.flyoutSize = size
	w.flyoutPlaced = true
	if !same {
		w.SetBounds(r)
	}
}

// Close destroys the window without asking ShouldClose.
func (w *Window) Close() {
	w.closeInternal(true)
}

func (w *Window) closeInternal(notifyHost bool) {
	if w.closed {
		return
	}
	if len(w.webViews) > 0 {
		children := slices.Clone(w.webViews)
		w.webViews = nil
		for _, child := range children {
			child.Close()
		}
	}
	if w.host != nil {
		if notifyHost && w.b != nil {
			w.b.send(kindWebReq, encodeWebReqMessage(w.id, webOpDestroy, "", ""))
		}
		w.closed = true
		w.focused = false
		w.visible = false
		w.host.webViews = slices.DeleteFunc(w.host.webViews, func(x *Window) bool { return x == w })
		if w.b != nil {
			w.b.removeWindow(w.id)
		}
		if !w.host.closed && w.host.surface != nil {
			w.host.surface.dropClosedWebView(w)
		}
		return
	}
	if notifyHost {
		w.sendAction(winActClose)
	}
	w.closed = true
	w.focused = false
	w.visible = false
	if w.surface != nil {
		w.surface.cleanupDrag()
		w.surface.Idle()
		w.surface.ReleaseGL()
	}
	if w.b != nil {
		w.b.removeWindow(w.id)
	}
	if w.h != nil {
		w.h.Closed()
	}
}

// The page half. Surface windows have no web view; standalone web windows
// forward navigation and script requests to their ArkWeb instance in the host.

func (w *Window) sendWebReq(op byte, arg1, arg2 string) {
	if !w.isWeb || w.closed || w.b == nil {
		return
	}
	w.b.send(kindWebReq, encodeWebReqMessage(w.id, op, arg1, arg2))
}

func (w *Window) WebViewHandle() uintptr {
	if w.isWeb {
		return uintptr(w.id)
	}
	return 0
}

func (w *Window) LoadURL(raw string) {
	if !w.isWeb || w.closed {
		return
	}
	w.currentURL = raw
	w.loading = true
	w.sendWebReq(webOpLoadURL, raw, "")
}

func (w *Window) LoadHTML(html, baseURL string) {
	if !w.isWeb || w.closed {
		return
	}
	if baseURL != "" {
		w.currentURL = baseURL
	}
	w.loading = true
	w.sendWebReq(webOpLoadHTML, html, baseURL)
}

func (w *Window) LoadFile(path, _ string) {
	if !w.isWeb || w.closed || path == "" {
		return
	}
	u := (&url.URL{Scheme: "file", Path: path}).String()
	w.LoadURL(u)
}

func (w *Window) Reload(ignoreCache bool) {
	if !w.isWeb || w.closed {
		return
	}
	w.loading = true
	arg := ""
	if ignoreCache {
		arg = "1"
	}
	w.sendWebReq(webOpReload, arg, "")
}

func (w *Window) StopLoading() {
	if !w.isWeb || w.closed {
		return
	}
	w.loading = false
	w.sendWebReq(webOpStop, "", "")
}

func (w *Window) GoBack() {
	w.sendWebReq(webOpBack, "", "")
}

func (w *Window) GoForward() {
	w.sendWebReq(webOpForward, "", "")
}

func (w *Window) CanGoBack() bool    { return w.canBack }
func (w *Window) CanGoForward() bool { return w.canForward }
func (w *Window) URL() string        { return w.currentURL }
func (w *Window) IsLoading() bool    { return w.loading }

func (w *Window) Eval(js string) {
	w.sendWebReq(webOpEval, js, "")
}

func (w *Window) SetContentRules([]any) {}

func (w *Window) SetUserAgent(ua string) {
	w.ua = ua
	w.sendWebReq(webOpSetUA, ua, "")
}

func (w *Window) UserAgent() string    { return w.ua }
func (w *Window) ProcessID() int       { return 0 }
func (w *Window) GPUProcessID() int    { return 0 }
func (w *Window) IsAudioMuted() bool   { return w.muted }
func (w *Window) CaptureState(platform.CaptureKind) platform.CaptureState {
	return platform.CaptureNone
}
func (w *Window) SetCaptureState(platform.CaptureKind, platform.CaptureState) {}
func (w *Window) TogglePictureInPicture()                                     {}
func (w *Window) PictureInPictureActive(cb func(bool))                        { cb(false) }
func (w *Window) SessionState() []byte                                        { return nil }
func (w *Window) RestoreSessionState([]byte) bool                             { return false }
func (w *Window) DroppedFiles() []string                                      { return nil }
func (w *Window) Print()                                                      {}
