//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/platform"
)

type testWebHandler struct {
	testWindowHandler
	navStarted   []string
	navCommitted []string
	loadFinished int
	titles       []string
	loadFailed   []string
}

func (h *testWebHandler) NavigationStarted(u string)   { h.navStarted = append(h.navStarted, u) }
func (h *testWebHandler) NavigationCommitted(u string) { h.navCommitted = append(h.navCommitted, u) }
func (h *testWebHandler) LoadFinished()                { h.loadFinished++ }
func (h *testWebHandler) TitleChanged(t string)        { h.titles = append(h.titles, t) }
func (h *testWebHandler) LoadFailed(u string, _ int, _ string) {
	h.loadFailed = append(h.loadFailed, u)
}

// Standalone web windows create an incognito ArkWeb subwindow without a drawing surface, forward UA and navigation commands, and handle host close requests cleanly.
func TestWebWindowLifecycleAndNavigationEvents(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	if !b.WebViewAvailable() {
		t.Fatal("WebViewAvailable() = false, want true")
	}

	store, err := b.App().NewDataStore(platform.DataStoreOptions{Ephemeral: true})
	if err != nil {
		t.Fatalf("NewDataStore: %v", err)
	}

	wh := &testWebHandler{}
	pw, err := b.NewWindow(&platform.WindowOptions{
		Title:           "Sign in to Bilibili",
		Width:           480,
		Height:          680,
		Center:          true,
		Resizable:       true,
		Movable:         true,
		Closable:        true,
		Surface:         false,
		DataStore:       store,
		UserAgent:       "Mozilla/5.0 TestUA",
		UserAgentSuffix: "MyGoMusic/1.0",
	}, wh)
	if err != nil {
		t.Fatalf("NewWindow(Surface=false): %v", err)
	}
	if pw.Surface() != nil {
		t.Fatal("web window Surface() != nil, want nil")
	}

	// Host receives kindWinCreate with winFlagWebView | winFlagWebIncognito, followed by initial UA.
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindWinCreate {
		t.Fatalf("hostConn.read #1 = (kind=%d, err=%v), want kindWinCreate", kind, err)
	}
	cfg, ok := decodeWinConfigMessage(payload)
	if !ok || cfg.flags&(winFlagWebView|winFlagWebIncognito) != (winFlagWebView|winFlagWebIncognito) {
		t.Fatalf("winCreate flags = %#x, want winFlagWebView|winFlagWebIncognito", cfg.flags)
	}

	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWebReq {
		t.Fatalf("hostConn.read #2 = (kind=%d, err=%v), want kindWebReq(SetUA)", kind, err)
	}
	winID, op, ua, _, ok := decodeWebReqMessage(payload)
	if !ok || winID != cfg.winID || op != webOpSetUA || ua != "Mozilla/5.0 TestUA MyGoMusic/1.0" {
		t.Fatalf("SetUA webReq = (%d, %d, %q, %v)", winID, op, ua, ok)
	}

	// LoadURL forwards URL to ArkWeb and updates loading state until webEvPageEnd arrives.
	loginURL := "https://passport.bilibili.com/login"
	pw.LoadURL(loginURL)
	if !pw.IsLoading() || pw.URL() != loginURL {
		t.Fatalf("after LoadURL: loading=%v url=%q", pw.IsLoading(), pw.URL())
	}
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWebReq {
		t.Fatalf("hostConn.read #3 = (kind=%d, err=%v), want kindWebReq(LoadURL)", kind, err)
	}
	if _, op, gotURL, _, ok := decodeWebReqMessage(payload); !ok || op != webOpLoadURL || gotURL != loginURL {
		t.Fatalf("LoadURL webReq = (op=%d, url=%q, ok=%v)", op, gotURL, ok)
	}

	// Simulate ArkWeb navigation lifecycle events back to engine.
	b.onWebEvent(encodeWebEventMessage(cfg.winID, webEvPageBegin, false, false, 0, loginURL, ""))
	b.onWebEvent(encodeWebEventMessage(cfg.winID, webEvTitle, true, false, 0, "哔哩哔哩登录", ""))
	b.onWebEvent(encodeWebEventMessage(cfg.winID, webEvPageEnd, true, false, 0, "https://www.bilibili.com/", ""))

	if pw.IsLoading() || !pw.CanGoBack() || pw.CanGoForward() || pw.URL() != "https://www.bilibili.com/" {
		t.Fatalf("after PageEnd: loading=%v back=%v fwd=%v url=%q", pw.IsLoading(), pw.CanGoBack(), pw.CanGoForward(), pw.URL())
	}
	if wh.loadFinished != 1 || len(wh.titles) != 1 || wh.titles[0] != "哔哩哔哩登录" {
		t.Fatalf("web handler callbacks: loadFinished=%d titles=%v", wh.loadFinished, wh.titles)
	}

	// Closing the web window via host winCloseRequest works without a Surface and triggers Closed().
	var closeIn [8]byte
	binary.LittleEndian.PutUint32(closeIn[0:], cfg.winID)
	closeIn[4] = inputWindow
	closeIn[5] = winCloseRequest
	b.onInput(closeIn[:])

	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindWinAction || payload[4] != winActCloseAllow {
		t.Fatalf("after winCloseRequest: kind=%d payload=%v err=%v", kind, payload, err)
	}
	if wh.closed != 1 {
		t.Fatalf("wh.closed = %d, want 1", wh.closed)
	}

	// Closing the ephemeral store clears incognito site data in the host.
	store.Close()
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindCookieReq {
		t.Fatalf("after store.Close(): kind=%d err=%v, want kindCookieReq", kind, err)
	}
	creq, ok := decodeCookieReqMessage(payload)
	if !ok || creq.op != cookieOpClearData || !creq.incognito {
		t.Fatalf("store.Close cookieReq = %+v (ok=%v)", creq, ok)
	}
}

// ListCookies filters returned cookies by RFC 6265 URL domain/path/Secure rules and drops expired entries, while SetCookie formats attributes for ArkWeb configCookieSync.
func TestCookieStoreListFilteringAndSetHeader(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)

	store, err := b.App().NewDataStore(platform.DataStoreOptions{Ephemeral: true})
	if err != nil {
		t.Fatalf("NewDataStore: %v", err)
	}

	exp := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	var setErr error
	setCalled := false
	b.App().SetCookie(store, platform.Cookie{
		Name:     "SESSDATA",
		Value:    "abc123",
		Domain:   ".bilibili.com",
		Path:     "/",
		Expires:  exp,
		Secure:   true,
		HTTPOnly: true,
		SameSite: "lax",
	}, func(err error) {
		setCalled = true
		setErr = err
	})

	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindCookieReq {
		t.Fatalf("SetCookie read: kind=%d err=%v", kind, err)
	}
	creq, ok := decodeCookieReqMessage(payload)
	if !ok || creq.op != cookieOpSet || !creq.incognito || creq.url != "https://bilibili.com/" {
		t.Fatalf("SetCookie wire = %+v (ok=%v)", creq, ok)
	}
	b.onCookieResult(encodeCookieResultMessage(creq.reqID, "", nil))
	if !setCalled || setErr != nil {
		t.Fatalf("SetCookie completion: called=%v err=%v", setCalled, setErr)
	}

	var got []platform.Cookie
	var listErr error
	b.App().ListCookies(store, "https://api.bilibili.com/x/web-interface/nav", func(cs []platform.Cookie, err error) {
		got = cs
		listErr = err
	})
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindCookieReq {
		t.Fatalf("ListCookies read: kind=%d err=%v", kind, err)
	}
	lreq, ok := decodeCookieReqMessage(payload)
	if !ok || lreq.op != cookieOpList || !lreq.incognito {
		t.Fatalf("ListCookies wire = %+v (ok=%v)", lreq, ok)
	}

	b.onCookieResult(encodeCookieResultMessage(lreq.reqID, "", []platform.Cookie{
		{Name: "SESSDATA", Value: "abc123", Domain: ".bilibili.com", Path: "/", Expires: exp, Secure: true, HTTPOnly: true, SameSite: "lax"},
		{Name: "bili_jct", Value: "csrf99", Domain: ".bilibili.com", Path: "/"},
		{Name: "MUSIC_U", Value: "other", Domain: ".music.163.com", Path: "/"},
		{Name: "expired", Value: "old", Domain: ".bilibili.com", Path: "/", Expires: time.Now().Add(-time.Hour)},
	}))
	if listErr != nil || len(got) != 2 || got[0].Name != "SESSDATA" || got[1].Name != "bili_jct" {
		t.Fatalf("ListCookies filtered = %+v (err=%v), want [SESSDATA, bili_jct]", got, listErr)
	}
}
