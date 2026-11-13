//go:build linux && (amd64 || arm64)

package ohos

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"syscall"
	"time"

	"github.com/egoist/mygo/internal/ohosproxy"
	"github.com/egoist/mygo/internal/ohosvideo"
	"github.com/egoist/mygo/internal/platform"
)

type wakeHold struct {
	display bool
	reason  string
}

type powerReqWire struct {
	keepSystem  bool
	keepDisplay bool
	keepRunning bool
	reason      string
}

type nowPlayingWire struct {
	active      bool
	state       byte
	metaChanged bool
	commands    platform.MediaCommand
	durationMs  int64
	positionMs  int64
	rate        float64
	title       string
	artist      string
	album       string
	artwork     []byte
}

type notifShowWire struct {
	reqID    uint32
	silent   bool
	id       string
	title    string
	subtitle string
	body     string
	group    string
	image    []byte
}

// AudioOpen implements ohosaudio.Bridge, forwarding a newly created shared-memory
// PCM ringbuffer descriptor to the Stage-model host process.
func (b *Backend) AudioOpen(streamID, sampleRate, channels, latencyMode uint32, shmFD int, shmBytes uint32) error {
	b.mu.Lock()
	c := b.conn
	quit := b.quit
	b.mu.Unlock()
	if quit || c == nil {
		return errors.New("ohos: host not connected")
	}
	payload := encodeAudioOpenMessage(streamID, sampleRate, channels, latencyMode, shmBytes)
	return c.writeWithFD(kindAudioOpen, payload, shmFD)
}

// AudioClose implements ohosaudio.Bridge, asking the host to stop and release
// the OH_AudioRenderer for streamID.
func (b *Backend) AudioClose(streamID uint32) error {
	b.mu.Lock()
	c := b.conn
	quit := b.quit
	b.mu.Unlock()
	if quit || c == nil {
		return nil
	}
	return c.write(kindAudioClose, encodeAudioCloseMessage(streamID))
}

func encodeAudioOpenMessage(streamID, sampleRate, channels, latencyMode, shmBytes uint32) []byte {
	p := make([]byte, 20)
	binary.LittleEndian.PutUint32(p[0:], streamID)
	binary.LittleEndian.PutUint32(p[4:], sampleRate)
	binary.LittleEndian.PutUint32(p[8:], channels)
	binary.LittleEndian.PutUint32(p[12:], latencyMode)
	binary.LittleEndian.PutUint32(p[16:], shmBytes)
	return p
}

func decodeAudioOpenMessage(p []byte) (streamID, sampleRate, channels, latencyMode, shmBytes uint32, ok bool) {
	if len(p) != 20 {
		return 0, 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint32(p[0:]),
		binary.LittleEndian.Uint32(p[4:]),
		binary.LittleEndian.Uint32(p[8:]),
		binary.LittleEndian.Uint32(p[12:]),
		binary.LittleEndian.Uint32(p[16:]),
		true
}

func encodeAudioCloseMessage(streamID uint32) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p[0:], streamID)
	return p
}

func decodeAudioCloseMessage(p []byte) (uint32, bool) {
	if len(p) != 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(p[0:]), true
}

// VideoOpen implements ohosvideo.Bridge, opening an OH_AVPlayer instance in the
// host process from an open file descriptor or a remote URL.
func (b *Backend) VideoOpen(playerID uint32, rawURL string, fd int, size int64) error {
	b.ensureReadLoop()
	b.mu.Lock()
	c := b.conn
	quit := b.quit
	b.mu.Unlock()
	if quit || c == nil {
		return errors.New("ohos: host not connected")
	}
	payload := encodeVideoOpenMessage(playerID, size, rawURL)
	if fd >= 0 {
		return c.writeWithFD(kindVideoOpen, payload, fd)
	}
	return c.write(kindVideoOpen, payload)
}

// VideoCmd implements ohosvideo.Bridge, sending a playback control command for playerID.
func (b *Backend) VideoCmd(playerID uint32, op byte, arg int64, farg float64) error {
	b.mu.Lock()
	c := b.conn
	quit := b.quit
	b.mu.Unlock()
	if quit || c == nil {
		return nil
	}
	return c.write(kindVideoCmd, encodeVideoCmdMessage(playerID, op, arg, farg))
}

func (b *Backend) onVideoShm(payload []byte, fd int) {
	playerID, slotBytes, w, h, ok := decodeVideoShmMessage(payload)
	if !ok {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return
	}
	ohosvideo.OnVideoShm(playerID, slotBytes, w, h, fd)
}

func (b *Backend) onVideoEvent(payload []byte) {
	playerID, ev, buffering, posMs, durMs, w, h, errText, ok := decodeVideoEventMessage(payload)
	if !ok {
		return
	}
	ohosvideo.OnVideoEvent(playerID, ev, buffering, posMs, durMs, w, h, errText)
}

func encodeVideoOpenMessage(playerID uint32, size int64, rawURL string) []byte {
	p := make([]byte, 12, 16+len(rawURL))
	binary.LittleEndian.PutUint32(p[0:], playerID)
	binary.LittleEndian.PutUint64(p[4:], uint64(size))
	return appendWireStr(p, rawURL)
}

func decodeVideoOpenMessage(p []byte) (playerID uint32, size int64, rawURL string, ok bool) {
	if len(p) < 16 {
		return 0, 0, "", false
	}
	playerID = binary.LittleEndian.Uint32(p[0:])
	size = int64(binary.LittleEndian.Uint64(p[4:]))
	s, off, ok := readWireStr(p, 12)
	if !ok || off != len(p) {
		return 0, 0, "", false
	}
	return playerID, size, s, true
}

func encodeVideoCmdMessage(playerID uint32, op byte, arg int64, farg float64) []byte {
	p := make([]byte, 24)
	binary.LittleEndian.PutUint32(p[0:], playerID)
	p[4] = op
	binary.LittleEndian.PutUint64(p[8:], uint64(arg))
	putF64(p[16:], farg)
	return p
}

func decodeVideoCmdMessage(p []byte) (playerID uint32, op byte, arg int64, farg float64, ok bool) {
	if len(p) != 24 {
		return 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint32(p[0:]),
		p[4],
		int64(binary.LittleEndian.Uint64(p[8:])),
		getF64(p[16:]),
		true
}

func encodeVideoShmMessage(playerID, slotBytes, w, h uint32) []byte {
	p := make([]byte, 16)
	binary.LittleEndian.PutUint32(p[0:], playerID)
	binary.LittleEndian.PutUint32(p[4:], slotBytes)
	binary.LittleEndian.PutUint32(p[8:], w)
	binary.LittleEndian.PutUint32(p[12:], h)
	return p
}

func decodeVideoShmMessage(p []byte) (playerID, slotBytes, w, h uint32, ok bool) {
	if len(p) < 16 {
		return 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint32(p[0:]),
		binary.LittleEndian.Uint32(p[4:]),
		binary.LittleEndian.Uint32(p[8:]),
		binary.LittleEndian.Uint32(p[12:]),
		true
}

func encodeVideoEventMessage(playerID uint32, ev, buffering byte, posMs, durMs int64, w, h uint32, errText string) []byte {
	p := make([]byte, 32, 36+len(errText))
	binary.LittleEndian.PutUint32(p[0:], playerID)
	p[4] = ev
	p[5] = buffering
	binary.LittleEndian.PutUint64(p[8:], uint64(posMs))
	binary.LittleEndian.PutUint64(p[16:], uint64(durMs))
	binary.LittleEndian.PutUint32(p[24:], w)
	binary.LittleEndian.PutUint32(p[28:], h)
	return appendWireStr(p, errText)
}

func decodeVideoEventMessage(p []byte) (playerID uint32, ev, buffering byte, posMs, durMs int64, w, h uint32, errText string, ok bool) {
	if len(p) < 36 {
		return 0, 0, 0, 0, 0, 0, 0, "", false
	}
	playerID = binary.LittleEndian.Uint32(p[0:])
	ev = p[4]
	buffering = p[5]
	posMs = int64(binary.LittleEndian.Uint64(p[8:]))
	durMs = int64(binary.LittleEndian.Uint64(p[16:]))
	w = binary.LittleEndian.Uint32(p[24:])
	h = binary.LittleEndian.Uint32(p[28:])
	s, off, ok := readWireStr(p, 32)
	if !ok || off != len(p) {
		return 0, 0, 0, 0, 0, 0, 0, "", false
	}
	return playerID, ev, buffering, posMs, durMs, w, h, s, true
}

// NowPlaying returns the HarmonyOS AVSession integration.
func (b *Backend) NowPlaying() platform.NowPlaying { return nowPlayingBackend{b: b} }

type nowPlayingBackend struct{ b *Backend }

func (n nowPlayingBackend) Set(info *platform.NowPlayingInfo) {
	b := n.b
	if b == nil {
		return
	}
	if info == nil {
		b.mu.Lock()
		wasActive := b.npActive
		b.npActive = false
		b.npHasMeta = false
		b.npTitle = ""
		b.npArtist = ""
		b.npAlbum = ""
		b.npArtLen = 0
		b.npArtSum = [32]byte{}
		b.mu.Unlock()
		if wasActive {
			b.send(kindNowPlaying, encodeNowPlayingMessage(nowPlayingWire{active: false}))
		}
		return
	}

	artLen := len(info.Artwork)
	var artSum [32]byte
	if artLen > 0 {
		artSum = sha256.Sum256(info.Artwork)
	}

	b.mu.Lock()
	metaChanged := !b.npHasMeta ||
		b.npTitle != info.Title ||
		b.npArtist != info.Artist ||
		b.npAlbum != info.Album ||
		b.npArtLen != artLen ||
		b.npArtSum != artSum
	b.npActive = true
	if metaChanged {
		b.npHasMeta = true
		b.npTitle = info.Title
		b.npArtist = info.Artist
		b.npAlbum = info.Album
		b.npArtLen = artLen
		b.npArtSum = artSum
	}
	b.mu.Unlock()

	st := npStateStopped
	switch info.State {
	case "playing":
		st = npStatePlaying
	case "paused":
		st = npStatePaused
	}
	rate := info.Rate
	if rate <= 0 {
		rate = 1
	}

	w := nowPlayingWire{
		active:      true,
		state:       st,
		metaChanged: metaChanged,
		commands:    info.Commands,
		durationMs:  info.Duration.Milliseconds(),
		positionMs:  info.Position.Milliseconds(),
		rate:        rate,
	}
	if metaChanged {
		w.title = info.Title
		w.artist = info.Artist
		w.album = info.Album
		w.artwork = info.Artwork
	}
	b.send(kindNowPlaying, encodeNowPlayingMessage(w))
}

func encodeNowPlayingMessage(w nowPlayingWire) []byte {
	buf := make([]byte, 32)
	if w.active {
		buf[0] = 1
	}
	buf[1] = w.state
	if w.metaChanged {
		buf[2] = 1
	}
	binary.LittleEndian.PutUint32(buf[4:], uint32(w.commands))
	binary.LittleEndian.PutUint64(buf[8:], uint64(w.durationMs))
	binary.LittleEndian.PutUint64(buf[16:], uint64(w.positionMs))
	putF64(buf[24:], w.rate)
	buf = appendWireStr(buf, w.title)
	buf = appendWireStr(buf, w.artist)
	buf = appendWireStr(buf, w.album)
	buf = appendWireBytes(buf, w.artwork)
	return buf
}

func decodeNowPlayingMessage(p []byte) (nowPlayingWire, bool) {
	if len(p) < 32 {
		return nowPlayingWire{}, false
	}
	var w nowPlayingWire
	w.active = p[0] != 0
	w.state = p[1]
	w.metaChanged = p[2] != 0
	w.commands = platform.MediaCommand(binary.LittleEndian.Uint32(p[4:]))
	w.durationMs = int64(binary.LittleEndian.Uint64(p[8:]))
	w.positionMs = int64(binary.LittleEndian.Uint64(p[16:]))
	w.rate = getF64(p[24:])
	off := 32
	var ok bool
	if w.title, off, ok = readWireStr(p, off); !ok {
		return nowPlayingWire{}, false
	}
	if w.artist, off, ok = readWireStr(p, off); !ok {
		return nowPlayingWire{}, false
	}
	if w.album, off, ok = readWireStr(p, off); !ok {
		return nowPlayingWire{}, false
	}
	if w.artwork, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return nowPlayingWire{}, false
	}
	return w, true
}

func encodeMediaCmdMessage(cmd platform.MediaCommand, posMs int64) []byte {
	p := make([]byte, 12)
	binary.LittleEndian.PutUint32(p[0:], uint32(cmd))
	binary.LittleEndian.PutUint64(p[4:], uint64(posMs))
	return p
}

func decodeMediaCmdMessage(p []byte) (platform.MediaCommand, int64, bool) {
	if len(p) != 12 {
		return 0, 0, false
	}
	cmd := platform.MediaCommand(binary.LittleEndian.Uint32(p[0:]))
	posMs := int64(binary.LittleEndian.Uint64(p[4:]))
	return cmd, posMs, true
}

func (b *Backend) onMediaCmd(payload []byte) {
	cmd, posMs, ok := decodeMediaCmdMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	app := b.app
	b.mu.Unlock()
	if app != nil {
		app.MediaCommand(cmd, time.Duration(posMs)*time.Millisecond)
	}
}

// NotificationsSupported reports that HarmonyOS NotificationKit notifications
// are available.
func (b *Backend) NotificationsSupported() bool { return true }

func (b *Backend) ShowNotification(n *platform.Notification, done func(error)) {
	if n == nil {
		if done != nil {
			done(nil)
		}
		return
	}
	if b.conn == nil {
		if done != nil {
			done(errors.New("ohos: host not connected"))
		}
		return
	}

	title := n.Title
	if title == "" && n.Sender != nil {
		title = n.Sender.Name
	}
	img := n.Image
	if len(img) == 0 && n.Sender != nil {
		img = n.Sender.Avatar
	}

	b.mu.Lock()
	b.nextNotifReq++
	reqID := b.nextNotifReq
	if b.pendingNotifs == nil {
		b.pendingNotifs = make(map[uint32]func(error))
	}
	b.pendingNotifs[reqID] = done
	b.mu.Unlock()

	b.send(kindNotifShow, encodeNotifShowMessage(notifShowWire{
		reqID:    reqID,
		silent:   n.Silent,
		id:       n.ID,
		title:    title,
		subtitle: n.Subtitle,
		body:     n.Body,
		group:    n.Group,
		image:    img,
	}))
}

func (b *Backend) RemoveNotification(id string) {
	if id == "" {
		return
	}
	b.send(kindNotifRemove, encodeNotifRemoveMessage(false, id))
}

func (b *Backend) RemoveAllNotifications() {
	b.send(kindNotifRemove, encodeNotifRemoveMessage(true, ""))
}

func encodeNotifShowMessage(w notifShowWire) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:], w.reqID)
	if w.silent {
		buf[4] = 1
	}
	buf = appendWireStr(buf, w.id)
	buf = appendWireStr(buf, w.title)
	buf = appendWireStr(buf, w.subtitle)
	buf = appendWireStr(buf, w.body)
	buf = appendWireStr(buf, w.group)
	buf = appendWireBytes(buf, w.image)
	return buf
}

func decodeNotifShowMessage(p []byte) (notifShowWire, bool) {
	if len(p) < 8 {
		return notifShowWire{}, false
	}
	var w notifShowWire
	w.reqID = binary.LittleEndian.Uint32(p[0:])
	w.silent = p[4] != 0
	off := 8
	var ok bool
	if w.id, off, ok = readWireStr(p, off); !ok {
		return notifShowWire{}, false
	}
	if w.title, off, ok = readWireStr(p, off); !ok {
		return notifShowWire{}, false
	}
	if w.subtitle, off, ok = readWireStr(p, off); !ok {
		return notifShowWire{}, false
	}
	if w.body, off, ok = readWireStr(p, off); !ok {
		return notifShowWire{}, false
	}
	if w.group, off, ok = readWireStr(p, off); !ok {
		return notifShowWire{}, false
	}
	if w.image, off, ok = readWireBytes(p, off); !ok || off != len(p) {
		return notifShowWire{}, false
	}
	return w, true
}

func encodeNotifRemoveMessage(all bool, id string) []byte {
	buf := make([]byte, 4)
	if all {
		buf[0] = 1
	}
	return appendWireStr(buf, id)
}

func decodeNotifRemoveMessage(p []byte) (all bool, id string, ok bool) {
	if len(p) < 4 {
		return false, "", false
	}
	all = p[0] != 0
	id, off, ok := readWireStr(p, 4)
	if !ok || off != len(p) {
		return false, "", false
	}
	return all, id, true
}

func encodeNotifResultMessage(reqID uint32, denied bool, errStr string) []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint32(buf[0:], reqID)
	if denied {
		buf[4] = 1
	}
	return appendWireStr(buf, errStr)
}

func decodeNotifResultMessage(p []byte) (reqID uint32, denied bool, errStr string, ok bool) {
	if len(p) < 8 {
		return 0, false, "", false
	}
	reqID = binary.LittleEndian.Uint32(p[0:])
	denied = p[4] != 0
	errStr, off, ok := readWireStr(p, 8)
	if !ok || off != len(p) {
		return 0, false, "", false
	}
	return reqID, denied, errStr, true
}

func encodeNotifClickedMessage(id string) []byte {
	return appendWireStr(nil, id)
}

func decodeNotifClickedMessage(p []byte) (string, bool) {
	id, off, ok := readWireStr(p, 0)
	if !ok || off != len(p) {
		return "", false
	}
	return id, true
}

func (b *Backend) onNotifResult(payload []byte) {
	reqID, denied, errStr, ok := decodeNotifResultMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	var done func(error)
	if b.pendingNotifs != nil {
		done = b.pendingNotifs[reqID]
		delete(b.pendingNotifs, reqID)
	}
	b.mu.Unlock()
	if done == nil {
		return
	}
	if denied {
		done(platform.ErrNotificationsDenied)
		return
	}
	if errStr != "" {
		done(errors.New(errStr))
		return
	}
	done(nil)
}

func (b *Backend) onNotifClicked(payload []byte) {
	id, ok := decodeNotifClickedMessage(payload)
	if !ok || id == "" {
		return
	}
	b.mu.Lock()
	if !b.readyDone {
		b.pendingNotifClicks = append(b.pendingNotifClicks, id)
		b.mu.Unlock()
		return
	}
	app := b.app
	b.mu.Unlock()
	if app != nil {
		app.NotificationClicked(id)
	}
}

// Power returns the HarmonyOS power, wake-lock, and background-task bridge.
func (b *Backend) Power() platform.Power { return powerBackend{b: b} }

type powerBackend struct{ b *Backend }

func (p powerBackend) Watch() {
	if p.b == nil {
		return
	}
	p.b.mu.Lock()
	p.b.powerWatching = true
	p.b.mu.Unlock()
}

func (p powerBackend) KeepAwake(display bool, reason string) func() {
	b := p.b
	if b == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextPowerHold++
	id := b.nextPowerHold
	if b.wakeHolds == nil {
		b.wakeHolds = make(map[uint64]wakeHold)
	}
	b.wakeHolds[id] = wakeHold{display: display, reason: reason}
	msg, send := b.syncPowerLocked()
	b.mu.Unlock()
	if send {
		b.send(kindPowerReq, msg)
	}

	return sync.OnceFunc(func() {
		b.mu.Lock()
		delete(b.wakeHolds, id)
		relMsg, relSend := b.syncPowerLocked()
		b.mu.Unlock()
		if relSend {
			b.send(kindPowerReq, relMsg)
		}
	})
}

func (p powerBackend) KeepRunning(reason string) func() {
	b := p.b
	if b == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextPowerHold++
	id := b.nextPowerHold
	if b.runHolds == nil {
		b.runHolds = make(map[uint64]string)
	}
	b.runHolds[id] = reason
	msg, send := b.syncPowerLocked()
	b.mu.Unlock()
	if send {
		b.send(kindPowerReq, msg)
	}

	return sync.OnceFunc(func() {
		b.mu.Lock()
		delete(b.runHolds, id)
		relMsg, relSend := b.syncPowerLocked()
		b.mu.Unlock()
		if relSend {
			b.send(kindPowerReq, relMsg)
		}
	})
}

func (p powerBackend) OnBattery() bool {
	if p.b == nil {
		return false
	}
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	return p.b.onBattery
}

func (p powerBackend) IdleTime() time.Duration {
	if p.b == nil {
		return 0
	}
	p.b.mu.Lock()
	last := p.b.lastInputAt
	p.b.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	if d := time.Since(last); d > 0 {
		return d
	}
	return 0
}

func (b *Backend) noteUserActivity() {
	b.mu.Lock()
	b.lastInputAt = time.Now()
	b.mu.Unlock()
}

func (b *Backend) syncPowerLocked() ([]byte, bool) {
	var cur powerReqWire
	var minID uint64
	for id, h := range b.wakeHolds {
		cur.keepSystem = true
		if h.display {
			cur.keepDisplay = true
		}
		if h.reason != "" && (minID == 0 || id < minID) {
			minID = id
			cur.reason = h.reason
		}
	}
	if len(b.runHolds) > 0 || cur.keepSystem {
		cur.keepRunning = true
	}
	for id, r := range b.runHolds {
		if r != "" && (minID == 0 || id < minID) {
			minID = id
			cur.reason = r
		}
	}
	if cur == b.lastPowerReq {
		return nil, false
	}
	b.lastPowerReq = cur
	return encodePowerReqMessage(cur), true
}

func encodePowerReqMessage(w powerReqWire) []byte {
	buf := make([]byte, 4)
	if w.keepSystem {
		buf[0] = 1
	}
	if w.keepDisplay {
		buf[1] = 1
	}
	if w.keepRunning {
		buf[2] = 1
	}
	return appendWireStr(buf, w.reason)
}

func decodePowerReqMessage(p []byte) (powerReqWire, bool) {
	if len(p) < 4 {
		return powerReqWire{}, false
	}
	reason, off, ok := readWireStr(p, 4)
	if !ok || off != len(p) {
		return powerReqWire{}, false
	}
	return powerReqWire{
		keepSystem:  p[0] != 0,
		keepDisplay: p[1] != 0,
		keepRunning: p[2] != 0,
		reason:      reason,
	}, true
}

func encodePowerStateMessage(onBattery bool, event string) []byte {
	buf := make([]byte, 4)
	if onBattery {
		buf[0] = 1
	}
	return appendWireStr(buf, event)
}

func decodePowerStateMessage(p []byte) (onBattery bool, event string, ok bool) {
	if len(p) < 4 {
		return false, "", false
	}
	event, off, ok := readWireStr(p, 4)
	if !ok || off != len(p) {
		return false, "", false
	}
	return p[0] != 0, event, true
}

func (b *Backend) onPowerState(payload []byte) {
	onBat, ev, ok := decodePowerStateMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	b.onBattery = onBat
	watching := b.powerWatching
	app := b.app
	b.mu.Unlock()
	if watching && ev != "" && app != nil {
		app.PowerEvent(ev)
	}
}

// Network returns the HarmonyOS network status and reachability monitor.
func (b *Backend) Network() platform.Network { return networkBackend{b: b} }

type networkBackend struct{ b *Backend }

func (n networkBackend) Watch() {
	if n.b == nil {
		return
	}
	n.b.mu.Lock()
	first := !n.b.netWatching
	n.b.netWatching = true
	n.b.mu.Unlock()
	if first {
		n.b.send(kindNetWatch, nil)
	}
}

func (n networkBackend) Status() platform.NetworkStatus {
	if n.b == nil {
		return platform.NetworkStatus{Online: true}
	}
	n.b.mu.Lock()
	defer n.b.mu.Unlock()
	return n.b.netStatus
}

func encodeNetStateMessage(online, metered bool) []byte {
	p := make([]byte, 4)
	if online {
		p[0] = 1
	}
	if metered {
		p[1] = 1
	}
	return p
}

func encodeNetStateProxyMessage(online, metered bool, proxy ohosproxy.Config) []byte {
	p := make([]byte, 8)
	if online {
		p[0] = 1
	}
	if metered {
		p[1] = 1
	}
	p[2] = 1
	excl := proxy.Exclusions
	if len(excl) > 1024 {
		excl = excl[:1024]
	}
	var port uint16
	if proxy.Port > 0 && proxy.Port <= 65535 {
		port = uint16(proxy.Port)
	}
	binary.LittleEndian.PutUint16(p[4:], port)
	binary.LittleEndian.PutUint16(p[6:], uint16(len(excl)))
	p = appendWireStr(p, proxy.Host)
	p = appendWireStr(p, proxy.Username)
	p = appendWireStr(p, proxy.Password)
	for _, item := range excl {
		p = appendWireStr(p, item)
	}
	return p
}

func decodeNetStateMessage(p []byte) (online, metered, ok bool) {
	online, metered, _, _, ok = decodeNetStateFullMessage(p)
	return online, metered, ok
}

func decodeNetStateFullMessage(p []byte) (online, metered, hasProxy bool, proxy ohosproxy.Config, ok bool) {
	if len(p) < 4 {
		return false, false, false, ohosproxy.Config{}, false
	}
	online = p[0] != 0
	metered = p[1] != 0
	if p[2] == 0 {
		if len(p) != 4 {
			return false, false, false, ohosproxy.Config{}, false
		}
		return online, metered, false, ohosproxy.Config{}, true
	}
	if len(p) < 8 {
		return false, false, false, ohosproxy.Config{}, false
	}
	proxy.Port = int(binary.LittleEndian.Uint16(p[4:]))
	exclCount := int(binary.LittleEndian.Uint16(p[6:]))
	if exclCount > 1024 {
		return false, false, false, ohosproxy.Config{}, false
	}
	off := 8
	if proxy.Host, off, ok = readWireStr(p, off); !ok {
		return false, false, false, ohosproxy.Config{}, false
	}
	if proxy.Username, off, ok = readWireStr(p, off); !ok {
		return false, false, false, ohosproxy.Config{}, false
	}
	if proxy.Password, off, ok = readWireStr(p, off); !ok {
		return false, false, false, ohosproxy.Config{}, false
	}
	if exclCount > 0 {
		proxy.Exclusions = make([]string, 0, exclCount)
		for range exclCount {
			var item string
			if item, off, ok = readWireStr(p, off); !ok {
				return false, false, false, ohosproxy.Config{}, false
			}
			proxy.Exclusions = append(proxy.Exclusions, item)
		}
	}
	if off != len(p) {
		return false, false, false, ohosproxy.Config{}, false
	}
	return online, metered, true, proxy, true
}

func (b *Backend) onNetState(payload []byte) {
	online, metered, hasProxy, proxy, ok := decodeNetStateFullMessage(payload)
	if !ok {
		return
	}
	if hasProxy {
		ohosproxy.Set(proxy)
	}
	st := platform.NetworkStatus{Online: online, Metered: metered}
	b.mu.Lock()
	b.netStatus = st
	watching := b.netWatching
	app := b.app
	b.mu.Unlock()
	if watching && app != nil {
		app.NetworkChanged(st)
	}
}
