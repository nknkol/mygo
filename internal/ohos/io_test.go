//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

// Owned clipboard data keeps lazy providers untouched until requested or flushed, syncs to the host on app blur, and yields ownership on external updates.
func TestClipboardLazyOwnershipFlushBlurAndExternalRead(t *testing.T) {
	b, s, hostConn, _ := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app
	go b.readLoop()
	t.Cleanup(b.Quit)

	custom := transfer.Format("application/vnd.mygo.track+json")
	var lazyCalls, releasedCount atomic.Int32
	data := transfer.New(
		transfer.NewItem(
			transfer.Bytes(transfer.HTML, []byte("<b>Song</b>")),
			transfer.Lazy(transfer.Text, func() ([]byte, error) {
				lazyCalls.Add(1)
				return []byte("Song"), nil
			}),
			transfer.Bytes(custom, []byte(`{"id":42}`)),
		),
	)

	if err := b.Clipboard().WriteData(data.Snapshot(), true, func() { releasedCount.Add(1) }); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if got := b.Clipboard().Formats(); !slices.Equal(got, []transfer.Format{transfer.HTML, transfer.Text, custom}) {
		t.Fatalf("Formats() = %v", got)
	}
	if lazyCalls.Load() != 0 || releasedCount.Load() != 0 {
		t.Fatalf("WriteData invoked lazy provider (%d) or release (%d)", lazyCalls.Load(), releasedCount.Load())
	}

	// Reading only custom MIME leaves the lazy Text provider untouched.
	customRead, err := b.Clipboard().ReadData([]transfer.Format{custom})
	if err != nil {
		t.Fatalf("ReadData(custom): %v", err)
	}
	rawCustom, err := customRead.Read(custom)
	if err != nil || string(rawCustom) != `{"id":42}` || lazyCalls.Load() != 0 {
		t.Fatalf("custom Read = (%q, %v), lazyCalls=%d", string(rawCustom), err, lazyCalls.Load())
	}

	// Losing app focus materializes shareable formats and pushes them to the host with concealed=true.
	b.onInput(encodeWindowInput(s.w.id, winFocus))
	b.onInput(encodeWindowInput(s.w.id, winBlur))
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindClipWrite {
		t.Fatalf("after winBlur: kind=%d err=%v, want kindClipWrite", kind, err)
	}
	cw, ok := decodeClipWriteMessage(payload)
	if !ok || cw.clear || !cw.concealed || cw.text != "Song" || cw.html != "<b>Song</b>" {
		t.Fatalf("winBlur clipWrite = %+v (ok=%v)", cw, ok)
	}
	if lazyCalls.Load() != 1 || releasedCount.Load() != 0 {
		t.Fatalf("after winBlur: lazyCalls=%d released=%d, want 1 and 0", lazyCalls.Load(), releasedCount.Load())
	}

	// Flush releases the owned provider hook once while preserving readable contents.
	if err := b.Clipboard().Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	kind, _, _, err = hostConn.read()
	if err != nil || kind != kindClipWrite {
		t.Fatalf("Flush message kind=%d err=%v", kind, err)
	}
	if releasedCount.Load() != 1 || b.Clipboard().ReadText() != "Song" {
		t.Fatalf("after Flush: released=%d text=%q", releasedCount.Load(), b.Clipboard().ReadText())
	}

	// Invalid PNG is rejected without disturbing current clipboard contents.
	if err := b.Clipboard().WriteImage([]byte("not-a-png")); err == nil {
		t.Fatal("WriteImage accepted corrupt PNG bytes")
	}
	if b.Clipboard().ReadText() != "Song" {
		t.Fatalf("failed WriteImage clobbered clipboard: %q", b.Clipboard().ReadText())
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewNRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	if err := b.Clipboard().WriteImage(pngBuf.Bytes()); err != nil {
		t.Fatalf("WriteImage(valid): %v", err)
	}
	_, _, _, _ = hostConn.read() // consume WriteImage kindClipWrite
	if !bytes.Equal(b.Clipboard().ReadImage(), pngBuf.Bytes()) {
		t.Fatal("ReadImage did not round-trip PNG bytes")
	}

	// External pasteboard update replaces owned clipboard and advertises external formats.
	if err := hostConn.write(kindClipChanged, encodeClipChangedMessage(7, true, []string{"text/plain", "text/uri"})); err != nil {
		t.Fatalf("write kindClipChanged: %v", err)
	}
	b.Step()
	if got := b.Clipboard().Formats(); !slices.Equal(got, []transfer.Format{transfer.Text, transfer.FileList, transfer.URIList}) {
		t.Fatalf("external Formats() = %v", got)
	}

	// External ReadData round-trips over kindClipRead -> kindClipData.
	go func() {
		k, p, _, rerr := hostConn.read()
		if rerr != nil || k != kindClipRead {
			return
		}
		reqID, fmts, ok := decodeClipReadMessage(p)
		if !ok || !slices.Equal(fmts, []string{"text/plain"}) {
			return
		}
		_ = hostConn.write(kindClipData, encodeClipDataMessage(reqID, "", []clipWireRep{
			{format: "text/plain", data: []byte("from harmonyos")},
		}))
	}()
	if got := b.Clipboard().ReadText(); got != "from harmonyos" {
		t.Fatalf("external ReadText() = %q, want %q", got, "from harmonyos")
	}

	// Ownership change mid-read returns platform.ErrClipboardChanged instead of mixing owners.
	go func() {
		k, p, _, rerr := hostConn.read()
		if rerr != nil || k != kindClipRead {
			return
		}
		reqID, _, ok := decodeClipReadMessage(p)
		if !ok {
			return
		}
		_ = hostConn.write(kindClipChanged, encodeClipChangedMessage(8, true, []string{"text/plain"}))
		_ = hostConn.write(kindClipData, encodeClipDataMessage(reqID, "", []clipWireRep{
			{format: "text/plain", data: []byte("stale")},
		}))
	}()
	if _, err := b.Clipboard().ReadData([]transfer.Format{transfer.Text}); !errors.Is(err, platform.ErrClipboardChanged) {
		t.Fatalf("mid-read ownership change err = %v, want ErrClipboardChanged", err)
	}
}

// Open, save, and message-box dialogs normalize MIME/extension filters and invoke main-thread callbacks once, and Shell calls bypass queued frames.
func TestDialogsAndShellRoundTrip(t *testing.T) {
	b, s, hostConn, _ := newTestSurface(t)
	go b.readLoop()
	t.Cleanup(b.Quit)

	var gotOpen []string
	b.Dialogs().ShowOpenDialog(s.w, &platform.OpenDialogOptions{
		Title:           "Import Music",
		OpenFiles:       true,
		OpenDirectories: false,
		Multiple:        true,
		Filters: []platform.FileFilter{
			{Name: "Audio", Extensions: []string{".MP3", "*.flac"}, MIMETypes: []string{"audio/ogg"}},
		},
	}, func(paths []string, err error) {
		if err != nil {
			t.Errorf("ShowOpenDialog err: %v", err)
		}
		gotOpen = paths
	})

	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindDialogOpen {
		t.Fatalf("ShowOpenDialog kind=%d err=%v", kind, err)
	}
	dlg, ok := decodeFileDialogMessage(payload)
	if !ok || dlg.winID != s.w.id || dlg.flags&(dlgFlagOpenFiles|dlgFlagMultiple) != (dlgFlagOpenFiles|dlgFlagMultiple) ||
		len(dlg.filters) != 1 || !slices.Equal(dlg.filters[0].Extensions, []string{"mp3", "flac", "ogg", "opus"}) {
		t.Fatalf("decoded open dialog = %+v (ok=%v)", dlg, ok)
	}
	if err := hostConn.write(kindDialogResult, encodeDialogResultMessage(dialogResultWire{
		reqID: dlg.reqID,
		paths: []string{"/storage/Users/currentUser/Music/a.flac", "/storage/Users/currentUser/Music/b.mp3"},
	})); err != nil {
		t.Fatalf("write dialog result: %v", err)
	}
	b.Step()
	if len(gotOpen) != 2 || gotOpen[0] != "/storage/Users/currentUser/Music/a.flac" {
		t.Fatalf("gotOpen = %v", gotOpen)
	}

	var gotMsg platform.MessageBoxResult
	b.Dialogs().ShowMessageBox(s.w, &platform.MessageBoxOptions{
		Type:            "warning",
		Title:           "Remove Playlist",
		Message:         "Delete this playlist?",
		Detail:          "Songs on disk will not be deleted.",
		Buttons:         []string{"Delete", "Cancel"},
		DefaultID:       0,
		CancelID:        1,
		DestructiveIDs:  []int{0},
		CheckboxLabel:   "Do not ask again",
		CheckboxChecked: false,
	}, func(res platform.MessageBoxResult, err error) {
		if err != nil {
			t.Errorf("ShowMessageBox err: %v", err)
		}
		gotMsg = res
	})
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindDialogMsg {
		t.Fatalf("ShowMessageBox kind=%d err=%v", kind, err)
	}
	mb, ok := decodeMessageBoxMessage(payload)
	if !ok || mb.cancelID != 1 || len(mb.destructiveIDs) != 1 || mb.destructiveIDs[0] != 0 {
		t.Fatalf("decoded message box = %+v (ok=%v)", mb, ok)
	}
	// Negative response from dismiss maps to CancelID.
	if err := hostConn.write(kindDialogResult, encodeDialogResultMessage(dialogResultWire{
		reqID:           mb.reqID,
		response:        -1,
		checkboxChecked: true,
	})); err != nil {
		t.Fatalf("write msgbox result: %v", err)
	}
	b.Step()
	if gotMsg.Response != 1 || !gotMsg.CheckboxChecked {
		t.Fatalf("gotMsg = %+v, want {Response:1 CheckboxChecked:true}", gotMsg)
	}

	// Queue a waiting frame before calling Shell.OpenExternal to verify kindShellResult bypasses kindFrame.
	b.mu.Lock()
	b.events = append(b.events, event{kind: kindFrame, fd: -1})
	b.mu.Unlock()

	if err := b.Shell().OpenExternal("not a url"); err == nil {
		t.Fatal("OpenExternal accepted URL without scheme")
	}

	tmpFile := filepath.Join(t.TempDir(), "cover.png")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	go func() {
		for range 2 {
			k, p, _, rerr := hostConn.read()
			if rerr != nil || k != kindShellReq {
				return
			}
			reqID, _, _, ok := decodeShellReqMessage(p)
			if !ok {
				return
			}
			_ = hostConn.write(kindShellResult, encodeShellResultMessage(reqID, ""))
		}
	}()

	if err := b.Shell().OpenExternal("https://github.com/nknkol/mygo"); err != nil {
		t.Fatalf("OpenExternal: %v", err)
	}
	if err := b.Shell().TrashItem(tmpFile); err != nil {
		t.Fatalf("TrashItem: %v", err)
	}
}

// URL schemes track registration state, and deep links or file opens arriving before Ready are buffered and delivered immediately after Ready.
func TestURLSchemesAndLaunchAppOpenBuffering(t *testing.T) {
	b, s, _, _ := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app
	s.w.visible = true

	if err := b.App().RegisterURLScheme("MyGo-Music", "dev.egoist.music", "MyGo Music"); err != nil {
		t.Fatalf("RegisterURLScheme: %v", err)
	}
	if !b.App().IsURLSchemeRegistered("mygo-music", "dev.egoist.music", "MyGo Music") {
		t.Fatal("IsURLSchemeRegistered = false after RegisterURLScheme")
	}
	if err := b.App().UnregisterURLScheme("mygo-music", "dev.egoist.music", "MyGo Music"); err != nil {
		t.Fatalf("UnregisterURLScheme: %v", err)
	}
	if b.App().IsURLSchemeRegistered("mygo-music", "dev.egoist.music", "MyGo Music") {
		t.Fatal("IsURLSchemeRegistered = true after UnregisterURLScheme")
	}

	// Arrive before Ready(): should buffer until flushPendingAppOpens().
	b.onAppOpen(encodeAppOpenMessage(appOpenURLs, []string{"mygo-music://song/123"}))
	b.onAppOpen(encodeAppOpenMessage(appOpenFiles, []string{"/storage/Users/currentUser/Music/demo.flac"}))
	if len(app.openURLs) != 0 || len(app.openFiles) != 0 {
		t.Fatalf("delivered before Ready: urls=%v files=%v", app.openURLs, app.openFiles)
	}

	b.flushPendingAppOpens()
	if !slices.Equal(app.openURLs, []string{"mygo-music://song/123"}) ||
		!slices.Equal(app.openFiles, []string{"/storage/Users/currentUser/Music/demo.flac"}) {
		t.Fatalf("after flushPendingAppOpens: urls=%v files=%v", app.openURLs, app.openFiles)
	}

	// Subsequent re-activation delivers immediately with hasVisibleWindows=true.
	b.onAppOpen(encodeAppOpenMessage(appOpenActivate, nil))
	if !slices.Equal(app.activated, []bool{true}) {
		t.Fatalf("activated = %v, want [true]", app.activated)
	}
}

// ReplaceFile preserves target permissions atomically, and NewFileWatch reports directory creations, writes, and removals via inotify.
func TestReplaceFileAndInotifyWatch(t *testing.T) {
	var b Backend
	dir := t.TempDir()

	target := filepath.Join(dir, "config.json")
	replacement := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := os.WriteFile(replacement, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement): %v", err)
	}
	if err := b.ReplaceFile(target, replacement); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("ReadFile(target) = (%q, %v)", string(got), err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("target perm = %v (err=%v), want 0600", info.Mode().Perm(), err)
	}

	notices := make(chan platform.FileWatchNotice, 16)
	fw, err := b.NewFileWatch(func(n platform.FileWatchNotice) {
		notices <- n
	})
	if err != nil {
		t.Fatalf("NewFileWatch: %v", err)
	}
	defer fw.Close()

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if err := fw.Add(platform.FileWatchTarget{
		ID:        99,
		Path:      dir,
		Info:      dirInfo,
		Directory: true,
	}); err != nil {
		t.Fatalf("fw.Add: %v", err)
	}

	watchedFile := filepath.Join(dir, "library.db")
	if err := os.WriteFile(watchedFile, []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile(watchedFile): %v", err)
	}

	deadline := time.After(2 * time.Second)
	seenLibrary := false
	for !seenLibrary {
		select {
		case n := <-notices:
			if n.Target == 99 && n.Name == "library.db" {
				seenLibrary = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for inotify notice for library.db")
		}
	}
}
