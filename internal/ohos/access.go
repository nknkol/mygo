//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"

	"github.com/egoist/mygo/internal/platform"
)

const (
	accessEvEnabled byte = 1
	accessEvAction  byte = 2
)

type accessEventWire struct {
	winID      uint32
	ev         byte
	actionKind byte
	nodeID     uint64
	from, to   int32
	text       string
}

func (s *Surface) UpdateAccessibility(t *platform.AccessTree) {
	if s == nil || s.w == nil || s.w.closed || s.w.b == nil || t == nil {
		return
	}
	s.w.b.send(kindAccessTree, encodeAccessTreeMessage(s.w.id, t))
}

func (b *Backend) onAccessEvent(payload []byte) {
	ev, ok := decodeAccessEventMessage(payload)
	if !ok {
		return
	}
	win := b.windowByID(ev.winID)
	if win == nil || win.closed || win.surface == nil {
		return
	}
	s := win.surface
	switch ev.ev {
	case accessEvEnabled:
		if !s.accessEnabled {
			s.accessEnabled = true
			s.send(platform.SurfaceEvent{Kind: platform.AccessibilityOn})
		}
	case accessEvAction:
		s.send(platform.SurfaceEvent{
			Kind:   platform.AccessAction,
			ID:     ev.nodeID,
			Action: platform.AccessActionKind(ev.actionKind),
			From:   int(ev.from),
			To:     int(ev.to),
			Text:   ev.text,
		})
	}
}

func encodeAccessTreeMessage(winID uint32, t *platform.AccessTree) []byte {
	var hdr [12]byte
	binary.LittleEndian.PutUint32(hdr[0:], winID)
	binary.LittleEndian.PutUint64(hdr[4:], t.Focus)
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, t.Selected)
	buf = appendUint32(buf, uint32(len(t.Announcements)))
	for _, a := range t.Announcements {
		buf = appendWireStr(buf, a)
	}
	buf = appendUint32(buf, uint32(len(t.Alerts)))
	for _, a := range t.Alerts {
		buf = appendWireStr(buf, a)
	}
	buf = appendUint32(buf, uint32(len(t.Nodes)))
	for _, n := range t.Nodes {
		var nb [84]byte
		binary.LittleEndian.PutUint64(nb[0:], n.ID)
		binary.LittleEndian.PutUint32(nb[8:], uint32(int32(n.Parent)))
		nb[12] = byte(n.Role)
		nb[13] = byte(n.Actions)
		nb[14] = byte(n.Current)
		nb[15] = byte(n.HasPopup)
		binary.LittleEndian.PutUint32(nb[16:], uint32(n.States))
		putF32(nb[20:], float32(n.Bounds.X))
		putF32(nb[24:], float32(n.Bounds.Y))
		putF32(nb[28:], float32(n.Bounds.W))
		putF32(nb[32:], float32(n.Bounds.H))
		putF64(nb[36:], n.Min)
		putF64(nb[44:], n.Max)
		putF64(nb[52:], n.Now)
		putF64(nb[60:], n.Step)
		binary.LittleEndian.PutUint32(nb[68:], uint32(int32(n.SelStart)))
		binary.LittleEndian.PutUint32(nb[72:], uint32(int32(n.SelEnd)))
		binary.LittleEndian.PutUint32(nb[76:], uint32(int32(n.Level)))
		binary.LittleEndian.PutUint16(nb[80:], uint16(max(0, min(n.PosInSet, 65535))))
		binary.LittleEndian.PutUint16(nb[82:], uint16(max(0, min(n.SetSize, 65535))))
		buf = append(buf, nb[:]...)
		buf = appendWireStr(buf, n.Label)
		buf = appendWireStr(buf, n.Value)
		buf = appendWireStr(buf, n.Placeholder)
		buf = appendWireStr(buf, n.Description)
		buf = appendWireStr(buf, n.RoleDescription)
	}
	return buf
}

func decodeAccessTreeMessage(p []byte) (winID uint32, tree platform.AccessTree, ok bool) {
	if len(p) < 12 {
		return 0, platform.AccessTree{}, false
	}
	winID = binary.LittleEndian.Uint32(p[0:])
	tree.Focus = binary.LittleEndian.Uint64(p[4:])
	off := 12
	if tree.Selected, off, ok = readWireStr(p, off); !ok {
		return 0, platform.AccessTree{}, false
	}
	annCount, off, ok := readUint32(p, off)
	if !ok || annCount > 256 {
		return 0, platform.AccessTree{}, false
	}
	if annCount > 0 {
		tree.Announcements = make([]string, 0, annCount)
		for range annCount {
			var s string
			if s, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			tree.Announcements = append(tree.Announcements, s)
		}
	}
	alertCount, off, ok := readUint32(p, off)
	if !ok || alertCount > 256 {
		return 0, platform.AccessTree{}, false
	}
	if alertCount > 0 {
		tree.Alerts = make([]string, 0, alertCount)
		for range alertCount {
			var s string
			if s, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			tree.Alerts = append(tree.Alerts, s)
		}
	}
	nodeCount, off, ok := readUint32(p, off)
	if !ok || nodeCount > 16384 {
		return 0, platform.AccessTree{}, false
	}
	if nodeCount > 0 {
		tree.Nodes = make([]platform.AccessNode, 0, nodeCount)
		for range nodeCount {
			if off+84 > len(p) {
				return 0, platform.AccessTree{}, false
			}
			nb := p[off : off+84]
			off += 84
			var n platform.AccessNode
			n.ID = binary.LittleEndian.Uint64(nb[0:])
			n.Parent = int(int32(binary.LittleEndian.Uint32(nb[8:])))
			n.Role = platform.AccessRole(nb[12])
			n.Actions = platform.AccessActions(nb[13])
			n.Current = platform.AccessCurrent(nb[14])
			n.HasPopup = platform.AccessPopup(nb[15])
			n.States = platform.AccessStates(binary.LittleEndian.Uint32(nb[16:]))
			n.Bounds = platform.RectF{
				X: float64(getF32(nb[20:])),
				Y: float64(getF32(nb[24:])),
				W: float64(getF32(nb[28:])),
				H: float64(getF32(nb[32:])),
			}
			n.Min = getF64(nb[36:])
			n.Max = getF64(nb[44:])
			n.Now = getF64(nb[52:])
			n.Step = getF64(nb[60:])
			n.SelStart = int(int32(binary.LittleEndian.Uint32(nb[68:])))
			n.SelEnd = int(int32(binary.LittleEndian.Uint32(nb[72:])))
			n.Level = int(int32(binary.LittleEndian.Uint32(nb[76:])))
			n.PosInSet = int(binary.LittleEndian.Uint16(nb[80:]))
			n.SetSize = int(binary.LittleEndian.Uint16(nb[82:]))
			if n.Label, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			if n.Value, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			if n.Placeholder, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			if n.Description, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			if n.RoleDescription, off, ok = readWireStr(p, off); !ok {
				return 0, platform.AccessTree{}, false
			}
			tree.Nodes = append(tree.Nodes, n)
		}
	}
	return winID, tree, off == len(p)
}

func encodeAccessEventMessage(w accessEventWire) []byte {
	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:], w.winID)
	hdr[4] = w.ev
	hdr[5] = w.actionKind
	binary.LittleEndian.PutUint64(hdr[8:], w.nodeID)
	binary.LittleEndian.PutUint32(hdr[16:], uint32(w.from))
	binary.LittleEndian.PutUint32(hdr[20:], uint32(w.to))
	buf := append([]byte(nil), hdr[:]...)
	buf = appendWireStr(buf, w.text)
	return buf
}

func decodeAccessEventMessage(p []byte) (accessEventWire, bool) {
	if len(p) < 24 {
		return accessEventWire{}, false
	}
	var w accessEventWire
	w.winID = binary.LittleEndian.Uint32(p[0:])
	w.ev = p[4]
	w.actionKind = p[5]
	w.nodeID = binary.LittleEndian.Uint64(p[8:])
	w.from = int32(binary.LittleEndian.Uint32(p[16:]))
	w.to = int32(binary.LittleEndian.Uint32(p[20:]))
	var off int
	var ok bool
	if w.text, off, ok = readWireStr(p, 24); !ok || off != len(p) {
		return accessEventWire{}, false
	}
	return w, true
}
