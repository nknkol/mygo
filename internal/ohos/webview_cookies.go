//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/egoist/mygo/internal/platform"
)

type dataStore struct {
	b         *Backend
	id        string
	ephemeral bool
	closed    bool
}

func (s *dataStore) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	if s.ephemeral && s.b != nil {
		s.b.startCookieCall(cookieReqWire{
			op:            cookieOpClearData,
			incognito:     true,
			siteDataTypes: uint32(platform.SiteDataAll),
		}, pendingCookieCall{})
	}
}

func isIncognitoStore(store platform.DataStore) bool {
	ds, ok := store.(*dataStore)
	return ok && ds != nil && ds.ephemeral
}

type cookieReqWire struct {
	reqID         uint32
	op            byte
	incognito     bool
	siteDataTypes uint32
	url           string
	headerOrName  string
	domain        string
	path          string
}

type pendingCookieCall struct {
	listDone func([]platform.Cookie, error)
	errDone  func(error)
	matchURL *url.URL
}

func (a appController) NewDataStore(opts platform.DataStoreOptions) (platform.DataStore, error) {
	return &dataStore{
		b:         a.b,
		id:        opts.ID,
		ephemeral: opts.Ephemeral,
	}, nil
}

func (a appController) ListCookies(store platform.DataStore, rawURL string, done func([]platform.Cookie, error)) {
	if a.b == nil || a.b.conn == nil {
		done(nil, platform.ErrUnsupported)
		return
	}
	var match *url.URL
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil {
			done(nil, err)
			return
		}
		match = u
	}
	a.b.startCookieCall(cookieReqWire{
		op:        cookieOpList,
		incognito: isIncognitoStore(store),
		url:       rawURL,
	}, pendingCookieCall{
		listDone: done,
		matchURL: match,
	})
}

func cookieOriginURL(domain, path string, secure bool) string {
	host := strings.TrimPrefix(strings.ToLower(domain), ".")
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	scheme := "http"
	if secure {
		scheme = "https"
	}
	if path == "" || path[0] != '/' {
		path = "/"
	}
	return scheme + "://" + host + path
}

func formatSetCookieHeader(c platform.Cookie) string {
	path := c.Path
	if path == "" {
		path = "/"
	}
	var b strings.Builder
	b.WriteString(c.Name)
	b.WriteByte('=')
	b.WriteString(c.Value)
	b.WriteString("; Path=")
	b.WriteString(path)
	if strings.HasPrefix(c.Domain, ".") {
		b.WriteString("; Domain=")
		b.WriteString(c.Domain)
	}
	if !c.Expires.IsZero() {
		b.WriteString("; Expires=")
		b.WriteString(c.Expires.UTC().Format(http.TimeFormat))
	}
	switch strings.ToLower(c.SameSite) {
	case "strict":
		b.WriteString("; SameSite=Strict")
	case "none":
		b.WriteString("; SameSite=None")
	case "lax":
		b.WriteString("; SameSite=Lax")
	}
	if c.Secure {
		b.WriteString("; Secure")
	}
	if c.HTTPOnly {
		b.WriteString("; HttpOnly")
	}
	return b.String()
}

func (a appController) SetCookie(store platform.DataStore, c platform.Cookie, done func(error)) {
	if a.b == nil || a.b.conn == nil {
		done(platform.ErrUnsupported)
		return
	}
	a.b.startCookieCall(cookieReqWire{
		op:           cookieOpSet,
		incognito:    isIncognitoStore(store),
		url:          cookieOriginURL(c.Domain, c.Path, c.Secure),
		headerOrName: formatSetCookieHeader(c),
		domain:       c.Domain,
		path:         c.Path,
	}, pendingCookieCall{errDone: done})
}

func (a appController) DeleteCookie(store platform.DataStore, key platform.CookieKey, done func(error)) {
	if a.b == nil || a.b.conn == nil {
		done(platform.ErrUnsupported)
		return
	}
	a.b.startCookieCall(cookieReqWire{
		op:           cookieOpDelete,
		incognito:    isIncognitoStore(store),
		url:          cookieOriginURL(key.Domain, key.Path, true),
		headerOrName: key.Name,
		domain:       key.Domain,
		path:         key.Path,
	}, pendingCookieCall{errDone: done})
}

func (a appController) ClearCookies(store platform.DataStore, done func(error)) {
	if a.b == nil || a.b.conn == nil {
		done(platform.ErrUnsupported)
		return
	}
	a.b.startCookieCall(cookieReqWire{
		op:        cookieOpClear,
		incognito: isIncognitoStore(store),
	}, pendingCookieCall{errDone: done})
}

func (a appController) ClearBrowsingData(store platform.DataStore, types platform.SiteData, done func(error)) {
	if a.b == nil || a.b.conn == nil {
		done(platform.ErrUnsupported)
		return
	}
	a.b.startCookieCall(cookieReqWire{
		op:            cookieOpClearData,
		incognito:     isIncognitoStore(store),
		siteDataTypes: uint32(types),
	}, pendingCookieCall{errDone: done})
}

func (b *Backend) startCookieCall(req cookieReqWire, cb pendingCookieCall) {
	b.mu.Lock()
	b.nextCookieReq++
	reqID := b.nextCookieReq
	if cb.listDone != nil || cb.errDone != nil {
		if b.pendingCookies == nil {
			b.pendingCookies = make(map[uint32]pendingCookieCall)
		}
		b.pendingCookies[reqID] = cb
	}
	b.mu.Unlock()

	req.reqID = reqID
	b.send(kindCookieReq, encodeCookieReqMessage(req))
}

func (b *Backend) onCookieResult(payload []byte) {
	reqID, errStr, cookies, ok := decodeCookieResultMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	cb, found := b.pendingCookies[reqID]
	if found {
		delete(b.pendingCookies, reqID)
	}
	b.mu.Unlock()
	if !found {
		return
	}

	var err error
	if errStr != "" {
		err = errors.New(errStr)
	}
	if cb.listDone != nil {
		if err != nil {
			cb.listDone(nil, err)
			return
		}
		now := time.Now()
		out := make([]platform.Cookie, 0, len(cookies))
		for _, c := range cookies {
			if !c.Expires.IsZero() && !c.Expires.After(now) {
				continue
			}
			if cb.matchURL != nil && c.Domain != "" && !platform.CookieMatchesURL(c, cb.matchURL) {
				continue
			}
			out = append(out, c)
		}
		cb.listDone(out, nil)
		return
	}
	if cb.errDone != nil {
		cb.errDone(err)
	}
}

func (b *Backend) onWebEvent(payload []byte) {
	winID, ev, canBack, canForward, errCode, text, desc, ok := decodeWebEventMessage(payload)
	if !ok {
		return
	}
	w := b.windowByID(winID)
	if w == nil || w.closed || !w.isWeb {
		return
	}

	switch ev {
	case webEvPageBegin:
		if text != "" {
			w.currentURL = text
		}
		w.loading = true
		w.canBack = canBack
		w.canForward = canForward
		if w.h != nil {
			w.h.NavigationStarted(text)
			w.h.NavigationCommitted(text)
		}
	case webEvPageEnd:
		if text != "" {
			w.currentURL = text
		}
		w.loading = false
		w.canBack = canBack
		w.canForward = canForward
		if w.h != nil {
			w.h.LoadFinished()
		}
	case webEvTitle:
		if w.h != nil {
			w.h.TitleChanged(text)
		}
	case webEvError:
		w.loading = false
		if w.h != nil {
			w.h.LoadFailed(text, int(errCode), desc)
		}
	default:
		w.onHybridWebEvent(ev, errCode, text, desc)
	}
}

func encodeCookieReqMessage(req cookieReqWire) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], req.reqID)
	buf[4] = req.op
	if req.incognito {
		buf[5] = 1
	}
	binary.LittleEndian.PutUint32(buf[8:], req.siteDataTypes)
	buf = appendWireStr(buf, req.url)
	buf = appendWireStr(buf, req.headerOrName)
	buf = appendWireStr(buf, req.domain)
	buf = appendWireStr(buf, req.path)
	return buf
}

func decodeCookieReqMessage(p []byte) (cookieReqWire, bool) {
	if len(p) < 12 {
		return cookieReqWire{}, false
	}
	var out cookieReqWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.op = p[4]
	out.incognito = p[5] != 0
	out.siteDataTypes = binary.LittleEndian.Uint32(p[8:])
	off := 12
	var ok bool
	if out.url, off, ok = readWireStr(p, off); !ok {
		return cookieReqWire{}, false
	}
	if out.headerOrName, off, ok = readWireStr(p, off); !ok {
		return cookieReqWire{}, false
	}
	if out.domain, off, ok = readWireStr(p, off); !ok {
		return cookieReqWire{}, false
	}
	if out.path, off, ok = readWireStr(p, off); !ok || off != len(p) {
		return cookieReqWire{}, false
	}
	return out, true
}

func encodeCookieResultMessage(reqID uint32, errStr string, cookies []platform.Cookie) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	buf = appendWireStr(buf, errStr)
	buf = appendUint32(buf, uint32(len(cookies)))
	for _, c := range cookies {
		var hdr [12]byte
		var expUnix int64
		if !c.Expires.IsZero() {
			expUnix = c.Expires.UTC().Unix()
		}
		binary.LittleEndian.PutUint64(hdr[0:], uint64(expUnix))
		var flags byte
		if c.Secure {
			flags |= 1
		}
		if c.HTTPOnly {
			flags |= 2
		}
		hdr[8] = flags
		buf = append(buf, hdr[:]...)
		buf = appendWireStr(buf, c.Name)
		buf = appendWireStr(buf, c.Value)
		buf = appendWireStr(buf, c.Domain)
		buf = appendWireStr(buf, c.Path)
		buf = appendWireStr(buf, c.SameSite)
	}
	return buf
}

func decodeCookieResultMessage(p []byte) (reqID uint32, errStr string, cookies []platform.Cookie, ok bool) {
	if len(p) < 8 {
		return 0, "", nil, false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	off := 4
	errStr, off, ok = readWireStr(p, off)
	if !ok {
		return 0, "", nil, false
	}
	count, off, ok := readUint32(p, off)
	if !ok || count > 8192 {
		return 0, "", nil, false
	}
	cookies = make([]platform.Cookie, 0, count)
	for range count {
		if off+12 > len(p) {
			return 0, "", nil, false
		}
		expUnix := int64(binary.LittleEndian.Uint64(p[off:]))
		flags := p[off+8]
		off += 12
		var c platform.Cookie
		c.Secure = flags&1 != 0
		c.HTTPOnly = flags&2 != 0
		if expUnix > 0 {
			c.Expires = time.Unix(expUnix, 0).UTC()
		}
		if c.Name, off, ok = readWireStr(p, off); !ok {
			return 0, "", nil, false
		}
		if c.Value, off, ok = readWireStr(p, off); !ok {
			return 0, "", nil, false
		}
		if c.Domain, off, ok = readWireStr(p, off); !ok {
			return 0, "", nil, false
		}
		if c.Path, off, ok = readWireStr(p, off); !ok {
			return 0, "", nil, false
		}
		if c.SameSite, off, ok = readWireStr(p, off); !ok {
			return 0, "", nil, false
		}
		if c.Path == "" {
			c.Path = "/"
		}
		cookies = append(cookies, c)
	}
	if off != len(p) {
		return 0, "", nil, false
	}
	return reqID, errStr, cookies, true
}

func encodeWebReqMessage(winID uint32, op byte, arg1, arg2 string) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:], winID)
	buf[4] = op
	buf = appendWireStr(buf, arg1)
	buf = appendWireStr(buf, arg2)
	return buf
}

func decodeWebReqMessage(p []byte) (winID uint32, op byte, arg1, arg2 string, ok bool) {
	if len(p) < 8 {
		return 0, 0, "", "", false
	}
	winID = binary.LittleEndian.Uint32(p[0:])
	op = p[4]
	off := 8
	if arg1, off, ok = readWireStr(p, off); !ok {
		return 0, 0, "", "", false
	}
	if arg2, off, ok = readWireStr(p, off); !ok || off != len(p) {
		return 0, 0, "", "", false
	}
	return winID, op, arg1, arg2, true
}

func encodeWebEventMessage(winID uint32, ev byte, canBack, canForward bool, errCode int32, text, desc string) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], winID)
	buf[4] = ev
	if canBack {
		buf[5] = 1
	}
	if canForward {
		buf[6] = 1
	}
	binary.LittleEndian.PutUint32(buf[8:], uint32(errCode))
	buf = appendWireStr(buf, text)
	buf = appendWireStr(buf, desc)
	return buf
}

func decodeWebEventMessage(p []byte) (winID uint32, ev byte, canBack, canForward bool, errCode int32, text, desc string, ok bool) {
	if len(p) < 12 {
		return 0, 0, false, false, 0, "", "", false
	}
	winID = binary.LittleEndian.Uint32(p[0:])
	ev = p[4]
	canBack = p[5] != 0
	canForward = p[6] != 0
	errCode = int32(binary.LittleEndian.Uint32(p[8:]))
	off := 12
	if text, off, ok = readWireStr(p, off); !ok {
		return 0, 0, false, false, 0, "", "", false
	}
	if desc, off, ok = readWireStr(p, off); !ok || off != len(p) {
		return 0, 0, false, false, 0, "", "", false
	}
	return winID, ev, canBack, canForward, errCode, text, desc, true
}
