//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"math"
	"unicode/utf8"

	"github.com/egoist/mygo/internal/platform"
)

// Encode helpers for kindInput messages (all carry u32 winID at offset 0).

func encodeMouseInput(winID uint32, op, button, pointerKind byte, mods platform.Modifiers, timeMs uint32, x, y, pressure float32) []byte {
	p := make([]byte, 28)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputMouse
	p[5] = op
	p[6] = button
	p[7] = pointerKind
	binary.LittleEndian.PutUint32(p[8:], uint32(mods))
	binary.LittleEndian.PutUint32(p[12:], timeMs)
	putF32(p[16:], x)
	putF32(p[20:], y)
	putF32(p[24:], pressure)
	return p
}

func encodeTouchInput(winID uint32, op byte, firstNew bool, mods platform.Modifiers, timeMs uint32, pts []touchPoint) []byte {
	p := make([]byte, 20+len(pts)*16)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputTouch
	p[5] = op
	if firstNew {
		p[6] = 1
	}
	binary.LittleEndian.PutUint32(p[8:], uint32(mods))
	binary.LittleEndian.PutUint32(p[12:], timeMs)
	binary.LittleEndian.PutUint32(p[16:], uint32(len(pts)))
	for i, pt := range pts {
		at := p[20+i*16:]
		binary.LittleEndian.PutUint32(at[0:], uint32(pt.id))
		putF32(at[4:], float32(pt.x))
		putF32(at[8:], float32(pt.y))
		putF32(at[12:], float32(pt.pressure))
	}
	return p
}

func encodeScrollInput(winID uint32, flags byte, mods platform.Modifiers, x, y, dx, dy float32) []byte {
	p := make([]byte, 28)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputScroll
	p[5] = flags
	binary.LittleEndian.PutUint32(p[8:], uint32(mods))
	putF32(p[12:], x)
	putF32(p[16:], y)
	putF32(p[20:], dx)
	putF32(p[24:], dy)
	return p
}

func encodeZoomInput(winID uint32, op byte, mods platform.Modifiers, x, y, scale float32) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputZoom
	p[5] = op
	binary.LittleEndian.PutUint32(p[8:], uint32(mods))
	putF32(p[12:], x)
	putF32(p[16:], y)
	putF32(p[20:], scale)
	return p
}

func encodeKeyInput(winID uint32, op byte, mods platform.Modifiers, arkCode int32, unicode uint32) []byte {
	p := make([]byte, 20)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputKey
	p[5] = op
	binary.LittleEndian.PutUint32(p[8:], uint32(mods))
	binary.LittleEndian.PutUint32(p[12:], uint32(arkCode))
	binary.LittleEndian.PutUint32(p[16:], unicode)
	return p
}

func encodeWindowInput(winID uint32, op byte) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputWindow
	p[5] = op
	return p
}

func encodeWindowRectInput(winID uint32, x, y, w, h int) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputWindow
	p[5] = winRect
	binary.LittleEndian.PutUint32(p[8:], uint32(int32(x)))
	binary.LittleEndian.PutUint32(p[12:], uint32(int32(y)))
	binary.LittleEndian.PutUint32(p[16:], uint32(int32(w)))
	binary.LittleEndian.PutUint32(p[20:], uint32(int32(h)))
	return p
}

func encodeWindowTitleBarInput(winID uint32, height, left, right int) []byte {
	p := make([]byte, 20)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputWindow
	p[5] = winTitleBar
	binary.LittleEndian.PutUint32(p[8:], uint32(int32(height)))
	binary.LittleEndian.PutUint32(p[12:], uint32(int32(left)))
	binary.LittleEndian.PutUint32(p[16:], uint32(int32(right)))
	return p
}

func encodeIMEInput(winID uint32, op byte, arg0, arg1 int32, text string) []byte {
	p := make([]byte, 16+len(text))
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = inputIME
	p[5] = op
	binary.LittleEndian.PutUint32(p[8:], uint32(arg0))
	binary.LittleEndian.PutUint32(p[12:], uint32(arg1))
	copy(p[16:], text)
	return p
}

func (b *Backend) onInput(payload []byte) {
	if len(payload) < 6 {
		return
	}
	winID := binary.LittleEndian.Uint32(payload[0:])
	body := payload[4:]
	if body[0] == inputWindow && (body[1] == winThemeLight || body[1] == winThemeDark) {
		b.setSystemDark(body[1] == winThemeDark)
		return
	}

	win := b.windowByID(winID)
	if win == nil || win.closed {
		return
	}
	s := win.surface

	switch body[0] {
	case inputMouse:
		s.onMouse(body)
	case inputTouch:
		s.onTouch(body)
	case inputScroll:
		s.onScroll(body)
	case inputZoom:
		s.onZoom(body)
	case inputKey:
		s.onKey(body)
	case inputWindow:
		s.onWindow(body)
	case inputIME:
		s.onIME(body)
	}
}

func (s *Surface) recordPointer(x, y float64) {
	s.lastX, s.lastY = x, y
	if s.w != nil && s.w.b != nil {
		s.w.b.mu.Lock()
		s.w.b.cursorPoint = platform.Point{
			X: s.w.bounds.X + int(math.Round(x)),
			Y: s.w.bounds.Y + int(math.Round(y)),
		}
		s.w.b.mu.Unlock()
	}
}

func (s *Surface) send(ev platform.SurfaceEvent) bool {
	if ev.Kind == platform.PointerDown || ev.Kind == platform.SurfaceBlur {
		s.clientComposition.Reset()
	}
	if !s.held.Track(ev) {
		return false
	}
	if s.w == nil || s.w.closed || s.w.h == nil {
		return false
	}
	return s.w.h.SurfaceEvent(ev)
}

func (s *Surface) onMouse(p []byte) {
	if len(p) < 24 {
		return
	}
	op := p[1]
	button := p[2]
	pk := platform.PointerMouse
	switch p[3] {
	case 1:
		pk = platform.PointerPen
	case 2:
		pk = platform.PointerTouch
	}
	mods := platform.Modifiers(binary.LittleEndian.Uint32(p[4:])) | s.down.heldMods()
	timeMs := binary.LittleEndian.Uint32(p[8:])
	x := float64(getF32(p[12:]))
	y := float64(getF32(p[16:]))
	pressure := float64(getF32(p[20:]))
	if op != mouseLeave {
		s.recordPointer(x, y)
	}

	switch op {
	case mouseMove:
		s.send(platform.SurfaceEvent{
			Kind:     platform.PointerMove,
			X:        x,
			Y:        y,
			Mods:     mods,
			Pointer:  pk,
			Pressure: pressure,
		})
	case mouseDown:
		s.touch.cancelFling()
		if button == 3 || button == 4 {
			k := platform.KeyBack
			if button == 4 {
				k = platform.KeyForward
			}
			s.send(platform.SurfaceEvent{Kind: platform.KeyPressed, Key: k, Mods: mods})
			return
		}
		if button <= 2 {
			clicks := s.clicks.Press(int(button), x, y, timeMs, platform.DefaultDoubleClick)
			s.send(platform.SurfaceEvent{
				Kind:     platform.PointerDown,
				X:        x,
				Y:        y,
				Button:   int(button),
				Clicks:   clicks,
				Mods:     mods,
				Pointer:  pk,
				Pressure: pressure,
			})
		}
	case mouseUp:
		if button == 3 || button == 4 {
			k := platform.KeyBack
			if button == 4 {
				k = platform.KeyForward
			}
			s.send(platform.SurfaceEvent{Kind: platform.KeyReleased, Key: k, Mods: mods})
			return
		}
		if button <= 2 {
			s.send(platform.SurfaceEvent{
				Kind:     platform.PointerUp,
				X:        x,
				Y:        y,
				Button:   int(button),
				Mods:     mods,
				Pointer:  pk,
				Pressure: pressure,
			})
		}
	case mouseCancel:
		s.send(platform.SurfaceEvent{Kind: platform.PointerCancel, X: x, Y: y, Mods: mods})
	case mouseLeave:
		s.send(platform.SurfaceEvent{Kind: platform.PointerLeave, Mods: mods})
	}
}

func (s *Surface) onTouch(p []byte) {
	if len(p) < 16 {
		return
	}
	op := p[1]
	firstNew := p[2] != 0
	mods := platform.Modifiers(binary.LittleEndian.Uint32(p[4:])) | s.down.heldMods()
	timeMs := binary.LittleEndian.Uint32(p[8:])
	n := int(binary.LittleEndian.Uint32(p[12:]))
	if n < 0 || 16+n*16 > len(p) {
		return
	}
	pts := make([]touchPoint, n)
	var pressure float64
	for i := range n {
		at := p[16+i*16:]
		pts[i] = touchPoint{
			id:       int32(binary.LittleEndian.Uint32(at[0:])),
			x:        float64(getF32(at[4:])),
			y:        float64(getF32(at[8:])),
			pressure: float64(getF32(at[12:])),
		}
		if i == 0 {
			pressure = pts[i].pressure
			s.recordPointer(pts[i].x, pts[i].y)
		}
	}

	var evs []platform.SurfaceEvent
	switch op {
	case touchDown:
		for i, pt := range pts {
			evs = append(evs, s.touch.begin(pt.id, pt.x, pt.y, timeMs, firstNew && i == 0)...)
		}
	case touchMove:
		evs = s.touch.moveBatch(pts, timeMs)
	case touchUp:
		for _, pt := range pts {
			evs = append(evs, s.touch.end(pt.id, pt.x, pt.y, timeMs, false)...)
		}
	case touchCancel:
		evs = s.touch.cancelAll()
	}

	for _, ev := range evs {
		ev.Mods = mods
		if ev.Kind == platform.PointerDown || ev.Kind == platform.PointerUp || ev.Kind == platform.PointerMove {
			ev.Pointer = platform.PointerTouch
			ev.Pressure = pressure
		}
		if ev.Kind == platform.PointerDown {
			dc := platform.DefaultDoubleClick
			dc.DX, dc.DY = max(dc.DX, touchSlop), max(dc.DY, touchSlop)
			ev.Clicks = s.clicks.Press(ev.Button, ev.X, ev.Y, timeMs, dc)
		}
		s.send(ev)
	}
	if s.touch.fling.active {
		s.RequestFrame()
	}
}

func (s *Surface) onScroll(p []byte) {
	if len(p) < 24 {
		return
	}
	s.touch.cancelFling()
	flags := p[1]
	mods := platform.Modifiers(binary.LittleEndian.Uint32(p[4:])) | s.down.heldMods()
	x := float64(getF32(p[8:]))
	y := float64(getF32(p[12:]))
	dx := float64(getF32(p[16:]))
	dy := float64(getF32(p[20:]))
	s.recordPointer(x, y)
	if dx == 0 && dy == 0 {
		return
	}
	s.send(platform.SurfaceEvent{
		Kind:     platform.PointerScroll,
		X:        x,
		Y:        y,
		DX:       dx,
		DY:       dy,
		Mods:     mods,
		Precise:  flags&scrollPrecise != 0,
		Momentum: flags&scrollMomentum != 0,
	})
}

func (s *Surface) onZoom(p []byte) {
	if len(p) < 20 {
		return
	}
	s.touch.cancelFling()
	op := p[1]
	mods := platform.Modifiers(binary.LittleEndian.Uint32(p[4:])) | s.down.heldMods()
	x := float64(getF32(p[8:]))
	y := float64(getF32(p[12:]))
	scale := float64(getF32(p[16:]))
	s.recordPointer(x, y)

	switch op {
	case zoomBegin:
		s.pinch = 1
	case zoomUpdate:
		if s.pinch > 0 && scale > 0 && scale != s.pinch {
			s.send(platform.SurfaceEvent{
				Kind:  platform.PointerZoom,
				X:     x,
				Y:     y,
				Scale: scale / s.pinch,
				Mods:  mods,
			})
		}
		if scale > 0 {
			s.pinch = scale
		}
	case zoomEnd:
		s.pinch = 0
	}
}

func (s *Surface) onKey(p []byte) {
	if len(p) < 16 {
		return
	}
	op := p[1]
	rawMods := platform.Modifiers(binary.LittleEndian.Uint32(p[4:]))
	arkCode := int32(binary.LittleEndian.Uint32(p[8:]))
	unicode := binary.LittleEndian.Uint32(p[12:])

	kind := platform.KeyPressed
	repeat := false
	if op == keyUp {
		kind = platform.KeyReleased
		s.down.release(arkCode)
	} else {
		repeat = s.down.press(arkCode)
		s.clientComposition.Reset()
	}
	mods := rawMods | s.down.heldMods()

	if bit := modifierForArkKey(arkCode); bit != 0 {
		if kind == platform.KeyPressed || s.down.otherSideHeld(arkCode) {
			mods |= bit
		} else {
			mods &^= bit
		}
		s.send(platform.SurfaceEvent{Kind: platform.ModifiersChanged, Mods: mods})
		_, physical, code := mapArkKey(arkCode, unicode)
		if code != platform.CodeUnknown {
			raw := platform.RawKeyPressed
			if kind == platform.KeyReleased {
				raw = platform.RawKeyReleased
			}
			s.send(platform.SurfaceEvent{Kind: raw, PhysicalKey: physical, Code: code, Mods: mods, Repeat: repeat})
		}
		return
	}

	key, physical, code := mapArkKey(arkCode, unicode)
	if key == platform.KeyUnknown {
		switch {
		case kind == platform.KeyReleased && physical != platform.KeyUnknown:
			s.send(platform.SurfaceEvent{Kind: kind, PhysicalKey: physical, Code: code, Mods: mods})
			return
		case !platform.KeyByPosition(key, physical, mods):
			if code != platform.CodeUnknown {
				raw := platform.RawKeyPressed
				if kind == platform.KeyReleased {
					raw = platform.RawKeyReleased
				}
				s.send(platform.SurfaceEvent{Kind: raw, PhysicalKey: physical, Code: code, Mods: mods, Repeat: repeat})
			}
			return
		}
	}

	taken := s.send(platform.SurfaceEvent{
		Kind:        kind,
		Key:         key,
		PhysicalKey: physical,
		Code:        code,
		Mods:        mods,
		Repeat:      repeat,
	})
	if kind == platform.KeyPressed && !taken && s.textInput &&
		mods&(platform.ModCtrl|platform.ModAlt|platform.ModSuper) == 0 &&
		unicode >= 0x20 && unicode != 0x7f {
		s.commit(string(rune(unicode)))
	}
}

func (s *Surface) onWindow(p []byte) {
	if len(p) < 2 {
		return
	}
	op := p[1]
	w := s.w
	if w == nil || w.closed {
		return
	}

	switch op {
	case winFocus:
		if !w.focused || !s.focusDelivered {
			wasFocused := w.focused && s.focusDelivered
			w.focused = true
			s.focusDelivered = true
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceFocus})
			if !wasFocused && w.h != nil {
				w.h.Focused()
			}
			if w.b != nil {
				w.b.mu.Lock()
				becameActive := !w.b.appActive
				w.b.appActive = true
				app := w.b.app
				w.b.mu.Unlock()
				if becameActive && app != nil {
					app.DidBecomeActive()
				}
			}
		}
	case winBlur:
		if w.focused {
			w.focused = false
			s.touch.cancelFling()
			s.touch.fingers = s.touch.fingers[:0]
			s.down.clear()
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceBlur})
			if w.h != nil {
				w.h.Blurred()
			}
			if w.b != nil {
				w.b.mu.Lock()
				stillActive := w.b.hasFocusedWindowLocked()
				resigned := w.b.appActive && !stillActive
				if resigned {
					w.b.appActive = false
				}
				app := w.b.app
				w.b.mu.Unlock()
				if resigned && app != nil {
					app.DidResignActive()
				}
			}
		}
	case winShow, winRestore:
		s.mu.Lock()
		wasOccluded := s.hostHidden || w.minimized
		s.hostHidden = false
		s.mu.Unlock()
		wasMin := w.minimized
		w.minimized = false
		if wasOccluded {
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceShown})
		}
		if wasMin && w.h != nil {
			w.h.Restored()
		}
	case winHide, winMinimize:
		s.mu.Lock()
		wasHostHidden := s.hostHidden
		wasVisible := !s.hostHidden && !w.minimized
		s.hostHidden = true
		s.mu.Unlock()
		if op == winHide && !w.visible && wasHostHidden {
			return
		}
		becameMin := !w.minimized
		w.minimized = true
		if wasVisible {
			s.touch.cancelFling()
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceHidden})
		}
		if becameMin && w.h != nil {
			w.h.Minimized()
		}
	case winMaximize:
		wasMin := w.minimized
		wasFS := w.fullscreen
		prevTB := w.TitleBar()
		w.minimized = false
		w.fullscreen = false
		s.setHostHidden(false)
		if !w.maximized {
			w.maximized = true
			if wasMin && w.h != nil {
				w.h.Restored()
			}
			if wasFS && w.h != nil {
				w.h.LeftFullScreen()
			}
			if w.h != nil {
				w.h.Maximized()
			}
		}
		if w.TitleBar() != prevTB && w.h != nil {
			w.h.TitleBarChanged()
		}
	case winUnmaximize:
		prevTB := w.TitleBar()
		wasFS := w.fullscreen
		w.fullscreen = false
		if w.maximized {
			w.maximized = false
			if w.h != nil {
				w.h.Unmaximized()
			}
		}
		if wasFS && w.h != nil {
			w.h.LeftFullScreen()
		}
		if w.TitleBar() != prevTB && w.h != nil {
			w.h.TitleBarChanged()
		}
	case winEnterFullScreen:
		prevTB := w.TitleBar()
		w.minimized = false
		s.setHostHidden(false)
		if !w.fullscreen {
			w.fullscreen = true
			if w.h != nil {
				w.h.EnteredFullScreen()
			}
		}
		if w.TitleBar() != prevTB && w.h != nil {
			w.h.TitleBarChanged()
		}
	case winLeaveFullScreen:
		prevTB := w.TitleBar()
		if w.fullscreen {
			w.fullscreen = false
			if w.h != nil {
				w.h.LeftFullScreen()
			}
		}
		if w.TitleBar() != prevTB && w.h != nil {
			w.h.TitleBarChanged()
		}
	case winCloseRequest:
		allow := true
		if w.h != nil {
			allow = w.h.ShouldClose()
		}
		if !allow {
			w.sendAction(winActCloseDeny)
			return
		}
		w.sendAction(winActCloseAllow)
		w.closeInternal(false)
	case winClosed:
		w.closeInternal(false)
	case winRect:
		if len(p) < 20 {
			return
		}
		nx := int(int32(binary.LittleEndian.Uint32(p[4:])))
		ny := int(int32(binary.LittleEndian.Uint32(p[8:])))
		nw := int(int32(binary.LittleEndian.Uint32(p[12:])))
		nh := int(int32(binary.LittleEndian.Uint32(p[16:])))
		moved := w.bounds.X != nx || w.bounds.Y != ny
		w.bounds.X, w.bounds.Y = nx, ny
		if nw > 0 && nh > 0 {
			w.bounds.Width, w.bounds.Height = nw, nh
		}
		if moved && w.h != nil {
			w.h.Moved()
		}
	case winTitleBar:
		if len(p) < 16 {
			return
		}
		prevTB := w.TitleBar()
		w.rawTitleBar = platform.TitleBar{
			Height: int(int32(binary.LittleEndian.Uint32(p[4:]))),
			Left:   int(int32(binary.LittleEndian.Uint32(p[8:]))),
			Right:  int(int32(binary.LittleEndian.Uint32(p[12:]))),
		}
		w.hasTitleBar = true
		if w.TitleBar() != prevTB && w.h != nil {
			w.h.TitleBarChanged()
		}
	}
}

func (s *Surface) onIME(p []byte) {
	if len(p) < 12 {
		return
	}
	op := p[1]
	arg0 := int32(binary.LittleEndian.Uint32(p[4:]))
	arg1 := int32(binary.LittleEndian.Uint32(p[8:]))
	text := string(p[12:])

	switch op {
	case imeCommit:
		s.commit(text)
	case imePreedit:
		s.preedit(text, int(arg0), int(arg1))
	case imePreeditEnd:
		s.preeditEnd()
	case imeDeleteSurrounding:
		s.deleteSurrounding(int(arg0), int(arg1))
	case imeSetSelection:
		s.setSelection(int(arg0), int(arg1))
	case imeCommand:
		if platform.EditCommands[text] {
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceCommand, Text: text})
		}
	}
}

func (s *Surface) commit(text string) {
	if !s.textInput {
		return
	}
	if c := s.input.Client; c != nil {
		s.clientComposition.Replace(c, nil, text)
		return
	}
	if text != "" {
		s.send(platform.SurfaceEvent{Kind: platform.TextInput, Text: text})
	}
}

func (s *Surface) preedit(text string, start, end int) {
	if !s.textInput {
		return
	}
	if c := s.input.Client; c != nil {
		s.clientComposition.Reset()
		var r *platform.TextRange
		if start >= 0 && end >= start {
			ctx := platform.ClientTextContext(c)
			mapped := ctx.Range(platform.TextRange{Start: start, End: end})
			r = &mapped
		}
		caret := platform.UTF16Len(text)
		c.SetMarkedText(r, text, platform.TextRange{Start: caret, End: caret})
		return
	}
	s.send(platform.SurfaceEvent{
		Kind:  platform.TextComposition,
		Text:  text,
		Caret: utf8.RuneCountInString(text),
	})
}

func (s *Surface) preeditEnd() {
	if !s.textInput {
		return
	}
	if c := s.input.Client; c != nil {
		s.clientComposition.End(c)
		return
	}
	s.send(platform.SurfaceEvent{Kind: platform.TextComposition})
}

func (s *Surface) deleteSurrounding(offset, count int) {
	if !s.textInput || count <= 0 {
		return
	}
	if c := s.input.Client; c != nil {
		sel := platform.NormalizeTextRange(c.Selection().Range)
		if sel.Start != sel.End && (offset == -1 || offset == 0) && count == 1 {
			c.ReplaceText(&sel, "")
			return
		}
		r := platform.ClientDeleteRange(c, offset, count)
		c.ReplaceText(&r, "")
		return
	}
	from := max(0, s.input.End+offset)
	to := max(from, from+count)
	s.send(platform.SurfaceEvent{
		Kind:    platform.TextInput,
		Replace: true,
		From:    from,
		To:      to,
	})
}

func (s *Surface) setSelection(start, end int) {
	if !s.textInput || start < 0 || end < start {
		return
	}
	if c := s.input.Client; c != nil {
		ctx := platform.ClientTextContext(c)
		r := ctx.Range(platform.TextRange{Start: start, End: end})
		sub, _ := c.TextForRange(r)
		if start == end {
			c.SetMarkedText(&r, "", platform.TextRange{})
			c.UnmarkText()
		} else if sub != "" {
			u := platform.UTF16Len(sub)
			c.SetMarkedText(&r, sub, platform.TextRange{Start: 0, End: u})
			c.UnmarkText()
		}
	}
}
