//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

type peripheralAppHandler struct {
	platform.AppHandler
	hotkeys      []int
	menuClicks   []int
	themeChanges int
}

func (h *peripheralAppHandler) HotkeyPressed(id int)   { h.hotkeys = append(h.hotkeys, id) }
func (h *peripheralAppHandler) MenuItemClicked(id int) { h.menuClicks = append(h.menuClicks, id) }
func (h *peripheralAppHandler) ThemeChanged()          { h.themeChanges++ }

type testTrayHandler struct {
	clicks       int
	rightClicks  int
	appearChange int
}

func (h *testTrayHandler) Clicked()           { h.clicks++ }
func (h *testTrayHandler) RightClicked()      { h.rightClicks++ }
func (h *testTrayHandler) AppearanceChanged() { h.appearChange++ }

// Secrets store, read and delete credentials across goroutines even before Init and map missing entries to ErrSecretNotFound.
func TestSecretsStoreRoundTripAndErrors(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	b := NewWithFD(fds[0])
	hostConn := &conn{fd: fds[1]}
	hostDone := make(chan struct{})
	t.Cleanup(func() {
		b.Quit()
		<-hostDone
		hostConn.close()
	})

	var mu sync.Mutex
	vault := map[string][]byte{}
	go func() {
		defer close(hostDone)
		for {
			kind, payload, fd, rerr := hostConn.read()
			if fd >= 0 {
				syscall.Close(fd)
			}
			if rerr != nil || kind == kindQuit {
				return
			}
			if kind != kindSecretReq {
				continue
			}
			reqID, op, svc, acct, sec, ok := decodeSecretReqMessage(payload)
			if !ok {
				continue
			}
			key := svc + "\x00" + acct
			var res secretWireRes
			mu.Lock()
			switch op {
			case secretOpSet:
				vault[key] = bytes.Clone(sec)
			case secretOpGet:
				if v, exists := vault[key]; exists {
					res.secret = bytes.Clone(v)
				} else {
					res.notFound = true
				}
			case secretOpDel:
				delete(vault, key)
			}
			mu.Unlock()
			_ = hostConn.write(kindSecretRes, encodeSecretResMessage(reqID, res))
		}
	}()

	secrets := b.Secrets()
	if _, err := secrets.Secret("dev.egoist.music", "token"); !errors.Is(err, platform.ErrSecretNotFound) {
		t.Fatalf("missing secret err = %v, want ErrSecretNotFound", err)
	}
	if err := secrets.SetSecret("dev.egoist.music", "empty", []byte{}); err != nil {
		t.Fatalf("SetSecret empty: %v", err)
	}
	gotEmpty, err := secrets.Secret("dev.egoist.music", "empty")
	if err != nil || len(gotEmpty) != 0 {
		t.Fatalf("Secret empty = (%q, %v), want empty nil", gotEmpty, err)
	}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			acct := string(rune('a' + idx))
			val := []byte("secret-" + acct)
			if err := secrets.SetSecret("svc", acct, val); err != nil {
				t.Errorf("SetSecret %s: %v", acct, err)
				return
			}
			got, err := secrets.Secret("svc", acct)
			if err != nil || !bytes.Equal(got, val) {
				t.Errorf("Secret %s = (%q, %v), want %q", acct, got, err, val)
				return
			}
			if err := secrets.DeleteSecret("svc", acct); err != nil {
				t.Errorf("DeleteSecret %s: %v", acct, err)
				return
			}
			if _, err := secrets.Secret("svc", acct); !errors.Is(err, platform.ErrSecretNotFound) {
				t.Errorf("after DeleteSecret %s: err = %v, want ErrSecretNotFound", acct, err)
			}
		}(i)
	}
	wg.Wait()
}

// Global hotkeys translate accelerators to HarmonyOS key codes, surface host errors, fire HotkeyPressed and unregister cleanly.
func TestGlobalHotkeyRegistrationAndTrigger(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	appH := &peripheralAppHandler{}
	b.app = appH

	type regCall struct {
		id       int32
		reg      bool
		finalKey int32
		preKeys  []int32
	}
	regCh := make(chan regCall, 4)
	hostDone := make(chan struct{})
	t.Cleanup(func() {
		b.Quit()
		<-hostDone
	})
	go func() {
		defer close(hostDone)
		for {
			kind, payload, fd, err := hostConn.read()
			if fd >= 0 {
				syscall.Close(fd)
			}
			if err != nil || kind == kindQuit {
				return
			}
			if kind != kindHotkeyReq {
				continue
			}
			reqID, hkID, reg, finalKey, preKeys, ok := decodeHotkeyReqMessage(payload)
			if !ok {
				continue
			}
			regCh <- regCall{id: hkID, reg: reg, finalKey: finalKey, preKeys: preKeys}
			if reqID != 0 {
				errStr := ""
				if hkID == 99 {
					errStr = "hotkey occupied by system"
				}
				_ = hostConn.write(kindHotkeyRes, encodeHotkeyResMessage(reqID, errStr))
			}
		}
	}()

	if err := b.RegisterHotkey(7, "CmdOrCtrl+Shift+M"); err != nil {
		t.Fatalf("RegisterHotkey: %v", err)
	}
	got := <-regCh
	if got.id != 7 || !got.reg || got.finalKey != 2029 || !slices.Equal(got.preKeys, []int32{2072, 2047}) {
		t.Fatalf("registered hotkey = %+v, want id=7 final=2029 pre=[2072 2047]", got)
	}

	if err := b.RegisterHotkey(99, "Alt+F4"); err == nil {
		t.Fatalf("expected error for occupied hotkey")
	}
	<-regCh

	if err := b.RegisterHotkey(1, ""); err == nil {
		t.Fatalf("expected error for empty accelerator")
	}

	var trig [4]byte
	binary.LittleEndian.PutUint32(trig[:], 7)
	if err := hostConn.write(kindHotkeyTrig, trig[:]); err != nil {
		t.Fatalf("write kindHotkeyTrig: %v", err)
	}
	b.Step()
	if !slices.Equal(appH.hotkeys, []int{7}) {
		t.Fatalf("hotkeys = %v, want [7]", appH.hotkeys)
	}

	b.UnregisterHotkey(7)
	unreg := <-regCh
	if unreg.id != 7 || unreg.reg {
		t.Fatalf("unregister = %+v, want id=7 reg=false", unreg)
	}
}

// System tray synchronizes icon, tooltip and menus, refreshes mutated items, dispatches clicks and tracks dark mode.
func TestTrayLifecycleMenuUpdateAndAppearance(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	appH := &peripheralAppHandler{}
	b.app = appH

	th := &testTrayHandler{}
	tr, err := b.NewTray(th)
	if err != nil {
		t.Fatalf("NewTray: %v", err)
	}
	if err := tr.SetImage([]byte{0x89, 'P', 'N', 'G'}, true); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindTrayUpdate {
		t.Fatalf("SetImage msg = (%d, %v)", kind, err)
	}
	w1, ok := decodeTrayUpdateMessage(payload)
	if !ok || w1.op != trayOpUpsert || w1.flags&(trayFlagHasImage|trayFlagTemplateImg) != (trayFlagHasImage|trayFlagTemplateImg) {
		t.Fatalf("SetImage wire = %+v, ok=%v", w1, ok)
	}

	tr.SetToolTip("MyGo Music")
	if _, payload, _, err = hostConn.read(); err != nil {
		t.Fatalf("read tooltip: %v", err)
	}
	w2, ok := decodeTrayUpdateMessage(payload)
	if !ok || w2.tooltip != "MyGo Music" {
		t.Fatalf("tooltip wire = %+v", w2)
	}

	tr.SetMenu(&platform.Menu{
		Items: []*platform.MenuItem{
			{ID: 11, Type: platform.MenuItemNormal, Label: "Play", Enabled: true, Visible: true},
			{Type: platform.MenuItemSeparator, Visible: true},
			{ID: 12, Type: platform.MenuItemCheckbox, Label: "Repeat", Enabled: true, Visible: true, Checked: true},
		},
	})
	if _, payload, _, err = hostConn.read(); err != nil {
		t.Fatalf("read menu: %v", err)
	}
	w3, ok := decodeTrayUpdateMessage(payload)
	if !ok || w3.flags&trayFlagHasMenu == 0 || len(w3.menuWire) != 3 {
		t.Fatalf("menu wire = %+v", w3)
	}

	b.UpdateMenuItem(&platform.MenuItem{
		ID:      11,
		Type:    platform.MenuItemNormal,
		Label:   "Pause",
		Enabled: true,
		Visible: true,
	})
	if _, payload, _, err = hostConn.read(); err != nil {
		t.Fatalf("read updated menu: %v", err)
	}
	w4, ok := decodeTrayUpdateMessage(payload)
	if !ok || len(w4.menuWire) != 3 || w4.menuWire[0].label != "Pause" {
		t.Fatalf("updated menu wire = %+v", w4)
	}

	b.onTrayEvent(encodeTrayEventMessage(w1.trayID, trayEvClick, 0))
	b.onTrayEvent(encodeTrayEventMessage(w1.trayID, trayEvRightClick, 0))
	b.onTrayEvent(encodeTrayEventMessage(w1.trayID, trayEvMenuItem, 11))
	if th.clicks != 1 || th.rightClicks != 1 || !slices.Equal(appH.menuClicks, []int{11}) {
		t.Fatalf("events: clicks=%d right=%d menu=%v", th.clicks, th.rightClicks, appH.menuClicks)
	}

	b.setSystemDark(true)
	if !tr.Dark() || th.appearChange != 1 {
		t.Fatalf("dark=%v appearChange=%d", tr.Dark(), th.appearChange)
	}

	tr.Destroy()
	if _, payload, _, err = hostConn.read(); err != nil {
		t.Fatalf("read destroy: %v", err)
	}
	w5, ok := decodeTrayUpdateMessage(payload)
	if !ok || w5.op != trayOpDestroy {
		t.Fatalf("destroy wire = %+v", w5)
	}
}

// Theme preferences load initial text scale and accessibility flags from the environment and emit ThemeChanged on live updates.
func TestThemePreferencesEnvAndLiveUpdate(t *testing.T) {
	t.Setenv("MYGO_OHOS_TEXT_SCALE", "1.25")
	t.Setenv("MYGO_OHOS_HIGH_CONTRAST", "1")
	b, _, _, _ := newTestSurface(t)
	appH := &peripheralAppHandler{}
	b.app = appH

	p := b.Theme().Preferences()
	if p.TextScale != 1.25 || !p.HighContrast || p.ReduceMotion {
		t.Fatalf("initial prefs = %+v", p)
	}

	b.onEnvInfo(encodeEnvInfoMessage(envInfoWire{
		textScaleMilli: 1500,
		prefFlags:      1 | 4, // hasPrefs + reduceMotion, highContrast cleared
	}))
	p2 := b.Theme().Preferences()
	if p2.TextScale != 1.5 || p2.HighContrast || !p2.ReduceMotion || appH.themeChanges != 1 {
		t.Fatalf("updated prefs = %+v, themeChanges = %d", p2, appH.themeChanges)
	}
}

// Surface drag-and-drop handles outgoing sessions, hover negotiation, file-URI drops and cancellation cleanup.
func TestSurfaceDataDragSourceAndDestination(t *testing.T) {
	b, s, hostConn, wh := newTestSurface(t)

	s.SetDropFormats([]transfer.Format{transfer.Text, transfer.FileList})
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindDragSetFormats {
		t.Fatalf("SetDropFormats msg = (%d, %v)", kind, err)
	}
	_, fmts, ok := decodeDragSetFormatsMessage(payload)
	if !ok || !slices.Equal(fmts, []string{"text/plain", "text/uri-list"}) {
		t.Fatalf("drop formats = %v, ok=%v", fmts, ok)
	}

	var doneRes *transfer.Result
	srcData := transfer.New(transfer.NewItem(transfer.Bytes(transfer.Text, []byte("hello drag"))))
	s.StartDataDrag(platform.DragRequest{
		Data:       srcData,
		Operations: transfer.Copy | transfer.Move,
		Session:    "sess-42",
		X:          24,
		Y:          36,
		Width:      2,
		Height:     2,
		Preview:    make([]byte, 16),
		Done: func(r transfer.Result) {
			cp := r
			doneRes = &cp
		},
	})
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindDragStart {
		t.Fatalf("StartDataDrag msg = (%d, %v)", kind, err)
	}
	ds, ok := decodeDragStartMessage(payload)
	if !ok || ds.session != "sess-42" || len(ds.reps) != 2 {
		t.Fatalf("drag start wire = %+v, ok=%v", ds, ok)
	}

	wh.take = func(ev platform.SurfaceEvent) bool {
		if ev.Kind == platform.DataDragOver && ev.Drag != nil {
			ev.Drag.Operation = transfer.Copy
			ev.Drag.Formats = []transfer.Format{transfer.FileList}
			return true
		}
		return ev.Kind == platform.DataDrop
	}

	// Hover + drop with external file URI list and live local session token.
	b.onDragEvent(encodeDragEventMessage(dragEventWire{
		winID:     s.w.id,
		ev:        dragEvEnter,
		pendingID: 9,
		ops:       ohosDragOpCopy | ohosDragOpMove,
		x:         50,
		y:         60,
		session:   "sess-42",
		formats:   []string{"general.file-uri", string(platform.DragSessionFormat)},
	}))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindDragReply {
		t.Fatalf("drag enter reply = (%d, %v)", kind, err)
	}
	_, pid, rop, taken, ok := decodeDragReplyMessage(payload)
	if !ok || pid != 9 || rop != ohosDragOpCopy || !taken {
		t.Fatalf("enter reply = pid=%d op=%d taken=%v", pid, rop, taken)
	}

	b.onDragEvent(encodeDragEventMessage(dragEventWire{
		winID:     s.w.id,
		ev:        dragEvDrop,
		pendingID: 10,
		ops:       ohosDragOpCopy,
		x:         55,
		y:         65,
		formats:   []string{"general.file-uri"},
		reps: []clipWireRep{
			{format: "general.file-uri", data: []byte("file:///data/storage/el2/base/song%201.flac\r\n")},
		},
	}))
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindDragReply {
		t.Fatalf("drop reply = (%d, %v)", kind, err)
	}
	var sawDrop bool
	for _, ev := range wh.events {
		if ev.Kind == platform.DataDrop {
			sawDrop = true
			if !slices.Equal(ev.Files, []string{"/data/storage/el2/base/song 1.flac"}) {
				t.Fatalf("drop files = %v", ev.Files)
			}
		}
	}
	if !sawDrop {
		t.Fatalf("expected DataDrop event")
	}

	// Complete outgoing drag via SourceEnded.
	b.onDragEvent(encodeDragEventMessage(dragEventWire{
		winID: s.w.id,
		ev:    dragEvSourceEnded,
		ops:   ohosDragOpMove,
	}))
	if doneRes == nil || doneRes.Operation != transfer.Move || doneRes.Canceled {
		t.Fatalf("Done result = %+v, want Move", doneRes)
	}
}

// Surface accessibility enables tree updates on demand, encodes full node snapshots and forwards assistive actions.
func TestSurfaceAccessibilityTreeAndActions(t *testing.T) {
	b, s, hostConn, wh := newTestSurface(t)

	b.onAccessEvent(encodeAccessEventMessage(accessEventWire{
		winID: s.w.id,
		ev:    accessEvEnabled,
	}))
	if len(wh.events) != 1 || wh.events[0].Kind != platform.AccessibilityOn {
		t.Fatalf("events after enable = %+v, want [AccessibilityOn]", wh.events)
	}

	tree := &platform.AccessTree{
		Focus:         102,
		Selected:      "已选择",
		Announcements: []string{"正在播放"},
		Alerts:        []string{"连接断开"},
		Nodes: []platform.AccessNode{
			{
				ID:      101,
				Parent:  -1,
				Role:    platform.RoleGroup,
				Label:   "Player",
				Bounds:  platform.RectF{X: 0, Y: 0, W: 400, H: 80},
				Actions: 0,
			},
			{
				ID:          102,
				Parent:      0,
				Role:        platform.RoleSlider,
				Label:       "Volume",
				Value:       "75%",
				Description: "Master volume",
				Bounds:      platform.RectF{X: 20, Y: 20, W: 120, H: 24},
				States:      platform.AccessFocusable,
				Actions:     platform.ActionFocus | platform.ActionIncrement | platform.ActionDecrement | platform.ActionSetValue,
				Min:         0,
				Max:         100,
				Now:         75,
				Step:        5,
			},
		},
	}
	s.UpdateAccessibility(tree)
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindAccessTree {
		t.Fatalf("UpdateAccessibility msg = (%d, %v)", kind, err)
	}
	winID, decoded, ok := decodeAccessTreeMessage(payload)
	if !ok || winID != s.w.id || decoded.Focus != 102 || len(decoded.Nodes) != 2 || decoded.Nodes[1].Now != 75 {
		t.Fatalf("decoded tree = (win=%d, ok=%v, %+v)", winID, ok, decoded)
	}

	b.onAccessEvent(encodeAccessEventMessage(accessEventWire{
		winID:      s.w.id,
		ev:         accessEvAction,
		actionKind: byte(platform.AccessSetValue),
		nodeID:     102,
		text:       "80",
	}))
	last := wh.events[len(wh.events)-1]
	if last.Kind != platform.AccessAction || last.ID != 102 || last.Action != platform.AccessSetValue || last.Text != "80" {
		t.Fatalf("AccessAction event = %+v", last)
	}
}
