//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/egoist/mygo/internal/ohosproxy"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/unsupported"
)

func (b *Backend) App() platform.AppController {
	return appController{
		AppController: (*unsupported.Backend)(nil).App(),
		b:             b,
	}
}

func (b *Backend) Screen() platform.Screen { return screenBackend{b: b} }

type appController struct {
	platform.AppController
	b *Backend
}

func (a appController) Activate() {
	if a.b == nil {
		return
	}
	a.b.mu.Lock()
	win := a.b.focusedOrFirstWindowLocked()
	a.b.mu.Unlock()
	if win != nil {
		win.Focus()
	}
}

func (a appController) IsActive() bool {
	if a.b == nil {
		return false
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.hasFocusedWindowLocked()
}

func (a appController) PreferredLanguages() []string {
	if a.b == nil {
		return []string{"en-US"}
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if len(a.b.langs) == 0 {
		return []string{"en-US"}
	}
	return slices.Clone(a.b.langs)
}

func (a appController) Region() platform.Region {
	if a.b == nil {
		return platform.Region{FirstWeekday: -1}
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.region
}

func (a appController) Package() (platform.PackageInfo, bool) {
	if a.b == nil {
		return platform.PackageInfo{}, false
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	if !a.b.hasPkg {
		return platform.PackageInfo{}, false
	}
	return a.b.pkg, true
}

func (a appController) SetBadge(label string) {
	if a.b == nil {
		return
	}
	a.b.mu.Lock()
	changed := a.b.badge != label
	a.b.badge = label
	hasConn := a.b.conn != nil
	a.b.mu.Unlock()
	if changed && hasConn {
		a.b.send(kindShellReq, encodeShellReqMessage(0, shellSetBadge, label))
	}
}

func (a appController) Badge() string {
	if a.b == nil {
		return ""
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.badge
}

func (a appController) Hide() {
	if a.b == nil {
		return
	}
	a.b.mu.Lock()
	if a.b.appHidden {
		a.b.mu.Unlock()
		return
	}
	a.b.appHidden = true
	a.b.hiddenWins = a.b.hiddenWins[:0]
	var wins []*Window
	for _, id := range a.b.winOrder {
		if w := a.b.windows[id]; w != nil && w.host == nil && !w.closed && w.visible {
			a.b.hiddenWins = append(a.b.hiddenWins, id)
			wins = append(wins, w)
		}
	}
	wasActive := a.b.appActive
	a.b.appActive = false
	app := a.b.app
	a.b.mu.Unlock()

	for _, w := range wins {
		w.Hide()
	}
	if wasActive && app != nil {
		app.DidResignActive()
	}
}

func (a appController) Unhide() {
	if a.b == nil {
		return
	}
	a.b.mu.Lock()
	if !a.b.appHidden {
		a.b.mu.Unlock()
		return
	}
	a.b.appHidden = false
	ids := slices.Clone(a.b.hiddenWins)
	a.b.hiddenWins = a.b.hiddenWins[:0]
	var wins []*Window
	for _, id := range ids {
		if w := a.b.windows[id]; w != nil && w.host == nil && !w.closed {
			wins = append(wins, w)
		}
	}
	a.b.mu.Unlock()

	for i, w := range wins {
		if i == len(wins)-1 {
			w.Show()
		} else {
			w.ShowInactive()
		}
	}
}

func (a appController) IsHidden() bool {
	if a.b == nil {
		return false
	}
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.appHidden
}

func (a appController) ShowAboutPanel(opts platform.AboutPanelOptions) {
	if a.b == nil {
		return
	}
	name := strings.TrimSpace(opts.ApplicationName)
	ver := strings.TrimSpace(opts.ApplicationVersion)
	if pkg, ok := a.Package(); ok {
		if name == "" {
			name = pkg.Name
		}
		if ver == "" {
			ver = pkg.Version
		}
	}
	if name == "" {
		name = "Application"
	}
	var details []string
	if buildVer := strings.TrimSpace(opts.Version); buildVer != "" && buildVer != ver {
		if ver != "" {
			details = append(details, "Version "+ver+" ("+buildVer+")")
		} else {
			details = append(details, "Version "+buildVer)
		}
	} else if ver != "" {
		details = append(details, "Version "+ver)
	}
	if cp := strings.TrimSpace(opts.Copyright); cp != "" {
		details = append(details, cp)
	}
	if cr := strings.TrimSpace(opts.Credits); cr != "" {
		details = append(details, cr)
	}
	a.b.mu.Lock()
	win := a.b.focusedOrFirstWindowLocked()
	a.b.mu.Unlock()
	var pw platform.Window
	if win != nil {
		pw = win
	}
	a.b.Dialogs().ShowMessageBox(pw, &platform.MessageBoxOptions{
		Type:    "info",
		Title:   name,
		Message: name,
		Detail:  strings.Join(details, "\n"),
		Buttons: []string{"OK"},
	}, func(platform.MessageBoxResult, error) {})
}

func (a appController) SetLanguages(tags []string, rtl bool) {
	if a.b == nil {
		return
	}
	a.b.mu.Lock()
	a.b.appLangs = slices.Clone(tags)
	a.b.appRTL = rtl
	a.b.mu.Unlock()
}

type screenBackend struct{ b *Backend }

func (s screenBackend) Displays() []platform.Display {
	if s.b == nil {
		return nil
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if len(s.b.displays) > 0 {
		return slices.Clone(s.b.displays)
	}
	sc := s.b.initScale
	if sc <= 0 {
		sc = 1
	}
	wDip := int(math.Round(float64(s.b.initW) / sc))
	hDip := int(math.Round(float64(s.b.initH) / sc))
	if wDip <= 0 {
		wDip = 1280
	}
	if hDip <= 0 {
		hDip = 800
	}
	r := platform.Rect{Width: wDip, Height: hDip}
	return []platform.Display{{
		ID:          0,
		Label:       "Built-in Display",
		Bounds:      r,
		WorkArea:    r,
		ScaleFactor: sc,
		Internal:    true,
		Primary:     true,
	}}
}

func (s screenBackend) CursorPoint() platform.Point {
	if s.b == nil {
		return platform.Point{}
	}
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.b.cursorPoint
}

// loadInitialEnv populates package, locale, region, and display metadata passed
// by the host at fork/exec time so App.Package, App.PreferredLanguages,
// App.Region, and Screen.Displays work even before Backend.Init.
func (b *Backend) loadInitialEnv() {
	b.region = platform.Region{FirstWeekday: -1}
	pkgID := strings.TrimSpace(os.Getenv("MYGO_OHOS_PKG_ID"))
	pkgName := strings.TrimSpace(os.Getenv("MYGO_OHOS_PKG_NAME"))
	pkgVer := strings.TrimSpace(os.Getenv("MYGO_OHOS_PKG_VER"))
	if pkgID != "" || pkgName != "" {
		b.hasPkg = true
		b.pkg = platform.PackageInfo{
			Name:       pkgName,
			Version:    pkgVer,
			Identifier: pkgID,
		}
	}
	if rawLangs := strings.TrimSpace(os.Getenv("MYGO_OHOS_LANGS")); rawLangs != "" {
		for _, part := range strings.Split(rawLangs, ",") {
			if tag := strings.TrimSpace(part); tag != "" {
				b.langs = append(b.langs, tag)
			}
		}
	}
	if loc := strings.TrimSpace(os.Getenv("MYGO_OHOS_LOCALE")); loc != "" {
		b.region.Locale = loc
	}
	if hc, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MYGO_OHOS_HOUR_CYCLE"))); err == nil && (hc == 12 || hc == 24) {
		b.region.HourCycle = hc
	}
	if fw, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MYGO_OHOS_FIRST_WEEKDAY"))); err == nil && fw >= -1 && fw <= 6 {
		b.region.FirstWeekday = fw
	}
	// Format: id:x,y,w,h:wx,wy,ww,wh:scaleMilli:rot:flags:label;...
	if rawDisp := strings.TrimSpace(os.Getenv("MYGO_OHOS_DISPLAYS")); rawDisp != "" {
		for _, entry := range strings.Split(rawDisp, ";") {
			if d, ok := parseDisplaySpec(entry); ok {
				b.displays = append(b.displays, d)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("MYGO_OHOS_ON_BATTERY")); v == "1" || v == "true" {
		b.onBattery = true
	}
	b.netStatus = platform.NetworkStatus{Online: true}
	if v := strings.TrimSpace(os.Getenv("MYGO_OHOS_NET_ONLINE")); v == "0" || v == "false" {
		b.netStatus.Online = false
	}
	if v := strings.TrimSpace(os.Getenv("MYGO_OHOS_NET_METERED")); v == "1" || v == "true" {
		b.netStatus.Metered = true
	}
	if rawScale := strings.TrimSpace(os.Getenv("MYGO_OHOS_TEXT_SCALE")); rawScale != "" {
		if f, err := strconv.ParseFloat(rawScale, 64); err == nil && f > 0 {
			if f > 10 {
				f /= 1000
			}
			b.prefs.TextScale = f
		}
	}
	if v := strings.TrimSpace(os.Getenv("MYGO_OHOS_HIGH_CONTRAST")); v == "1" || v == "true" {
		b.prefs.HighContrast = true
	}
	if v := strings.TrimSpace(os.Getenv("MYGO_OHOS_REDUCE_MOTION")); v == "1" || v == "true" {
		b.prefs.ReduceMotion = true
	}
	if cfg, ok := ohosproxy.DecodeEnv(os.Getenv(ohosproxy.EnvVar)); ok {
		ohosproxy.Set(cfg)
	}
}

func parseDisplaySpec(spec string) (platform.Display, bool) {
	parts := strings.SplitN(strings.TrimSpace(spec), ":", 7)
	if len(parts) < 6 {
		return platform.Display{}, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return platform.Display{}, false
	}
	bounds, ok1 := parseRectCSV(parts[1])
	work, ok2 := parseRectCSV(parts[2])
	scaleMilli, err3 := strconv.Atoi(parts[3])
	rot, err4 := strconv.Atoi(parts[4])
	flags, err5 := strconv.Atoi(parts[5])
	if !ok1 || !ok2 || err3 != nil || err4 != nil || err5 != nil || bounds.Width <= 0 || bounds.Height <= 0 {
		return platform.Display{}, false
	}
	scale := float64(scaleMilli) / 1000
	if scale <= 0 {
		scale = 1
	}
	var label string
	if len(parts) == 7 {
		label = parts[6]
	}
	return platform.Display{
		ID:          id,
		Label:       label,
		Bounds:      bounds,
		WorkArea:    work,
		ScaleFactor: scale,
		Rotation:    rot,
		Primary:     flags&1 != 0,
		Internal:    flags&2 != 0,
	}, true
}

func parseRectCSV(s string) (platform.Rect, bool) {
	nums := strings.Split(s, ",")
	if len(nums) != 4 {
		return platform.Rect{}, false
	}
	x, e1 := strconv.Atoi(nums[0])
	y, e2 := strconv.Atoi(nums[1])
	w, e3 := strconv.Atoi(nums[2])
	h, e4 := strconv.Atoi(nums[3])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return platform.Rect{}, false
	}
	return platform.Rect{X: x, Y: y, Width: w, Height: h}, true
}

func (b *Backend) onEnvInfo(payload []byte) {
	info, ok := decodeEnvInfoMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	if info.hasPkg {
		b.hasPkg = true
		b.pkg = info.pkg
	}
	localeChanged := false
	if len(info.langs) > 0 && !slices.Equal(b.langs, info.langs) {
		b.langs = info.langs
		localeChanged = true
	}
	if info.region.Locale != "" || info.region.HourCycle != 0 {
		if b.region != info.region {
			b.region = info.region
			localeChanged = true
		}
	}
	displaysChanged := false
	if len(info.displays) > 0 && !slices.Equal(b.displays, info.displays) {
		b.displays = info.displays
		displaysChanged = true
	}
	prefsChanged := false
	if info.textScaleMilli > 0 || info.prefFlags != 0 {
		next := b.prefs
		if info.textScaleMilli > 0 {
			next.TextScale = float64(info.textScaleMilli) / 1000
		}
		if info.prefFlags&1 != 0 {
			next.HighContrast = info.prefFlags&2 != 0
			next.ReduceMotion = info.prefFlags&4 != 0
			next.ReduceTransparency = info.prefFlags&8 != 0
		}
		if b.prefs != next {
			b.prefs = next
			prefsChanged = true
		}
	}
	app := b.app
	b.mu.Unlock()

	if app != nil {
		if displaysChanged {
			app.DisplaysChanged()
		}
		if localeChanged {
			app.LocaleChanged()
		}
		if prefsChanged {
			app.ThemeChanged()
		}
	}
}

func (b *Backend) SetApplicationMenu(m *platform.Menu) {
	if b == nil {
		return
	}
	var cloned *platform.Menu
	if m != nil {
		cloned = &platform.Menu{
			RightToLeft: m.RightToLeft,
			Items:       cloneMenuItems(m.Items),
		}
	}
	b.mu.Lock()
	b.appMenu = cloned
	b.mu.Unlock()
}

// PopupMenu shows m on pw (or the active window when pw is nil) at pos in the
// window's content DIPs (or the last pointer position when pos is nil), and
// pumps nested events until the user picks an item or dismisses the menu.
func (b *Backend) PopupMenu(m *platform.Menu, pw platform.Window, pos *platform.Point) {
	if m == nil || len(m.Items) == 0 {
		return
	}
	w, _ := pw.(*Window)
	b.mu.Lock()
	if w == nil {
		w = b.focusedOrFirstWindowLocked()
	}
	rtl := m.RightToLeft || b.appRTL
	b.mu.Unlock()
	if w == nil || w.closed || b.conn == nil {
		return
	}

	var x, y int
	if w.surface != nil {
		x, y = int(math.Round(w.surface.lastX)), int(math.Round(w.surface.lastY))
	}
	if pos != nil {
		x, y = pos.X, pos.Y
	}
	payload, visibleCount := encodePopupMenuMessage(0, w.id, x, y, rtl, m.Items)
	if visibleCount == 0 {
		return
	}

	b.mu.Lock()
	b.nextPopupReq++
	reqID := b.nextPopupReq
	if b.popupResults == nil {
		b.popupResults = make(map[uint32]int32)
	}
	b.mu.Unlock()

	binary.LittleEndian.PutUint32(payload[0:], reqID)
	b.send(kindPopupMenu, payload)

	var chosen int
	for {
		b.mu.Lock()
		res, done := b.popupResults[reqID]
		if done {
			delete(b.popupResults, reqID)
		}
		closed := w.closed || b.quit
		b.mu.Unlock()
		if done {
			chosen = int(res)
			break
		}
		if closed {
			break
		}
		b.Step()
	}

	if chosen != 0 && !w.closed && w.h != nil {
		w.h.MenuItemClicked(chosen)
	}
}

func (b *Backend) onPopupResult(payload []byte) {
	if len(payload) < 8 {
		return
	}
	reqID := binary.LittleEndian.Uint32(payload[0:])
	chosenID := int32(binary.LittleEndian.Uint32(payload[4:]))
	b.mu.Lock()
	if b.popupResults == nil {
		b.popupResults = make(map[uint32]int32)
	}
	b.popupResults[reqID] = chosenID
	b.mu.Unlock()
}

type popupWireItem struct {
	id      int32
	kind    byte
	enabled bool
	checked bool
	label   string
	accel   string
	items   []popupWireItem
}

func buildPopupWireItems(items []*platform.MenuItem) []popupWireItem {
	var out []popupWireItem
	for _, it := range items {
		if it == nil || !it.Visible {
			continue
		}
		if it.Type == platform.MenuItemSeparator {
			if len(out) == 0 || out[len(out)-1].kind == byte(platform.MenuItemSeparator) {
				continue
			}
			out = append(out, popupWireItem{kind: byte(platform.MenuItemSeparator)})
			continue
		}
		w := popupWireItem{
			id:      int32(it.ID),
			kind:    byte(it.Type),
			enabled: it.Enabled,
			checked: it.Checked && (it.Type == platform.MenuItemCheckbox || it.Type == platform.MenuItemRadio),
			label:   strings.ReplaceAll(it.Label, "&", ""),
			accel:   formatMenuAccelerator(it.Accelerator),
		}
		if it.Type == platform.MenuItemSubmenu && it.Submenu != nil {
			w.items = buildPopupWireItems(it.Submenu.Items)
		}
		out = append(out, w)
	}
	if len(out) > 0 && out[len(out)-1].kind == byte(platform.MenuItemSeparator) {
		out = out[:len(out)-1]
	}
	return out
}

func encodePopupMenuMessage(reqID, winID uint32, x, y int, rtl bool, items []*platform.MenuItem) ([]byte, int) {
	wireItems := buildPopupWireItems(items)
	if len(wireItems) == 0 {
		return nil, 0
	}
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	binary.LittleEndian.PutUint32(buf[4:], winID)
	binary.LittleEndian.PutUint32(buf[8:], uint32(int32(x)))
	binary.LittleEndian.PutUint32(buf[12:], uint32(int32(y)))
	if rtl {
		buf[16] = 1
	}
	buf = appendPopupWireSlice(buf, wireItems)
	return buf, len(wireItems)
}

func appendPopupWireSlice(buf []byte, items []popupWireItem) []byte {
	buf = appendUint32(buf, uint32(len(items)))
	for _, it := range items {
		var hdr [8]byte
		binary.LittleEndian.PutUint32(hdr[0:], uint32(it.id))
		hdr[4] = it.kind
		var flags byte
		if it.enabled {
			flags |= 1
		}
		if it.checked {
			flags |= 2
		}
		hdr[5] = flags
		buf = append(buf, hdr[:]...)
		buf = appendWireStr(buf, it.label)
		buf = appendWireStr(buf, it.accel)
		buf = appendPopupWireSlice(buf, it.items)
	}
	return buf
}

func decodePopupMenuMessage(p []byte) (reqID, winID uint32, x, y int32, rtl bool, items []popupWireItem, ok bool) {
	if len(p) < 20 {
		return 0, 0, 0, 0, false, nil, false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	winID = binary.LittleEndian.Uint32(p[4:])
	x = int32(binary.LittleEndian.Uint32(p[8:]))
	y = int32(binary.LittleEndian.Uint32(p[12:]))
	rtl = p[16] != 0
	var next int
	items, next, ok = readPopupWireSlice(p, 20)
	if !ok || next != len(p) {
		return 0, 0, 0, 0, false, nil, false
	}
	return reqID, winID, x, y, rtl, items, true
}

func readPopupWireSlice(p []byte, off int) ([]popupWireItem, int, bool) {
	count, off, ok := readUint32(p, off)
	if !ok || count > 512 {
		return nil, off, false
	}
	if count == 0 {
		return nil, off, true
	}
	out := make([]popupWireItem, 0, count)
	for range count {
		if off+8 > len(p) {
			return nil, off, false
		}
		id := int32(binary.LittleEndian.Uint32(p[off:]))
		kind := p[off+4]
		flags := p[off+5]
		off += 8
		label, o1, ok1 := readWireStr(p, off)
		if !ok1 {
			return nil, off, false
		}
		accel, o2, ok2 := readWireStr(p, o1)
		if !ok2 {
			return nil, off, false
		}
		subs, o3, ok3 := readPopupWireSlice(p, o2)
		if !ok3 {
			return nil, off, false
		}
		off = o3
		out = append(out, popupWireItem{
			id:      id,
			kind:    kind,
			enabled: flags&1 != 0,
			checked: flags&2 != 0,
			label:   label,
			accel:   accel,
			items:   subs,
		})
	}
	return out, off, true
}

func formatMenuAccelerator(a string) string {
	if a == "" {
		return ""
	}
	parts := strings.Split(a, "+")
	var b strings.Builder
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		last := i == len(parts)-1
		switch strings.ToLower(part) {
		case "cmdorctrl", "commandorcontrol", "ctrl", "control":
			b.WriteString("Ctrl+")
		case "cmd", "command", "super", "meta":
			b.WriteString("Super+")
		case "alt", "option":
			b.WriteString("Alt+")
		case "shift":
			b.WriteString("Shift+")
		default:
			if last {
				b.WriteString(strings.ToUpper(part[:1]) + part[1:])
			}
		}
	}
	return b.String()
}
