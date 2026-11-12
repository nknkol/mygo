//go:build linux && (amd64 || arm64)

// Package ohos is the backend for HarmonyOS, where the window belongs to
// another process: ArkTS makes the surface, a native module in that process
// owns it, and this process draws into a buffer whose descriptor arrives
// over a socket. Nothing here talks to the graphics stack — it reads and
// writes one descriptor and the bytes behind it.
package ohos

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"syscall"

	"github.com/egoist/mygo/internal/platform"
)

// Messages are u8 kind | u32 len | payload. Window-scoped messages carry
// u32 winID at offset 0 (1 = main Ability window, 2+ = subwindows).
const (
	kindSurface = 1 // {u32 winID, u32 width, u32 height, u32 scale×1000[, u32 milliHz[, u8 fresh]]}
	kindFrame   = 2 // {u32 winID, u32 seq, u32 id, u32 width, u32 height, u32 stride, u32 size} + fd
	kindInput   = 3 // {u32 winID, u8 subkind, u8 op, ...}
	kindQuit    = 4 // {}

	kindRequest     = 5  // {u32 winID}: we want a buffer
	kindPresent     = 6  // {u32 winID, u32 seq, u32 n, n × {i32 x, i32 y, u32 w, u32 h}}
	kindTextInput   = 7  // {u32 winID, u8 flags, 3×pad, 4×f32 caret, i32 selStart, i32 selEnd, u32 n, utf8[n]}
	kindCursor      = 8  // {u32 winID, u8 cursor, 3×pad}
	kindAbort       = 9  // {u32 winID, u32 seq}: skip presenting the current buffer
	kindFrameRate   = 10 // {u32 winID, u32 fps}: expected VSync rate, 0 for display native rate
	kindEnvInfo     = 11 // host -> engine: package, locale/region, displays snapshot
	kindPopupResult = 12 // host -> engine: {u32 reqID, i32 chosenID}
	kindWinCreate   = 13 // engine -> host: create/configure initial window
	kindWinConfig   = 14 // engine -> host: update window properties by mask
	kindWinAction   = 15 // engine -> host: {u32 winID, u8 action, 3×pad}
	kindPopupMenu   = 16 // engine -> host: {u32 reqID, u32 winID, i32 x, i32 y, u8 rtl, 3×pad, items...}

	kindClipChanged  = 17 // host -> engine: {u32 changeCount, u8 external, 3×pad, u32 mimeCount, mimes...}
	kindClipData     = 18 // host -> engine: {u32 reqID, str err, u32 repCount, reps...}
	kindDialogResult = 19 // host -> engine: {u32 reqID, i32 response, u8 checkbox, 3×pad, str err, paths...}
	kindShellResult  = 20 // host -> engine: {u32 reqID, str err}
	kindAppOpen      = 21 // host -> engine: {u8 op, 3×pad, u32 count, items...}

	kindClipWrite  = 22 // engine -> host: {u8 clear, u8 concealed, 2×pad, str text, str html, str uriList, bytes png}
	kindClipRead   = 23 // engine -> host: {u32 reqID, u32 fmtCount, fmts...}
	kindDialogOpen = 24 // engine -> host: open file/folder picker
	kindDialogSave = 25 // engine -> host: save file picker
	kindDialogMsg  = 26 // engine -> host: message box
	kindShellReq   = 27 // engine -> host: {u32 reqID, u8 op, 3×pad, str target}

	kindMediaCmd     = 28 // host -> engine: {u32 cmd, i64 posMs}
	kindNotifResult  = 29 // host -> engine: {u32 reqID, u8 denied, 3×pad, str err}
	kindNotifClicked = 30 // host -> engine: {str id}
	kindPowerState   = 31 // host -> engine: {u8 onBattery, 3×pad, str event}
	kindNetState     = 32 // host -> engine: {u8 online, u8 metered, 2×pad}

	kindAudioOpen   = 33 // engine -> host: {u32 streamID, u32 rate, u32 channels, u32 latencyMode, u32 shmBytes} + fd
	kindAudioClose  = 34 // engine -> host: {u32 streamID}
	kindNowPlaying  = 35 // engine -> host: {u8 active, u8 state, u8 metaChanged, u8 pad, u32 cmds, i64 durMs, i64 posMs, f64 rate, ...}
	kindNotifShow   = 36 // engine -> host: {u32 reqID, u8 silent, 3×pad, str id, str title, str subtitle, str body, str group, bytes image}
	kindNotifRemove = 37 // engine -> host: {u8 all, 3×pad, str id}
	kindPowerReq    = 38 // engine -> host: {u8 keepSys, u8 keepDisp, u8 keepRun, u8 pad, str reason}
	kindNetWatch    = 39 // engine -> host: {}

	kindCookieReq     = 40 // engine -> host: {u32 reqID, u8 op, u8 incognito, 2×pad, u32 siteDataTypes, str url, str headerOrName, str domain, str path}
	kindCookieResult  = 41 // host -> engine: {u32 reqID, str err, u32 count, cookies...}
	kindWebReq        = 42 // engine -> host: {u32 winID, u8 op, 3×pad, str arg1, str arg2}
	kindWebEvent      = 43 // host -> engine: {u32 winID, u8 ev, u8 canBack, u8 canForward, u8 pad, i32 errCode, str text, str desc}
	kindWebSetup      = 44 // engine -> host: configure standalone or embedded ArkWeb (scripts, schemes, flags)
	kindPlaceWebViews = 45 // engine -> host: position embedded WebViews and surface hit-test regions
	kindWebAsyncReq   = 46 // engine -> host: CallAsyncFunction / CapturePage / PrintToPDF
	kindWebAsyncRes   = 47 // host -> engine: {u32 reqID, str text, str err, bytes data}
	kindSchemeReq     = 48 // host -> engine: custom protocol request or cancellation
	kindSchemeRes     = 49 // engine -> host: custom protocol response headers/body/finish/fail

	kindSecretReq      = 50 // engine -> host: {u32 reqID, u8 op, 3×pad, str service, str account, bytes secret}
	kindSecretRes      = 51 // host -> engine: {u32 reqID, u8 notFound, 3×pad, str err, bytes secret}
	kindHotkeyReq      = 52 // engine -> host: {u32 reqID, i32 hotkeyID, u8 register, 3×pad, i32 finalKey, u32 preCount, preKeys...}
	kindHotkeyRes      = 53 // host -> engine: {u32 reqID, str err}
	kindHotkeyTrig     = 54 // host -> engine: {i32 hotkeyID}
	kindTrayUpdate     = 55 // engine -> host: {u32 trayID, u8 op, u8 flags, 2×pad, str id, str title, str tooltip, bytes png, menu...}
	kindTrayEvent      = 56 // host -> engine: {u32 trayID, u8 ev, 3×pad, i32 menuItemID}
	kindDragSetFormats = 57 // engine -> host: {u32 winID, u32 count, formats...}
	kindDragStart      = 58 // engine -> host: {u32 winID, u32 ops, f64 x, f64 y, i32 w, i32 h, i32 hotX, i32 hotY, str session, bytes preview, reps...}
	kindDragCancel     = 59 // engine -> host: {u32 winID}
	kindDragEvent      = 60 // host -> engine: {u32 winID, u8 ev, u8 canceled, u8 escaped, u8 pad, u32 pendingID, u32 ops, u32 suggestedOp, f64 x, f64 y, ...}
	kindDragReply      = 61 // engine -> host: {u32 winID, u32 pendingID, u32 op, u8 taken, 3×pad}
	kindAccessTree     = 62 // engine -> host: accessibility tree snapshot & live announcements
	kindAccessEvent    = 63 // host -> engine: {u32 winID, u8 ev, u8 actionKind, 2×pad, u64 nodeID, i32 from, i32 to, str text}

	kindInternal = 255 // internal main-thread callback queued via Backend.post
)

// Cookie & WebsiteDataStore operations (kindCookieReq).
const (
	cookieOpList      byte = 1
	cookieOpSet       byte = 2
	cookieOpDelete    byte = 3
	cookieOpClear     byte = 4
	cookieOpClearData byte = 5
)

// ArkWeb operations (kindWebReq) and events (kindWebEvent).
const (
	webOpLoadURL   byte = 1
	webOpLoadHTML  byte = 2
	webOpReload    byte = 3
	webOpStop      byte = 4
	webOpBack      byte = 5
	webOpForward   byte = 6
	webOpEval      byte = 7
	webOpSetUA     byte = 8
	webOpSetZoom   byte = 9
	webOpSetMuted  byte = 10
	webOpDevTools  byte = 11
	webOpFocus     byte = 12
	webOpTabInto   byte = 13
	webOpSetBg     byte = 14
	webOpDestroy   byte = 15
	webOpPermReply byte = 16

	webEvPageBegin      byte = 1
	webEvPageEnd        byte = 2
	webEvTitle          byte = 3
	webEvError          byte = 4
	webEvMessage        byte = 5
	webEvAudioState     byte = 6
	webEvWindowNew      byte = 7
	webEvWindowClose    byte = 8
	webEvRenderExited   byte = 9
	webEvPermReq        byte = 10
	webEvDownloadStart  byte = 11
	webEvSchemeDownload byte = 12
	webEvTabOut         byte = 13
	webEvUnhandledKey   byte = 14
	webEvEmbeddedPress  byte = 15
	webEvEmbeddedScroll byte = 16
)

// ArkWeb setup flags (kindWebSetup), async ops (kindWebAsyncReq) & scheme ops (kindSchemeRes).
const (
	webSetupIncognito         uint32 = 1 << 0
	webSetupDevTools          uint32 = 1 << 1
	webSetupAutoplay          uint32 = 1 << 2
	webSetupPinchZoom         uint32 = 1 << 3
	webSetupScrollToContainer uint32 = 1 << 4
	webSetupTransparent       uint32 = 1 << 5
	webSetupHasBg             uint32 = 1 << 6
	webSetupScriptsOnly       uint32 = 1 << 7

	userScriptAtDocEnd  byte = 1 << 0
	userScriptAllFrames byte = 1 << 1

	webAsyncEval     byte = 1
	webAsyncCapture  byte = 2
	webAsyncPrintPDF byte = 3

	schemeOpHead   byte = 1
	schemeOpBody   byte = 2
	schemeOpFinish byte = 3
	schemeOpFail   byte = 4
)

// NowPlaying playback state bytes (kindNowPlaying).
const (
	npStateStopped byte = 0
	npStatePlaying byte = 1
	npStatePaused  byte = 2
)

// App open operations (kindAppOpen).
const (
	appOpenURLs     byte = 1
	appOpenFiles    byte = 2
	appOpenActivate byte = 3
)

// Shell operations (kindShellReq).
const (
	shellOpenExternal byte = 1
	shellOpenPath     byte = 2
	shellShowInFolder byte = 3
	shellTrash        byte = 4
)

// Open/Save dialog option flags (kindDialogOpen / kindDialogSave).
const (
	dlgFlagOpenFiles       byte = 1 << 0
	dlgFlagOpenDirectories byte = 1 << 1
	dlgFlagMultiple        byte = 1 << 2
	dlgFlagShowHidden      byte = 1 << 3
	dlgFlagCreateDirs      byte = 1 << 4
)

// Input subkinds carried inside kindInput payloads (after u32 winID).
const (
	inputMouse  = 1
	inputTouch  = 2
	inputScroll = 3
	inputZoom   = 4
	inputKey    = 5
	inputWindow = 6
	inputIME    = 7
)

// Mouse operations (inputMouse).
const (
	mouseMove   = 1
	mouseDown   = 2
	mouseUp     = 3
	mouseCancel = 4
	mouseLeave  = 5
)

// Touch operations (inputTouch).
const (
	touchDown   = 1
	touchMove   = 2
	touchUp     = 3
	touchCancel = 4
)

// Scroll flags (inputScroll op byte).
const (
	scrollPrecise  = 1 << 0
	scrollMomentum = 1 << 1
)

// Zoom operations (inputZoom).
const (
	zoomBegin  = 1
	zoomUpdate = 2
	zoomEnd    = 3
)

// Key operations (inputKey).
const (
	keyDown = 1
	keyUp   = 2
)

// Window state operations (inputWindow).
const (
	winFocus           = 1
	winBlur            = 2
	winShow            = 3
	winHide            = 4
	winThemeLight      = 5
	winThemeDark       = 6
	winMinimize        = 7
	winRestore         = 8
	winMaximize        = 9
	winUnmaximize      = 10
	winEnterFullScreen = 11
	winLeaveFullScreen = 12
	winCloseRequest    = 13
	winClosed          = 14
	winRect            = 15 // {u32 winID, u8 inputWindow, u8 winRect, u16 pad, i32 x, i32 y, i32 w, i32 h}
	winTitleBar        = 16 // {u32 winID, u8 inputWindow, u8 winTitleBar, u16 pad, i32 height, i32 left, i32 right}
	winQuitRequest     = 17 // {u32 winID, u8 inputWindow, u8 winQuitRequest, u16 pad}: OS/Dock/Tray quit request
)

// IME operations (inputIME).
const (
	imeCommit            = 1 // text = committed UTF-8
	imePreedit           = 2 // arg0 = start, arg1 = end, text = preview UTF-8
	imePreeditEnd        = 3 // finish preview
	imeDeleteSurrounding = 4 // arg0 = rune offset from caret, arg1 = rune count
	imeSetSelection      = 5 // arg0 = UTF-16 start, arg1 = UTF-16 end
	imeCommand           = 6 // text = SurfaceCommand name ("copy", "cut", "paste", "selectAll")
)

// TextInput flags (kindTextInput byte 4).
const (
	textInputActive        = 1 << 0
	textInputNoInputMethod = 1 << 1
)

// Window style & creation flags (kindWinCreate / kindWinConfig).
const (
	winFlagResizable      uint32 = 1 << 0
	winFlagMinimizable    uint32 = 1 << 1
	winFlagMaximizable    uint32 = 1 << 2
	winFlagClosable       uint32 = 1 << 3
	winFlagModal          uint32 = 1 << 4
	winFlagHiddenTitleBar uint32 = 1 << 5
	winFlagHideWinButtons uint32 = 1 << 6
	winFlagAlwaysOnTop    uint32 = 1 << 7
	winFlagMaximized      uint32 = 1 << 8
	winFlagFullScreen     uint32 = 1 << 9
	winFlagCenter         uint32 = 1 << 10
	winFlagFrameless      uint32 = 1 << 11
	winFlagHasBg          uint32 = 1 << 12
	winFlagMovable        uint32 = 1 << 13
	winFlagWebView        uint32 = 1 << 14
	winFlagWebIncognito   uint32 = 1 << 15
	winFlagTransparent    uint32 = 1 << 16
)

// Configuration field mask (kindWinConfig).
const (
	cfgMaskTitle   uint32 = 1 << 0
	cfgMaskBounds  uint32 = 1 << 1
	cfgMaskMinSize uint32 = 1 << 2
	cfgMaskMaxSize uint32 = 1 << 3
	cfgMaskFlags   uint32 = 1 << 4
	cfgMaskBg      uint32 = 1 << 5
)

// Window lifecycle / geometry actions (kindWinAction).
const (
	winActShow            byte = 1
	winActShowInactive    byte = 2
	winActHide            byte = 3
	winActClose           byte = 4
	winActFocus           byte = 5
	winActBlur            byte = 6
	winActMinimize        byte = 7
	winActMaximize        byte = 8
	winActUnmaximize      byte = 9
	winActRestore         byte = 10
	winActEnterFullScreen byte = 11
	winActExitFullScreen  byte = 12
	winActCenter          byte = 13
	winActStartDrag       byte = 14
	winActCloseAllow      byte = 15
	winActCloseDeny       byte = 16
	winActEnableWebLayer  byte = 17
	winActFocusSurface    byte = 18
	winActQuitDeny        byte = 19
)

// rect is a rectangle in the content's own coordinates, origin top left, as
// package ui counts them.
type rect struct {
	x, y int32
	w, h uint32
}

type touchPoint struct {
	id       int32
	x, y     float64
	pressure float64
}

type textInputWire struct {
	winID         uint32
	active        bool
	noInputMethod bool
	caretX        float32
	caretY        float32
	caretW        float32
	caretH        float32
	selStart      int32
	selEnd        int32
	text          string
}

type winConfigWire struct {
	winID          uint32
	parentOrMask   uint32 // parentID in kindWinCreate, cfgMask* in kindWinConfig
	x, y, w, h     int32
	minW, minH     int32
	maxW, maxH     int32
	flags          uint32
	titleBarHeight int32
	bgRGBA         uint32
	title          string
}

type envInfoWire struct {
	hasPkg         bool
	pkg            platform.PackageInfo
	langs          []string
	region         platform.Region
	displays       []platform.Display
	textScaleMilli uint32
	prefFlags      byte // bit 0: hasPrefs, bit 1: highContrast, bit 2: reduceMotion, bit 3: reduceTransparency
}

// conn is the socket to the host: one reader and one writer, both on the
// same descriptor.
type conn struct {
	fd        int
	wmu       sync.Mutex
	closeOnce sync.Once
}

func (c *conn) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.fd >= 0 {
			_ = syscall.Shutdown(c.fd, syscall.SHUT_RDWR)
			_ = syscall.Close(c.fd)
		}
	})
}

func (c *conn) write(kind byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	msg := make([]byte, 5+len(payload))
	msg[0] = kind
	binary.LittleEndian.PutUint32(msg[1:], uint32(len(payload)))
	copy(msg[5:], payload)
	for len(msg) > 0 {
		n, err := syscall.Write(c.fd, msg)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		msg = msg[n:]
	}
	return nil
}

func (c *conn) writeWithFD(kind byte, payload []byte, fd int) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	msg := make([]byte, 5+len(payload))
	msg[0] = kind
	binary.LittleEndian.PutUint32(msg[1:], uint32(len(payload)))
	copy(msg[5:], payload)
	oob := syscall.UnixRights(fd)
	for {
		n, err := syscall.SendmsgN(c.fd, msg, oob, nil, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		msg = msg[n:]
		break
	}
	for len(msg) > 0 {
		n, err := syscall.Write(c.fd, msg)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		msg = msg[n:]
	}
	return nil
}

// read returns the next message, with the descriptor the host attached to
// it, -1 when there is none.
func (c *conn) read() (kind byte, payload []byte, fd int, err error) {
	fd = -1
	hdr := make([]byte, 5)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := syscall.Recvmsg(c.fd, hdr, oob, 0)
	if err != nil {
		if err == syscall.EINTR {
			return c.read()
		}
		return 0, nil, -1, err
	}
	if n == 0 {
		return kindQuit, nil, -1, nil
	}
	for n < len(hdr) {
		m, err := syscall.Read(c.fd, hdr[n:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return 0, nil, -1, err
		}
		if m == 0 {
			return 0, nil, -1, fmt.Errorf("ohos: short message header")
		}
		n += m
	}
	if oobn > 0 {
		if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil && len(msgs) > 0 {
			if fds, err := syscall.ParseUnixRights(&msgs[0]); err == nil && len(fds) > 0 {
				fd = fds[0]
			}
		}
	}
	kind = hdr[0]
	length := int(binary.LittleEndian.Uint32(hdr[1:]))
	if length > 64<<20 {
		return 0, nil, fd, fmt.Errorf("ohos: payload of %d bytes", length)
	}
	payload = make([]byte, length)
	for total := 0; total < length; {
		m, err := syscall.Read(c.fd, payload[total:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return 0, nil, fd, err
		}
		if m == 0 {
			return 0, nil, fd, fmt.Errorf("ohos: short payload")
		}
		total += m
	}
	return kind, payload, fd, nil
}

func requestMessage(winID uint32) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, winID)
	return p
}

// presentMessage makes the payload of a PRESENT: which window and frame, and
// which rectangles it changed. No rectangles means the whole buffer.
func presentMessage(winID, seq uint32, rects []rect) []byte {
	p := make([]byte, 12+len(rects)*16)
	binary.LittleEndian.PutUint32(p[0:], winID)
	binary.LittleEndian.PutUint32(p[4:], seq)
	binary.LittleEndian.PutUint32(p[8:], uint32(len(rects)))
	for i, r := range rects {
		at := p[12+i*16:]
		binary.LittleEndian.PutUint32(at[0:], uint32(r.x))
		binary.LittleEndian.PutUint32(at[4:], uint32(r.y))
		binary.LittleEndian.PutUint32(at[8:], r.w)
		binary.LittleEndian.PutUint32(at[12:], r.h)
	}
	return p
}

func abortMessage(winID, seq uint32) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], winID)
	binary.LittleEndian.PutUint32(p[4:], seq)
	return p
}

func frameRateMessage(winID, fps uint32) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], winID)
	binary.LittleEndian.PutUint32(p[4:], fps)
	return p
}

func cursorMessage(winID uint32, c platform.Cursor) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = byte(c)
	return p
}

func winActionMessage(winID uint32, action byte) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], winID)
	p[4] = action
	return p
}

func winConfigMessage(c winConfigWire) []byte {
	p := make([]byte, 56+len(c.title))
	binary.LittleEndian.PutUint32(p[0:], c.winID)
	binary.LittleEndian.PutUint32(p[4:], c.parentOrMask)
	binary.LittleEndian.PutUint32(p[8:], uint32(c.x))
	binary.LittleEndian.PutUint32(p[12:], uint32(c.y))
	binary.LittleEndian.PutUint32(p[16:], uint32(c.w))
	binary.LittleEndian.PutUint32(p[20:], uint32(c.h))
	binary.LittleEndian.PutUint32(p[24:], uint32(c.minW))
	binary.LittleEndian.PutUint32(p[28:], uint32(c.minH))
	binary.LittleEndian.PutUint32(p[32:], uint32(c.maxW))
	binary.LittleEndian.PutUint32(p[36:], uint32(c.maxH))
	binary.LittleEndian.PutUint32(p[40:], c.flags)
	binary.LittleEndian.PutUint32(p[44:], uint32(c.titleBarHeight))
	binary.LittleEndian.PutUint32(p[48:], c.bgRGBA)
	binary.LittleEndian.PutUint32(p[52:], uint32(len(c.title)))
	copy(p[56:], c.title)
	return p
}

func decodeWinConfigMessage(p []byte) (winConfigWire, bool) {
	if len(p) < 56 {
		return winConfigWire{}, false
	}
	n := int(binary.LittleEndian.Uint32(p[52:]))
	if n < 0 || 56+n > len(p) {
		return winConfigWire{}, false
	}
	return winConfigWire{
		winID:          binary.LittleEndian.Uint32(p[0:]),
		parentOrMask:   binary.LittleEndian.Uint32(p[4:]),
		x:              int32(binary.LittleEndian.Uint32(p[8:])),
		y:              int32(binary.LittleEndian.Uint32(p[12:])),
		w:              int32(binary.LittleEndian.Uint32(p[16:])),
		h:              int32(binary.LittleEndian.Uint32(p[20:])),
		minW:           int32(binary.LittleEndian.Uint32(p[24:])),
		minH:           int32(binary.LittleEndian.Uint32(p[28:])),
		maxW:           int32(binary.LittleEndian.Uint32(p[32:])),
		maxH:           int32(binary.LittleEndian.Uint32(p[36:])),
		flags:          binary.LittleEndian.Uint32(p[40:]),
		titleBarHeight: int32(binary.LittleEndian.Uint32(p[44:])),
		bgRGBA:         binary.LittleEndian.Uint32(p[48:]),
		title:          string(p[56 : 56+n]),
	}, true
}

func textInputMessage(w textInputWire) []byte {
	var flags byte
	if w.active {
		flags |= textInputActive
	}
	if w.noInputMethod {
		flags |= textInputNoInputMethod
	}
	p := make([]byte, 36+len(w.text))
	binary.LittleEndian.PutUint32(p[0:], w.winID)
	p[4] = flags
	putF32(p[8:], w.caretX)
	putF32(p[12:], w.caretY)
	putF32(p[16:], w.caretW)
	putF32(p[20:], w.caretH)
	binary.LittleEndian.PutUint32(p[24:], uint32(w.selStart))
	binary.LittleEndian.PutUint32(p[28:], uint32(w.selEnd))
	binary.LittleEndian.PutUint32(p[32:], uint32(len(w.text)))
	copy(p[36:], w.text)
	return p
}

func decodeTextInputMessage(p []byte) (textInputWire, bool) {
	if len(p) < 36 {
		return textInputWire{}, false
	}
	n := int(binary.LittleEndian.Uint32(p[32:]))
	if n < 0 || 36+n > len(p) {
		return textInputWire{}, false
	}
	flags := p[4]
	return textInputWire{
		winID:         binary.LittleEndian.Uint32(p[0:]),
		active:        flags&textInputActive != 0,
		noInputMethod: flags&textInputNoInputMethod != 0,
		caretX:        getF32(p[8:]),
		caretY:        getF32(p[12:]),
		caretW:        getF32(p[16:]),
		caretH:        getF32(p[20:]),
		selStart:      int32(binary.LittleEndian.Uint32(p[24:])),
		selEnd:        int32(binary.LittleEndian.Uint32(p[28:])),
		text:          string(p[36 : 36+n]),
	}, true
}

func encodeEnvInfoMessage(info envInfoWire) []byte {
	var buf []byte
	var hdr [12]byte
	if info.hasPkg {
		hdr[0] = 1
	}
	binary.LittleEndian.PutUint32(hdr[4:], uint32(info.region.FirstWeekday))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(info.region.HourCycle))
	buf = append(buf, hdr[:]...)
	buf = appendWireStr(buf, info.pkg.Name)
	buf = appendWireStr(buf, info.pkg.Version)
	buf = appendWireStr(buf, info.pkg.Identifier)
	buf = appendWireStr(buf, info.region.Locale)
	buf = appendUint32(buf, uint32(len(info.langs)))
	for _, l := range info.langs {
		buf = appendWireStr(buf, l)
	}
	buf = appendUint32(buf, uint32(len(info.displays)))
	for _, d := range info.displays {
		var db [48]byte
		binary.LittleEndian.PutUint64(db[0:], uint64(d.ID))
		binary.LittleEndian.PutUint32(db[8:], uint32(int32(d.Bounds.X)))
		binary.LittleEndian.PutUint32(db[12:], uint32(int32(d.Bounds.Y)))
		binary.LittleEndian.PutUint32(db[16:], uint32(int32(d.Bounds.Width)))
		binary.LittleEndian.PutUint32(db[20:], uint32(int32(d.Bounds.Height)))
		binary.LittleEndian.PutUint32(db[24:], uint32(int32(d.WorkArea.X)))
		binary.LittleEndian.PutUint32(db[28:], uint32(int32(d.WorkArea.Y)))
		binary.LittleEndian.PutUint32(db[32:], uint32(int32(d.WorkArea.Width)))
		binary.LittleEndian.PutUint32(db[36:], uint32(int32(d.WorkArea.Height)))
		binary.LittleEndian.PutUint32(db[40:], uint32(math.Round(d.ScaleFactor*1000)))
		var dflags uint16
		if d.Primary {
			dflags |= 1
		}
		if d.Internal {
			dflags |= 2
		}
		binary.LittleEndian.PutUint16(db[44:], uint16(d.Rotation))
		binary.LittleEndian.PutUint16(db[46:], dflags)
		buf = append(buf, db[:]...)
		buf = appendWireStr(buf, d.Label)
	}
	if info.textScaleMilli > 0 || info.prefFlags != 0 {
		var pb [8]byte
		binary.LittleEndian.PutUint32(pb[0:], info.textScaleMilli)
		pb[4] = info.prefFlags | 1
		buf = append(buf, pb[:]...)
	}
	return buf
}

func decodeEnvInfoMessage(p []byte) (envInfoWire, bool) {
	if len(p) < 12 {
		return envInfoWire{}, false
	}
	var out envInfoWire
	out.hasPkg = p[0] != 0
	out.region.FirstWeekday = int(int32(binary.LittleEndian.Uint32(p[4:])))
	out.region.HourCycle = int(int32(binary.LittleEndian.Uint32(p[8:])))
	off := 12
	var ok bool
	if out.pkg.Name, off, ok = readWireStr(p, off); !ok {
		return envInfoWire{}, false
	}
	if out.pkg.Version, off, ok = readWireStr(p, off); !ok {
		return envInfoWire{}, false
	}
	if out.pkg.Identifier, off, ok = readWireStr(p, off); !ok {
		return envInfoWire{}, false
	}
	if out.region.Locale, off, ok = readWireStr(p, off); !ok {
		return envInfoWire{}, false
	}
	langCount, off, ok := readUint32(p, off)
	if !ok || langCount > 64 {
		return envInfoWire{}, false
	}
	out.langs = make([]string, 0, langCount)
	for range langCount {
		var s string
		if s, off, ok = readWireStr(p, off); !ok {
			return envInfoWire{}, false
		}
		if s != "" {
			out.langs = append(out.langs, s)
		}
	}
	dispCount, off, ok := readUint32(p, off)
	if !ok || dispCount > 32 {
		return envInfoWire{}, false
	}
	out.displays = make([]platform.Display, 0, dispCount)
	for range dispCount {
		if off+48 > len(p) {
			return envInfoWire{}, false
		}
		db := p[off : off+48]
		off += 48
		label, nextOff, ok := readWireStr(p, off)
		if !ok {
			return envInfoWire{}, false
		}
		off = nextOff
		dflags := binary.LittleEndian.Uint16(db[46:])
		scale := float64(binary.LittleEndian.Uint32(db[40:])) / 1000
		if scale <= 0 {
			scale = 1
		}
		out.displays = append(out.displays, platform.Display{
			ID:    int64(binary.LittleEndian.Uint64(db[0:])),
			Label: label,
			Bounds: platform.Rect{
				X:      int(int32(binary.LittleEndian.Uint32(db[8:]))),
				Y:      int(int32(binary.LittleEndian.Uint32(db[12:]))),
				Width:  int(int32(binary.LittleEndian.Uint32(db[16:]))),
				Height: int(int32(binary.LittleEndian.Uint32(db[20:]))),
			},
			WorkArea: platform.Rect{
				X:      int(int32(binary.LittleEndian.Uint32(db[24:]))),
				Y:      int(int32(binary.LittleEndian.Uint32(db[28:]))),
				Width:  int(int32(binary.LittleEndian.Uint32(db[32:]))),
				Height: int(int32(binary.LittleEndian.Uint32(db[36:]))),
			},
			ScaleFactor: scale,
			Rotation:    int(int16(binary.LittleEndian.Uint16(db[44:]))),
			Primary:     dflags&1 != 0,
			Internal:    dflags&2 != 0,
		})
	}
	if off+8 <= len(p) {
		out.textScaleMilli = binary.LittleEndian.Uint32(p[off:])
		out.prefFlags = p[off+4]
	}
	return out, true
}

func encodePopupResultMessage(reqID uint32, chosenID int32) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[0:], reqID)
	binary.LittleEndian.PutUint32(p[4:], uint32(chosenID))
	return p
}

func appendUint32(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendUint64(b []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendWireStr(b []byte, s string) []byte {
	b = appendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func appendWireBytes(b []byte, data []byte) []byte {
	b = appendUint32(b, uint32(len(data)))
	return append(b, data...)
}

func readUint32(b []byte, off int) (uint32, int, bool) {
	if off < 0 || off+4 > len(b) {
		return 0, off, false
	}
	return binary.LittleEndian.Uint32(b[off:]), off + 4, true
}

func readUint64(b []byte, off int) (uint64, int, bool) {
	if off < 0 || off+8 > len(b) {
		return 0, off, false
	}
	return binary.LittleEndian.Uint64(b[off:]), off + 8, true
}

func readWireStr(b []byte, off int) (string, int, bool) {
	n, off, ok := readUint32(b, off)
	if !ok || int(n) < 0 || off+int(n) > len(b) {
		return "", off, false
	}
	return string(b[off : off+int(n)]), off + int(n), true
}

func readWireBytes(b []byte, off int) ([]byte, int, bool) {
	n, off, ok := readUint32(b, off)
	if !ok || int(n) < 0 || off+int(n) > len(b) {
		return nil, off, false
	}
	out := make([]byte, int(n))
	copy(out, b[off:off+int(n)])
	return out, off + int(n), true
}

func putF32(b []byte, v float32) {
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
}

func getF32(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

func putF64(b []byte, v float64) {
	binary.LittleEndian.PutUint64(b, math.Float64bits(v))
}

func getF64(b []byte) float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}
