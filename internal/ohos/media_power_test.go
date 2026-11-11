//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/ohosaudio"
	"github.com/egoist/mygo/internal/platform"
)

// AudioOpen passes the shared-memory PCM ringbuffer descriptor over SCM_RIGHTS so the host maps and reads written frames, and Close sends kindAudioClose.
func TestAudioBridgePassesSharedMemoryFDAndCloses(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	t.Cleanup(b.Quit)

	s, chunkFrames, err := ohosaudio.Open(48000, 2, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("ohosaudio.Open: %v", err)
	}

	kind, payload, recvFD, err := hostConn.read()
	if err != nil || kind != kindAudioOpen || recvFD < 0 {
		s.Close()
		t.Fatalf("hostConn.read = (kind=%d, fd=%d, err=%v), want kindAudioOpen with fd", kind, recvFD, err)
	}
	defer syscall.Close(recvFD)

	streamID, rate, ch, lat, shmBytes, ok := decodeAudioOpenMessage(payload)
	if !ok || streamID == 0 || rate != 48000 || ch != 2 || lat != ohosaudio.LatencyNormal {
		s.Close()
		t.Fatalf("decodeAudioOpenMessage = (%d, %d, %d, %d, %d, %v)", streamID, rate, ch, lat, shmBytes, ok)
	}

	hdr, ring, unmap, err := ohosaudio.MapForConsumer(recvFD, shmBytes, 2)
	if err != nil {
		s.Close()
		t.Fatalf("MapForConsumer: %v", err)
	}
	defer unmap()

	samples := make([]float32, chunkFrames*2)
	samples[0], samples[1], samples[2], samples[3] = 0.125, -0.125, 0.75, -0.75
	if err := s.Write(samples); err != nil {
		s.Close()
		t.Fatalf("s.Write: %v", err)
	}
	if hdr.WriteFrames.Load() != uint64(chunkFrames) || ring[0] != 0.125 || ring[3] != -0.75 {
		s.Close()
		t.Fatalf("consumer view mismatch: writeFrames=%d head=%v", hdr.WriteFrames.Load(), ring[:4])
	}

	s.Close()
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindAudioClose {
		t.Fatalf("after Close: kind=%d err=%v, want kindAudioClose", kind, err)
	}
	if closeID, ok := decodeAudioCloseMessage(payload); !ok || closeID != streamID {
		t.Fatalf("decodeAudioCloseMessage = (%d, %v), want (%d, true)", closeID, ok, streamID)
	}
}

// NowPlaying.Set sends artwork and strings only when track metadata changes, clears active sessions once on nil, and routes AVSession commands to AppHandler.MediaCommand.
func TestNowPlayingDeduplicatesArtworkAndRoutesMediaCommands(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	coverA := []byte{0x89, 0x50, 0x4e, 0x47, 1, 2, 3}
	coverB := []byte{0x89, 0x50, 0x4e, 0x47, 9, 8, 7, 6}
	cmds := platform.MediaPlay | platform.MediaPause | platform.MediaNext | platform.MediaSeek

	// Initial track pushes full metadata + cover bytes.
	b.NowPlaying().Set(&platform.NowPlayingInfo{
		Title:    "Haruhikage",
		Artist:   "CRYCHIC",
		Album:    "MyGO!!!!!",
		Artwork:  coverA,
		Duration: 245 * time.Second,
		Position: 12 * time.Second,
		Rate:     1,
		State:    "playing",
		Commands: cmds,
	})
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindNowPlaying {
		t.Fatalf("initial NowPlaying read = (%d, %v)", kind, err)
	}
	np1, ok := decodeNowPlayingMessage(payload)
	if !ok || !np1.active || np1.state != npStatePlaying || !np1.metaChanged ||
		np1.title != "Haruhikage" || np1.artist != "CRYCHIC" || !bytes.Equal(np1.artwork, coverA) ||
		np1.durationMs != 245000 || np1.positionMs != 12000 || np1.commands != cmds {
		t.Fatalf("np1 = %+v (ok=%v)", np1, ok)
	}

	// Progress/pause update with identical track & artwork omits strings and cover bytes.
	b.NowPlaying().Set(&platform.NowPlayingInfo{
		Title:    "Haruhikage",
		Artist:   "CRYCHIC",
		Album:    "MyGO!!!!!",
		Artwork:  coverA,
		Duration: 245 * time.Second,
		Position: 48500 * time.Millisecond,
		Rate:     1,
		State:    "paused",
		Commands: cmds,
	})
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindNowPlaying {
		t.Fatalf("progress NowPlaying read = (%d, %v)", kind, err)
	}
	np2, ok := decodeNowPlayingMessage(payload)
	if !ok || !np2.active || np2.state != npStatePaused || np2.metaChanged ||
		np2.title != "" || len(np2.artwork) != 0 || np2.positionMs != 48500 {
		t.Fatalf("np2 = %+v (ok=%v), expected metaChanged=false and omitted artwork", np2, ok)
	}

	// Cover swap triggers metaChanged=true again.
	b.NowPlaying().Set(&platform.NowPlayingInfo{
		Title:    "Haruhikage",
		Artist:   "CRYCHIC",
		Album:    "MyGO!!!!!",
		Artwork:  coverB,
		Duration: 245 * time.Second,
		Position: 50 * time.Second,
		State:    "playing",
		Commands: cmds,
	})
	_, payload, _, err = hostConn.read()
	if err != nil {
		t.Fatalf("cover-change read: %v", err)
	}
	np3, ok := decodeNowPlayingMessage(payload)
	if !ok || !np3.metaChanged || !bytes.Equal(np3.artwork, coverB) {
		t.Fatalf("np3 = %+v (ok=%v), expected metaChanged=true with new artwork", np3, ok)
	}

	// Clearing sends active=false once; a second nil is a no-op.
	b.NowPlaying().Set(nil)
	b.NowPlaying().Set(nil)
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindNowPlaying {
		t.Fatalf("clear NowPlaying read = (%d, %v)", kind, err)
	}
	npClear, ok := decodeNowPlayingMessage(payload)
	if !ok || npClear.active {
		t.Fatalf("npClear = %+v (ok=%v), want active=false", npClear, ok)
	}

	// AVSession commands dispatch through Step() to AppHandler.MediaCommand.
	if err := hostConn.write(kindMediaCmd, encodeMediaCmdMessage(platform.MediaTogglePlayPause, 0)); err != nil {
		t.Fatalf("write MediaTogglePlayPause: %v", err)
	}
	if err := hostConn.write(kindMediaCmd, encodeMediaCmdMessage(platform.MediaSeek, 93250)); err != nil {
		t.Fatalf("write MediaSeek: %v", err)
	}
	go b.readLoop()
	t.Cleanup(b.Quit)
	b.Step()
	b.Step()

	wantCmds := []recordedMediaCmd{
		{cmd: platform.MediaTogglePlayPause, pos: 0},
		{cmd: platform.MediaSeek, pos: 93250 * time.Millisecond},
	}
	if !slices.Equal(app.mediaCmds, wantCmds) {
		t.Fatalf("mediaCmds = %+v, want %+v", app.mediaCmds, wantCmds)
	}
}

// ShowNotification forwards sender fallbacks and resolves permission denial, RemoveNotification cancels by ID or all, and pre-Ready notification clicks buffer until launch completes.
func TestNotificationsShowDeniedRemoveAndLaunchClickBuffering(t *testing.T) {
	b, _, hostConn, _ := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app
	if !b.NotificationsSupported() {
		t.Fatal("NotificationsSupported() = false, want true")
	}

	// Pre-Ready notification click from cold-launch Want buffers until flushPendingAppOpens.
	b.onNotifClicked(encodeNotifClickedMessage("track-change-1"))
	if len(app.notifClicks) != 0 {
		t.Fatalf("clicked delivered before Ready: %v", app.notifClicks)
	}
	b.flushPendingAppOpens()
	if !slices.Equal(app.notifClicks, []string{"track-change-1"}) {
		t.Fatalf("notifClicks after Ready = %v, want [track-change-1]", app.notifClicks)
	}

	// Live notification click after Ready delivers immediately.
	b.onNotifClicked(encodeNotifClickedMessage("track-change-2"))
	if !slices.Equal(app.notifClicks, []string{"track-change-1", "track-change-2"}) {
		t.Fatalf("notifClicks after live click = %v", app.notifClicks)
	}

	// Sender Name + Avatar fallback when Title/Image are empty, followed by successful resolve.
	avatar := []byte{1, 2, 3, 4}
	var gotErr1 error = errors.New("not called")
	b.ShowNotification(&platform.Notification{
		ID:       "msg-42",
		Subtitle: "Direct Message",
		Body:     "Practice starts at 7",
		Silent:   true,
		Group:    "band",
		Sender:   &platform.NotificationSender{ID: "anon", Name: "Anon Chihaya", Avatar: avatar},
	}, func(err error) { gotErr1 = err })

	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindNotifShow {
		t.Fatalf("ShowNotification read = (%d, %v)", kind, err)
	}
	ns, ok := decodeNotifShowMessage(payload)
	if !ok || ns.id != "msg-42" || ns.title != "Anon Chihaya" || ns.subtitle != "Direct Message" ||
		ns.body != "Practice starts at 7" || !ns.silent || ns.group != "band" || !bytes.Equal(ns.image, avatar) {
		t.Fatalf("notifShowWire = %+v (ok=%v)", ns, ok)
	}
	b.onNotifResult(encodeNotifResultMessage(ns.reqID, false, ""))
	if gotErr1 != nil {
		t.Fatalf("ShowNotification success callback err = %v, want nil", gotErr1)
	}

	// Permission denial maps to platform.ErrNotificationsDenied.
	var gotErr2 error
	b.ShowNotification(&platform.Notification{ID: "denied-1", Title: "Now Playing", Body: "Song"}, func(err error) {
		gotErr2 = err
	})
	_, payload, _, err = hostConn.read()
	if err != nil {
		t.Fatalf("second ShowNotification read: %v", err)
	}
	ns2, _ := decodeNotifShowMessage(payload)
	b.onNotifResult(encodeNotifResultMessage(ns2.reqID, true, "1600004"))
	if !errors.Is(gotErr2, platform.ErrNotificationsDenied) {
		t.Fatalf("denied ShowNotification err = %v, want ErrNotificationsDenied", gotErr2)
	}

	// RemoveNotification and RemoveAllNotifications emit kindNotifRemove.
	b.RemoveNotification("") // ignored
	b.RemoveNotification("msg-42")
	b.RemoveAllNotifications()

	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindNotifRemove {
		t.Fatalf("RemoveNotification read = (%d, %v)", kind, err)
	}
	if all, id, ok := decodeNotifRemoveMessage(payload); !ok || all || id != "msg-42" {
		t.Fatalf("RemoveNotification decoded = (%v, %q, %v)", all, id, ok)
	}
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindNotifRemove {
		t.Fatalf("RemoveAllNotifications read = (%d, %v)", kind, err)
	}
	if all, id, ok := decodeNotifRemoveMessage(payload); !ok || !all || id != "" {
		t.Fatalf("RemoveAllNotifications decoded = (%v, %q, %v)", all, id, ok)
	}
}

// Power ref-counts KeepAwake and KeepRunning without redundant host messages, gates PowerEvent on Watch, and tracks IdleTime from user input.
func TestPowerRefCountedHoldsEventsAndIdleTime(t *testing.T) {
	t.Setenv("MYGO_OHOS_ON_BATTERY", "1")
	b, _, hostConn, _ := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	if !b.Power().OnBattery() {
		t.Fatal("initial OnBattery() = false, want true from MYGO_OHOS_ON_BATTERY=1")
	}

	// KeepRunning -> keepRunning=true, keepSystem=false, keepDisplay=false.
	relRun := b.Power().KeepRunning("background audio")
	kind, payload, _, err := hostConn.read()
	if err != nil || kind != kindPowerReq {
		t.Fatalf("KeepRunning read = (%d, %v)", kind, err)
	}
	pr, ok := decodePowerReqMessage(payload)
	if !ok || pr != (powerReqWire{keepSystem: false, keepDisplay: false, keepRunning: true, reason: "background audio"}) {
		t.Fatalf("KeepRunning powerReq = %+v (ok=%v)", pr, ok)
	}

	// Second KeepRunning with same aggregate flags + earlier reason produces no duplicate wire frame.
	relRun2 := b.Power().KeepRunning("second hold")

	// KeepAwake(display=true) upgrades keepSystem=true and keepDisplay=true.
	relDisp := b.Power().KeepAwake(true, "lyrics")
	kind, payload, _, err = hostConn.read()
	if err != nil || kind != kindPowerReq {
		t.Fatalf("KeepAwake(display) read = (%d, %v)", kind, err)
	}
	pr, ok = decodePowerReqMessage(payload)
	if !ok || !pr.keepSystem || !pr.keepDisplay || !pr.keepRunning {
		t.Fatalf("KeepAwake(display) powerReq = %+v (ok=%v)", pr, ok)
	}

	// Releasing display wake hold drops keepSystem/keepDisplay while KeepRunning stays active.
	relDisp()
	relDisp() // idempotent
	_, payload, _, err = hostConn.read()
	if err != nil {
		t.Fatalf("release display read: %v", err)
	}
	pr, ok = decodePowerReqMessage(payload)
	if !ok || pr.keepSystem || pr.keepDisplay || !pr.keepRunning {
		t.Fatalf("after relDisp powerReq = %+v (ok=%v)", pr, ok)
	}

	// Releasing first runHold switches reason to "second hold"; releasing second clears everything.
	relRun()
	_, payload, _, err = hostConn.read()
	if err != nil {
		t.Fatalf("relRun read: %v", err)
	}
	pr, _ = decodePowerReqMessage(payload)
	if !pr.keepRunning || pr.reason != "second hold" {
		t.Fatalf("after relRun powerReq = %+v", pr)
	}
	relRun2()
	_, payload, _, err = hostConn.read()
	if err != nil {
		t.Fatalf("relRun2 read: %v", err)
	}
	pr, _ = decodePowerReqMessage(payload)
	if pr != (powerReqWire{}) {
		t.Fatalf("after all holds released powerReq = %+v, want zero", pr)
	}

	// Power state updates OnBattery even before Watch(), and delivers PowerEvent once Watch() is called.
	b.onPowerState(encodePowerStateMessage(false, "lock-screen"))
	if b.Power().OnBattery() || len(app.powerEvents) != 0 {
		t.Fatalf("before Watch: onBattery=%v events=%v", b.Power().OnBattery(), app.powerEvents)
	}
	b.Power().Watch()
	b.onPowerState(encodePowerStateMessage(true, "suspend"))
	b.onPowerState(encodePowerStateMessage(true, "")) // battery-only update emits no PowerEvent
	b.onPowerState(encodePowerStateMessage(false, "resume"))
	if b.Power().OnBattery() || !slices.Equal(app.powerEvents, []string{"suspend", "resume"}) {
		t.Fatalf("after Watch: onBattery=%v events=%v", b.Power().OnBattery(), app.powerEvents)
	}

	// IdleTime ignores window focus events and resets when real pointer/keyboard input arrives.
	b.mu.Lock()
	b.lastInputAt = time.Now().Add(-5 * time.Second)
	b.mu.Unlock()
	b.onInput(encodeWindowInput(1, winFocus))
	if idle := b.Power().IdleTime(); idle < 4*time.Second {
		t.Fatalf("winFocus reset IdleTime to %v, want >= 4s", idle)
	}
	b.onInput(encodeMouseInput(1, mouseMove, 255, 0, 0, 100, 20, 30, 0))
	if idle := b.Power().IdleTime(); idle >= 2*time.Second {
		t.Fatalf("mouseMove did not reset IdleTime: got %v", idle)
	}
}

// Network.Watch subscribes once and delivers reachability transitions, and Stage background winHide occludes the surface without marking the window minimized.
func TestNetworkWatchStatusAndStageHideOcclusion(t *testing.T) {
	t.Setenv("MYGO_OHOS_NET_ONLINE", "1")
	t.Setenv("MYGO_OHOS_NET_METERED", "1")
	b, s, hostConn, h := newTestSurface(t)
	app := &testAppHandler{}
	b.app = app

	if st := b.Network().Status(); st != (platform.NetworkStatus{Online: true, Metered: true}) {
		t.Fatalf("initial Network.Status() = %+v, want {Online:true Metered:true}", st)
	}

	// Pre-Watch state updates Status() silently.
	b.onNetState(encodeNetStateMessage(false, false))
	if st := b.Network().Status(); st.Online || len(app.netChanges) != 0 {
		t.Fatalf("pre-Watch net state: status=%+v changes=%v", st, app.netChanges)
	}

	// Watch sends kindNetWatch exactly once across repeated calls.
	b.Network().Watch()
	b.Network().Watch()
	kind, _, _, err := hostConn.read()
	if err != nil || kind != kindNetWatch {
		t.Fatalf("Network.Watch read = (%d, %v), want kindNetWatch", kind, err)
	}

	b.onNetState(encodeNetStateMessage(true, false))
	if st := b.Network().Status(); st != (platform.NetworkStatus{Online: true, Metered: false}) ||
		!slices.Equal(app.netChanges, []platform.NetworkStatus{{Online: true, Metered: false}}) {
		t.Fatalf("post-Watch net state: status=%+v changes=%v", st, app.netChanges)
	}

	// Stage backgrounding (EntryAbility.onBackground -> winHide on visible window) occludes rendering
	// and emits SurfaceHidden without falsely reporting IsMinimized() == true, then un-occludes on winShow.
	s.w.Show()
	_, _, _, _ = hostConn.read() // drain winActShow
	h.events = nil
	b.onInput(encodeWindowInput(1, winHide))
	if !s.Occluded() || s.w.IsMinimized() || !s.w.IsVisible() || h.minimized != 0 ||
		len(h.events) != 1 || h.events[0].Kind != platform.SurfaceHidden {
		t.Fatalf("after Stage winHide: occluded=%v min=%v vis=%v minCount=%d events=%+v",
			s.Occluded(), s.w.IsMinimized(), s.w.IsVisible(), h.minimized, h.events)
	}
	b.onInput(encodeWindowInput(1, winShow))
	if s.Occluded() || s.w.IsMinimized() || len(h.events) != 2 || h.events[1].Kind != platform.SurfaceShown {
		t.Fatalf("after Stage winShow: occluded=%v min=%v events=%+v", s.Occluded(), s.w.IsMinimized(), h.events)
	}
}
