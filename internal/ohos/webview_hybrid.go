//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/egoist/mygo/internal/bridge"
	"github.com/egoist/mygo/internal/platform"
)

type webSetupWire struct {
	webID     uint32
	hostWinID uint32
	flags     uint32
	bgRGBA    uint32
	zoom      float64
	ua        string
	schemes   []string
	scripts   []platform.UserScript
}

type placedWebViewWire struct {
	webID   uint32
	frame   platform.RectF
	clip    platform.RectF
	pageW   float64
	pageH   float64
	radii   [4]float64
}

type placeWebViewsWire struct {
	winID          uint32
	views          []placedWebViewWire
	surfaceRegions []platform.RectF
}

type webAsyncReqWire struct {
	reqID   uint32
	webID   uint32
	op      byte
	body    string
	pdfOpts platform.PDFOptions
}

type webAsyncResWire struct {
	reqID  uint32
	text   string
	errStr string
	data   []byte
}

type pendingWebAsyncCall struct {
	strDone   func(string, error)
	bytesDone func([]byte, error)
}

type schemeReqWire struct {
	reqID   uint32
	webID   uint32
	cancel  bool
	method  string
	url     string
	headers [][2]string
	body    []byte
}

type schemeResWire struct {
	reqID   uint32
	op      byte
	status  int32
	headers [][2]string
	chunk   []byte
	errStr  string
}

type schemeResponder struct {
	b         *Backend
	reqID     uint32
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	responded bool
	done      bool
}

func (s *Surface) NewWebView(opts *platform.WindowOptions, h platform.WindowHandler) (platform.WebView, error) {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil {
		return nil, platform.ErrUnsupported
	}
	b := s.w.b
	b.mu.Lock()
	b.nextWinID++
	id := b.nextWinID
	scale := b.primaryScaleLocked()
	b.mu.Unlock()

	if scale <= 0 {
		scale = 1
	}

	wv := &Window{
		b:         b,
		id:        id,
		host:      s.w,
		isWeb:     true,
		h:         h,
		zoom:      1,
		hasShadow: false,
	}
	if opts != nil {
		wv.opts = *opts
		if ds, ok := opts.DataStore.(*dataStore); ok && ds != nil && ds.ephemeral {
			wv.webIncognito = true
		}
		wv.ua = strings.TrimSpace(opts.UserAgent)
		if suf := strings.TrimSpace(opts.UserAgentSuffix); suf != "" {
			if wv.ua != "" {
				wv.ua += " " + suf
			} else {
				wv.ua = suf
			}
		}
		if opts.Zoom > 0 {
			wv.zoom = opts.Zoom
		}
		wv.devToolsOpen = opts.DevTools
		if opts.BackgroundColor != nil {
			wv.bgColor = *opts.BackgroundColor
			wv.hasBg = true
		}
	}

	b.mu.Lock()
	if b.windows == nil {
		b.windows = make(map[uint32]*Window)
	}
	b.windows[id] = wv
	b.winOrder = append(b.winOrder, id)
	b.mu.Unlock()

	s.w.webViews = append(s.w.webViews, wv)

	if !s.layered {
		s.layered = true
		s.w.sendAction(winActEnableWebLayer)
	}

	wv.sendWebSetup(false)
	if wv.ua != "" {
		wv.sendWebReq(webOpSetUA, wv.ua, "")
	}
	if wv.zoom > 0 && wv.zoom != 1 {
		wv.sendWebReq(webOpSetZoom, strconv.FormatFloat(wv.zoom, 'f', -1, 64), "")
	}
	return wv, nil
}

func (s *Surface) PlaceWebViews(views []platform.WebViewPlacement) {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil {
		return
	}
	if len(views) == 0 && len(s.placed) == 0 && len(s.w.webViews) == 0 {
		return
	}
	kept := make([]platform.WebViewPlacement, 0, len(views))
	for _, p := range views {
		wv, ok := p.WebView.(*Window)
		if !ok || wv == nil || wv.closed || wv.host != s.w {
			continue
		}
		p.Covers = slices.Clone(p.Covers)
		kept = append(kept, p)
	}
	if placementsEqual(s.placed, kept) {
		return
	}
	s.placed = kept
	s.sendPlacedWebViews(kept)
}

func (s *Surface) refreshPlacedWebViews() {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil {
		return
	}
	if len(s.placed) == 0 && len(s.w.webViews) == 0 {
		return
	}
	s.sendPlacedWebViews(s.placed)
}

func (s *Surface) dropClosedWebView(closed *Window) {
	if s == nil || len(s.placed) == 0 {
		return
	}
	next := make([]platform.WebViewPlacement, 0, len(s.placed))
	changed := false
	for _, p := range s.placed {
		if wv, ok := p.WebView.(*Window); ok && wv == closed {
			changed = true
			continue
		}
		next = append(next, p)
	}
	if changed {
		s.placed = next
		s.sendPlacedWebViews(next)
	}
}

func (s *Surface) sendPlacedWebViews(views []platform.WebViewPlacement) {
	surfW, surfH, _ := s.Size()
	wireViews := make([]placedWebViewWire, 0, len(views))
	for _, p := range views {
		wv, ok := p.WebView.(*Window)
		if !ok || wv == nil || wv.closed {
			continue
		}
		pw, ph := p.Size()
		wireViews = append(wireViews, placedWebViewWire{
			webID: wv.id,
			frame: p.Frame,
			clip:  p.Clip,
			pageW: pw,
			pageH: ph,
			radii: p.Radii,
		})
	}
	regions := computeSurfaceRegions(surfW, surfH, views)
	s.w.b.send(kindPlaceWebViews, encodePlaceWebViewsMessage(placeWebViewsWire{
		winID:          s.w.id,
		views:          wireViews,
		surfaceRegions: regions,
	}))
}

func placementsEqual(a, b []platform.WebViewPlacement) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].WebView != b[i].WebView ||
			a[i].Frame != b[i].Frame ||
			a[i].Clip != b[i].Clip ||
			a[i].Width != b[i].Width ||
			a[i].Height != b[i].Height ||
			a[i].Radii != b[i].Radii ||
			!slices.Equal(a[i].Covers, b[i].Covers) {
			return false
		}
	}
	return true
}

// computeSurfaceRegions returns the disjoint rectangles in surface DIPs where
// the native surface (rather than an underlying embedded WebView) takes pointer
// hit-testing: the whole surface minus each placed WebView's clip, with each
// WebView's Covers restored on top in paint order.
func computeSurfaceRegions(surfW, surfH float64, views []platform.WebViewPlacement) []platform.RectF {
	if surfW <= 0 || surfH <= 0 {
		return nil
	}
	full := platform.RectF{X: 0, Y: 0, W: surfW, H: surfH}
	regions := []platform.RectF{full}
	for _, p := range views {
		if p.WebView == nil {
			continue
		}
		hole := intersectRectF(p.Clip, full)
		if hole.W <= 0 || hole.H <= 0 {
			continue
		}
		regions = subtractRectF(regions, hole)
		for _, cov := range p.Covers {
			c := intersectRectF(cov, hole)
			if c.W <= 0 || c.H <= 0 {
				continue
			}
			regions = subtractRectF(regions, c)
			regions = append(regions, c)
		}
	}
	return regions
}

func intersectRectF(a, b platform.RectF) platform.RectF {
	x0 := max(a.X, b.X)
	y0 := max(a.Y, b.Y)
	x1 := min(a.X+a.W, b.X+b.W)
	y1 := min(a.Y+a.H, b.Y+b.H)
	if x1 <= x0 || y1 <= y0 {
		return platform.RectF{}
	}
	return platform.RectF{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

func subtractRectF(list []platform.RectF, cut platform.RectF) []platform.RectF {
	if cut.W <= 0 || cut.H <= 0 || len(list) == 0 {
		return list
	}
	cx0, cy0 := cut.X, cut.Y
	cx1, cy1 := cut.X+cut.W, cut.Y+cut.H
	out := make([]platform.RectF, 0, len(list)+3)
	for _, r := range list {
		rx0, ry0 := r.X, r.Y
		rx1, ry1 := r.X+r.W, r.Y+r.H
		if cx1 <= rx0 || cx0 >= rx1 || cy1 <= ry0 || cy0 >= ry1 {
			out = append(out, r)
			continue
		}
		if cy0 > ry0 {
			out = append(out, platform.RectF{X: rx0, Y: ry0, W: rx1 - rx0, H: cy0 - ry0})
		}
		if cy1 < ry1 {
			out = append(out, platform.RectF{X: rx0, Y: cy1, W: rx1 - rx0, H: ry1 - cy1})
		}
		midY0 := max(ry0, cy0)
		midY1 := min(ry1, cy1)
		if midY1 > midY0 {
			if cx0 > rx0 {
				out = append(out, platform.RectF{X: rx0, Y: midY0, W: cx0 - rx0, H: midY1 - midY0})
			}
			if cx1 < rx1 {
				out = append(out, platform.RectF{X: cx1, Y: midY0, W: rx1 - cx1, H: midY1 - midY0})
			}
		}
	}
	return out
}

func (w *Window) needsWebSetup() bool {
	return w.host != nil ||
		len(w.opts.UserScripts) > 0 ||
		len(w.opts.Schemes) > 0 ||
		w.devToolsOpen ||
		w.opts.Autoplay ||
		w.opts.PinchZoom ||
		w.opts.ScrollToContainer ||
		w.hiddenTitleBar()
}

func (w *Window) sendWebSetup(scriptsOnly bool) {
	if !w.isWeb || w.closed || w.b == nil {
		return
	}
	var flags uint32
	if w.webIncognito {
		flags |= webSetupIncognito
	}
	if w.devToolsOpen {
		flags |= webSetupDevTools
	}
	if w.opts.Autoplay {
		flags |= webSetupAutoplay
	}
	if w.opts.PinchZoom {
		flags |= webSetupPinchZoom
	}
	if w.opts.ScrollToContainer {
		flags |= webSetupScrollToContainer
	}
	if w.opts.Transparent {
		flags |= webSetupTransparent
	}
	if w.hasBg {
		flags |= webSetupHasBg
	}
	if scriptsOnly {
		flags |= webSetupScriptsOnly
	}

	var hostWinID uint32
	if w.host != nil {
		hostWinID = w.host.id
	}

	scripts := slices.Clone(w.opts.UserScripts)
	if w.host == nil && w.hiddenTitleBar() {
		scripts = append(scripts, platform.UserScript{
			Source: bridge.TitleBarScript(w.TitleBar(), w.zoom),
		})
	}

	wire := webSetupWire{
		webID:     w.id,
		hostWinID: hostWinID,
		flags:     flags,
		bgRGBA:    w.packedBgRGBA(),
		zoom:      w.zoom,
		ua:        w.ua,
		schemes:   slices.Clone(w.opts.Schemes),
		scripts:   scripts,
	}
	w.b.send(kindWebSetup, encodeWebSetupMessage(wire))
}

func (w *Window) SetUserScripts(scripts []platform.UserScript) {
	w.opts.UserScripts = slices.Clone(scripts)
	w.sendWebSetup(true)
}

func (w *Window) CallAsyncFunction(body string, cb func(string, error)) {
	if !w.isWeb || w.closed || w.b == nil || w.b.conn == nil {
		if cb != nil {
			cb("", platform.ErrUnsupported)
		}
		return
	}
	w.b.startWebAsyncCall(webAsyncReqWire{
		webID: w.id,
		op:    webAsyncEval,
		body:  body,
	}, pendingWebAsyncCall{strDone: cb})
}

func (w *Window) SetZoom(f float64) {
	if f <= 0 {
		return
	}
	w.zoom = f
	w.sendWebReq(webOpSetZoom, strconv.FormatFloat(f, 'f', -1, 64), "")
}

func (w *Window) Zoom() float64 { return w.zoom }

func (w *Window) IsAudible() bool { return w.audible }

func (w *Window) SetAudioMuted(m bool) {
	w.muted = m
	arg := "0"
	if m {
		arg = "1"
	}
	w.sendWebReq(webOpSetMuted, arg, "")
}

func (w *Window) OpenDevTools() {
	w.devToolsOpen = true
	w.sendWebReq(webOpDevTools, "1", "")
}

func (w *Window) CloseDevTools() {
	w.devToolsOpen = false
	w.sendWebReq(webOpDevTools, "0", "")
}

func (w *Window) IsDevToolsOpened() bool { return w.devToolsOpen }

func (w *Window) CapturePage(cb func([]byte, error)) {
	if !w.isWeb || w.closed || w.b == nil || w.b.conn == nil {
		if cb != nil {
			cb(nil, platform.ErrUnsupported)
		}
		return
	}
	w.b.startWebAsyncCall(webAsyncReqWire{
		webID: w.id,
		op:    webAsyncCapture,
	}, pendingWebAsyncCall{bytesDone: cb})
}

func (w *Window) PrintToPDF(opts platform.PDFOptions, cb func([]byte, error)) {
	if !w.isWeb || w.closed || w.b == nil || w.b.conn == nil {
		if cb != nil {
			cb(nil, platform.ErrUnsupported)
		}
		return
	}
	w.b.startWebAsyncCall(webAsyncReqWire{
		webID:   w.id,
		op:      webAsyncPrintPDF,
		pdfOpts: opts,
	}, pendingWebAsyncCall{bytesDone: cb})
}

func (w *Window) TabInto(back bool) {
	if !w.isWeb || w.closed {
		return
	}
	w.focused = true
	arg := "0"
	if back {
		arg = "1"
	}
	w.sendWebReq(webOpTabInto, arg, "")
}

func (b *Backend) startWebAsyncCall(req webAsyncReqWire, cb pendingWebAsyncCall) {
	b.mu.Lock()
	b.nextWebAsyncReq++
	reqID := b.nextWebAsyncReq
	if cb.strDone != nil || cb.bytesDone != nil {
		if b.pendingWebAsync == nil {
			b.pendingWebAsync = make(map[uint32]pendingWebAsyncCall)
		}
		b.pendingWebAsync[reqID] = cb
	}
	b.mu.Unlock()

	req.reqID = reqID
	b.send(kindWebAsyncReq, encodeWebAsyncReqMessage(req))
}

func (b *Backend) onWebAsyncRes(payload []byte) {
	res, ok := decodeWebAsyncResMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	cb, found := b.pendingWebAsync[res.reqID]
	if found {
		delete(b.pendingWebAsync, res.reqID)
	}
	b.mu.Unlock()
	if !found {
		return
	}

	var err error
	if res.errStr != "" {
		err = errors.New(res.errStr)
	}
	if cb.strDone != nil {
		cb.strDone(res.text, err)
	}
	if cb.bytesDone != nil {
		if err != nil {
			cb.bytesDone(nil, err)
		} else {
			cb.bytesDone(res.data, nil)
		}
	}
}

func (b *Backend) onSchemeReq(payload []byte) {
	req, ok := decodeSchemeReqMessage(payload)
	if !ok {
		return
	}
	if req.cancel {
		b.mu.Lock()
		resp, found := b.schemeTasks[req.reqID]
		if found {
			delete(b.schemeTasks, req.reqID)
		}
		b.mu.Unlock()
		if resp != nil {
			resp.cancelFromHost()
		}
		return
	}

	w := b.windowByID(req.webID)
	if w == nil || w.closed || !w.isWeb || w.h == nil {
		b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
			reqID:  req.reqID,
			op:     schemeOpFail,
			errStr: "ohos: webview closed",
		}))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	resp := &schemeResponder{
		b:      b,
		reqID:  req.reqID,
		ctx:    ctx,
		cancel: cancel,
	}
	b.mu.Lock()
	if b.schemeTasks == nil {
		b.schemeTasks = make(map[uint32]*schemeResponder)
	}
	b.schemeTasks[req.reqID] = resp
	b.mu.Unlock()

	hdr := make(map[string][]string, len(req.headers))
	for _, kv := range req.headers {
		k := http.CanonicalHeaderKey(kv[0])
		hdr[k] = append(hdr[k], kv[1])
	}
	var bodyReader io.ReadCloser
	if len(req.body) > 0 {
		bodyReader = io.NopCloser(bytes.NewReader(req.body))
	}
	method := req.method
	if method == "" {
		method = http.MethodGet
	}

	w.h.SchemeRequest(&platform.SchemeRequest{
		Context:   ctx,
		Method:    method,
		URL:       req.url,
		Header:    hdr,
		Body:      bodyReader,
		Responder: resp,
	})
}

func (r *schemeResponder) cancelFromHost() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
	r.cancel()
}

func (r *schemeResponder) dropTask() {
	if r.b == nil {
		return
	}
	r.b.mu.Lock()
	delete(r.b.schemeTasks, r.reqID)
	r.b.mu.Unlock()
}

func (r *schemeResponder) Respond(status int, header map[string][]string) {
	r.mu.Lock()
	if r.done || r.responded {
		r.mu.Unlock()
		return
	}
	r.responded = true
	r.mu.Unlock()

	if status <= 0 {
		status = http.StatusOK
	}
	var flat [][2]string
	for k, vals := range header {
		for _, v := range vals {
			flat = append(flat, [2]string{k, v})
		}
	}
	if r.b != nil {
		r.b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
			reqID:   r.reqID,
			op:      schemeOpHead,
			status:  int32(status),
			headers: flat,
		}))
	}
}

func (r *schemeResponder) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	_ = r.WriteBody(p)
}

func (r *schemeResponder) WriteBody(p []byte) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return context.Canceled
	}
	if !r.responded {
		r.mu.Unlock()
		r.Respond(http.StatusOK, nil)
		r.mu.Lock()
	}
	r.mu.Unlock()

	const maxChunk = 256 << 10
	for len(p) > 0 {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n := min(len(p), maxChunk)
		chunk := bytes.Clone(p[:n])
		p = p[n:]
		if r.b != nil {
			r.b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
				reqID: r.reqID,
				op:    schemeOpBody,
				chunk: chunk,
			}))
		}
	}
	return nil
}

func (r *schemeResponder) Finish() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	needHead := !r.responded
	r.responded = true
	r.done = true
	r.mu.Unlock()

	if needHead && r.b != nil {
		r.b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
			reqID:  r.reqID,
			op:     schemeOpHead,
			status: http.StatusOK,
		}))
	}
	r.dropTask()
	r.cancel()
	if r.b != nil {
		r.b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
			reqID: r.reqID,
			op:    schemeOpFinish,
		}))
	}
}

func (r *schemeResponder) Fail(err error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	r.mu.Unlock()

	r.dropTask()
	r.cancel()
	msg := "scheme request failed"
	if err != nil && err.Error() != "" {
		msg = err.Error()
	}
	if r.b != nil {
		r.b.send(kindSchemeRes, encodeSchemeResMessage(schemeResWire{
			reqID:  r.reqID,
			op:     schemeOpFail,
			errStr: msg,
		}))
	}
}

func (w *Window) onHybridWebEvent(ev byte, errCode int32, text, desc string) {
	switch ev {
	case webEvMessage:
		if w.h != nil {
			w.h.Message(text)
		}
	case webEvAudioState:
		aud := errCode != 0
		if w.audible != aud {
			w.audible = aud
			if w.h != nil {
				w.h.AudibleChanged(aud)
			}
		}
	case webEvWindowNew:
		if w.h != nil {
			target := w.h.NewWindow(platform.NewWindowRequest{
				URL:           text,
				Disposition:   desc,
				UserInitiated: errCode != 0,
			})
			if target != nil && text != "" {
				target.LoadURL(text)
			}
		}
	case webEvWindowClose:
		if w.h != nil {
			w.h.ClosedByPage()
		}
	case webEvRenderExited:
		reason := text
		if reason == "" {
			reason = "crashed"
		}
		if w.h != nil {
			w.h.RenderProcessGone(reason)
		}
	case webEvPermReq:
		allow := false
		if w.h != nil {
			var kinds []string
			for k := range strings.SplitSeq(desc, ",") {
				if k = strings.TrimSpace(k); k != "" {
					kinds = append(kinds, k)
				}
			}
			if len(kinds) > 0 {
				allow = w.h.PermissionRequested(kinds, text)
			}
		}
		ans := "0"
		if allow {
			ans = "1"
		}
		w.sendWebReq(webOpPermReply, strconv.FormatInt(int64(errCode), 10), ans)
	case webEvSchemeDownload:
		if w.h != nil && text != "" {
			w.h.SchemeDownload(text)
		}
	case webEvDownloadStart:
		if w.h == nil || text == "" {
			return
		}
		if w.isCustomSchemeURL(text) {
			w.h.SchemeDownload(text)
			return
		}
		suggested := desc
		if suggested == "" {
			suggested = suggestDownloadFilename(text)
		}
		savePath := w.h.DownloadStarted(text, suggested)
		if savePath == "" {
			return
		}
		w.startURLDownload(text, savePath)
	case webEvTabOut:
		if w.host == nil || w.host.closed || w.host.surface == nil {
			return
		}
		w.focused = false
		w.host.sendAction(winActFocusSurface)
		var mods platform.Modifiers
		if errCode != 0 {
			mods |= platform.ModShift
		}
		w.host.surface.send(platform.SurfaceEvent{
			Kind:    platform.WebViewTabOut,
			WebView: w,
			Mods:    mods,
		})
	case webEvUnhandledKey:
		if w.host == nil || w.host.closed || w.host.surface == nil {
			return
		}
		mods := platform.Modifiers(uint32(errCode) & 0x0F)
		repeat := (uint32(errCode) & 0x10) != 0
		key, phys, code := mapDOMKeyEvent(text, desc)
		if key == platform.KeyUnknown && !platform.KeyByPosition(key, phys, mods) {
			return
		}
		w.host.surface.send(platform.SurfaceEvent{
			Kind:        platform.WebViewKey,
			WebView:     w,
			Key:         key,
			PhysicalKey: phys,
			Code:        code,
			Mods:        mods,
			Repeat:      repeat,
		})
	case webEvEmbeddedPress:
		if w.host == nil || w.host.closed || w.host.surface == nil {
			return
		}
		x, y, ok := parseFloatPair(text)
		if !ok {
			return
		}
		btn := max(0, min(2, int(errCode)))
		w.host.surface.recordPointer(x, y)
		w.host.surface.send(platform.SurfaceEvent{
			Kind:   platform.WebViewPress,
			X:      x,
			Y:      y,
			Button: btn,
		})
	case webEvEmbeddedScroll:
		if w.host == nil || w.host.closed || w.host.surface == nil {
			return
		}
		x, y, ok1 := parseFloatPair(text)
		dx, dy, ok2 := parseFloatPair(desc)
		if !ok1 || !ok2 || (dx == 0 && dy == 0) {
			return
		}
		mods := platform.Modifiers(uint32(errCode) & 0x0F)
		w.host.surface.recordPointer(x, y)
		w.host.surface.send(platform.SurfaceEvent{
			Kind:    platform.PointerScroll,
			X:       x,
			Y:       y,
			DX:      dx,
			DY:      dy,
			Mods:    mods,
			Precise: true,
		})
	}
}

func (w *Window) isCustomSchemeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	if s == "mygo" {
		return true
	}
	for _, cs := range w.opts.Schemes {
		if strings.EqualFold(cs, s) {
			return true
		}
	}
	return false
}

func suggestDownloadFilename(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err == nil && u.Path != "" {
		base := filepath.Base(u.Path)
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "download"
}

func (w *Window) startURLDownload(rawURL, dstPath string) {
	b := w.b
	if b == nil {
		return
	}
	ua := w.ua
	var store platform.DataStore
	if w.webIncognito {
		store = &dataStore{b: b, ephemeral: true}
	}
	finish := func(err error) {
		b.post(func() {
			if !w.closed && w.h != nil {
				w.h.DownloadFinished(rawURL, dstPath, err)
			}
		})
	}

	if strings.HasPrefix(rawURL, "data:") {
		go func() {
			data, err := decodeDataURL(rawURL)
			if err != nil {
				finish(err)
				return
			}
			if mkErr := os.MkdirAll(filepath.Dir(dstPath), 0o755); mkErr != nil {
				finish(mkErr)
				return
			}
			finish(os.WriteFile(dstPath, data, 0o644))
		}()
		return
	}

	appController{b: b}.ListCookies(store, rawURL, func(cookies []platform.Cookie, _ error) {
		go func() {
			req, err := http.NewRequest(http.MethodGet, rawURL, nil)
			if err != nil {
				finish(err)
				return
			}
			if ua != "" {
				req.Header.Set("User-Agent", ua)
			}
			if len(cookies) > 0 {
				var parts []string
				for _, c := range cookies {
					parts = append(parts, c.Name+"="+c.Value)
				}
				req.Header.Set("Cookie", strings.Join(parts, "; "))
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				finish(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 400 {
				finish(errors.New("http status " + strconv.Itoa(resp.StatusCode)))
				return
			}
			if mkErr := os.MkdirAll(filepath.Dir(dstPath), 0o755); mkErr != nil {
				finish(mkErr)
				return
			}
			tmp := dstPath + ".part"
			f, err := os.Create(tmp)
			if err != nil {
				finish(err)
				return
			}
			_, copyErr := io.Copy(f, resp.Body)
			closeErr := f.Close()
			if copyErr != nil {
				_ = os.Remove(tmp)
				finish(copyErr)
				return
			}
			if closeErr != nil {
				_ = os.Remove(tmp)
				finish(closeErr)
				return
			}
			if renErr := os.Rename(tmp, dstPath); renErr != nil {
				_ = os.Remove(tmp)
				finish(renErr)
				return
			}
			finish(nil)
		}()
	})
}

func decodeDataURL(raw string) ([]byte, error) {
	rest, ok := strings.CutPrefix(raw, "data:")
	if !ok {
		return nil, errors.New("invalid data url")
	}
	meta, body, ok := strings.Cut(rest, ",")
	if !ok {
		return nil, errors.New("invalid data url")
	}
	if strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return base64.StdEncoding.DecodeString(body)
	}
	decoded, err := url.PathUnescape(body)
	if err != nil {
		return []byte(body), nil
	}
	return []byte(decoded), nil
}

func parseFloatPair(s string) (float64, float64, bool) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, false
	}
	x, err1 := strconv.ParseFloat(strings.TrimSpace(a), 64)
	y, err2 := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return x, y, true
}

func mapDOMKeyEvent(domKey, domCode string) (key, physical platform.Key, code platform.KeyCode) {
	code = platform.KeyCodeForDOM(domCode)
	physical = physicalKeyForDOMCode(domCode)
	switch domKey {
	case "Enter":
		key = platform.KeyEnter
	case "Escape":
		key = platform.KeyEscape
	case "Backspace":
		key = platform.KeyBackspace
	case "Tab":
		key = platform.KeyTab
	case " ":
		key = platform.KeySpace
	case "Delete":
		key = platform.KeyDelete
	case "Insert":
		key = platform.KeyInsert
	case "Home":
		key = platform.KeyHome
	case "End":
		key = platform.KeyEnd
	case "PageUp":
		key = platform.KeyPageUp
	case "PageDown":
		key = platform.KeyPageDown
	case "ArrowLeft":
		key = platform.KeyLeft
	case "ArrowRight":
		key = platform.KeyRight
	case "ArrowUp":
		key = platform.KeyUp
	case "ArrowDown":
		key = platform.KeyDown
	case "ContextMenu":
		key = platform.KeyContextMenu
	case "BrowserBack":
		key = platform.KeyBack
	case "BrowserForward":
		key = platform.KeyForward
	case "F1":
		key = platform.KeyF1
	case "F2":
		key = platform.KeyF2
	case "F3":
		key = platform.KeyF3
	case "F4":
		key = platform.KeyF4
	case "F5":
		key = platform.KeyF5
	case "F6":
		key = platform.KeyF6
	case "F7":
		key = platform.KeyF7
	case "F8":
		key = platform.KeyF8
	case "F9":
		key = platform.KeyF9
	case "F10":
		key = platform.KeyF10
	case "F11":
		key = platform.KeyF11
	case "F12":
		key = platform.KeyF12
	default:
		if r, size := utf8.DecodeRuneInString(domKey); size == len(domKey) && r != utf8.RuneError {
			key = platform.KeyForRune(r)
		}
		if key == platform.KeyUnknown && physical != platform.KeyUnknown && len(domKey) == 1 {
			// Shifted ASCII punctuation (e.g. '!' on Digit1) falls back to US physical key.
			key = physical
		}
	}
	return key, physical, code
}

func physicalKeyForDOMCode(domCode string) platform.Key {
	switch {
	case len(domCode) == 4 && strings.HasPrefix(domCode, "Key") && domCode[3] >= 'A' && domCode[3] <= 'Z':
		return platform.KeyA + platform.Key(domCode[3]-'A')
	case len(domCode) == 6 && strings.HasPrefix(domCode, "Digit") && domCode[5] >= '0' && domCode[5] <= '9':
		return platform.Key0 + platform.Key(domCode[5]-'0')
	}
	switch domCode {
	case "Minus":
		return platform.KeyMinus
	case "Equal":
		return platform.KeyEqual
	case "BracketLeft":
		return platform.KeyBracketLeft
	case "BracketRight":
		return platform.KeyBracketRight
	case "Backslash":
		return platform.KeyBackslash
	case "Semicolon":
		return platform.KeySemicolon
	case "Quote":
		return platform.KeyQuote
	case "Backquote":
		return platform.KeyBackquote
	case "Comma":
		return platform.KeyComma
	case "Period":
		return platform.KeyPeriod
	case "Slash":
		return platform.KeySlash
	case "Space":
		return platform.KeySpace
	}
	return platform.KeyUnknown
}

// ---------- Wire Encoding / Decoding ----------

func encodeWebSetupMessage(w webSetupWire) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint32(buf[0:], w.webID)
	binary.LittleEndian.PutUint32(buf[4:], w.hostWinID)
	binary.LittleEndian.PutUint32(buf[8:], w.flags)
	binary.LittleEndian.PutUint32(buf[12:], w.bgRGBA)
	binary.LittleEndian.PutUint64(buf[16:], math.Float64bits(w.zoom))
	buf = appendWireStr(buf, w.ua)
	buf = appendUint32(buf, uint32(len(w.schemes)))
	for _, sc := range w.schemes {
		buf = appendWireStr(buf, sc)
	}
	buf = appendUint32(buf, uint32(len(w.scripts)))
	for _, us := range w.scripts {
		var sf byte
		if us.AtDocumentEnd {
			sf |= userScriptAtDocEnd
		}
		if us.AllFrames {
			sf |= userScriptAllFrames
		}
		buf = append(buf, sf, 0, 0, 0)
		buf = appendWireStr(buf, us.Source)
	}
	return buf
}

func decodeWebSetupMessage(p []byte) (webSetupWire, bool) {
	if len(p) < 24 {
		return webSetupWire{}, false
	}
	var out webSetupWire
	out.webID = binary.LittleEndian.Uint32(p[0:])
	out.hostWinID = binary.LittleEndian.Uint32(p[4:])
	out.flags = binary.LittleEndian.Uint32(p[8:])
	out.bgRGBA = binary.LittleEndian.Uint32(p[12:])
	out.zoom = math.Float64frombits(binary.LittleEndian.Uint64(p[16:]))
	off := 24
	var ok bool
	if out.ua, off, ok = readWireStr(p, off); !ok {
		return webSetupWire{}, false
	}
	scCount, off, ok := readUint32(p, off)
	if !ok || scCount > 64 {
		return webSetupWire{}, false
	}
	out.schemes = make([]string, 0, scCount)
	for range scCount {
		var sc string
		if sc, off, ok = readWireStr(p, off); !ok {
			return webSetupWire{}, false
		}
		out.schemes = append(out.schemes, sc)
	}
	usCount, off, ok := readUint32(p, off)
	if !ok || usCount > 256 {
		return webSetupWire{}, false
	}
	out.scripts = make([]platform.UserScript, 0, usCount)
	for range usCount {
		if off+4 > len(p) {
			return webSetupWire{}, false
		}
		sf := p[off]
		off += 4
		src, nextOff, ok := readWireStr(p, off)
		if !ok {
			return webSetupWire{}, false
		}
		off = nextOff
		out.scripts = append(out.scripts, platform.UserScript{
			Source:        src,
			AtDocumentEnd: sf&userScriptAtDocEnd != 0,
			AllFrames:     sf&userScriptAllFrames != 0,
		})
	}
	if off != len(p) {
		return webSetupWire{}, false
	}
	return out, true
}

func encodePlaceWebViewsMessage(w placeWebViewsWire) []byte {
	buf := make([]byte, 12+len(w.views)*60+len(w.surfaceRegions)*16)
	binary.LittleEndian.PutUint32(buf[0:], w.winID)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(w.views)))
	binary.LittleEndian.PutUint32(buf[8:], uint32(len(w.surfaceRegions)))
	off := 12
	for _, v := range w.views {
		binary.LittleEndian.PutUint32(buf[off+0:], v.webID)
		putF32(buf[off+4:], float32(v.frame.X))
		putF32(buf[off+8:], float32(v.frame.Y))
		putF32(buf[off+12:], float32(v.frame.W))
		putF32(buf[off+16:], float32(v.frame.H))
		putF32(buf[off+20:], float32(v.clip.X))
		putF32(buf[off+24:], float32(v.clip.Y))
		putF32(buf[off+28:], float32(v.clip.W))
		putF32(buf[off+32:], float32(v.clip.H))
		putF32(buf[off+36:], float32(v.pageW))
		putF32(buf[off+40:], float32(v.pageH))
		putF32(buf[off+44:], float32(v.radii[0]))
		putF32(buf[off+48:], float32(v.radii[1]))
		putF32(buf[off+52:], float32(v.radii[2]))
		putF32(buf[off+56:], float32(v.radii[3]))
		off += 60
	}
	for _, r := range w.surfaceRegions {
		putF32(buf[off+0:], float32(r.X))
		putF32(buf[off+4:], float32(r.Y))
		putF32(buf[off+8:], float32(r.W))
		putF32(buf[off+12:], float32(r.H))
		off += 16
	}
	return buf
}

func decodePlaceWebViewsMessage(p []byte) (placeWebViewsWire, bool) {
	if len(p) < 12 {
		return placeWebViewsWire{}, false
	}
	winID := binary.LittleEndian.Uint32(p[0:])
	vc := int(binary.LittleEndian.Uint32(p[4:]))
	rc := int(binary.LittleEndian.Uint32(p[8:]))
	if vc < 0 || vc > 1024 || rc < 0 || rc > 8192 || 12+vc*60+rc*16 != len(p) {
		return placeWebViewsWire{}, false
	}
	out := placeWebViewsWire{
		winID:          winID,
		views:          make([]placedWebViewWire, vc),
		surfaceRegions: make([]platform.RectF, rc),
	}
	off := 12
	for i := range vc {
		out.views[i] = placedWebViewWire{
			webID: binary.LittleEndian.Uint32(p[off+0:]),
			frame: platform.RectF{
				X: float64(getF32(p[off+4:])),
				Y: float64(getF32(p[off+8:])),
				W: float64(getF32(p[off+12:])),
				H: float64(getF32(p[off+16:])),
			},
			clip: platform.RectF{
				X: float64(getF32(p[off+20:])),
				Y: float64(getF32(p[off+24:])),
				W: float64(getF32(p[off+28:])),
				H: float64(getF32(p[off+32:])),
			},
			pageW: float64(getF32(p[off+36:])),
			pageH: float64(getF32(p[off+40:])),
			radii: [4]float64{
				float64(getF32(p[off+44:])),
				float64(getF32(p[off+48:])),
				float64(getF32(p[off+52:])),
				float64(getF32(p[off+56:])),
			},
		}
		off += 60
	}
	for i := range rc {
		out.surfaceRegions[i] = platform.RectF{
			X: float64(getF32(p[off+0:])),
			Y: float64(getF32(p[off+4:])),
			W: float64(getF32(p[off+8:])),
			H: float64(getF32(p[off+12:])),
		}
		off += 16
	}
	return out, true
}

func encodeWebAsyncReqMessage(r webAsyncReqWire) []byte {
	buf := make([]byte, 60)
	binary.LittleEndian.PutUint32(buf[0:], r.reqID)
	binary.LittleEndian.PutUint32(buf[4:], r.webID)
	buf[8] = r.op
	if r.pdfOpts.Landscape {
		buf[9] |= 1
	}
	if r.pdfOpts.Background {
		buf[9] |= 2
	}
	binary.LittleEndian.PutUint64(buf[12:], math.Float64bits(r.pdfOpts.PageWidth))
	binary.LittleEndian.PutUint64(buf[20:], math.Float64bits(r.pdfOpts.PageHeight))
	binary.LittleEndian.PutUint64(buf[28:], math.Float64bits(r.pdfOpts.MarginTop))
	binary.LittleEndian.PutUint64(buf[36:], math.Float64bits(r.pdfOpts.MarginRight))
	binary.LittleEndian.PutUint64(buf[44:], math.Float64bits(r.pdfOpts.MarginBottom))
	binary.LittleEndian.PutUint64(buf[52:], math.Float64bits(r.pdfOpts.MarginLeft))
	buf = appendWireStr(buf, r.body)
	return buf
}

func decodeWebAsyncReqMessage(p []byte) (webAsyncReqWire, bool) {
	if len(p) < 60 {
		return webAsyncReqWire{}, false
	}
	var out webAsyncReqWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.webID = binary.LittleEndian.Uint32(p[4:])
	out.op = p[8]
	pflags := p[9]
	out.pdfOpts = platform.PDFOptions{
		Landscape:    pflags&1 != 0,
		Background:   pflags&2 != 0,
		PageWidth:    math.Float64frombits(binary.LittleEndian.Uint64(p[12:])),
		PageHeight:   math.Float64frombits(binary.LittleEndian.Uint64(p[20:])),
		MarginTop:    math.Float64frombits(binary.LittleEndian.Uint64(p[28:])),
		MarginRight:  math.Float64frombits(binary.LittleEndian.Uint64(p[36:])),
		MarginBottom: math.Float64frombits(binary.LittleEndian.Uint64(p[44:])),
		MarginLeft:   math.Float64frombits(binary.LittleEndian.Uint64(p[52:])),
	}
	body, off, ok := readWireStr(p, 60)
	if !ok || off != len(p) {
		return webAsyncReqWire{}, false
	}
	out.body = body
	return out, true
}

func encodeWebAsyncResMessage(r webAsyncResWire) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf[0:], r.reqID)
	buf = appendWireStr(buf, r.text)
	buf = appendWireStr(buf, r.errStr)
	buf = appendWireBytes(buf, r.data)
	return buf
}

func decodeWebAsyncResMessage(p []byte) (webAsyncResWire, bool) {
	if len(p) < 4 {
		return webAsyncResWire{}, false
	}
	var out webAsyncResWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	off := 4
	var ok bool
	if out.text, off, ok = readWireStr(p, off); !ok {
		return webAsyncResWire{}, false
	}
	if out.errStr, off, ok = readWireStr(p, off); !ok {
		return webAsyncResWire{}, false
	}
	if out.data, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return webAsyncResWire{}, false
	}
	return out, true
}

func encodeSchemeReqMessage(r schemeReqWire) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], r.reqID)
	binary.LittleEndian.PutUint32(buf[4:], r.webID)
	if r.cancel {
		buf[8] = 1
	}
	buf = appendWireStr(buf, r.method)
	buf = appendWireStr(buf, r.url)
	buf = appendUint32(buf, uint32(len(r.headers)))
	for _, kv := range r.headers {
		buf = appendWireStr(buf, kv[0])
		buf = appendWireStr(buf, kv[1])
	}
	buf = appendWireBytes(buf, r.body)
	return buf
}

func decodeSchemeReqMessage(p []byte) (schemeReqWire, bool) {
	if len(p) < 12 {
		return schemeReqWire{}, false
	}
	var out schemeReqWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.webID = binary.LittleEndian.Uint32(p[4:])
	out.cancel = p[8] != 0
	off := 12
	var ok bool
	if out.method, off, ok = readWireStr(p, off); !ok {
		return schemeReqWire{}, false
	}
	if out.url, off, ok = readWireStr(p, off); !ok {
		return schemeReqWire{}, false
	}
	hc, off, ok := readUint32(p, off)
	if !ok || hc > 512 {
		return schemeReqWire{}, false
	}
	out.headers = make([][2]string, 0, hc)
	for range hc {
		var k, v string
		if k, off, ok = readWireStr(p, off); !ok {
			return schemeReqWire{}, false
		}
		if v, off, ok = readWireStr(p, off); !ok {
			return schemeReqWire{}, false
		}
		out.headers = append(out.headers, [2]string{k, v})
	}
	if out.body, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return schemeReqWire{}, false
	}
	return out, true
}

func encodeSchemeResMessage(r schemeResWire) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], r.reqID)
	buf[4] = r.op
	binary.LittleEndian.PutUint32(buf[8:], uint32(r.status))
	buf = appendUint32(buf, uint32(len(r.headers)))
	for _, kv := range r.headers {
		buf = appendWireStr(buf, kv[0])
		buf = appendWireStr(buf, kv[1])
	}
	buf = appendWireBytes(buf, r.chunk)
	buf = appendWireStr(buf, r.errStr)
	return buf
}

func decodeSchemeResMessage(p []byte) (schemeResWire, bool) {
	if len(p) < 12 {
		return schemeResWire{}, false
	}
	var out schemeResWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.op = p[4]
	out.status = int32(binary.LittleEndian.Uint32(p[8:]))
	off := 12
	hc, off, ok := readUint32(p, off)
	if !ok || hc > 512 {
		return schemeResWire{}, false
	}
	out.headers = make([][2]string, 0, hc)
	for range hc {
		var k, v string
		if k, off, ok = readWireStr(p, off); !ok {
			return schemeResWire{}, false
		}
		if v, off, ok = readWireStr(p, off); !ok {
			return schemeResWire{}, false
		}
		out.headers = append(out.headers, [2]string{k, v})
	}
	if out.chunk, off, ok = readWireBytes(p, off); !ok {
		return schemeResWire{}, false
	}
	if out.errStr, off, ok = readWireStr(p, off); !ok || off != len(p) {
		return schemeResWire{}, false
	}
	return out, true
}
