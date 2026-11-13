//go:build linux && (amd64 || arm64)

package ohos

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/egoist/mygo/internal/platform"
)

type pendingDialogKind byte

const (
	dialogKindOpen pendingDialogKind = 1
	dialogKindSave pendingDialogKind = 2
	dialogKindMsg  pendingDialogKind = 3
)

type pendingDialog struct {
	kind     pendingDialogKind
	cancelID int
	openCB   func([]string, error)
	saveCB   func(string, error)
	msgCB    func(platform.MessageBoxResult, error)
}

type dialogResultWire struct {
	reqID           uint32
	response        int32
	checkboxChecked bool
	err             string
	paths           []string
}

type fileDialogWire struct {
	reqID       uint32
	winID       uint32
	flags       byte
	title       string
	defaultPath string
	buttonLabel string
	filters     []platform.FileFilter
}

type messageBoxWire struct {
	reqID           uint32
	winID           uint32
	defaultID       int32
	cancelID        int32
	checkboxChecked bool
	boxType         string
	title           string
	message         string
	detail          string
	checkboxLabel   string
	buttons         []string
	destructiveIDs  []int32
}

type appOpenEvent struct {
	op    byte
	items []string
}

func (b *Backend) Dialogs() platform.Dialogs { return dialogsBackend{b: b} }

func (b *Backend) Shell() platform.Shell { return shellBackend{b: b} }

type dialogsBackend struct{ b *Backend }

func (d dialogsBackend) ShowOpenDialog(parent platform.Window, opts *platform.OpenDialogOptions, cb func(paths []string, err error)) {
	if cb == nil {
		return
	}
	b := d.b
	if b == nil || b.conn == nil {
		cb(nil, nil)
		return
	}
	var o platform.OpenDialogOptions
	if opts != nil {
		o = *opts
	}
	if !o.OpenFiles && !o.OpenDirectories {
		o.OpenFiles = true
	}
	var flags byte
	if o.OpenFiles {
		flags |= dlgFlagOpenFiles
	}
	if o.OpenDirectories {
		flags |= dlgFlagOpenDirectories
	}
	if o.Multiple {
		flags |= dlgFlagMultiple
	}
	if o.ShowHiddenFiles {
		flags |= dlgFlagShowHidden
	}
	if o.CreateDirectories {
		flags |= dlgFlagCreateDirs
	}
	winID := d.resolveWinID(parent)

	b.mu.Lock()
	if b.quit {
		b.mu.Unlock()
		cb(nil, nil)
		return
	}
	b.nextDialogReq++
	reqID := b.nextDialogReq
	if b.pendingDialogs == nil {
		b.pendingDialogs = make(map[uint32]pendingDialog)
	}
	b.pendingDialogs[reqID] = pendingDialog{
		kind:   dialogKindOpen,
		openCB: cb,
	}
	b.mu.Unlock()

	payload := encodeFileDialogMessage(fileDialogWire{
		reqID:       reqID,
		winID:       winID,
		flags:       flags,
		title:       o.Title,
		defaultPath: o.DefaultPath,
		buttonLabel: o.ButtonLabel,
		filters:     normalizeDialogFilters(o.Filters),
	})
	b.send(kindDialogOpen, payload)
}

func (d dialogsBackend) ShowSaveDialog(parent platform.Window, opts *platform.SaveDialogOptions, cb func(path string, err error)) {
	if cb == nil {
		return
	}
	b := d.b
	if b == nil || b.conn == nil {
		cb("", nil)
		return
	}
	var o platform.SaveDialogOptions
	if opts != nil {
		o = *opts
	}
	var flags byte
	if o.ShowHiddenFiles {
		flags |= dlgFlagShowHidden
	}
	if o.CreateDirectories {
		flags |= dlgFlagCreateDirs
	}
	winID := d.resolveWinID(parent)

	b.mu.Lock()
	if b.quit {
		b.mu.Unlock()
		cb("", nil)
		return
	}
	b.nextDialogReq++
	reqID := b.nextDialogReq
	if b.pendingDialogs == nil {
		b.pendingDialogs = make(map[uint32]pendingDialog)
	}
	b.pendingDialogs[reqID] = pendingDialog{
		kind:   dialogKindSave,
		saveCB: cb,
	}
	b.mu.Unlock()

	payload := encodeFileDialogMessage(fileDialogWire{
		reqID:       reqID,
		winID:       winID,
		flags:       flags,
		title:       o.Title,
		defaultPath: o.DefaultPath,
		buttonLabel: o.ButtonLabel,
		filters:     normalizeDialogFilters(o.Filters),
	})
	b.send(kindDialogSave, payload)
}

func (d dialogsBackend) ShowMessageBox(parent platform.Window, opts *platform.MessageBoxOptions, cb func(res platform.MessageBoxResult, err error)) {
	if cb == nil {
		return
	}
	var o platform.MessageBoxOptions
	if opts != nil {
		o = *opts
	}
	buttons := slices.Clone(o.Buttons)
	if len(buttons) == 0 {
		buttons = []string{"OK"}
	}
	cancelID := o.CancelID
	if cancelID < 0 || cancelID >= len(buttons) {
		cancelID = 0
	}
	defaultID := o.DefaultID
	if defaultID < 0 || defaultID >= len(buttons) {
		defaultID = 0
	}

	b := d.b
	if b == nil || b.conn == nil {
		cb(platform.MessageBoxResult{Response: cancelID, CheckboxChecked: o.CheckboxChecked}, nil)
		return
	}
	winID := d.resolveWinID(parent)

	b.mu.Lock()
	if b.quit {
		b.mu.Unlock()
		cb(platform.MessageBoxResult{Response: cancelID, CheckboxChecked: o.CheckboxChecked}, nil)
		return
	}
	b.nextDialogReq++
	reqID := b.nextDialogReq
	if b.pendingDialogs == nil {
		b.pendingDialogs = make(map[uint32]pendingDialog)
	}
	b.pendingDialogs[reqID] = pendingDialog{
		kind:     dialogKindMsg,
		cancelID: cancelID,
		msgCB:    cb,
	}
	b.mu.Unlock()

	destr := make([]int32, 0, len(o.DestructiveIDs))
	for _, id := range o.DestructiveIDs {
		destr = append(destr, int32(id))
	}
	payload := encodeMessageBoxMessage(messageBoxWire{
		reqID:           reqID,
		winID:           winID,
		defaultID:       int32(defaultID),
		cancelID:        int32(cancelID),
		checkboxChecked: o.CheckboxChecked,
		boxType:         o.Type,
		title:           o.Title,
		message:         o.Message,
		detail:          o.Detail,
		checkboxLabel:   o.CheckboxLabel,
		buttons:         buttons,
		destructiveIDs:  destr,
	})
	b.send(kindDialogMsg, payload)
}

func (d dialogsBackend) resolveWinID(parent platform.Window) uint32 {
	if w, ok := parent.(*Window); ok && w != nil && !w.closed {
		return w.id
	}
	if d.b != nil {
		d.b.mu.Lock()
		w := d.b.focusedOrFirstWindowLocked()
		d.b.mu.Unlock()
		if w != nil {
			return w.id
		}
	}
	return 1
}

func (b *Backend) onDialogResult(payload []byte) {
	res, ok := decodeDialogResultMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	pd, exists := b.pendingDialogs[res.reqID]
	if exists {
		delete(b.pendingDialogs, res.reqID)
	}
	b.mu.Unlock()
	if !exists {
		return
	}
	var err error
	if res.err != "" {
		err = errors.New(res.err)
	}
	switch pd.kind {
	case dialogKindOpen:
		if pd.openCB != nil {
			pd.openCB(res.paths, err)
		}
	case dialogKindSave:
		if pd.saveCB != nil {
			var p string
			if len(res.paths) > 0 {
				p = res.paths[0]
			}
			pd.saveCB(p, err)
		}
	case dialogKindMsg:
		if pd.msgCB != nil {
			resp := int(res.response)
			if resp < 0 {
				resp = pd.cancelID
			}
			pd.msgCB(platform.MessageBoxResult{
				Response:        resp,
				CheckboxChecked: res.checkboxChecked,
			}, err)
		}
	}
}

type shellBackend struct{ b *Backend }

func (s shellBackend) OpenExternal(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("ohos: invalid external URL %q", rawURL)
	}
	return s.callHost(shellOpenExternal, trimmed)
}

func (s shellBackend) OpenPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("ohos: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	return s.callHost(shellOpenPath, abs)
}

func (s shellBackend) ShowItemInFolder(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	_ = s.callHost(shellShowInFolder, abs)
}

func (s shellBackend) TrashItem(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("ohos: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err != nil {
		return err
	}
	return s.callHost(shellTrash, abs)
}

func (s shellBackend) PreviewItem(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("ohos: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	return s.callHost(shellPreviewItem, abs)
}

func (shellBackend) Beep()                                 {}
func (shellBackend) FileIcon(string, int) ([]byte, error)  { return nil, platform.ErrUnsupported }
func (shellBackend) TypeIcon(string, int) ([]byte, error)  { return nil, platform.ErrUnsupported }

func (s shellBackend) OpenApplication(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return errors.New("ohos: empty application target")
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../") {
		abs, err := filepath.Abs(trimmed)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return err
		}
		trimmed = abs
	}
	return s.callHost(shellOpenApp, trimmed)
}
func (shellBackend) DefaultApp(string) (platform.Application, error) {
	return platform.Application{}, platform.ErrUnsupported
}

func (s shellBackend) callHost(op byte, target string) error {
	b := s.b
	if b == nil || b.conn == nil {
		return platform.ErrUnsupported
	}
	b.mu.Lock()
	b.nextShellReq++
	reqID := b.nextShellReq
	if b.shellResults == nil {
		b.shellResults = make(map[uint32]string)
	}
	b.mu.Unlock()

	b.send(kindShellReq, encodeShellReqMessage(reqID, op, target))

	var errStr string
	for {
		b.mu.Lock()
		res, done := b.shellResults[reqID]
		if done {
			delete(b.shellResults, reqID)
		}
		quit := b.quit
		b.mu.Unlock()
		if done {
			errStr = res
			break
		}
		if quit {
			return errors.New("ohos: event loop stopped")
		}
		b.Step()
	}
	if errStr != "" {
		return errors.New(errStr)
	}
	return nil
}

func (b *Backend) onShellResult(payload []byte) {
	reqID, errStr, ok := decodeShellResultMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	if b.shellResults == nil {
		b.shellResults = make(map[uint32]string)
	}
	b.shellResults[reqID] = errStr
	b.mu.Unlock()
}

// URL scheme registration & deep-link / file-open / activation delivery.

func (a appController) RegisterURLScheme(scheme, _, _ string) error {
	if a.b == nil {
		return nil
	}
	s := strings.ToLower(strings.TrimSpace(scheme))
	if s == "" {
		return errors.New("ohos: empty URL scheme")
	}
	a.b.mu.Lock()
	if a.b.schemes == nil {
		a.b.schemes = make(map[string]bool)
	}
	a.b.schemes[s] = true
	a.b.mu.Unlock()
	return nil
}

func (a appController) UnregisterURLScheme(scheme, _, _ string) error {
	if a.b == nil {
		return nil
	}
	s := strings.ToLower(strings.TrimSpace(scheme))
	a.b.mu.Lock()
	delete(a.b.schemes, s)
	a.b.mu.Unlock()
	return nil
}

func (a appController) IsURLSchemeRegistered(scheme, _, _ string) bool {
	if a.b == nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(scheme))
	a.b.mu.Lock()
	defer a.b.mu.Unlock()
	return a.b.schemes[s]
}

func (b *Backend) onAppOpen(payload []byte) {
	ev, ok := decodeAppOpenMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	if !b.readyDone {
		b.pendingAppOpens = append(b.pendingAppOpens, ev)
		b.mu.Unlock()
		return
	}
	app := b.app
	hasVis := b.hasVisibleWindowLocked()
	b.mu.Unlock()
	if app != nil {
		deliverAppOpen(app, ev, hasVis)
	}
}

func (b *Backend) flushPendingAppOpens() {
	b.mu.Lock()
	b.readyDone = true
	pending := b.pendingAppOpens
	b.pendingAppOpens = nil
	clicks := b.pendingNotifClicks
	b.pendingNotifClicks = nil
	app := b.app
	hasVis := b.hasVisibleWindowLocked()
	b.mu.Unlock()
	if app == nil {
		return
	}
	for _, ev := range pending {
		deliverAppOpen(app, ev, hasVis)
	}
	for _, id := range clicks {
		app.NotificationClicked(id)
	}
}

func (b *Backend) hasVisibleWindowLocked() bool {
	for _, id := range b.winOrder {
		if w := b.windows[id]; w != nil && !w.closed && w.visible && !w.minimized {
			return true
		}
	}
	return false
}

func deliverAppOpen(app platform.AppHandler, ev appOpenEvent, hasVisibleWindows bool) {
	switch ev.op {
	case appOpenURLs:
		if len(ev.items) > 0 {
			app.OpenURLs(ev.items)
		}
	case appOpenFiles:
		if len(ev.items) > 0 {
			app.OpenFiles(ev.items)
		}
	case appOpenActivate:
		app.Activated(hasVisibleWindows)
	}
}

func normalizeDialogFilters(filters []platform.FileFilter) []platform.FileFilter {
	if len(filters) == 0 {
		return nil
	}
	out := make([]platform.FileFilter, 0, len(filters))
	for _, f := range filters {
		var exts []string
		addExt := func(e string) {
			e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), "*.")
			e = strings.TrimPrefix(e, ".")
			if e != "" && !slices.Contains(exts, e) {
				exts = append(exts, e)
			}
		}
		for _, e := range f.Extensions {
			if e == "*" {
				exts = []string{"*"}
				break
			}
			addExt(e)
		}
		if !slices.Equal(exts, []string{"*"}) {
			for _, m := range f.MIMETypes {
				for _, mapped := range mimeToExtensions(m) {
					addExt(mapped)
				}
			}
		}
		if len(exts) > 0 {
			out = append(out, platform.FileFilter{Name: f.Name, Extensions: exts})
		}
	}
	return out
}

func mimeToExtensions(mime string) []string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "*/*":
		return []string{"*"}
	case "audio/*":
		return []string{"mp3", "flac", "wav", "ogg", "m4a", "aac", "opus", "wma"}
	case "audio/mpeg", "audio/mp3":
		return []string{"mp3"}
	case "audio/flac", "audio/x-flac":
		return []string{"flac"}
	case "audio/wav", "audio/x-wav":
		return []string{"wav"}
	case "audio/ogg":
		return []string{"ogg", "opus"}
	case "audio/mp4", "audio/x-m4a":
		return []string{"m4a"}
	case "audio/aac":
		return []string{"aac"}
	case "image/*":
		return []string{"png", "jpg", "jpeg", "webp", "gif", "bmp", "svg"}
	case "image/png":
		return []string{"png"}
	case "image/jpeg":
		return []string{"jpg", "jpeg"}
	case "image/webp":
		return []string{"webp"}
	case "image/gif":
		return []string{"gif"}
	case "image/svg+xml":
		return []string{"svg"}
	case "video/*":
		return []string{"mp4", "mkv", "webm", "mov", "avi"}
	case "text/plain":
		return []string{"txt"}
	case "text/html":
		return []string{"html", "htm"}
	case "text/markdown":
		return []string{"md", "markdown"}
	case "application/json":
		return []string{"json"}
	case "application/pdf":
		return []string{"pdf"}
	case "application/zip":
		return []string{"zip"}
	default:
		return nil
	}
}

// Wire codecs for dialogs, shell, and app-open events.

func encodeFileDialogMessage(w fileDialogWire) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], w.reqID)
	binary.LittleEndian.PutUint32(buf[4:], w.winID)
	buf[8] = w.flags
	buf = appendWireStr(buf, w.title)
	buf = appendWireStr(buf, w.defaultPath)
	buf = appendWireStr(buf, w.buttonLabel)
	buf = appendUint32(buf, uint32(len(w.filters)))
	for _, f := range w.filters {
		buf = appendWireStr(buf, f.Name)
		buf = appendUint32(buf, uint32(len(f.Extensions)))
		for _, ext := range f.Extensions {
			buf = appendWireStr(buf, ext)
		}
	}
	return buf
}

func decodeFileDialogMessage(p []byte) (fileDialogWire, bool) {
	if len(p) < 12 {
		return fileDialogWire{}, false
	}
	var out fileDialogWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.winID = binary.LittleEndian.Uint32(p[4:])
	out.flags = p[8]
	off := 12
	var ok bool
	if out.title, off, ok = readWireStr(p, off); !ok {
		return fileDialogWire{}, false
	}
	if out.defaultPath, off, ok = readWireStr(p, off); !ok {
		return fileDialogWire{}, false
	}
	if out.buttonLabel, off, ok = readWireStr(p, off); !ok {
		return fileDialogWire{}, false
	}
	fCount, off, ok := readUint32(p, off)
	if !ok || fCount > 64 {
		return fileDialogWire{}, false
	}
	out.filters = make([]platform.FileFilter, 0, fCount)
	for range fCount {
		var name string
		if name, off, ok = readWireStr(p, off); !ok {
			return fileDialogWire{}, false
		}
		eCount, nextOff, ok2 := readUint32(p, off)
		if !ok2 || eCount > 256 {
			return fileDialogWire{}, false
		}
		off = nextOff
		exts := make([]string, 0, eCount)
		for range eCount {
			var ext string
			if ext, off, ok = readWireStr(p, off); !ok {
				return fileDialogWire{}, false
			}
			exts = append(exts, ext)
		}
		out.filters = append(out.filters, platform.FileFilter{Name: name, Extensions: exts})
	}
	if off != len(p) {
		return fileDialogWire{}, false
	}
	return out, true
}

func encodeMessageBoxMessage(w messageBoxWire) []byte {
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint32(buf[0:], w.reqID)
	binary.LittleEndian.PutUint32(buf[4:], w.winID)
	binary.LittleEndian.PutUint32(buf[8:], uint32(w.defaultID))
	binary.LittleEndian.PutUint32(buf[12:], uint32(w.cancelID))
	if w.checkboxChecked {
		buf[16] = 1
	}
	buf = appendWireStr(buf, w.boxType)
	buf = appendWireStr(buf, w.title)
	buf = appendWireStr(buf, w.message)
	buf = appendWireStr(buf, w.detail)
	buf = appendWireStr(buf, w.checkboxLabel)
	buf = appendUint32(buf, uint32(len(w.buttons)))
	for _, b := range w.buttons {
		buf = appendWireStr(buf, b)
	}
	buf = appendUint32(buf, uint32(len(w.destructiveIDs)))
	for _, id := range w.destructiveIDs {
		buf = appendUint32(buf, uint32(id))
	}
	return buf
}

func decodeMessageBoxMessage(p []byte) (messageBoxWire, bool) {
	if len(p) < 20 {
		return messageBoxWire{}, false
	}
	var out messageBoxWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.winID = binary.LittleEndian.Uint32(p[4:])
	out.defaultID = int32(binary.LittleEndian.Uint32(p[8:]))
	out.cancelID = int32(binary.LittleEndian.Uint32(p[12:]))
	out.checkboxChecked = p[16] != 0
	off := 20
	var ok bool
	if out.boxType, off, ok = readWireStr(p, off); !ok {
		return messageBoxWire{}, false
	}
	if out.title, off, ok = readWireStr(p, off); !ok {
		return messageBoxWire{}, false
	}
	if out.message, off, ok = readWireStr(p, off); !ok {
		return messageBoxWire{}, false
	}
	if out.detail, off, ok = readWireStr(p, off); !ok {
		return messageBoxWire{}, false
	}
	if out.checkboxLabel, off, ok = readWireStr(p, off); !ok {
		return messageBoxWire{}, false
	}
	bCount, off, ok := readUint32(p, off)
	if !ok || bCount > 32 {
		return messageBoxWire{}, false
	}
	out.buttons = make([]string, 0, bCount)
	for range bCount {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return messageBoxWire{}, false
		}
		out.buttons = append(out.buttons, s)
	}
	dCount, off, ok := readUint32(p, off)
	if !ok || dCount > 32 {
		return messageBoxWire{}, false
	}
	out.destructiveIDs = make([]int32, 0, dCount)
	for range dCount {
		var u uint32
		if u, off, ok = readUint32(p, off); !ok {
			return messageBoxWire{}, false
		}
		out.destructiveIDs = append(out.destructiveIDs, int32(u))
	}
	if off != len(p) {
		return messageBoxWire{}, false
	}
	return out, true
}

func encodeDialogResultMessage(w dialogResultWire) []byte {
	buf := make([]byte, 12)
	binary.LittleEndian.PutUint32(buf[0:], w.reqID)
	binary.LittleEndian.PutUint32(buf[4:], uint32(w.response))
	if w.checkboxChecked {
		buf[8] = 1
	}
	buf = appendWireStr(buf, w.err)
	buf = appendUint32(buf, uint32(len(w.paths)))
	for _, p := range w.paths {
		buf = appendWireStr(buf, p)
	}
	return buf
}

func decodeDialogResultMessage(p []byte) (dialogResultWire, bool) {
	if len(p) < 12 {
		return dialogResultWire{}, false
	}
	var out dialogResultWire
	out.reqID = binary.LittleEndian.Uint32(p[0:])
	out.response = int32(binary.LittleEndian.Uint32(p[4:]))
	out.checkboxChecked = p[8] != 0
	off := 12
	var ok bool
	if out.err, off, ok = readWireStr(p, off); !ok {
		return dialogResultWire{}, false
	}
	count, off, ok := readUint32(p, off)
	if !ok || count > 4096 {
		return dialogResultWire{}, false
	}
	out.paths = make([]string, 0, count)
	for range count {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return dialogResultWire{}, false
		}
		if s != "" {
			out.paths = append(out.paths, s)
		}
	}
	if off != len(p) {
		return dialogResultWire{}, false
	}
	return out, true
}

func encodeShellReqMessage(reqID uint32, op byte, target string) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	buf[4] = op
	buf = appendWireStr(buf, target)
	return buf
}

func decodeShellReqMessage(p []byte) (reqID uint32, op byte, target string, ok bool) {
	if len(p) < 8 {
		return 0, 0, "", false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	op = p[4]
	var off int
	target, off, ok = readWireStr(p, 8)
	if !ok || off != len(p) {
		return 0, 0, "", false
	}
	return reqID, op, target, true
}

func encodeShellResultMessage(reqID uint32, errStr string) []byte {
	var buf []byte
	buf = appendUint32(buf, reqID)
	buf = appendWireStr(buf, errStr)
	return buf
}

func decodeShellResultMessage(p []byte) (reqID uint32, errStr string, ok bool) {
	var off int
	reqID, off, ok = readUint32(p, 0)
	if !ok {
		return 0, "", false
	}
	errStr, off, ok = readWireStr(p, off)
	if !ok || off != len(p) {
		return 0, "", false
	}
	return reqID, errStr, true
}

func encodeAppOpenMessage(op byte, items []string) []byte {
	buf := make([]byte, 4)
	buf[0] = op
	buf = appendUint32(buf, uint32(len(items)))
	for _, s := range items {
		buf = appendWireStr(buf, s)
	}
	return buf
}

func decodeAppOpenMessage(p []byte) (appOpenEvent, bool) {
	if len(p) < 4 {
		return appOpenEvent{}, false
	}
	op := p[0]
	count, off, ok := readUint32(p, 4)
	if !ok || count > 256 {
		return appOpenEvent{}, false
	}
	items := make([]string, 0, count)
	for range count {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return appOpenEvent{}, false
		}
		if s != "" {
			items = append(items, s)
		}
	}
	if off != len(p) {
		return appOpenEvent{}, false
	}
	return appOpenEvent{op: op, items: items}, true
}
