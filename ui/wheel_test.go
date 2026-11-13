package ui

import (
	"math"
	"testing"
	"time"

	"github.com/egoist/mygo/internal/platform"
)

func TestWheelGlides(t *testing.T) {
	var s ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			coreBox(c).Height(1000).Shrink(0)
		})
	}, 200, 200)
	tt.Move(100, 100)
	// A notch of a wheel glides there, as browsers scroll it.
	tt.Wheel(100, 100, 0, 100)
	tt.Frame()
	if s.Y != 0 {
		t.Errorf("a notch scrolled at once, to %v", s.Y)
	}
	tt.Advance(50 * time.Millisecond)
	if s.Y <= 0 || s.Y >= 100 {
		t.Errorf("a notch on its way: at %v", s.Y)
	}
	// Notches on the way add up.
	tt.Wheel(100, 100, 0, 100)
	tt.Advance(time.Second)
	if s.Y != 200 || tt.FramePending() {
		t.Errorf("after two notches: at %v, frames pending %v", s.Y, tt.FramePending())
	}
	// Past the end, they stop there.
	for range 10 {
		tt.Wheel(100, 100, 0, 100)
	}
	tt.Advance(time.Second)
	if s.Y != 800 {
		t.Errorf("past the end: at %v", s.Y)
	}
	// The app setting the offset takes over.
	tt.Wheel(100, 100, 0, -100)
	tt.Advance(50 * time.Millisecond)
	s.Y = 10
	tt.Advance(time.Second)
	if s.Y != 10 {
		t.Errorf("set to 10 on the way: at %v", s.Y)
	}
	// A touchpad scrolls at once.
	tt.Scroll(100, 100, 0, 30)
	if s.Y != 40 {
		t.Errorf("a touchpad scrolled to %v", s.Y)
	}
}

func TestWheelMomentumReachesInput(t *testing.T) {
	var got InputEvent
	tt := coreNewTester(func(c *context) {
		coreBox(c).Fill().HandleInput(func(ev InputEvent) bool {
			got = ev
			return true
		})
	}, 200, 200)
	tt.Move(100, 100)
	tt.send(platform.SurfaceEvent{Kind: platform.PointerScroll, X: 100, Y: 100, DY: -40, Precise: true, Momentum: true})
	if got.Kind != InputScroll || !got.Precise || !got.Momentum {
		t.Errorf("input got kind %v, precise %v, momentum %v; want InputScroll, true, true", got.Kind, got.Precise, got.Momentum)
	}
}

func TestWheelReducedMotion(t *testing.T) {
	var s ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			coreBox(c).Height(1000).Shrink(0)
		})
	}, 200, 200)
	tt.SetPreferences(Preferences{ReduceMotion: true, TextScale: 1})
	tt.Move(100, 100)
	tt.Wheel(100, 100, 0, 100)
	if s.Y != 100 {
		t.Errorf("with less motion, a notch scrolled to %v", s.Y)
	}
}

func TestWheelInnerAtEnd(t *testing.T) {
	var outer, inner ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&outer).Children(func() {
			coreScroll(c).Height(100).Shrink(0).TrackScroll(&inner).Children(func() {
				coreBox(c).Height(150).Shrink(0)
			})
			coreBox(c).Height(1000).Shrink(0)
		})
	}, 200, 200)
	tt.Move(100, 50)
	// The inner container takes the notches it has room for, the outer
	// the next.
	tt.Wheel(100, 50, 0, 40)
	tt.Wheel(100, 50, 0, 40)
	tt.Advance(time.Second)
	if inner.Y != 50 || outer.Y != 0 {
		t.Errorf("two notches: inner at %v, outer at %v", inner.Y, outer.Y)
	}
	tt.Wheel(100, 50, 0, 40)
	tt.Advance(time.Second)
	if inner.Y != 50 || outer.Y != 40 {
		t.Errorf("three notches: inner at %v, outer at %v", inner.Y, outer.Y)
	}
}

// The app scrolling a List, or an element coming into view, takes over
// from the notches of a wheel on their way, as setting a ScrollState does.
func TestWheelListScrollTakesOver(t *testing.T) {
	var s ListState
	tt := coreNewTester(func(c *context) {
		coreList(c, &s, 200, func(i int) { coreBox(c).Height(20) }).Fill()
	}, 200, 200)
	offset := func() float64 { return tt.rt.states[tt.rt.c.root.first.id].scrollY }
	tt.Move(100, 100)
	tt.Wheel(100, 100, 0, 300)
	tt.Advance(30 * time.Millisecond)
	if offset() <= 0 {
		t.Fatalf("a notch on its way: at %v", offset())
	}
	s.ScrollTo(0, Start)
	tt.Advance(time.Second)
	if offset() != 0 {
		t.Errorf("ScrollTo(0) on the way: at %v", offset())
	}
	// A notch after the app's ScrollTo glides as usual.
	tt.Wheel(100, 100, 0, 100)
	tt.Advance(time.Second)
	if math.Abs(offset()-100) > 0.01 {
		t.Errorf("a notch after ScrollTo: at %v", offset())
	}
	tt.Wheel(100, 100, 0, -100)
	tt.Advance(30 * time.Millisecond)
	s.ScrollToEnd()
	tt.Advance(time.Second)
	if end := float64(200*20 - 200); math.Abs(offset()-end) > 0.01 {
		t.Errorf("ScrollToEnd on the way up: at %v, want %v", offset(), end)
	}
}

func TestWheelRevealTakesOver(t *testing.T) {
	reveal := false
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().Children(func() {
			for i := range 50 {
				e := coreBox(c).Height(40).Shrink(0)
				if i == 0 && reveal {
					e.ScrollIntoView()
				}
			}
		})
	}, 200, 200)
	offset := func() float64 { return tt.rt.states[tt.rt.c.root.first.id].scrollY }
	tt.Move(100, 100)
	tt.Wheel(100, 100, 0, 400)
	tt.Advance(30 * time.Millisecond)
	if offset() <= 0 {
		t.Fatalf("a notch on its way: at %v", offset())
	}
	reveal = true
	tt.Frame()
	reveal = false
	tt.Advance(time.Second)
	if offset() != 0 {
		t.Errorf("the first box brought into view on the way: at %v", offset())
	}
}

// TestWheelReverseAtEnd reproduces a wheel gliding to the end of the
// content, and the user turning it the other way while the notches are
// still on their way: the remaining travel must not absorb the turn, or
// the wheel seems to not respond until the old notches run out.
func TestWheelReverseAtEnd(t *testing.T) {
	var s ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			coreBox(c).Height(1000).Shrink(0)
		})
	}, 200, 200)
	tt.Move(100, 100)
	// The notches of a fast wheel add up on their way.
	for range 10 {
		tt.Wheel(100, 100, 0, 100)
	}
	tt.Advance(50 * time.Millisecond)
	if s.Y <= 0 || s.Y >= 800 {
		t.Fatalf("setup: gliding at %v, want between 0 and 800", s.Y)
	}
	// Keep scrolling to the end, then turn the wheel the other way at once.
	for range 10 {
		tt.Wheel(100, 100, 0, 100)
	}
	// Advance frame by frame until the content reaches the end, rather than
	// relying on a fixed time that may land it a hair short: arm64 fuses
	// float multiply-add into FMA, so the notches have not run out at 50 ms
	// and the content is still on its way (e.g. at 770). Reaching the end
	// is what matters, not how long it took.
	for i := 0; s.Y < 800 && i < 100; i++ {
		tt.Advance(16 * time.Millisecond)
	}
	if s.Y != 800 {
		t.Fatalf("setup: at the end %v, want 800", s.Y)
	}
	tt.Wheel(100, 100, 0, -100)
	tt.Advance(50 * time.Millisecond)
	if s.Y >= 800 {
		t.Fatalf("the wheel did not turn back at the end: still at %v, want < 800", s.Y)
	}
}

// Consecutive wheel notches carry the glide's velocity forward smoothly without restarting from zero.
func TestWheelConsecutiveNotchesKeepMomentum(t *testing.T) {
	var s ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			coreBox(c).Height(2000).Shrink(0)
		})
	}, 200, 200)
	tt.Move(100, 100)
	tt.Wheel(100, 100, 0, 100)
	tt.Advance(17 * time.Millisecond)
	firstStep := s.Y
	if firstStep <= 0 {
		t.Fatalf("first notch did not move: got %v", firstStep)
	}
	tt.Advance(17 * time.Millisecond)

	// A second notch arriving mid-glide builds on the existing spring velocity
	// rather than starting over from zero speed as the first frame did.
	tt.Wheel(100, 100, 0, 100)
	atNotch := s.Y
	tt.Advance(17 * time.Millisecond)
	secondStep := s.Y - atNotch
	if secondStep <= firstStep*2 {
		t.Fatalf("second notch did not carry momentum: secondStep=%v, firstStep=%v", secondStep, firstStep)
	}

	tt.Advance(time.Second)
	if math.Abs(float64(s.Y-200)) > 0.01 || tt.FramePending() {
		t.Fatalf("settled at %v (pending=%v), want 200 and false", s.Y, tt.FramePending())
	}
}

// Touchpad scrolling moves flowing rows and glyphs continuously from the very first fractional frame without height breathing, jump on start, settle twitch, or dropping subpixel glyphs to transforms.
func TestScrollSubpixelMotionAndSettle(t *testing.T) {
	var s ScrollState
	rowColor := RGB(40, 120, 220)
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			for range 20 {
				coreBox(c).Height(20).Shrink(0).Background(rowColor).Children(func() {
					coreText(c, "Smooth row")
				})
			}
		})
	}, 200, 200)
	tt.SetScale(1.9)
	tt.Move(100, 100)

	findRow := func() (y, h float32, ok bool) {
		for _, op := range tt.rt.scene.Ops {
			if op.Color == rowColor.scene() && op.Rect.H > 30 && op.Rect.H < 45 {
				return op.Rect.Y, op.Rect.H, true
			}
		}
		return 0, 0, false
	}

	y0, wantH, ok := findRow()
	if !ok {
		t.Fatal("initial row fill not found in scene")
	}
	if len(tt.rt.scene.Glyphs) == 0 {
		t.Fatal("expected text glyphs in scene")
	}
	g0Y := tt.rt.scene.Glyphs[0].Y

	// Frame 1 of scrolling moves both row and glyph by the exact fractional device-pixel delta (-0.05 * 1.9 = -0.095px) without any mode-switch jump.
	tt.Scroll(100, 100, 0, 0.05)
	if tt.FramePending() {
		t.Fatal("precise scroll left an artificial settle timer pending")
	}
	y1, h1, ok := findRow()
	if !ok {
		t.Fatal("row fill not found on first scroll frame")
	}
	if h1 != wantH {
		t.Fatalf("row height changed on scroll start: got %v, want %v", h1, wantH)
	}
	if got, want := y1-y0, float32(-0.05*1.9); math.Abs(float64(got-want)) > 1e-3 {
		t.Fatalf("first-frame row delta = %v, want %v (y0=%v, y1=%v)", got, want, y0, y1)
	}
	g1 := tt.rt.scene.Glyphs[0]
	if got, want := g1.Y-g0Y, float32(-0.05*1.9); math.Abs(float64(got-want)) > 1e-3 {
		t.Fatalf("first-frame glyph Y delta = %v, want %v (g0=%v, g1=%v)", got, want, g0Y, g1.Y)
	}
	if g1.Transform.Set {
		t.Fatalf("untransformed scroll glyph unexpectedly set Transform %+v", g1.Transform)
	}

	// Intermittent scrolling with >100 ms pauses never twitches while stationary and keeps pixel height constant.
	for range 5 {
		tt.Advance(150 * time.Millisecond)
		yBeforePauseEnd, _, _ := findRow()
		if yBeforePauseEnd != y1 {
			t.Fatalf("row twitched during stationary pause: got %v, want %v", yBeforePauseEnd, y1)
		}
		tt.Scroll(100, 100, 0, 0.23)
		var h float32
		y1, h, ok = findRow()
		if !ok || h != wantH {
			t.Fatalf("intermittent scroll row ok=%v h=%v, want h=%v", ok, h, wantH)
		}
	}
}

// A live window whose first VSync frame after a mouse wheel notch carries a timestamp at or before the input event still steps smoothly on the first frame instead of stalling for one refresh.
func TestWheelFirstFrameLeadIn(t *testing.T) {
	var s ScrollState
	tt := coreNewTester(func(c *context) {
		coreScroll(c).Fill().TrackScroll(&s).Children(func() {
			coreBox(c).Height(2000).Shrink(0)
		})
	}, 200, 200)
	tt.Move(100, 100)
	wheelAt := tt.rt.now()
	tt.Wheel(100, 100, 0, 90)
	// Simulate a live VSync callback whose start timestamp predates the wheel event by 2 ms.
	savedClock := tt.rt.clock
	tt.rt.clock = nil
	defer func() { tt.rt.clock = savedClock }()
	tt.send(platform.SurfaceEvent{Kind: platform.SurfaceFrame, Paced: true, Time: wheelAt.Add(-2 * time.Millisecond)})
	if s.Y <= 0 {
		t.Fatalf("first VSync frame with predated timestamp stalled at %v, want > 0", s.Y)
	}
}
