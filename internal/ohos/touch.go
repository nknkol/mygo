//go:build linux && (amd64 || arm64)

package ohos

import (
	"math"
	"slices"

	"github.com/egoist/mygo/internal/platform"
)

const (
	touchSlop        = 8    // DIPs before a tap becomes a scroll
	longPress        = 500  // milliseconds for secondary-click press-and-hold
	flingMinSpeed    = 140  // DIP/s needed to start momentum after finger lift
	flingMaxSpeed    = 6500 // DIP/s cap on initial momentum velocity
	flingStopSpeed   = 18   // DIP/s below which momentum stops
	flingDecayPerSec = 4.5  // exponential friction rate (1/s)
)

type touchSample struct {
	x, y float64
	ms   uint32
}

// touchGesture follows touchscreen fingers by their ArkUI pointer IDs and
// turns taps, press-and-hold, one-finger drags/flicks, and two-finger
// scroll+pinch into SurfaceEvents.
type touchGesture struct {
	fingers        []touchFinger
	tap            bool
	scrolled       bool
	startX, startY float64
	start          uint32
	x, y, dist     float64
	history        []touchSample
	fling          touchFling
}

type touchFinger struct {
	id   int32
	x, y float64
}

type touchFling struct {
	active bool
	x, y   float64
	vx, vy float64 // scroll velocity in DIP/s (same sign as PointerScroll DX/DY)
}

func (g *touchGesture) cancelFling() {
	g.fling = touchFling{}
}

func (g *touchGesture) begin(id int32, x, y float64, ms uint32, firstNew bool) []platform.SurfaceEvent {
	g.cancelFling()
	if firstNew {
		g.fingers = g.fingers[:0]
	}
	if g.find(id) >= 0 {
		return nil
	}
	g.fingers = append(g.fingers, touchFinger{id: id, x: x, y: y})
	g.tap = len(g.fingers) == 1
	g.scrolled = false
	g.history = g.history[:0]
	if g.tap {
		g.startX, g.startY, g.start = x, y, ms
		g.recordSample(x, y, ms)
	}
	g.anchor()
	return nil
}

func (g *touchGesture) moveBatch(pts []touchPoint, ms uint32) []platform.SurfaceEvent {
	movedPrimary := false
	for _, pt := range pts {
		i := g.find(pt.id)
		if i < 0 {
			continue
		}
		g.fingers[i].x, g.fingers[i].y = pt.x, pt.y
		if i < 2 {
			movedPrimary = true
		}
	}
	if !movedPrimary || len(g.fingers) == 0 {
		return nil
	}
	var out []platform.SurfaceEvent
	if len(g.fingers) == 1 {
		x, y := g.fingers[0].x, g.fingers[0].y
		if g.tap && math.Hypot(x-g.startX, y-g.startY) < touchSlop {
			return nil
		}
		g.tap = false
		g.scrolled = true
		g.recordSample(x, y, ms)
		return g.scroll(out, x, y)
	}
	g.history = g.history[:0]
	g.scrolled = false
	a, b := g.fingers[0], g.fingers[1]
	mx, my := (a.x+b.x)/2, (a.y+b.y)/2
	out = g.scroll(out, mx, my)
	if d := math.Hypot(a.x-b.x, a.y-b.y); d > 0 {
		if g.dist > 0 && d != g.dist {
			out = append(out, platform.SurfaceEvent{
				Kind:  platform.PointerZoom,
				X:     mx,
				Y:     my,
				Scale: d / g.dist,
			})
		}
		g.dist = d
	}
	return out
}

func (g *touchGesture) cancelAll() []platform.SurfaceEvent {
	if len(g.fingers) == 0 {
		return nil
	}
	g.fingers = g.fingers[:0]
	g.tap = false
	g.scrolled = false
	g.history = g.history[:0]
	g.cancelFling()
	return []platform.SurfaceEvent{
		{Kind: platform.PointerCancel},
		{Kind: platform.PointerLeave},
	}
}

func (g *touchGesture) move(id int32, x, y float64, ms uint32) []platform.SurfaceEvent {
	return g.moveBatch([]touchPoint{{id: id, x: x, y: y}}, ms)
}

func (g *touchGesture) end(id int32, x, y float64, ms uint32, canceled bool) []platform.SurfaceEvent {
	i := g.find(id)
	if i < 0 {
		return nil
	}
	var out []platform.SurfaceEvent
	if !canceled {
		out = g.move(id, x, y, ms)
	}
	g.fingers = slices.Delete(g.fingers, i, i+1)
	if len(g.fingers) > 0 {
		g.history = g.history[:0]
		g.scrolled = false
		g.anchor()
		return out
	}
	if canceled {
		g.tap = false
		g.scrolled = false
		g.history = g.history[:0]
		return append(out,
			platform.SurfaceEvent{Kind: platform.PointerCancel},
			platform.SurfaceEvent{Kind: platform.PointerLeave},
		)
	}
	if g.tap {
		button := 0
		if ms-g.start >= longPress {
			button = 1
		}
		out = append(out,
			platform.SurfaceEvent{Kind: platform.PointerDown, X: g.startX, Y: g.startY, Button: button},
			platform.SurfaceEvent{Kind: platform.PointerUp, X: g.startX, Y: g.startY, Button: button},
		)
	} else if g.scrolled {
		g.startFling(x, y, ms)
	}
	g.tap = false
	g.scrolled = false
	g.history = g.history[:0]
	return append(out, platform.SurfaceEvent{Kind: platform.PointerLeave})
}

func (g *touchGesture) scroll(out []platform.SurfaceEvent, x, y float64) []platform.SurfaceEvent {
	if x != g.x || y != g.y {
		out = append(out, platform.SurfaceEvent{
			Kind:    platform.PointerScroll,
			X:       g.x,
			Y:       g.y,
			DX:      g.x - x,
			DY:      g.y - y,
			Precise: true,
		})
	}
	g.x, g.y = x, y
	return out
}

func (g *touchGesture) anchor() {
	a := g.fingers[0]
	g.x, g.y, g.dist = a.x, a.y, 0
	if len(g.fingers) > 1 {
		b := g.fingers[1]
		g.x, g.y, g.dist = (a.x+b.x)/2, (a.y+b.y)/2, math.Hypot(a.x-b.x, a.y-b.y)
	}
}

func (g *touchGesture) find(id int32) int {
	return slices.IndexFunc(g.fingers, func(f touchFinger) bool { return f.id == id })
}

func (g *touchGesture) recordSample(x, y float64, ms uint32) {
	// Keep the last ~100 ms of touch samples for velocity estimation.
	cutoff := 0
	for cutoff < len(g.history) && ms-g.history[cutoff].ms > 100 {
		cutoff++
	}
	if cutoff > 0 {
		g.history = slices.Delete(g.history, 0, cutoff)
	}
	g.history = append(g.history, touchSample{x: x, y: y, ms: ms})
}

func (g *touchGesture) startFling(x, y float64, ms uint32) {
	if len(g.history) < 2 {
		return
	}
	last := g.history[len(g.history)-1]
	if ms-last.ms > 80 {
		return // finger paused before lifting
	}
	first := g.history[0]
	dtMs := last.ms - first.ms
	if dtMs == 0 || dtMs > 250 {
		return
	}
	dt := float64(dtMs) / 1000.0
	// Scroll direction is opposite finger motion (finger up -> positive DY).
	vx := (first.x - last.x) / dt
	vy := (first.y - last.y) / dt
	speed := math.Hypot(vx, vy)
	if speed < flingMinSpeed {
		return
	}
	if speed > flingMaxSpeed {
		scale := flingMaxSpeed / speed
		vx *= scale
		vy *= scale
	}
	g.fling = touchFling{active: true, x: x, y: y, vx: vx, vy: vy}
}

// stepFling advances active touch momentum by dt seconds and returns the
// incremental PointerScroll event, if any.
func (g *touchGesture) stepFling(dt float64) (platform.SurfaceEvent, bool) {
	if !g.fling.active || dt <= 0 {
		return platform.SurfaceEvent{}, false
	}
	decay := math.Exp(-flingDecayPerSec * dt)
	// Exact integral of v(t) = v0 * exp(-k*t) over [0, dt]: v0 * (1 - decay) / k.
	factor := (1 - decay) / flingDecayPerSec
	dx := g.fling.vx * factor
	dy := g.fling.vy * factor
	g.fling.vx *= decay
	g.fling.vy *= decay
	if math.Hypot(g.fling.vx, g.fling.vy) < flingStopSpeed || (math.Abs(dx) < 0.05 && math.Abs(dy) < 0.05) {
		g.fling = touchFling{}
		return platform.SurfaceEvent{}, false
	}
	return platform.SurfaceEvent{
		Kind:     platform.PointerScroll,
		X:        g.fling.x,
		Y:        g.fling.y,
		DX:       dx,
		DY:       dy,
		Precise:  true,
		Momentum: true,
	}, true
}
