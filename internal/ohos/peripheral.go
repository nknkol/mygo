//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/egoist/mygo/internal/accelerator"
	"github.com/egoist/mygo/internal/platform"
)

const (
	secretOpSet byte = 1
	secretOpGet byte = 2
	secretOpDel byte = 3
)

const (
	trayOpUpsert  byte = 1
	trayOpDestroy byte = 2

	trayFlagHasImage    byte = 1 << 0
	trayFlagTemplateImg byte = 1 << 1
	trayFlagHasMenu     byte = 1 << 2

	trayEvClick      byte = 1
	trayEvRightClick byte = 2
	trayEvMenuItem   byte = 3
)

type secretWireRes struct {
	notFound bool
	err      string
	secret   []byte
}

func (b *Backend) Secrets() platform.Secrets { return secretsBackend{b: b} }

type secretsBackend struct{ b *Backend }

func (s secretsBackend) SetSecret(service, account string, secret []byte) error {
	if service == "" && account == "" {
		return errors.New("ohos: empty secret service and account")
	}
	if len(secret) > 1023 {
		return errors.New("ohos: secret exceeds HarmonyOS Asset Store 1023-byte limit")
	}
	_, err := s.call(secretOpSet, service, account, secret)
	return err
}

func (s secretsBackend) Secret(service, account string) ([]byte, error) {
	if service == "" && account == "" {
		return nil, platform.ErrSecretNotFound
	}
	res, err := s.call(secretOpGet, service, account, nil)
	if err != nil {
		return nil, err
	}
	if res.notFound {
		return nil, platform.ErrSecretNotFound
	}
	return bytes.Clone(res.secret), nil
}

func (s secretsBackend) DeleteSecret(service, account string) error {
	if service == "" && account == "" {
		return nil
	}
	_, err := s.call(secretOpDel, service, account, nil)
	return err
}

func (s secretsBackend) call(op byte, service, account string, secret []byte) (secretWireRes, error) {
	b := s.b
	if b == nil {
		return secretWireRes{}, platform.ErrUnsupported
	}
	b.ensureReadLoop()
	b.mu.Lock()
	if b.conn == nil || b.quit {
		b.mu.Unlock()
		return secretWireRes{}, errors.New("ohos: host connection unavailable")
	}
	b.nextSecretReq++
	reqID := b.nextSecretReq
	ch := make(chan secretWireRes, 1)
	if b.secretWaiters == nil {
		b.secretWaiters = make(map[uint32]chan secretWireRes)
	}
	b.secretWaiters[reqID] = ch
	b.mu.Unlock()

	b.send(kindSecretReq, encodeSecretReqMessage(reqID, op, service, account, secret))
	res, ok := <-ch
	if !ok {
		return secretWireRes{}, errors.New("ohos: connection closed while awaiting secret store")
	}
	if res.err != "" {
		return secretWireRes{}, errors.New(res.err)
	}
	return res, nil
}

func (b *Backend) handleSecretRes(payload []byte) {
	reqID, res, ok := decodeSecretResMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	ch := b.secretWaiters[reqID]
	if ch != nil {
		delete(b.secretWaiters, reqID)
	}
	b.mu.Unlock()
	if ch != nil {
		ch <- res
		close(ch)
	}
}

func encodeSecretReqMessage(reqID uint32, op byte, service, account string, secret []byte) []byte {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:], reqID)
	hdr[4] = op
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, service)
	buf = appendWireStr(buf, account)
	buf = appendWireBytes(buf, secret)
	return buf
}

func decodeSecretReqMessage(p []byte) (reqID uint32, op byte, service, account string, secret []byte, ok bool) {
	if len(p) < 8 {
		return 0, 0, "", "", nil, false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	op = p[4]
	off := 8
	if service, off, ok = readWireStr(p, off); !ok {
		return 0, 0, "", "", nil, false
	}
	if account, off, ok = readWireStr(p, off); !ok {
		return 0, 0, "", "", nil, false
	}
	if secret, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return 0, 0, "", "", nil, false
	}
	return reqID, op, service, account, secret, true
}

func encodeSecretResMessage(reqID uint32, res secretWireRes) []byte {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:], reqID)
	if res.notFound {
		hdr[4] = 1
	}
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, res.err)
	buf = appendWireBytes(buf, res.secret)
	return buf
}

func decodeSecretResMessage(p []byte) (uint32, secretWireRes, bool) {
	if len(p) < 8 {
		return 0, secretWireRes{}, false
	}
	reqID := binary.LittleEndian.Uint32(p[0:])
	var res secretWireRes
	res.notFound = p[4] != 0
	off := 8
	var ok bool
	if res.err, off, ok = readWireStr(p, off); !ok {
		return 0, secretWireRes{}, false
	}
	if res.secret, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return 0, secretWireRes{}, false
	}
	return reqID, res, true
}

// Global Hotkeys (MultimodalInput OH_Input_AddHotkeyMonitor).

func (b *Backend) RegisterHotkey(id int, acc string) error {
	if b == nil {
		return platform.ErrUnsupported
	}
	parsed, err := accelerator.Parse(acc, "linux")
	if err != nil {
		return err
	}
	finalKey, preKeys, err := ohosHotkeyCodes(parsed)
	if err != nil {
		return err
	}
	b.ensureReadLoop()
	b.mu.Lock()
	if b.conn == nil || b.quit {
		b.mu.Unlock()
		return errors.New("ohos: host connection unavailable")
	}
	b.nextHotkeyReq++
	reqID := b.nextHotkeyReq
	ch := make(chan string, 1)
	if b.hotkeyWaiters == nil {
		b.hotkeyWaiters = make(map[uint32]chan string)
	}
	b.hotkeyWaiters[reqID] = ch
	b.mu.Unlock()

	b.send(kindHotkeyReq, encodeHotkeyReqMessage(reqID, int32(id), true, finalKey, preKeys))
	errStr, ok := <-ch
	if !ok {
		return errors.New("ohos: connection closed while registering hotkey")
	}
	if errStr != "" {
		return errors.New(errStr)
	}
	return nil
}

func (b *Backend) UnregisterHotkey(id int) {
	if b == nil || b.conn == nil {
		return
	}
	b.send(kindHotkeyReq, encodeHotkeyReqMessage(0, int32(id), false, 0, nil))
}

func (b *Backend) handleHotkeyRes(payload []byte) {
	reqID, errStr, ok := decodeHotkeyResMessage(payload)
	if !ok || reqID == 0 {
		return
	}
	b.mu.Lock()
	ch := b.hotkeyWaiters[reqID]
	if ch != nil {
		delete(b.hotkeyWaiters, reqID)
	}
	b.mu.Unlock()
	if ch != nil {
		ch <- errStr
		close(ch)
	}
}

func (b *Backend) onHotkeyTrig(payload []byte) {
	if len(payload) < 4 {
		return
	}
	id := int(int32(binary.LittleEndian.Uint32(payload[0:])))
	b.mu.Lock()
	app := b.app
	b.mu.Unlock()
	if app != nil {
		app.HotkeyPressed(id)
	}
}

func ohosHotkeyCodes(a accelerator.Accelerator) (finalKey int32, preKeys []int32, err error) {
	if a.Has(accelerator.Ctrl) {
		preKeys = append(preKeys, 2072) // KEYCODE_CTRL_LEFT
	}
	if a.Has(accelerator.Alt) {
		preKeys = append(preKeys, 2045) // KEYCODE_ALT_LEFT
	}
	if a.Has(accelerator.Shift) {
		preKeys = append(preKeys, 2047) // KEYCODE_SHIFT_LEFT
	}
	if a.Has(accelerator.Super) {
		preKeys = append(preKeys, 2076) // KEYCODE_META_LEFT
	}
	code, ok := mapAcceleratorKeyToOHOS(a.Key)
	if !ok {
		return 0, nil, fmt.Errorf("ohos: unsupported hotkey key %q", a.Key)
	}
	return code, preKeys, nil
}

func mapAcceleratorKeyToOHOS(k string) (int32, bool) {
	if len(k) == 1 {
		ch := k[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return 2017 + int32(ch-'a'), true
		case ch >= '0' && ch <= '9':
			return 2000 + int32(ch-'0'), true
		}
		switch ch {
		case ',':
			return 2043, true
		case '.':
			return 2044, true
		case '`':
			return 2056, true
		case '-':
			return 2057, true
		case '=':
			return 2058, true
		case '[':
			return 2059, true
		case ']':
			return 2060, true
		case '\\':
			return 2061, true
		case ';':
			return 2062, true
		case '\'':
			return 2063, true
		case '/':
			return 2064, true
		case '+':
			return 2066, true
		}
	}
	if len(k) >= 2 && k[0] == 'F' {
		var n int
		if _, err := fmt.Sscanf(k[1:], "%d", &n); err == nil && n >= 1 && n <= 12 {
			return 2090 + int32(n-1), true
		}
	}
	if len(k) == 4 && k[:3] == "Num" && k[3] >= '0' && k[3] <= '9' {
		return 2103 + int32(k[3]-'0'), true
	}
	switch k {
	case "Up":
		return 2012, true
	case "Down":
		return 2013, true
	case "Left":
		return 2014, true
	case "Right":
		return 2015, true
	case "Tab":
		return 2049, true
	case "Space":
		return 2050, true
	case "Enter":
		return 2054, true
	case "Backspace":
		return 2055, true
	case "PageUp":
		return 2068, true
	case "PageDown":
		return 2069, true
	case "Escape":
		return 2070, true
	case "Delete":
		return 2071, true
	case "CapsLock":
		return 2074, true
	case "ScrollLock":
		return 2075, true
	case "PrintScreen":
		return 2079, true
	case "Home":
		return 2081, true
	case "End":
		return 2082, true
	case "Insert":
		return 2083, true
	case "NumLock":
		return 2102, true
	case "NumDiv":
		return 2113, true
	case "NumMult":
		return 2114, true
	case "NumSub":
		return 2115, true
	case "NumAdd":
		return 2116, true
	case "NumDec":
		return 2117, true
	case "MediaPlayPause":
		return 10, true
	case "MediaStop":
		return 11, true
	case "MediaNextTrack":
		return 12, true
	case "MediaPreviousTrack":
		return 13, true
	case "VolumeUp":
		return 16, true
	case "VolumeDown":
		return 17, true
	case "VolumeMute":
		return 22, true
	}
	return 0, false
}

func encodeHotkeyReqMessage(reqID uint32, hotkeyID int32, register bool, finalKey int32, preKeys []int32) []byte {
	buf := make([]byte, 20+len(preKeys)*4)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	binary.LittleEndian.PutUint32(buf[4:], uint32(hotkeyID))
	if register {
		buf[8] = 1
	}
	binary.LittleEndian.PutUint32(buf[12:], uint32(finalKey))
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(preKeys)))
	for i, pk := range preKeys {
		binary.LittleEndian.PutUint32(buf[20+i*4:], uint32(pk))
	}
	return buf
}

func decodeHotkeyReqMessage(p []byte) (reqID uint32, hotkeyID int32, register bool, finalKey int32, preKeys []int32, ok bool) {
	if len(p) < 20 {
		return 0, 0, false, 0, nil, false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	hotkeyID = int32(binary.LittleEndian.Uint32(p[4:]))
	register = p[8] != 0
	finalKey = int32(binary.LittleEndian.Uint32(p[12:]))
	n := binary.LittleEndian.Uint32(p[16:])
	if n > 8 || 20+int(n)*4 != len(p) {
		return 0, 0, false, 0, nil, false
	}
	preKeys = make([]int32, n)
	for i := range int(n) {
		preKeys[i] = int32(binary.LittleEndian.Uint32(p[20+i*4:]))
	}
	return reqID, hotkeyID, register, finalKey, preKeys, true
}

func encodeHotkeyResMessage(reqID uint32, errStr string) []byte {
	var buf []byte
	buf = appendUint32(buf, reqID)
	buf = appendWireStr(buf, errStr)
	return buf
}

func decodeHotkeyResMessage(p []byte) (uint32, string, bool) {
	reqID, off, ok := readUint32(p, 0)
	if !ok {
		return 0, "", false
	}
	errStr, off, ok := readWireStr(p, off)
	if !ok || off != len(p) {
		return 0, "", false
	}
	return reqID, errStr, true
}

// System Tray (@kit.DeskTopExtensionKit statusBarManager).

type trayItem struct {
	b         *Backend
	trayID    uint32
	h         platform.TrayHandler
	id        string
	title     string
	tooltip   string
	png       []byte
	template  bool
	hasImage  bool
	menuItems []*platform.MenuItem
	hasMenu   bool
	destroyed bool
}

type trayWire struct {
	trayID   uint32
	op       byte
	flags    byte
	id       string
	title    string
	tooltip  string
	png      []byte
	menuWire []popupWireItem
}

func (b *Backend) NewTray(h platform.TrayHandler) (platform.Tray, error) {
	if b == nil {
		return nil, platform.ErrUnsupported
	}
	b.mu.Lock()
	b.nextTrayID++
	id := b.nextTrayID
	tr := &trayItem{
		b:      b,
		trayID: id,
		h:      h,
	}
	if b.trays == nil {
		b.trays = make(map[uint32]*trayItem)
	}
	b.trays[id] = tr
	b.mu.Unlock()
	return tr, nil
}

func (t *trayItem) SetID(id string) {
	if t == nil || t.destroyed {
		return
	}
	t.id = id
	t.sync(false, false)
}

func (t *trayItem) Dark() bool {
	if t == nil || t.b == nil {
		return false
	}
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	return t.b.systemDark
}

func (t *trayItem) SetImage(png []byte, template bool) error {
	if t == nil || t.destroyed {
		return nil
	}
	t.png = bytes.Clone(png)
	t.template = template
	t.hasImage = len(png) > 0
	t.sync(true, false)
	return nil
}

func (t *trayItem) SetTitle(title string) {
	if t == nil || t.destroyed {
		return
	}
	t.title = title
	t.sync(false, false)
}

func (t *trayItem) SetToolTip(tip string) {
	if t == nil || t.destroyed {
		return
	}
	t.tooltip = tip
	t.sync(false, false)
}

func (t *trayItem) SetMenu(m *platform.Menu) {
	if t == nil || t.destroyed {
		return
	}
	if m == nil {
		t.menuItems = nil
		t.hasMenu = false
	} else {
		t.menuItems = cloneMenuItems(m.Items)
		t.hasMenu = true
	}
	t.sync(false, true)
}

func (t *trayItem) PopUpMenu(m *platform.Menu) {
	if t == nil || t.destroyed || m == nil || t.b == nil {
		return
	}
	t.SetMenu(m)
}

func (t *trayItem) SetActivatable(bool) {}

func (t *trayItem) Bounds() platform.Rect { return platform.Rect{} }

func (t *trayItem) Destroy() {
	if t == nil || t.destroyed {
		return
	}
	t.destroyed = true
	if t.b != nil {
		t.b.mu.Lock()
		delete(t.b.trays, t.trayID)
		t.b.mu.Unlock()
		t.b.send(kindTrayUpdate, encodeTrayUpdateMessage(trayWire{
			trayID: t.trayID,
			op:     trayOpDestroy,
		}))
	}
}

func (t *trayItem) sync(includeImage, includeMenu bool) {
	if t.b == nil || t.destroyed {
		return
	}
	var flags byte
	var img []byte
	if includeImage && t.hasImage {
		flags |= trayFlagHasImage
		if t.template {
			flags |= trayFlagTemplateImg
		}
		img = t.png
	}
	var menuWire []popupWireItem
	if includeMenu {
		flags |= trayFlagHasMenu
		menuWire = buildPopupWireItems(t.menuItems)
	}
	t.b.send(kindTrayUpdate, encodeTrayUpdateMessage(trayWire{
		trayID:   t.trayID,
		op:       trayOpUpsert,
		flags:    flags,
		id:       t.id,
		title:    t.title,
		tooltip:  t.tooltip,
		png:      img,
		menuWire: menuWire,
	}))
}

func (b *Backend) UpdateMenuItem(item *platform.MenuItem) {
	if b == nil || item == nil {
		return
	}
	b.mu.Lock()
	if b.appMenu != nil {
		updateMenuItemInTree(b.appMenu.Items, item)
	}
	for _, w := range b.windows {
		if w != nil && w.menu != nil {
			updateMenuItemInTree(w.menu.Items, item)
		}
	}
	trays := make([]*trayItem, 0, len(b.trays))
	for _, tr := range b.trays {
		trays = append(trays, tr)
	}
	b.mu.Unlock()
	for _, tr := range trays {
		if tr == nil || tr.destroyed || !tr.hasMenu {
			continue
		}
		if updateMenuItemInTree(tr.menuItems, item) {
			tr.sync(false, true)
		}
	}
}

func matchMenuAccelerator(w *Window, arkCode int32, unicode uint32, mods platform.Modifiers) bool {
	if w == nil || w.closed || w.b == nil {
		return false
	}
	b := w.b
	b.mu.Lock()
	var items []*platform.MenuItem
	fromWinMenu := false
	if w.menu != nil {
		items = cloneMenuItems(w.menu.Items)
		fromWinMenu = true
	} else if b.appMenu != nil && !w.opts.Panel && w.opts.Flyout == nil {
		items = cloneMenuItems(b.appMenu.Items)
	}
	app := b.app
	b.mu.Unlock()
	if len(items) == 0 {
		return false
	}
	id, ok := findMatchingMenuAccelerator(items, arkCode, unicode, mods)
	if !ok {
		return false
	}
	if fromWinMenu || app == nil {
		if w.h != nil {
			w.h.MenuItemClicked(id)
			return true
		}
		return false
	}
	app.MenuItemClicked(id)
	return true
}

func findMatchingMenuAccelerator(items []*platform.MenuItem, arkCode int32, unicode uint32, mods platform.Modifiers) (int, bool) {
	for _, it := range items {
		if it == nil || !it.Visible || !it.Enabled {
			continue
		}
		if it.Type == platform.MenuItemSubmenu {
			if it.Submenu != nil {
				if id, ok := findMatchingMenuAccelerator(it.Submenu.Items, arkCode, unicode, mods); ok {
					return id, true
				}
			}
			continue
		}
		if it.Type == platform.MenuItemSeparator || it.ID == 0 || it.Accelerator == "" {
			continue
		}
		if acceleratorMatchesKey(it.Accelerator, arkCode, unicode, mods) {
			return it.ID, true
		}
	}
	return 0, false
}

func acceleratorMatchesKey(spec string, arkCode int32, unicode uint32, mods platform.Modifiers) bool {
	acc, err := accelerator.Parse(spec, "linux")
	if err != nil {
		return false
	}
	var wantMods platform.Modifiers
	if acc.Has(accelerator.Super) {
		wantMods |= platform.ModSuper
	}
	if acc.Has(accelerator.Ctrl) {
		wantMods |= platform.ModCtrl
	}
	if acc.Has(accelerator.Alt) {
		wantMods |= platform.ModAlt
	}
	if acc.Has(accelerator.Shift) {
		wantMods |= platform.ModShift
	}
	heldMods := mods & (platform.ModSuper | platform.ModCtrl | platform.ModAlt | platform.ModShift)
	if acc.Key == "+" {
		if arkCode == 2066 || arkCode == 2116 {
			return heldMods == wantMods
		}
		if unicode == '+' || (arkCode == 2058 && heldMods&platform.ModShift != 0) {
			return (heldMods &^ platform.ModShift) == (wantMods &^ platform.ModShift)
		}
		return false
	}
	if heldMods != wantMods {
		return false
	}
	wantCode, ok := mapAcceleratorKeyToOHOS(acc.Key)
	return ok && wantCode == arkCode
}

func cloneMenuItems(items []*platform.MenuItem) []*platform.MenuItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*platform.MenuItem, len(items))
	for i, it := range items {
		if it == nil {
			continue
		}
		cp := *it
		if it.Submenu != nil {
			cp.Submenu = &platform.Menu{
				RightToLeft: it.Submenu.RightToLeft,
				Items:       cloneMenuItems(it.Submenu.Items),
			}
		}
		out[i] = &cp
	}
	return out
}

func updateMenuItemInTree(items []*platform.MenuItem, target *platform.MenuItem) bool {
	updated := false
	for i, it := range items {
		if it == nil {
			continue
		}
		if it.ID != 0 && it.ID == target.ID {
			cp := *target
			if target.Submenu != nil {
				cp.Submenu = &platform.Menu{
					RightToLeft: target.Submenu.RightToLeft,
					Items:       cloneMenuItems(target.Submenu.Items),
				}
			}
			items[i] = &cp
			updated = true
			continue
		}
		if it.Submenu != nil && updateMenuItemInTree(it.Submenu.Items, target) {
			updated = true
		}
	}
	return updated
}

func (b *Backend) onTrayEvent(payload []byte) {
	if len(payload) < 12 {
		return
	}
	trayID := binary.LittleEndian.Uint32(payload[0:])
	ev := payload[4]
	menuItemID := int(int32(binary.LittleEndian.Uint32(payload[8:])))

	b.mu.Lock()
	tr := b.trays[trayID]
	app := b.app
	b.mu.Unlock()

	if tr == nil || tr.destroyed {
		return
	}
	switch ev {
	case trayEvClick:
		if tr.h != nil {
			tr.h.Clicked()
		}
	case trayEvRightClick:
		if tr.h != nil {
			tr.h.RightClicked()
		}
	case trayEvMenuItem:
		if app != nil && menuItemID != 0 {
			app.MenuItemClicked(menuItemID)
		}
	}
}

func encodeTrayUpdateMessage(w trayWire) []byte {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:], w.trayID)
	hdr[4] = w.op
	hdr[5] = w.flags
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, w.id)
	buf = appendWireStr(buf, w.title)
	buf = appendWireStr(buf, w.tooltip)
	buf = appendWireBytes(buf, w.png)
	buf = appendPopupWireSlice(buf, w.menuWire)
	return buf
}

func decodeTrayUpdateMessage(p []byte) (trayWire, bool) {
	if len(p) < 8 {
		return trayWire{}, false
	}
	var w trayWire
	w.trayID = binary.LittleEndian.Uint32(p[0:])
	w.op = p[4]
	w.flags = p[5]
	off := 8
	var ok bool
	if w.id, off, ok = readWireStr(p, off); !ok {
		return trayWire{}, false
	}
	if w.title, off, ok = readWireStr(p, off); !ok {
		return trayWire{}, false
	}
	if w.tooltip, off, ok = readWireStr(p, off); !ok {
		return trayWire{}, false
	}
	if w.png, off, ok = readWireBytes(p, off); !ok {
		return trayWire{}, false
	}
	if w.menuWire, off, ok = readPopupWireSlice(p, off); !ok || off != len(p) {
		return trayWire{}, false
	}
	return w, slices.Contains([]byte{trayOpUpsert, trayOpDestroy}, w.op)
}

func encodeTrayEventMessage(trayID uint32, ev byte, menuItemID int32) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], trayID)
	buf[4] = ev
	binary.LittleEndian.PutUint32(buf[8:], uint32(menuItemID))
	return buf
}
