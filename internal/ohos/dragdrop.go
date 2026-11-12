//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"net/url"
	"slices"
	"strings"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

const (
	dragEvEnter       byte = 1
	dragEvMove        byte = 2
	dragEvLeave       byte = 3
	dragEvDrop        byte = 4
	dragEvSourceEnded byte = 5
)

const (
	ohosDragOpCopy uint32 = 1 << 0
	ohosDragOpMove uint32 = 1 << 1
	ohosDragOpLink uint32 = 1 << 2
)

type dragStartWire struct {
	winID      uint32
	ops        uint32
	x, y       float64
	width      int32
	height     int32
	hotX, hotY int32
	session    string
	preview    []byte
	reps       []clipWireRep
}

type dragEventWire struct {
	winID       uint32
	ev          byte
	canceled    bool
	escaped     bool
	pendingID   uint32
	ops         uint32
	suggestedOp uint32
	x, y        float64
	session     string
	err         string
	formats     []string
	reps        []clipWireRep
}

func ohosDragOps(op transfer.Operation) uint32 {
	var out uint32
	if op&transfer.Copy != 0 {
		out |= ohosDragOpCopy
	}
	if op&transfer.Move != 0 {
		out |= ohosDragOpMove
	}
	return out
}

func goDragOps(mask uint32) transfer.Operation {
	var op transfer.Operation
	if mask&ohosDragOpCopy != 0 {
		op |= transfer.Copy
	}
	if mask&ohosDragOpMove != 0 {
		op |= transfer.Move
	}
	return op
}

func (s *Surface) SetDropFormats(formats []transfer.Format) {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil {
		return
	}
	s.dropFormats = slices.Clone(formats)
	var wireFmts []string
	add := func(name string) {
		if name != "" && !slices.Contains(wireFmts, name) {
			wireFmts = append(wireFmts, name)
		}
	}
	for _, f := range formats {
		if f == transfer.FileList {
			add(string(transfer.URIList))
			continue
		}
		add(string(f))
	}
	s.w.b.send(kindDragSetFormats, encodeDragSetFormatsMessage(s.w.id, wireFmts))
}

func (s *Surface) StartDataDrag(r platform.DragRequest) {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil || s.w.b.conn == nil {
		if r.Done != nil {
			r.Done(transfer.Result{Err: errors.New("ohos: surface unavailable for drag")})
		}
		return
	}
	if s.dragReq != nil {
		if r.Done != nil {
			r.Done(transfer.Result{Err: errors.New("ohos: drag already active on surface")})
		}
		return
	}

	fmts := r.Data.Formats()
	var mat transfer.Data
	if len(fmts) > 0 {
		var err error
		mat, err = r.Data.Materialize(fmts)
		if err != nil {
			if r.Done != nil {
				r.Done(transfer.Result{Err: err})
			}
			return
		}
	}

	var reps []clipWireRep
	for _, f := range fmts {
		if f == platform.DragSessionFormat {
			continue
		}
		raw, err := mat.Read(f)
		if err != nil {
			continue
		}
		wireFmt := string(f)
		if f == transfer.FileList {
			wireFmt = string(transfer.URIList)
		}
		reps = append(reps, clipWireRep{format: wireFmt, data: bytes.Clone(raw)})
	}
	if r.Session != "" {
		reps = append(reps, clipWireRep{
			format: string(platform.DragSessionFormat),
			data:   []byte(r.Session),
		})
	}

	reqCopy := r
	s.dragReq = &reqCopy
	s.dragCanceled = false
	s.dragEscaped = false

	b := s.w.b
	if r.Session != "" {
		b.mu.Lock()
		if b.dragSessions == nil {
			b.dragSessions = make(map[string]*Surface)
		}
		b.dragSessions[r.Session] = s
		b.mu.Unlock()
	}

	b.send(kindDragStart, encodeDragStartMessage(dragStartWire{
		winID:   s.w.id,
		ops:     ohosDragOps(r.Operations),
		x:       r.X,
		y:       r.Y,
		width:   int32(r.Width),
		height:  int32(r.Height),
		hotX:    int32(r.HotX),
		hotY:    int32(r.HotY),
		session: r.Session,
		preview: r.Preview,
		reps:    reps,
	}))
}

func (s *Surface) CancelDataDrag() {
	if s == nil || s.dragReq == nil || s.w == nil || s.w.b == nil {
		return
	}
	s.dragCanceled = true
	s.dragEscaped = true
	s.w.b.send(kindDragCancel, requestMessage(s.w.id))
}

func (s *Surface) cleanupDrag() {
	if s == nil || s.dragReq == nil {
		return
	}
	r := s.dragReq
	s.dragReq = nil
	if s.w != nil && s.w.b != nil {
		b := s.w.b
		if r.Session != "" {
			b.mu.Lock()
			delete(b.dragSessions, r.Session)
			b.mu.Unlock()
		}
		b.send(kindDragCancel, requestMessage(s.w.id))
	}
	if r.Done != nil {
		r.Done(transfer.Result{Canceled: true, Escaped: true})
	}
}

func (b *Backend) onDragEvent(payload []byte) {
	ev, ok := decodeDragEventMessage(payload)
	if !ok {
		return
	}
	win := b.windowByID(ev.winID)
	if win == nil || win.closed || win.surface == nil {
		if ev.pendingID != 0 {
			b.send(kindDragReply, encodeDragReplyMessage(ev.winID, ev.pendingID, 0, false))
		}
		return
	}
	s := win.surface

	switch ev.ev {
	case dragEvSourceEnded:
		if s.dragReq == nil {
			return
		}
		r := s.dragReq
		s.dragReq = nil
		if r.Session != "" {
			b.mu.Lock()
			delete(b.dragSessions, r.Session)
			b.mu.Unlock()
		}
		var err error
		if ev.err != "" {
			err = errors.New(ev.err)
		}
		op := goDragOps(ev.ops)
		if s.dragCanceled || ev.canceled || err != nil {
			op = transfer.None
		}
		canceled := op == transfer.None && err == nil
		escaped := canceled && (s.dragEscaped || ev.escaped)
		pt := b.Screen().CursorPoint()
		if r.Done != nil {
			r.Done(transfer.Result{
				Operation: op,
				Canceled:  canceled,
				Escaped:   escaped,
				Point:     image.Point{X: pt.X, Y: pt.Y},
				Err:       err,
			})
		}

	case dragEvEnter, dragEvMove:
		s.recordPointer(ev.x, ev.y)
		d := b.buildDragOffer(ev)
		taken := s.send(platform.SurfaceEvent{
			Kind: platform.DataDragOver,
			X:    ev.x,
			Y:    ev.y,
			Drag: d,
		})
		if ev.pendingID != 0 {
			b.send(kindDragReply, encodeDragReplyMessage(ev.winID, ev.pendingID, ohosDragOps(d.Operation), taken))
		}

	case dragEvLeave:
		s.send(platform.SurfaceEvent{
			Kind: platform.DataDragLeave,
			Drag: &platform.DataDragEvent{},
		})

	case dragEvDrop:
		s.recordPointer(ev.x, ev.y)
		d := b.buildDragOffer(ev)
		if !s.send(platform.SurfaceEvent{
			Kind: platform.DataDragOver,
			X:    ev.x,
			Y:    ev.y,
			Drag: d,
		}) {
			if ev.pendingID != 0 {
				b.send(kindDragReply, encodeDragReplyMessage(ev.winID, ev.pendingID, 0, false))
			}
			return
		}
		var paths []string
		if d.HasFiles {
			paths = extractDropPaths(ev.reps)
		}
		if len(d.Data.Formats()) == 0 {
			want := d.Formats
			if len(want) == 0 && d.HasFiles {
				want = []transfer.Format{transfer.FileList, transfer.URIList}
			}
			if len(want) > 0 {
				if data, err := buildTransferDataFromHost(ev.reps, want); err == nil {
					d.Data = data
				}
			}
		}
		taken := s.send(platform.SurfaceEvent{
			Kind:  platform.DataDrop,
			X:     ev.x,
			Y:     ev.y,
			Files: paths,
			Drag:  d,
		})
		if ev.pendingID != 0 {
			b.send(kindDragReply, encodeDragReplyMessage(ev.winID, ev.pendingID, ohosDragOps(d.Operation), taken))
		}
	}
}

func (b *Backend) buildDragOffer(ev dragEventWire) *platform.DataDragEvent {
	ops := goDragOps(ev.ops)
	if ops == 0 {
		ops = transfer.Copy
	}
	suggested := goDragOps(ev.suggestedOp)
	if suggested == 0 {
		if ops&transfer.Copy != 0 {
			suggested = transfer.Copy
		} else if ops&transfer.Move != 0 {
			suggested = transfer.Move
		}
	}
	d := &platform.DataDragEvent{
		Offer: transfer.Offer{
			Operations: ops,
			Suggested:  suggested,
		},
	}
	session := ev.session
	for _, raw := range ev.formats {
		if raw == string(platform.DragSessionFormat) {
			continue
		}
		for _, f := range mapHostMimesToFormats([]string{raw}) {
			if f == platform.DragSessionFormat {
				continue
			}
			if !slices.Contains(d.Offer.Formats, f) {
				d.Offer.Formats = append(d.Offer.Formats, f)
			}
			if f == transfer.FileList || f == transfer.URIList {
				d.HasFiles = true
				if !slices.Contains(d.Offer.Formats, transfer.FileList) {
					d.Offer.Formats = append(d.Offer.Formats, transfer.FileList)
				}
				if !slices.Contains(d.Offer.Formats, transfer.URIList) {
					d.Offer.Formats = append(d.Offer.Formats, transfer.URIList)
				}
			}
		}
	}
	if session == "" {
		for _, rep := range ev.reps {
			if rep.format == string(platform.DragSessionFormat) {
				session = string(rep.data)
				break
			}
		}
	}
	if session != "" {
		b.mu.Lock()
		_, alive := b.dragSessions[session]
		b.mu.Unlock()
		if alive {
			d.Session = session
		}
	}
	return d
}

func extractDropPaths(reps []clipWireRep) []string {
	var paths []string
	for _, r := range reps {
		switch r.format {
		case string(transfer.URIList), string(transfer.FileList), "text/uri", "general.file-uri":
			for _, line := range strings.Split(strings.ReplaceAll(string(r.data), "\r\n", "\n"), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if p := uriOrRawToPath(line); p != "" && !slices.Contains(paths, p) {
					paths = append(paths, p)
				}
			}
		}
	}
	return paths
}

func uriOrRawToPath(s string) string {
	if strings.HasPrefix(s, "/") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return ""
	}
	if u.Path == "" {
		return ""
	}
	if unescaped, err := url.PathUnescape(u.Path); err == nil && unescaped != "" {
		return unescaped
	}
	return u.Path
}

func encodeDragSetFormatsMessage(winID uint32, formats []string) []byte {
	var buf []byte
	buf = appendUint32(buf, winID)
	buf = appendUint32(buf, uint32(len(formats)))
	for _, f := range formats {
		buf = appendWireStr(buf, f)
	}
	return buf
}

func decodeDragSetFormatsMessage(p []byte) (winID uint32, formats []string, ok bool) {
	var off int
	winID, off, ok = readUint32(p, 0)
	if !ok {
		return 0, nil, false
	}
	n, off, ok := readUint32(p, off)
	if !ok || n > 128 {
		return 0, nil, false
	}
	formats = make([]string, 0, n)
	for range n {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return 0, nil, false
		}
		formats = append(formats, s)
	}
	return winID, formats, off == len(p)
}

func encodeDragStartMessage(w dragStartWire) []byte {
	var hdr [40]byte
	binary.LittleEndian.PutUint32(hdr[0:], w.winID)
	binary.LittleEndian.PutUint32(hdr[4:], w.ops)
	putF64(hdr[8:], w.x)
	putF64(hdr[16:], w.y)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(w.width))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(w.height))
	binary.LittleEndian.PutUint32(hdr[32:], uint32(w.hotX))
	binary.LittleEndian.PutUint32(hdr[36:], uint32(w.hotY))
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, w.session)
	buf = appendWireBytes(buf, w.preview)
	buf = appendUint32(buf, uint32(len(w.reps)))
	for _, r := range w.reps {
		buf = appendWireStr(buf, r.format)
		buf = appendWireBytes(buf, r.data)
	}
	return buf
}

func decodeDragStartMessage(p []byte) (dragStartWire, bool) {
	if len(p) < 40 {
		return dragStartWire{}, false
	}
	var w dragStartWire
	w.winID = binary.LittleEndian.Uint32(p[0:])
	w.ops = binary.LittleEndian.Uint32(p[4:])
	w.x = getF64(p[8:])
	w.y = getF64(p[16:])
	w.width = int32(binary.LittleEndian.Uint32(p[24:]))
	w.height = int32(binary.LittleEndian.Uint32(p[28:]))
	w.hotX = int32(binary.LittleEndian.Uint32(p[32:]))
	w.hotY = int32(binary.LittleEndian.Uint32(p[36:]))
	off := 40
	var ok bool
	if w.session, off, ok = readWireStr(p, off); !ok {
		return dragStartWire{}, false
	}
	if w.preview, off, ok = readWireBytes(p, off); !ok {
		return dragStartWire{}, false
	}
	repCount, off, ok := readUint32(p, off)
	if !ok || repCount > 128 {
		return dragStartWire{}, false
	}
	w.reps = make([]clipWireRep, 0, repCount)
	for range repCount {
		var fmtName string
		var data []byte
		if fmtName, off, ok = readWireStr(p, off); !ok {
			return dragStartWire{}, false
		}
		if data, off, ok = readWireBytes(p, off); !ok {
			return dragStartWire{}, false
		}
		w.reps = append(w.reps, clipWireRep{format: fmtName, data: data})
	}
	return w, off == len(p)
}

func encodeDragEventMessage(w dragEventWire) []byte {
	var hdr [36]byte
	binary.LittleEndian.PutUint32(hdr[0:], w.winID)
	hdr[4] = w.ev
	if w.canceled {
		hdr[5] = 1
	}
	if w.escaped {
		hdr[6] = 1
	}
	binary.LittleEndian.PutUint32(hdr[8:], w.pendingID)
	binary.LittleEndian.PutUint32(hdr[12:], w.ops)
	binary.LittleEndian.PutUint32(hdr[16:], w.suggestedOp)
	putF64(hdr[20:], w.x)
	putF64(hdr[28:], w.y)
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, w.session)
	buf = appendWireStr(buf, w.err)
	buf = appendUint32(buf, uint32(len(w.formats)))
	for _, f := range w.formats {
		buf = appendWireStr(buf, f)
	}
	buf = appendUint32(buf, uint32(len(w.reps)))
	for _, r := range w.reps {
		buf = appendWireStr(buf, r.format)
		buf = appendWireBytes(buf, r.data)
	}
	return buf
}

func decodeDragEventMessage(p []byte) (dragEventWire, bool) {
	if len(p) < 36 {
		return dragEventWire{}, false
	}
	var w dragEventWire
	w.winID = binary.LittleEndian.Uint32(p[0:])
	w.ev = p[4]
	w.canceled = p[5] != 0
	w.escaped = p[6] != 0
	w.pendingID = binary.LittleEndian.Uint32(p[8:])
	w.ops = binary.LittleEndian.Uint32(p[12:])
	w.suggestedOp = binary.LittleEndian.Uint32(p[16:])
	w.x = getF64(p[20:])
	w.y = getF64(p[28:])
	off := 36
	var ok bool
	if w.session, off, ok = readWireStr(p, off); !ok {
		return dragEventWire{}, false
	}
	if w.err, off, ok = readWireStr(p, off); !ok {
		return dragEventWire{}, false
	}
	fmtCount, off, ok := readUint32(p, off)
	if !ok || fmtCount > 128 {
		return dragEventWire{}, false
	}
	w.formats = make([]string, 0, fmtCount)
	for range fmtCount {
		var f string
		if f, off, ok = readWireStr(p, off); !ok {
			return dragEventWire{}, false
		}
		w.formats = append(w.formats, f)
	}
	repCount, off, ok := readUint32(p, off)
	if !ok || repCount > 128 {
		return dragEventWire{}, false
	}
	w.reps = make([]clipWireRep, 0, repCount)
	for range repCount {
		var fmtName string
		var data []byte
		if fmtName, off, ok = readWireStr(p, off); !ok {
			return dragEventWire{}, false
		}
		if data, off, ok = readWireBytes(p, off); !ok {
			return dragEventWire{}, false
		}
		w.reps = append(w.reps, clipWireRep{format: fmtName, data: data})
	}
	return w, off == len(p)
}

func encodeDragReplyMessage(winID, pendingID, op uint32, taken bool) []byte {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf[0:], winID)
	binary.LittleEndian.PutUint32(buf[4:], pendingID)
	binary.LittleEndian.PutUint32(buf[8:], op)
	if taken {
		buf[12] = 1
	}
	return buf
}

func decodeDragReplyMessage(p []byte) (winID, pendingID, op uint32, taken, ok bool) {
	if len(p) != 16 {
		return 0, 0, 0, false, false
	}
	return binary.LittleEndian.Uint32(p[0:]),
		binary.LittleEndian.Uint32(p[4:]),
		binary.LittleEndian.Uint32(p[8:]),
		p[12] != 0,
		true
}
