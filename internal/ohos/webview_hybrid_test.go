//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/internal/platform"
)

type hybridWebHandler struct {
	testWindowHandler
	messages      []string
	audibleStates []bool
	permOrigin    string
	permKinds     []string
	permAllow     bool
	openTarget    platform.Window
	openReqs      []platform.NewWindowRequest
	schemeReqs    []*platform.SchemeRequest
	schemeDLs     []string
	dlSavePath    string
	dlFinishedURL string
	dlFinishedErr error
	dlDone        bool
	surfEvents    []platform.SurfaceEvent
	closedOrder   *[]string
	label         string
}

func (h *hybridWebHandler) Message(msg string)              { h.messages = append(h.messages, msg) }
func (h *hybridWebHandler) AudibleChanged(aud bool)         { h.audibleStates = append(h.audibleStates, aud) }
func (h *hybridWebHandler) SchemeRequest(r *platform.SchemeRequest) { h.schemeReqs = append(h.schemeReqs, r) }
func (h *hybridWebHandler) SchemeDownload(u string)         { h.schemeDLs = append(h.schemeDLs, u) }
func (h *hybridWebHandler) SurfaceEvent(ev platform.SurfaceEvent) bool {
	h.surfEvents = append(h.surfEvents, ev)
	return false
}
func (h *hybridWebHandler) PermissionRequested(kinds []string, origin string) bool {
	h.permKinds = kinds
	h.permOrigin = origin
	return h.permAllow
}
func (h *hybridWebHandler) NewWindow(req platform.NewWindowRequest) platform.Window {
	h.openReqs = append(h.openReqs, req)
	return h.openTarget
}
func (h *hybridWebHandler) DownloadStarted(_, _ string) string { return h.dlSavePath }
func (h *hybridWebHandler) DownloadFinished(u, _ string, err error) {
	h.dlFinishedURL = u
	h.dlFinishedErr = err
	h.dlDone = true
}
func (h *hybridWebHandler) Closed() {
	if h.closedOrder != nil {
		*h.closedOrder = append(*h.closedOrder, h.label)
	}
}

// Custom protocol requests stream status, headers and body chunks back to the host and cancel their context when aborted.
func TestWebSchemeRequestStreamingAndCancellation(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	wh := &hybridWebHandler{}
	pw, err := b.NewWindow(&platform.WindowOptions{
		Surface: false,
		Schemes: []string{"mygo", "app"},
		UserScripts: []platform.UserScript{
			{Source: "window.__boot = 1;", AllFrames: true},
		},
	}, wh)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	winID := uint32(pw.Handle())

	// Drain WinCreate and initial WebSetup.
	if kind, _, _, err := hostConn.read(); err != nil || kind != kindWinCreate {
		t.Fatalf("read #1 = (%d, %v), want kindWinCreate", kind, err)
	}
	kind, setupPayload, _, err := hostConn.read()
	if err != nil || kind != kindWebSetup {
		t.Fatalf("read #2 = (%d, %v), want kindWebSetup", kind, err)
	}
	setup, ok := decodeWebSetupMessage(setupPayload)
	if !ok || setup.webID != winID || len(setup.schemes) != 2 || len(setup.scripts) != 1 {
		t.Fatalf("decoded webSetup = %+v (ok=%v)", setup, ok)
	}

	// Host dispatches a POST custom scheme request with headers and body.
	b.onSchemeReq(encodeSchemeReqMessage(schemeReqWire{
		reqID:   77,
		webID:   winID,
		method:  "POST",
		url:     "mygo://app/api/echo",
		headers: [][2]string{{"content-type", "application/json"}, {"x-test", "42"}},
		body:    []byte(`{"hello":"ohos"}`),
	}))

	if len(wh.schemeReqs) != 1 {
		t.Fatalf("schemeReqs len = %d, want 1", len(wh.schemeReqs))
	}
	req := wh.schemeReqs[0]
	gotBody, _ := io.ReadAll(req.Body)
	if req.Method != "POST" || req.URL != "mygo://app/api/echo" || string(gotBody) != `{"hello":"ohos"}` {
		t.Fatalf("scheme req = (%s %s body=%q)", req.Method, req.URL, gotBody)
	}
	if req.Header["X-Test"][0] != "42" {
		t.Fatalf("canonical header X-Test = %v, want [42]", req.Header["X-Test"])
	}

	req.Responder.Respond(201, map[string][]string{"Content-Type": {"text/html; charset=utf-8"}})
	bw := req.Responder.(platform.SchemeBodyWriter)
	if err := bw.WriteBody([]byte("<h1>ok</h1>")); err != nil {
		t.Fatalf("WriteBody: %v", err)
	}
	req.Responder.Finish()

	var ops []schemeResWire
	for range 3 {
		k, p, _, err := hostConn.read()
		if err != nil || k != kindSchemeRes {
			t.Fatalf("read schemeRes = (%d, %v)", k, err)
		}
		res, ok := decodeSchemeResMessage(p)
		if !ok {
			t.Fatal("decodeSchemeResMessage failed")
		}
		ops = append(ops, res)
	}
	if ops[0].op != schemeOpHead || ops[0].status != 201 ||
		ops[1].op != schemeOpBody || !bytes.Equal(ops[1].chunk, []byte("<h1>ok</h1>")) ||
		ops[2].op != schemeOpFinish {
		t.Fatalf("scheme response sequence = %+v", ops)
	}

	// Second request cancelled mid-flight by host cancels req.Context and blocks further body writes.
	b.onSchemeReq(encodeSchemeReqMessage(schemeReqWire{
		reqID:  78,
		webID:  winID,
		method: "GET",
		url:    "app://bundle/stream",
	}))
	req2 := wh.schemeReqs[1]
	b.onSchemeReq(encodeSchemeReqMessage(schemeReqWire{
		reqID:  78,
		webID:  winID,
		cancel: true,
	}))
	if req2.Context.Err() == nil {
		t.Fatal("cancelled scheme request Context.Err() == nil, want non-nil")
	}
	if err := req2.Responder.(platform.SchemeBodyWriter).WriteBody([]byte("late")); err == nil {
		t.Fatal("WriteBody after host cancel succeeded, want error")
	}
}

// CallAsyncFunction, CapturePage and PrintToPDF round-trip async results and propagate SyntaxError prefixes for bridge eval fallback.
func TestWebAsyncEvalCaptureAndPrintToPDF(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	pw, err := b.NewWindow(&platform.WindowOptions{Surface: false}, &testWindowHandler{})
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	_, _, _, _ = hostConn.read() // kindWinCreate

	// 1. Compile-time SyntaxError preserves "SyntaxError" prefix required by bridge.go.
	var syntaxErr error
	pw.CallAsyncFunction("let x = 1; return x;", func(_ string, err error) {
		syntaxErr = err
	})
	k, p, _, err := hostConn.read()
	if err != nil || k != kindWebAsyncReq {
		t.Fatalf("read async eval #1 = (%d, %v)", k, err)
	}
	req1, ok := decodeWebAsyncReqMessage(p)
	if !ok || req1.op != webAsyncEval || req1.body != "let x = 1; return x;" {
		t.Fatalf("decoded req1 = %+v (ok=%v)", req1, ok)
	}
	b.onWebAsyncRes(encodeWebAsyncResMessage(webAsyncResWire{
		reqID:  req1.reqID,
		errStr: "SyntaxError: Unexpected identifier 'x'",
	}))
	if syntaxErr == nil || !strings.HasPrefix(syntaxErr.Error(), "SyntaxError") {
		t.Fatalf("syntaxErr = %v, want SyntaxError prefix", syntaxErr)
	}

	// 2. Awaited async function result.
	var gotVal string
	pw.CallAsyncFunction("return await Promise.resolve('42')", func(res string, err error) {
		if err != nil {
			t.Errorf("unexpected eval err: %v", err)
		}
		gotVal = res
	})
	_, p2, _, _ := hostConn.read()
	req2, _ := decodeWebAsyncReqMessage(p2)
	b.onWebAsyncRes(encodeWebAsyncResMessage(webAsyncResWire{
		reqID: req2.reqID,
		text:  "42",
	}))
	if gotVal != "42" {
		t.Fatalf("CallAsyncFunction result = %q, want 42", gotVal)
	}

	// 3. CapturePage & PrintToPDF options and binary payloads.
	var gotPNG, gotPDF []byte
	pw.CapturePage(func(png []byte, err error) {
		if err != nil {
			t.Errorf("CapturePage err: %v", err)
		}
		gotPNG = png
	})
	_, p3, _, _ := hostConn.read()
	req3, ok := decodeWebAsyncReqMessage(p3)
	if !ok || req3.op != webAsyncCapture {
		t.Fatalf("CapturePage req = %+v", req3)
	}
	b.onWebAsyncRes(encodeWebAsyncResMessage(webAsyncResWire{
		reqID: req3.reqID,
		data:  []byte{0x89, 'P', 'N', 'G'},
	}))

	pdfOpts := platform.PDFOptions{
		Landscape:    true,
		Background:   true,
		PageWidth:    8.5,
		PageHeight:   11.0,
		MarginTop:    0.5,
		MarginBottom: 0.5,
	}
	pw.PrintToPDF(pdfOpts, func(pdf []byte, err error) {
		if err != nil {
			t.Errorf("PrintToPDF err: %v", err)
		}
		gotPDF = pdf
	})
	_, p4, _, _ := hostConn.read()
	req4, ok := decodeWebAsyncReqMessage(p4)
	if !ok || req4.op != webAsyncPrintPDF || req4.pdfOpts != pdfOpts {
		t.Fatalf("PrintToPDF req = %+v, want %+v", req4.pdfOpts, pdfOpts)
	}
	b.onWebAsyncRes(encodeWebAsyncResMessage(webAsyncResWire{
		reqID: req4.reqID,
		data:  []byte("%PDF-1.7"),
	}))

	if !bytes.Equal(gotPNG, []byte{0x89, 'P', 'N', 'G'}) || !bytes.Equal(gotPDF, []byte("%PDF-1.7")) {
		t.Fatalf("CapturePage=%x PrintToPDF=%q", gotPNG, gotPDF)
	}
}

// Embedded WebViews cut holes out of the surface hit-test region while restoring Covers, forward press/tab/key/wheel events, and close before their host window.
func TestEmbeddedWebViewPlacementHitRegionsAndEvents(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	var closedOrder []string
	winH := &hybridWebHandler{closedOrder: &closedOrder, label: "host"}
	win, err := b.NewWindow(&platform.WindowOptions{
		Surface: true,
		Width:   800,
		Height:  600,
	}, winH)
	if err != nil {
		t.Fatalf("NewWindow(Surface=true): %v", err)
	}
	_, _, _, _ = hostConn.read() // kindWinCreate

	surf := win.Surface()
	webH := &hybridWebHandler{closedOrder: &closedOrder, label: "embedded"}
	wv, err := surf.NewWebView(&platform.WindowOptions{
		ScrollToContainer: true,
		Schemes:           []string{"mygo"},
	}, webH)
	if err != nil {
		t.Fatalf("Surface.NewWebView: %v", err)
	}

	// First embedded WebView enables transparent surface compositing and sends its setup.
	k1, p1, _, err := hostConn.read()
	if err != nil || k1 != kindWinAction || p1[4] != winActEnableWebLayer {
		t.Fatalf("after NewWebView #1 = (kind=%d payload=%v err=%v), want winActEnableWebLayer", k1, p1, err)
	}
	k2, p2, _, err := hostConn.read()
	if err != nil || k2 != kindWebSetup {
		t.Fatalf("after NewWebView #2 = (kind=%d err=%v), want kindWebSetup", k2, err)
	}
	setup, ok := decodeWebSetupMessage(p2)
	if !ok || setup.hostWinID != uint32(win.Handle()) || setup.flags&webSetupScrollToContainer == 0 {
		t.Fatalf("embedded setup = %+v", setup)
	}

	// Place embedded WebView at (100, 50, 400x300) with a popover cover at (200, 100, 80x60).
	place := platform.WebViewPlacement{
		WebView: wv,
		Frame:   platform.RectF{X: 100, Y: 50, W: 400, H: 300},
		Clip:    platform.RectF{X: 100, Y: 50, W: 400, H: 300},
		Radii:   [4]float64{12, 12, 12, 12},
		Covers:  []platform.RectF{{X: 200, Y: 100, W: 80, H: 60}},
	}
	surf.PlaceWebViews([]platform.WebViewPlacement{place})

	k3, p3, _, err := hostConn.read()
	if err != nil || k3 != kindPlaceWebViews {
		t.Fatalf("PlaceWebViews msg = (%d, %v), want kindPlaceWebViews", k3, err)
	}
	pwWire, ok := decodePlaceWebViewsMessage(p3)
	if !ok || len(pwWire.views) != 1 {
		t.Fatalf("decoded PlaceWebViews = %+v (ok=%v)", pwWire, ok)
	}

	pointInRegions := func(x, y float64) bool {
		for _, r := range pwWire.surfaceRegions {
			if r.Contains(x, y) {
				return true
			}
		}
		return false
	}
	if !pointInRegions(20, 20) {
		t.Fatal("point (20,20) outside WebView should belong to surfaceRegions")
	}
	if pointInRegions(150, 150) {
		t.Fatal("uncovered point (150,150) inside WebView clip must NOT belong to surfaceRegions")
	}
	if !pointInRegions(220, 120) {
		t.Fatal("covered point (220,120) inside popover Cover MUST belong to surfaceRegions")
	}

	// Press, wheel scroll (ScrollToContainer), Shift+Tab out, and unhandled Ctrl+K key forward to host Surface.
	webID := setup.webID
	b.onWebEvent(encodeWebEventMessage(webID, webEvEmbeddedPress, false, false, 0, "150,160", ""))
	b.onWebEvent(encodeWebEventMessage(webID, webEvEmbeddedScroll, false, false, int32(platform.ModShift), "150,160", "0,48"))
	b.onWebEvent(encodeWebEventMessage(webID, webEvTabOut, false, false, 1, "", ""))
	// Drain winActFocusSurface triggered by TabOut.
	kFocus, pFocus, _, err := hostConn.read()
	if err != nil || kFocus != kindWinAction || pFocus[4] != winActFocusSurface {
		t.Fatalf("after TabOut msg = (%d, %v, %v), want winActFocusSurface", kFocus, pFocus, err)
	}
	b.onWebEvent(encodeWebEventMessage(webID, webEvUnhandledKey, false, false, int32(platform.ModCtrl), "k", "KeyK"))

	if len(winH.surfEvents) != 4 {
		t.Fatalf("surfEvents count = %d, want 4: %+v", len(winH.surfEvents), winH.surfEvents)
	}
	if ev := winH.surfEvents[0]; ev.Kind != platform.WebViewPress || ev.X != 150 || ev.Y != 160 {
		t.Fatalf("event[0] = %+v, want WebViewPress@(150,160)", ev)
	}
	if ev := winH.surfEvents[1]; ev.Kind != platform.PointerScroll || ev.DY != 48 || !ev.Precise || ev.Mods != platform.ModShift {
		t.Fatalf("event[1] = %+v, want PointerScroll DY=48 Precise ModShift", ev)
	}
	if ev := winH.surfEvents[2]; ev.Kind != platform.WebViewTabOut || ev.WebView != wv || ev.Mods != platform.ModShift {
		t.Fatalf("event[2] = %+v, want WebViewTabOut ModShift", ev)
	}
	if ev := winH.surfEvents[3]; ev.Kind != platform.WebViewKey || ev.WebView != wv || ev.Key != platform.KeyK || ev.Code != platform.CodeKeyK || ev.Mods != platform.ModCtrl {
		t.Fatalf("event[3] = %+v, want WebViewKey Ctrl+K", ev)
	}

	// Closing the host window destroys embedded WebViews without firing embedded Closed(), then calls host Closed().
	win.Close()
	kDestroy, pDestroy, _, err := hostConn.read()
	if err != nil || kDestroy != kindWebReq {
		t.Fatalf("on host Close #1 = (%d, %v), want kindWebReq(webOpDestroy)", kDestroy, err)
	}
	if gotID, op, _, _, ok := decodeWebReqMessage(pDestroy); !ok || gotID != webID || op != webOpDestroy {
		t.Fatalf("destroy msg = (%d, %d, %v), want (%d, webOpDestroy)", gotID, op, ok, webID)
	}
	if len(closedOrder) != 1 || closedOrder[0] != "host" {
		t.Fatalf("closedOrder = %v, want [host] only", closedOrder)
	}
}

// Standalone hybrid WebView dispatches bridge messages, permissions, window.open targets and data URL downloads.
func TestWebHybridBridgePermissionsWindowOpenAndDownloads(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	childWin, err := b.NewWindow(&platform.WindowOptions{Surface: false}, &testWindowHandler{})
	if err != nil {
		t.Fatalf("NewWindow child: %v", err)
	}
	_, _, _, _ = hostConn.read()

	tmpFile := filepath.Join(t.TempDir(), "hello.txt")
	wh := &hybridWebHandler{
		permAllow:  true,
		openTarget: childWin,
		dlSavePath: tmpFile,
	}
	pw, err := b.NewWindow(&platform.WindowOptions{Surface: false, Schemes: []string{"mygo"}}, wh)
	if err != nil {
		t.Fatalf("NewWindow main: %v", err)
	}
	_, _, _, _ = hostConn.read() // kindWinCreate
	_, _, _, _ = hostConn.read() // kindWebSetup
	winID := uint32(pw.Handle())

	// Bridge message and audio state.
	b.onWebEvent(encodeWebEventMessage(winID, webEvMessage, false, false, 0, `{"t":"call","id":1}`, ""))
	b.onWebEvent(encodeWebEventMessage(winID, webEvAudioState, false, false, 1, "", ""))
	if len(wh.messages) != 1 || wh.messages[0] != `{"t":"call","id":1}` || !pw.IsAudible() {
		t.Fatalf("messages=%v audible=%v", wh.messages, pw.IsAudible())
	}

	// Permission request replies with webOpPermReply.
	b.onWebEvent(encodeWebEventMessage(winID, webEvPermReq, false, false, 9, "https://example.com", "camera,microphone"))
	k, p, _, err := hostConn.read()
	if err != nil || k != kindWebReq {
		t.Fatalf("perm reply read = (%d, %v)", k, err)
	}
	if _, op, idStr, allowStr, ok := decodeWebReqMessage(p); !ok || op != webOpPermReply || idStr != "9" || allowStr != "1" {
		t.Fatalf("perm reply = (%d, %q, %q, %v)", op, idStr, allowStr, ok)
	}

	// window.open navigates the returned child window to the target URL.
	b.onWebEvent(encodeWebEventMessage(winID, webEvWindowNew, false, false, 1, "https://example.com/popup", "new-window"))
	k, p, _, err = hostConn.read()
	if err != nil || k != kindWebReq {
		t.Fatalf("window.open LoadURL = (%d, %v)", k, err)
	}
	if gotID, op, urlStr, _, ok := decodeWebReqMessage(p); !ok || gotID != uint32(childWin.Handle()) || op != webOpLoadURL || urlStr != "https://example.com/popup" {
		t.Fatalf("child LoadURL = (%d, %d, %q, %v)", gotID, op, urlStr, ok)
	}

	// Custom scheme download routes to SchemeDownload; data: URL download writes file and posts DownloadFinished on Step().
	b.onWebEvent(encodeWebEventMessage(winID, webEvDownloadStart, false, false, 0, "mygo://app/export.csv", "export.csv"))
	if len(wh.schemeDLs) != 1 || wh.schemeDLs[0] != "mygo://app/export.csv" {
		t.Fatalf("schemeDLs = %v", wh.schemeDLs)
	}

	b.onWebEvent(encodeWebEventMessage(winID, webEvDownloadStart, false, false, 0, "data:text/plain;base64,SGVsbG8gT0hPUw==", "hello.txt"))
	for !wh.dlDone {
		b.Step()
	}
	gotBytes, err := os.ReadFile(tmpFile)
	if err != nil || wh.dlFinishedErr != nil || string(gotBytes) != "Hello OHOS" {
		t.Fatalf("downloaded file = %q (readErr=%v dlErr=%v)", gotBytes, err, wh.dlFinishedErr)
	}
}
