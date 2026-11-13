//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/egoist/mygo/internal/ohosproxy"
	"github.com/egoist/mygo/internal/ohosvideo"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/wait"
)

// Extended kindNetState payloads and MYGO_OHOS_HTTP_PROXY update the live HarmonyOS proxy cache while keeping 4-byte legacy payloads compatible.
func TestNetStateProxyPushAndInitialEnv(t *testing.T) {
	t.Cleanup(ohosproxy.Clear)
	ohosproxy.Clear()

	initial := ohosproxy.Config{
		Host:       "10.0.0.2",
		Port:       8888,
		Exclusions: []string{"localhost"},
	}
	t.Setenv(ohosproxy.EnvVar, ohosproxy.EncodeEnv(initial))
	b, _, hostConn, _ := newTestSurface(t)
	t.Cleanup(b.Quit)

	got, ok := ohosproxy.Get()
	if !ok || got.Host != "10.0.0.2" || got.Port != 8888 {
		t.Fatalf("initial ohosproxy.Get = (%+v, %v), want 10.0.0.2:8888", got, ok)
	}

	b.Network().Watch()
	kind, _, _, err := hostConn.read()
	if err != nil || kind != kindNetWatch {
		t.Fatalf("hostConn.read = (%d, %v), want kindNetWatch", kind, err)
	}

	app := &testAppHandler{}
	b.app = app

	live := ohosproxy.Config{
		Host:       "proxy.corp.local",
		Port:       3128,
		Username:   "alice",
		Password:   "s3cr3t",
		Exclusions: []string{"*.internal", "192.168.0.0/16"},
	}
	if err := hostConn.write(kindNetState, encodeNetStateProxyMessage(true, true, live)); err != nil {
		t.Fatalf("hostConn.write kindNetState: %v", err)
	}
	b.ensureReadLoop()
	b.Step()

	got, ok = ohosproxy.Get()
	if !ok || got.Host != "proxy.corp.local" || got.Port != 3128 || got.Username != "alice" || got.Password != "s3cr3t" || !slices.Equal(got.Exclusions, live.Exclusions) {
		t.Fatalf("live ohosproxy.Get = (%+v, %v), want %+v", got, ok, live)
	}
	if len(app.netChanges) != 1 || !app.netChanges[0].Online || !app.netChanges[0].Metered {
		t.Fatalf("app.netChanges = %+v, want [{Online:true Metered:true}]", app.netChanges)
	}

	// Clearing the system proxy via an empty Config updates ohosproxy to empty while still delivering NetworkChanged.
	if err := hostConn.write(kindNetState, encodeNetStateProxyMessage(false, false, ohosproxy.Config{})); err != nil {
		t.Fatalf("hostConn.write clear proxy: %v", err)
	}
	b.Step()
	got, ok = ohosproxy.Get()
	if !ok || got.Host != "" || got.Port != 0 {
		t.Fatalf("cleared ohosproxy.Get = (%+v, %v), want empty config", got, ok)
	}
}

// AppController badge, hide/unhide, about panel, and RTL language direction drive their corresponding window, dialog, and shell messages.
func TestAppControllerBadgeHideUnhideAboutAndRTL(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	t.Cleanup(b.Quit)
	app := &testAppHandler{}
	b.app = app
	b.appActive = true

	// Create and show a main window so Hide/Unhide has a visible window to manage.
	wh := &testWindowHandler{}
	pw, err := b.NewWindow(&platform.WindowOptions{Title: "Main", Width: 800, Height: 600, Surface: true}, wh)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	w := pw.(*Window)
	if kind, _, _, err := hostConn.read(); err != nil || kind != kindWinCreate {
		t.Fatalf("expected kindWinCreate, got (%d, %v)", kind, err)
	}
	w.Show()
	if kind, _, _, err := hostConn.read(); err != nil || kind != kindWinAction {
		t.Fatalf("expected kindWinAction Show, got (%d, %v)", kind, err)
	}

	b.App().SetBadge("7")
	if got := b.App().Badge(); got != "7" {
		t.Fatalf("Badge() = %q, want 7", got)
	}
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindShellReq {
		t.Fatalf("SetBadge message = (%d, %v), want kindShellReq", kind, err)
	}
	reqID, op, target, ok := decodeShellReqMessage(payload)
	if !ok || reqID != 0 || op != shellSetBadge || target != "7" {
		t.Fatalf("decodeShellReqMessage = (%d, %d, %q, %v), want (0, shellSetBadge, \"7\", true)", reqID, op, target, ok)
	}

	b.App().Hide()
	if !b.App().IsHidden() || w.IsVisible() || app.resigned != 1 {
		t.Fatalf("after Hide: IsHidden=%v winVisible=%v resigned=%d", b.App().IsHidden(), w.IsVisible(), app.resigned)
	}
	if kind, payload, _, err = hostConn.read(); err != nil || kind != kindWinAction || payload[4] != winActHide {
		t.Fatalf("expected winActHide, got kind=%d payload=%v err=%v", kind, payload, err)
	}

	b.App().Unhide()
	if b.App().IsHidden() || !w.IsVisible() {
		t.Fatalf("after Unhide: IsHidden=%v winVisible=%v", b.App().IsHidden(), w.IsVisible())
	}
	if kind, payload, _, err = hostConn.read(); err != nil || kind != kindWinAction || payload[4] != winActShow {
		t.Fatalf("expected winActShow, got kind=%d payload=%v err=%v", kind, payload, err)
	}

	b.App().ShowAboutPanel(platform.AboutPanelOptions{
		ApplicationName:    "MyGo Studio",
		ApplicationVersion: "1.2.0",
		Copyright:          "Copyright 2026",
	})
	if kind, _, _, err = hostConn.read(); err != nil || kind != kindDialogMsg {
		t.Fatalf("ShowAboutPanel sent kind=%d err=%v, want kindDialogMsg", kind, err)
	}

	// SetLanguages with rtl=true causes PopupMenu to encode rtl=1 even when Menu.RightToLeft is false.
	b.App().SetLanguages([]string{"ar"}, true)
	b.ensureReadLoop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		kind, p, _, rerr := hostConn.read()
		if rerr != nil || kind != kindPopupMenu {
			return
		}
		pReqID, _, _, _, rtl, _, decOK := decodePopupMenuMessage(p)
		if !decOK || !rtl {
			t.Errorf("PopupMenu rtl = %v (ok=%v), want true", rtl, decOK)
		}
		var reply [8]byte
		binary.LittleEndian.PutUint32(reply[0:], pReqID)
		_ = hostConn.write(kindPopupResult, reply[:])
	}()
	b.PopupMenu(&platform.Menu{Items: []*platform.MenuItem{{ID: 1, Label: " Item ", Visible: true, Enabled: true}}}, w, &platform.Point{X: 10, Y: 10})
	<-done
}

// Window.SetIcon validates PNG headers, and application or window menu accelerators fire on KeyPressed while respecting UpdateMenuItem state changes.
func TestWindowIconAndMenuAccelerators(t *testing.T) {
	b, s, hostConn, wh := newTestSurface(t)
	t.Cleanup(b.Quit)
	app := &peripheralAppHandler{}
	b.app = app
	w := s.w
	s.textInput = true

	var pngBuf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	if err := w.SetIcon(pngBuf.Bytes()); err != nil {
		t.Fatalf("SetIcon(valid): %v", err)
	}
	if err := w.SetIcon(nil); err != nil {
		t.Fatalf("SetIcon(nil): %v", err)
	}
	if err := w.SetIcon([]byte("not a png")); err == nil {
		t.Fatal("SetIcon accepted invalid PNG bytes")
	}
	_ = hostConn

	// Install an application menu with Ctrl+N (ID 10) and CmdOrCtrl+Plus (ID 11).
	b.SetApplicationMenu(&platform.Menu{
		Items: []*platform.MenuItem{
			{
				ID:      1,
				Type:    platform.MenuItemSubmenu,
				Visible: true,
				Enabled: true,
				Submenu: &platform.Menu{
					Items: []*platform.MenuItem{
						{ID: 10, Label: "New", Accelerator: "CmdOrCtrl+N", Visible: true, Enabled: true},
						{ID: 11, Label: "Zoom In", Accelerator: "CmdOrCtrl+Plus", Visible: true, Enabled: true},
					},
				},
			},
		},
	})

	sendKey := func(op byte, mods platform.Modifiers, arkCode int32, uni uint32) {
		var p [20]byte
		binary.LittleEndian.PutUint32(p[0:], w.id)
		p[4] = inputKey
		p[5] = op
		binary.LittleEndian.PutUint32(p[8:], uint32(mods))
		binary.LittleEndian.PutUint32(p[12:], uint32(arkCode))
		binary.LittleEndian.PutUint32(p[16:], uni)
		b.onInput(p[:])
	}

	// Ctrl+N (arkCode 2030 = 'N') triggers app menu item 10.
	sendKey(keyDown, platform.ModCtrl, 2030, 'n')
	sendKey(keyUp, platform.ModCtrl, 2030, 'n')
	if !slices.Equal(app.menuClicks, []int{10}) {
		t.Fatalf("app.menuClicks = %v, want [10]", app.menuClicks)
	}

	// Ctrl+Shift+= (arkCode 2058 = '=', unicode '+') triggers CmdOrCtrl+Plus (item 11).
	sendKey(keyDown, platform.ModCtrl|platform.ModShift, 2058, '+')
	sendKey(keyUp, platform.ModCtrl|platform.ModShift, 2058, '+')
	if !slices.Equal(app.menuClicks, []int{10, 11}) {
		t.Fatalf("app.menuClicks after zoom-in = %v, want [10 11]", app.menuClicks)
	}

	// Disabling item 10 via UpdateMenuItem stops Ctrl+N from firing.
	b.UpdateMenuItem(&platform.MenuItem{ID: 10, Label: "New", Accelerator: "CmdOrCtrl+N", Visible: true, Enabled: false})
	sendKey(keyDown, platform.ModCtrl, 2030, 'n')
	sendKey(keyUp, platform.ModCtrl, 2030, 'n')
	if !slices.Equal(app.menuClicks, []int{10, 11}) {
		t.Fatalf("disabled item still fired: %v", app.menuClicks)
	}

	// Setting a window-specific menu overrides the application menu and suppresses printable text commit when matched.
	wh.events = nil
	w.SetMenu(&platform.Menu{
		Items: []*platform.MenuItem{
			{ID: 42, Label: "Quick Jump", Accelerator: "K", Visible: true, Enabled: true},
		},
	})
	sendKey(keyDown, 0, 2027, 'k')
	sendKey(keyUp, 0, 2027, 'k')
	if !slices.Equal(wh.menuClicks, []int{42}) {
		t.Fatalf("wh.menuClicks = %v, want [42]", wh.menuClicks)
	}
	for _, ev := range wh.events {
		if ev.Kind == platform.TextInput {
			t.Fatalf("unexpected TextInput %q committed when menu accelerator matched", ev.Text)
		}
	}
}

// Shell.PreviewItem and Shell.OpenApplication validate inputs and dispatch shellPreviewItem and shellOpenApp to the host.
func TestShellPreviewItemAndOpenApplication(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	t.Cleanup(b.Quit)
	b.ensureReadLoop()

	tmpFile := filepath.Join(t.TempDir(), "sample.pdf")
	if err := os.WriteFile(tmpFile, []byte("%PDF-1.4"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := b.Shell().PreviewItem(filepath.Join(t.TempDir(), "missing.pdf")); err == nil {
		t.Fatal("PreviewItem accepted non-existent file")
	}
	if err := b.Shell().OpenApplication("   "); err == nil {
		t.Fatal("OpenApplication accepted blank target")
	}

	var gotOps []byte
	var gotTargets []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			kind, payload, _, err := hostConn.read()
			if err != nil || kind != kindShellReq {
				return
			}
			reqID, op, target, ok := decodeShellReqMessage(payload)
			if !ok {
				return
			}
			gotOps = append(gotOps, op)
			gotTargets = append(gotTargets, target)
			_ = hostConn.write(kindShellResult, encodeShellResultMessage(reqID, ""))
		}
	}()

	if err := b.Shell().PreviewItem(tmpFile); err != nil {
		t.Fatalf("PreviewItem: %v", err)
	}
	if err := b.Shell().OpenApplication("com.huawei.hmos.files"); err != nil {
		t.Fatalf("OpenApplication: %v", err)
	}
	<-done

	if !slices.Equal(gotOps, []byte{shellPreviewItem, shellOpenApp}) {
		t.Fatalf("shell ops = %v, want [%d %d]", gotOps, shellPreviewItem, shellOpenApp)
	}
	if len(gotTargets) != 2 || gotTargets[0] != tmpFile || gotTargets[1] != "com.huawei.hmos.files" {
		t.Fatalf("shell targets = %v, want [%q \"com.huawei.hmos.files\"]", gotTargets, tmpFile)
	}
}

// Backend bridges ohosvideo.OpenData, kindVideoShm SCM_RIGHTS descriptors, kindVideoEvent notifications, and kindVideoCmd playback controls over the Unix socketpair.
func TestVideoBridgeWireAndShmRoundTrip(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	t.Cleanup(b.Quit)
	ohosvideo.SetBridge(b)
	t.Cleanup(func() { ohosvideo.ClearBridge(b) })

	eng, err := ohosvideo.OpenData([]byte("mp4-wire-test"))
	if err != nil {
		t.Fatalf("ohosvideo.OpenData: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	kind, payload, openFD, err := hostConn.read()
	if err != nil || kind != kindVideoOpen || openFD < 0 {
		t.Fatalf("hostConn.read VideoOpen = (kind=%d, fd=%d, err=%v)", kind, openFD, err)
	}
	_ = syscall.Close(openFD)
	playerID, size, rawURL, ok := decodeVideoOpenMessage(payload)
	if !ok || size != int64(len("mp4-wire-test")) || rawURL != "" {
		t.Fatalf("decodeVideoOpenMessage = (id=%d, size=%d, url=%q, ok=%v)", playerID, size, rawURL, ok)
	}

	const w, h = 16, 12
	slotBytes := ohosvideo.SlotNeededBytes(w, h, ohosvideo.FormatBGRA)
	total := ohosvideo.HeaderBytes + slotBytes*ohosvideo.NumSlots
	shmFile, err := os.CreateTemp(t.TempDir(), "wire-video-shm-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer shmFile.Close()
	if err := shmFile.Truncate(int64(total)); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	mem, err := syscall.Mmap(int(shmFile.Fd()), 0, total, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Munmap(mem) })

	hdr := (*ohosvideo.ShmHeader)(unsafe.Pointer(&mem[0]))
	hdr.BusySlot.Store(-1)
	hdr.SlotBytes = uint32(slotBytes)
	hdr.Format = ohosvideo.FormatBGRA
	hdr.Width.Store(w)
	hdr.Height.Store(h)
	hdr.DurMs.Store(2400)
	hdr.PosMs.Store(300)
	slot := mem[ohosvideo.HeaderBytes : ohosvideo.HeaderBytes+slotBytes]
	for i := 0; i < len(slot); i += 4 {
		slot[i+0] = 255 // B
		slot[i+1] = 0
		slot[i+2] = 0
		slot[i+3] = 255
	}
	hdr.LatestSlot.Store(0)
	hdr.WriteSeq.Store(1)

	if err := hostConn.writeWithFD(kindVideoShm, encodeVideoShmMessage(playerID, uint32(slotBytes), w, h), int(shmFile.Fd())); err != nil {
		t.Fatalf("writeWithFD kindVideoShm: %v", err)
	}
	if err := hostConn.write(kindVideoEvent, encodeVideoEventMessage(playerID, ohosvideo.EvFrame, 0, 300, 2400, w, h, "")); err != nil {
		t.Fatalf("write kindVideoEvent: %v", err)
	}

	wait.Until(t, "video shm frame delivered through Backend readLoop", func() bool {
		_, _, hasFrame := eng.Frame(time.Now())
		return hasFrame
	})
	if gotW, gotH := eng.Size(); gotW != w || gotH != h {
		t.Fatalf("eng.Size() = %dx%d, want %dx%d", gotW, gotH, w, h)
	}
	if eng.Duration() != 2400*time.Millisecond || eng.Position() != 300*time.Millisecond {
		t.Fatalf("Duration=%v Position=%v, want 2.4s / 300ms", eng.Duration(), eng.Position())
	}

	eng.SetRate(1.5)
	kind, cmdPayload, _, err := hostConn.read()
	if err != nil || kind != kindVideoCmd {
		t.Fatalf("expected kindVideoCmd, got (%d, %v)", kind, err)
	}
	gotID, op, _, farg, ok := decodeVideoCmdMessage(cmdPayload)
	if !ok || gotID != playerID || op != ohosvideo.CmdSetRate || farg != 1.5 {
		t.Fatalf("decodeVideoCmdMessage = (%d, %d, %v, %v)", gotID, op, farg, ok)
	}
}
