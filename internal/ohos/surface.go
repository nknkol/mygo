//go:build linux && (amd64 || arm64)

package ohos

import (
	"image"
	"math"
	"sync"
	"syscall"
	"unsafe"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

// maxDamageHistory bounds how many past frames of damage rects Surface keeps
// so a buffer returning from the swapchain ring can catch up on everything
// that changed since it was last presented.
const maxDamageHistory = 16

type frameDamage struct {
	seq   uint64
	w, h  int
	whole bool
	rects []image.Rectangle
}

// Surface draws into buffers the host owns and hands over one at a time.
type Surface struct {
	w *Window

	mu            sync.Mutex
	width, height int
	scale         float64
	hz            float64
	hostHidden    bool

	cur       *buffer
	curSeq    uint32
	buffers   map[uint32]*buffer
	renderSeq uint64
	history   []frameDamage
	seq       uint32
	waiting   bool
	inFrame   bool
	wantNext  bool
	frameRate uint32
	rateSent  bool

	// Input, IME and cursor state, owned by the main thread.
	focusDelivered    bool
	cursor            platform.Cursor
	cursorSent        bool
	input             platform.TextInputState
	textInput         bool
	lastWire          textInputWire
	wireSent          bool
	clientComposition platform.InputComposition
	held              platform.HeldKeys
	down              pressedKeys
	clicks            platform.ClickCounter
	touch             touchGesture
	pinch             float64
	lastX, lastY      float64
	pointerIn         bool
	layered           bool
	placed            []platform.WebViewPlacement
	glCtx             uintptr
	gl                *glTarget
	dragReq           *platform.DragRequest
	dragCanceled      bool
	dragEscaped       bool
	dropFormats       []transfer.Format
	accessEnabled     bool
}

type buffer struct {
	id       uint32
	mem      []byte
	stride   int
	w, h     int
	size     int
	lastSeq  uint64
	lastW    int
	lastH    int
	lastUsed uint64
}

func (s *Surface) Native() platform.SurfaceNative {
	if s == nil || !gpuAvailable() {
		return platform.SurfaceNative{}
	}
	return platform.SurfaceNative{OHOSSurface: s}
}

func (s *Surface) Size() (width, height, scale float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scale <= 0 {
		return float64(s.width), float64(s.height), 1
	}
	return float64(s.width) / s.scale, float64(s.height) / s.scale, s.scale
}

func (s *Surface) RefreshRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hz
}

func (s *Surface) setHostHidden(hidden bool) {
	s.mu.Lock()
	s.hostHidden = hidden
	s.mu.Unlock()
}

// RequestFrame asks the host for the next buffer of this window, at the
// display's next refresh.
func (s *Surface) RequestFrame() {
	if s.w == nil || s.w.closed {
		return
	}
	s.mu.Lock()
	if s.cur != nil {
		if s.inFrame {
			s.wantNext = true
		}
		s.mu.Unlock()
		return
	}
	if s.waiting {
		s.mu.Unlock()
		return
	}
	s.waiting = true
	s.mu.Unlock()
	if s.w.b != nil {
		s.w.b.send(kindRequest, requestMessage(s.w.id))
	}
}

func (s *Surface) PresentPixels(pix []byte, stride, width, height int) {
	s.presentInternal(pix, stride, width, height, nil, true, false)
}

func (s *Surface) PresentDamage(pix []byte, stride, width, height int, damage []image.Rectangle) {
	s.presentInternal(pix, stride, width, height, damage, false, false)
}

func (s *Surface) presentInternal(pix []byte, stride, width, height int, damage []image.Rectangle, whole, fromGPU bool) {
	s.mu.Lock()
	b := s.cur
	if b == nil {
		s.mu.Unlock()
		return
	}
	s.cur = nil

	s.renderSeq++
	rseq := s.renderSeq
	b.lastUsed = rseq

	pixBounds := image.Rect(0, 0, width, height)
	entry := frameDamage{seq: rseq, w: width, h: height, whole: whole}
	if !whole {
		clipped := make([]image.Rectangle, 0, len(damage))
		area := 0
		for _, r := range damage {
			r = r.Intersect(pixBounds)
			if r.Empty() {
				continue
			}
			clipped = append(clipped, r)
			area += r.Dx() * r.Dy()
		}
		if area*2 >= width*height && width > 0 && height > 0 {
			entry.whole = true
		} else {
			entry.rects = clipped
		}
	}
	if len(s.history) == maxDamageHistory {
		copy(s.history, s.history[1:])
		s.history[maxDamageHistory-1] = entry
	} else {
		s.history = append(s.history, entry)
	}

	paintWhole := entry.whole ||
		b.lastSeq == 0 ||
		b.lastW != width || b.lastH != height ||
		rseq-b.lastSeq > uint64(len(s.history))
	if !paintWhole {
		for i := len(s.history) - 1; i >= 0 && s.history[i].seq > b.lastSeq; i-- {
			h := s.history[i]
			if h.whole || h.w != width || h.h != height {
				paintWhole = true
				break
			}
		}
	}

	var acc []image.Rectangle
	if !paintWhole {
		accArea := 0
		for i := len(s.history) - 1; i >= 0 && s.history[i].seq > b.lastSeq; i-- {
			for _, r := range s.history[i].rects {
				covered := false
				for _, prev := range acc {
					if r.In(prev) {
						covered = true
						break
					}
				}
				if covered {
					continue
				}
				acc = append(acc, r)
				accArea += r.Dx() * r.Dy()
				if accArea*2 >= width*height && width > 0 && height > 0 {
					paintWhole = true
					break
				}
			}
			if paintWhole {
				break
			}
		}
	}

	var rects []rect
	if paintWhole {
		if fromGPU {
			copyRectGPUFlipY(b, pix, stride, width, height, pixBounds)
		} else {
			copyRectBGRAtoRGBA(b, pix, stride, width, height, pixBounds)
		}
	} else {
		rects = make([]rect, 0, len(acc))
		bufBounds := image.Rect(0, 0, min(width, b.w), min(height, b.h))
		for _, r := range acc {
			r = r.Intersect(bufBounds)
			if r.Empty() {
				continue
			}
			if fromGPU {
				copyRectGPUFlipY(b, pix, stride, width, height, r)
			} else {
				copyRectBGRAtoRGBA(b, pix, stride, width, height, r)
			}
			rects = append(rects, rect{int32(r.Min.X), int32(r.Min.Y), uint32(r.Dx()), uint32(r.Dy())})
		}
	}
	b.lastSeq = rseq
	b.lastW = width
	b.lastH = height
	seq := s.seq
	s.seq++
	s.mu.Unlock()

	if s.w != nil && s.w.b != nil {
		s.w.b.send(kindPresent, presentMessage(s.w.id, seq, rects))
	}
}

// copyRectBGRAtoRGBA copies top-down premultiplied BGRA pixels from raster.Image
// into OHNativeWindow's top-down premultiplied RGBA_8888 buffer.
func copyRectBGRAtoRGBA(b *buffer, pix []byte, stride, width, height int, r image.Rectangle) {
	r = r.Intersect(image.Rect(0, 0, min(width, b.w), min(height, b.h)))
	if r.Empty() {
		return
	}
	n := r.Dx() * 4
	if r.Min.X == 0 && n == stride && stride == b.stride {
		src := r.Min.Y * stride
		end := r.Max.Y * stride
		if end <= len(pix) && end <= len(b.mem) {
			swizzleBGRAtoRGBA(b.mem[src:end], pix[src:end])
			return
		}
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		src := y*stride + r.Min.X*4
		dst := y*b.stride + r.Min.X*4
		if src+n > len(pix) || dst+n > len(b.mem) {
			return
		}
		swizzleBGRAtoRGBA(b.mem[dst:dst+n], pix[src:src+n])
	}
}

func swizzleBGRAtoRGBA(dst, src []byte) {
	n := min(len(dst), len(src)) &^ 3
	if n == 0 {
		return
	}
	dp := unsafe.Pointer(&dst[0])
	sp := unsafe.Pointer(&src[0])
	i := 0
	for ; i+8 <= n; i += 8 {
		v := *(*uint64)(unsafe.Add(sp, i))
		*(*uint64)(unsafe.Add(dp, i)) = (v & 0xff00ff00ff00ff00) |
			((v & 0x00ff000000ff0000) >> 16) |
			((v & 0x000000ff000000ff) << 16)
	}
	for ; i+4 <= n; i += 4 {
		b0, g0, r0, a0 := src[i], src[i+1], src[i+2], src[i+3]
		dst[i], dst[i+1], dst[i+2], dst[i+3] = r0, g0, b0, a0
	}
}

// copyRectGPUFlipY copies OpenGL ES's bottom-up RGBA_8888 framebuffer rows into
// OHNativeWindow's top-down RGBA_8888 buffer for top-down damage rect r.
func copyRectGPUFlipY(b *buffer, pix []byte, stride, width, height int, r image.Rectangle) {
	r = r.Intersect(image.Rect(0, 0, min(width, b.w), min(height, b.h)))
	if r.Empty() {
		return
	}
	n := r.Dx() * 4
	xOff := r.Min.X * 4
	for y := r.Min.Y; y < r.Max.Y; y++ {
		srcY := height - 1 - y
		src := srcY*stride + xOff
		dst := y*b.stride + xOff
		if src < 0 || src+n > len(pix) || dst+n > len(b.mem) {
			return
		}
		copy(b.mem[dst:dst+n], pix[src:src+n])
	}
}

func (s *Surface) SetCursor(c platform.Cursor) {
	if s.cursorSent && s.cursor == c {
		return
	}
	s.cursor = c
	s.cursorSent = true
	if s.w != nil && !s.w.closed && s.w.b != nil {
		s.w.b.send(kindCursor, cursorMessage(s.w.id, c))
	}
}

func (s *Surface) SetTextInput(t platform.TextInputState) {
	if s.textInput && (!t.Active || s.input.Client != t.Client || t.NoInputMethod) {
		s.clientComposition.Reset()
	}
	s.input = t
	s.textInput = t.Active

	var winID uint32
	if s.w != nil {
		winID = s.w.id
	}
	wire := textInputWire{winID: winID}
	if t.Active {
		caret := t.Caret
		var text string
		var selStart, selEnd int32
		if t.Client != nil {
			ctx := platform.ClientTextContext(t.Client)
			text = ctx.Text
			selStart = int32(ctx.Start)
			selEnd = int32(ctx.End)
			if caret == (platform.RectF{}) {
				if b, _, ok := t.Client.BoundsForRange(t.Client.Selection().Range); ok {
					caret = b
				}
			}
		} else {
			runes := []rune(t.Text)
			a := max(0, min(t.Start, len(runes)))
			z := max(a, min(t.End, len(runes)))
			text = t.Text
			selStart = int32(platform.UTF16Len(string(runes[:a])))
			selEnd = int32(platform.UTF16Len(string(runes[:z])))
		}
		wire = textInputWire{
			winID:         winID,
			active:        true,
			noInputMethod: t.NoInputMethod,
			caretX:        float32(caret.X),
			caretY:        float32(caret.Y),
			caretW:        float32(max(caret.W, 1)),
			caretH:        float32(max(caret.H, 1)),
			selStart:      selStart,
			selEnd:        selEnd,
			text:          text,
		}
	}
	if s.wireSent && wire == s.lastWire {
		return
	}
	s.lastWire = wire
	s.wireSent = true
	if s.w != nil && !s.w.closed && s.w.b != nil {
		s.w.b.send(kindTextInput, textInputMessage(wire))
	}
}

func (s *Surface) FrameNow() { s.RequestFrame() }

func (s *Surface) SetFrameRate(fps float64) {
	var rate uint32
	if fps > 0 {
		rate = max(1, uint32(math.Round(fps)))
	}
	s.mu.Lock()
	if s.rateSent && s.frameRate == rate {
		s.mu.Unlock()
		return
	}
	s.frameRate = rate
	s.rateSent = true
	s.mu.Unlock()
	if s.w != nil && !s.w.closed && s.w.b != nil {
		s.w.b.send(kindFrameRate, frameRateMessage(s.w.id, rate))
	}
}

// Idle releases cached swapchain mappings, GPU target buffers and damage
// history once package ui has dropped its backing image after frameIdle or when
// the window closes.
func (s *Surface) Idle() {
	s.releaseGLTarget()
	s.mu.Lock()
	s.history = nil
	for id, b := range s.buffers {
		if b == s.cur {
			continue
		}
		syscall.Munmap(b.mem)
		delete(s.buffers, id)
	}
	s.mu.Unlock()
}

// Occluded reports whether nothing of the surface shows: while the host
// surface is hidden or the window is minimized/closed, drawing is skipped.
func (s *Surface) Occluded() bool {
	s.mu.Lock()
	hidden := s.hostHidden
	s.mu.Unlock()
	return s.w == nil || s.w.closed || s.w.minimized || hidden
}
