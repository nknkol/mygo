//go:build linux && (amd64 || arm64)

package ohos

import (
	"fmt"
	"image"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	eglOpenGLESAPI          = 0x30A0
	eglRenderableType       = 0x3040
	eglOpenGLES3Bit         = 0x0040
	eglRedSize              = 0x3024
	eglGreenSize            = 0x3023
	eglBlueSize             = 0x3022
	eglAlphaSize            = 0x3021
	eglContextClientVersion = 0x3098
	eglNone                 = 0x3038
	eglNativeBufferOHOS     = 0x34E1

	glTex2D               = 0x0DE1
	glTexMinFilter        = 0x2801
	glTexMagFilter        = 0x2800
	glTexWrapS            = 0x2802
	glTexWrapT            = 0x2803
	glLinearFilter        = 0x2601
	glClampEdge           = 0x812F
	glFBO                 = 0x8D40
	glColorAttach0        = 0x8CE0
	glFramebufferComplete = 0x8CD5

	nbFmtRGBA8888       = 12
	nbUsageCPURead      = 1 << 0
	nbUsageCPUWrite     = 1 << 1
	nbUsageMemDMA       = 1 << 3
	nbUsageMemMMZCache  = 1 << 5
	nbUsageHWRender     = 1 << 8
	nbUsageHWTexture    = 1 << 9
	nbUsageCPUReadOften = 1 << 16

	dmaBufIoctlSync = 0x40086200 // _IOW('b', 0, uint64)
	dmaBufSyncRead  = 1 << 0
	dmaBufSyncStart = 0 << 2
	dmaBufSyncEnd   = 1 << 2
)

type ohNativeBufferConfig struct {
	width  int32
	height int32
	format int32
	usage  int32
	stride int32
}

type ohBufferHandle struct {
	fd     int32
	width  int32
	stride int32
	height int32
	size   int32
	format int32
	usage  uint64
}

type gpuDriver struct {
	dpy uintptr
	cfg uintptr

	eglBindAPI           func(api uint32) uint32
	eglCreateContext     func(dpy, cfg, share uintptr, attribs *int32) uintptr
	eglDestroyContext    func(dpy, ctx uintptr) uint32
	eglMakeCurrent       func(dpy, draw, read, ctx uintptr) uint32
	eglGetCurrentContext func() uintptr
	eglCreateImageKHR    func(dpy, ctx uintptr, target uint32, buf uintptr, attribs *int32) uintptr
	eglDestroyImageKHR   func(dpy, img uintptr) uint32

	nbAlloc  func(cfg *ohNativeBufferConfig) uintptr
	nbMap    func(nb uintptr, addr *uintptr) int32
	nbUnmap  func(nb uintptr) int32
	nbUnref  func(nb uintptr) int32
	nwFromNB func(nb uintptr) uintptr
	nwFreeNB func(nwb uintptr)
	nwGetBH  func(nwb uintptr) uintptr

	glGenFramebuffers            func(n int32, out *uint32)
	glDeleteFramebuffers         func(n int32, fbs *uint32)
	glBindFramebuffer            func(target, fb uint32)
	glCheckFramebufferStatus     func(target uint32) uint32
	glFramebufferTexture2D       func(target, attachment, textarget, tex uint32, level int32)
	glGenTextures                func(n int32, out *uint32)
	glDeleteTextures             func(n int32, texs *uint32)
	glBindTexture                func(target, tex uint32)
	glTexParameteri              func(target, pname uint32, param int32)
	glEGLImageTargetTexture2DOES func(target uint32, img uintptr)
	glFinish                     func()
}

var (
	gpuOnce sync.Once
	gpuDrv  *gpuDriver
	gpuErr  error
)

func getGPUDriver() (*gpuDriver, error) {
	if os.Getenv("MYGO_SOFTWARE") == "1" || os.Getenv("MYGO_GPU") == "0" {
		return nil, fmt.Errorf("ohos: GPU disabled by environment")
	}
	gpuOnce.Do(func() {
		gpuDrv, gpuErr = initGPUDriver()
	})
	return gpuDrv, gpuErr
}

func gpuAvailable() bool {
	drv, err := getGPUDriver()
	return err == nil && drv != nil
}

func initGPUDriver() (*gpuDriver, error) {
	eglLib, err := purego.Dlopen("libEGL.so", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("ohos: load libEGL.so: %w", err)
	}
	glesLib, err := purego.Dlopen("libGLESv3.so", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("ohos: load libGLESv3.so: %w", err)
	}
	nbLib, err := purego.Dlopen("libnative_buffer.so", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("ohos: load libnative_buffer.so: %w", err)
	}
	nwLib, err := purego.Dlopen("libnative_window.so", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("ohos: load libnative_window.so: %w", err)
	}

	var (
		eglGetDisplay     func(native uintptr) uintptr
		eglInitialize     func(dpy uintptr, major, minor *int32) uint32
		eglChooseConfig   func(dpy uintptr, attribs *int32, configs *uintptr, size int32, num *int32) uint32
		eglGetProcAddress func(name string) uintptr
	)
	drv := &gpuDriver{}

	reg := func(lib uintptr, dst any, name string) error {
		sym, err := purego.Dlsym(lib, name)
		if err != nil || sym == 0 {
			return fmt.Errorf("ohos: missing symbol %s", name)
		}
		purego.RegisterFunc(dst, sym)
		return nil
	}
	for _, item := range []struct {
		lib  uintptr
		dst  any
		name string
	}{
		{eglLib, &eglGetDisplay, "eglGetDisplay"},
		{eglLib, &eglInitialize, "eglInitialize"},
		{eglLib, &drv.eglBindAPI, "eglBindAPI"},
		{eglLib, &eglChooseConfig, "eglChooseConfig"},
		{eglLib, &drv.eglCreateContext, "eglCreateContext"},
		{eglLib, &drv.eglDestroyContext, "eglDestroyContext"},
		{eglLib, &eglGetProcAddress, "eglGetProcAddress"},
		{eglLib, &drv.eglMakeCurrent, "eglMakeCurrent"},
		{eglLib, &drv.eglGetCurrentContext, "eglGetCurrentContext"},

		{nbLib, &drv.nbAlloc, "OH_NativeBuffer_Alloc"},
		{nbLib, &drv.nbMap, "OH_NativeBuffer_Map"},
		{nbLib, &drv.nbUnmap, "OH_NativeBuffer_Unmap"},
		{nbLib, &drv.nbUnref, "OH_NativeBuffer_Unreference"},

		{nwLib, &drv.nwFromNB, "OH_NativeWindow_CreateNativeWindowBufferFromNativeBuffer"},
		{nwLib, &drv.nwFreeNB, "OH_NativeWindow_DestroyNativeWindowBuffer"},
		{nwLib, &drv.nwGetBH, "OH_NativeWindow_GetBufferHandleFromNative"},

		{glesLib, &drv.glGenFramebuffers, "glGenFramebuffers"},
		{glesLib, &drv.glDeleteFramebuffers, "glDeleteFramebuffers"},
		{glesLib, &drv.glBindFramebuffer, "glBindFramebuffer"},
		{glesLib, &drv.glCheckFramebufferStatus, "glCheckFramebufferStatus"},
		{glesLib, &drv.glFramebufferTexture2D, "glFramebufferTexture2D"},
		{glesLib, &drv.glGenTextures, "glGenTextures"},
		{glesLib, &drv.glDeleteTextures, "glDeleteTextures"},
		{glesLib, &drv.glBindTexture, "glBindTexture"},
		{glesLib, &drv.glTexParameteri, "glTexParameteri"},
		{glesLib, &drv.glFinish, "glFinish"},
	} {
		if err := reg(item.lib, item.dst, item.name); err != nil {
			return nil, err
		}
	}

	createImgSym := eglGetProcAddress("eglCreateImageKHR")
	destroyImgSym := eglGetProcAddress("eglDestroyImageKHR")
	targetTexSym := eglGetProcAddress("glEGLImageTargetTexture2DOES")
	if createImgSym == 0 || destroyImgSym == 0 || targetTexSym == 0 {
		return nil, fmt.Errorf("ohos: missing EGL_OHOS_image_native_buffer / OES_EGL_image entry points")
	}
	purego.RegisterFunc(&drv.eglCreateImageKHR, createImgSym)
	purego.RegisterFunc(&drv.eglDestroyImageKHR, destroyImgSym)
	purego.RegisterFunc(&drv.glEGLImageTargetTexture2DOES, targetTexSym)

	dpy := eglGetDisplay(0)
	if dpy == 0 {
		return nil, fmt.Errorf("ohos: eglGetDisplay failed")
	}
	var maj, min int32
	if eglInitialize(dpy, &maj, &min) == 0 {
		return nil, fmt.Errorf("ohos: eglInitialize failed")
	}
	if drv.eglBindAPI(eglOpenGLESAPI) == 0 {
		return nil, fmt.Errorf("ohos: eglBindAPI(GLES) failed")
	}
	cfgAttribs := [...]int32{
		eglRenderableType, eglOpenGLES3Bit,
		eglRedSize, 8,
		eglGreenSize, 8,
		eglBlueSize, 8,
		eglAlphaSize, 8,
		eglNone,
	}
	var cfg uintptr
	var ncfg int32
	if eglChooseConfig(dpy, &cfgAttribs[0], &cfg, 1, &ncfg) == 0 || ncfg == 0 || cfg == 0 {
		return nil, fmt.Errorf("ohos: no EGL OpenGL ES 3 config")
	}
	// Verify surfaceless context creation works on this driver.
	ctxAttribs := [...]int32{eglContextClientVersion, 3, eglNone}
	probe := drv.eglCreateContext(dpy, cfg, 0, &ctxAttribs[0])
	if probe == 0 {
		return nil, fmt.Errorf("ohos: eglCreateContext(GLES3) failed")
	}
	drv.eglDestroyContext(dpy, probe)

	drv.dpy = dpy
	drv.cfg = cfg
	return drv, nil
}

func (d *gpuDriver) newContext() (uintptr, error) {
	if d.eglBindAPI(eglOpenGLESAPI) == 0 {
		return 0, fmt.Errorf("ohos: eglBindAPI(GLES) failed")
	}
	ctxAttribs := [...]int32{eglContextClientVersion, 3, eglNone}
	ctx := d.eglCreateContext(d.dpy, d.cfg, 0, &ctxAttribs[0])
	if ctx == 0 {
		return 0, fmt.Errorf("ohos: eglCreateContext(GLES3) failed")
	}
	return ctx, nil
}

func (d *gpuDriver) makeCurrent(ctx uintptr) error {
	if d.eglGetCurrentContext() == ctx {
		return nil
	}
	if d.eglMakeCurrent(d.dpy, 0, 0, ctx) == 0 {
		return fmt.Errorf("ohos: eglMakeCurrent failed")
	}
	return nil
}

type glTarget struct {
	w, h   int
	stride int
	fd     int
	pix    []byte
	nb     uintptr
	nwb    uintptr
	img    uintptr
	tex    uint32
	fb     uint32
}

func (d *gpuDriver) newTarget(w, h int) (*glTarget, error) {
	baseUsage := int32(nbUsageCPURead | nbUsageMemDMA | nbUsageHWRender | nbUsageHWTexture)
	var nb uintptr
	for _, usage := range []int32{
		baseUsage | nbUsageCPUReadOften | nbUsageMemMMZCache,
		baseUsage | nbUsageCPUReadOften,
		baseUsage | nbUsageCPUWrite | nbUsageMemMMZCache,
		baseUsage | nbUsageCPUWrite,
	} {
		cfg := ohNativeBufferConfig{
			width:  int32(w),
			height: int32(h),
			format: nbFmtRGBA8888,
			usage:  usage,
		}
		nb = d.nbAlloc(&cfg)
		if nb != 0 {
			break
		}
	}
	if nb == 0 {
		return nil, fmt.Errorf("ohos: OH_NativeBuffer_Alloc(%dx%d) failed", w, h)
	}

	t := &glTarget{w: w, h: h, fd: -1, nb: nb}
	var virAddr uintptr
	if rc := d.nbMap(nb, &virAddr); rc != 0 || virAddr == 0 {
		d.destroyTarget(0, t)
		return nil, fmt.Errorf("ohos: OH_NativeBuffer_Map(%dx%d) rc=%d", w, h, rc)
	}
	t.nwb = d.nwFromNB(nb)
	if t.nwb == 0 {
		d.destroyTarget(0, t)
		return nil, fmt.Errorf("ohos: CreateNativeWindowBufferFromNativeBuffer failed")
	}
	bhp := d.nwGetBH(t.nwb)
	if bhp == 0 {
		d.destroyTarget(0, t)
		return nil, fmt.Errorf("ohos: GetBufferHandleFromNative failed")
	}
	bh := (*ohBufferHandle)(*(*unsafe.Pointer)(unsafe.Pointer(&bhp)))
	if bh.stride <= 0 || bh.size < bh.stride*int32(h) {
		d.destroyTarget(0, t)
		return nil, fmt.Errorf("ohos: invalid NativeBuffer geometry stride=%d size=%d", bh.stride, bh.size)
	}
	t.stride = int(bh.stride)
	t.fd = int(bh.fd)
	t.pix = unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&virAddr))), int(bh.size))

	none := int32(eglNone)
	t.img = d.eglCreateImageKHR(d.dpy, 0, eglNativeBufferOHOS, t.nwb, &none)
	if t.img == 0 {
		d.destroyTarget(0, t)
		return nil, fmt.Errorf("ohos: eglCreateImageKHR(EGL_NATIVE_BUFFER_OHOS) failed")
	}

	d.glGenTextures(1, &t.tex)
	d.glBindTexture(glTex2D, t.tex)
	d.glTexParameteri(glTex2D, glTexMinFilter, glLinearFilter)
	d.glTexParameteri(glTex2D, glTexMagFilter, glLinearFilter)
	d.glTexParameteri(glTex2D, glTexWrapS, glClampEdge)
	d.glTexParameteri(glTex2D, glTexWrapT, glClampEdge)
	d.glEGLImageTargetTexture2DOES(glTex2D, t.img)
	d.glBindTexture(glTex2D, 0)

	d.glGenFramebuffers(1, &t.fb)
	d.glBindFramebuffer(glFBO, t.fb)
	d.glFramebufferTexture2D(glFBO, glColorAttach0, glTex2D, t.tex, 0)
	if status := d.glCheckFramebufferStatus(glFBO); status != glFramebufferComplete {
		d.glBindFramebuffer(glFBO, 0)
		d.destroyTarget(d.eglGetCurrentContext(), t)
		return nil, fmt.Errorf("ohos: incomplete OH_NativeBuffer framebuffer (%#x)", status)
	}
	return t, nil
}

func (d *gpuDriver) destroyTarget(ctx uintptr, t *glTarget) {
	if t == nil {
		return
	}
	if ctx != 0 && d.eglGetCurrentContext() == ctx {
		if t.fb != 0 {
			d.glDeleteFramebuffers(1, &t.fb)
			t.fb = 0
		}
		if t.tex != 0 {
			d.glDeleteTextures(1, &t.tex)
			t.tex = 0
		}
	}
	if t.img != 0 {
		d.eglDestroyImageKHR(d.dpy, t.img)
		t.img = 0
	}
	if t.nwb != 0 {
		d.nwFreeNB(t.nwb)
		t.nwb = 0
	}
	if t.pix != nil && t.nb != 0 {
		d.nbUnmap(t.nb)
		t.pix = nil
	}
	if t.nb != 0 {
		d.nbUnref(t.nb)
		t.nb = 0
	}
}

func dmaBufSync(fd int, flags uint64) {
	if fd < 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), dmaBufIoctlSync, uintptr(unsafe.Pointer(&flags)))
}

// BeginGL makes this surface's surfaceless OpenGL ES 3.x context current and
// binds its OH_NativeBuffer-backed framebuffer for w×h device pixels.
func (s *Surface) BeginGL(w, h int) (reallocated bool, err error) {
	drv, err := getGPUDriver()
	if err != nil {
		return false, err
	}
	if s.glCtx == 0 {
		ctx, err := drv.newContext()
		if err != nil {
			return false, err
		}
		s.glCtx = ctx
	}
	if err := drv.makeCurrent(s.glCtx); err != nil {
		return false, err
	}
	if w <= 0 || h <= 0 {
		return false, nil
	}
	if s.gl != nil && s.gl.w == w && s.gl.h == h {
		drv.glBindFramebuffer(glFBO, s.gl.fb)
		return false, nil
	}
	if s.gl != nil {
		drv.destroyTarget(s.glCtx, s.gl)
		s.gl = nil
	}
	target, err := drv.newTarget(w, h)
	if err != nil {
		return false, err
	}
	s.gl = target
	drv.glBindFramebuffer(glFBO, target.fb)
	return true, nil
}

// EndGL flushes OpenGL ES rendering into the surface's OH_NativeBuffer and
// copies only the damaged regions (flipping OpenGL's bottom-up rows into
// OHNativeWindow's top-down RGBA_8888 swapchain buffer).
func (s *Surface) EndGL(w, h int, damage []image.Rectangle, whole, unchanged bool) error {
	if unchanged {
		return nil
	}
	drv, err := getGPUDriver()
	if err != nil {
		return err
	}
	t := s.gl
	if t == nil || t.w != w || t.h != h {
		return fmt.Errorf("ohos: EndGL without matching %dx%d target", w, h)
	}
	drv.glFinish()
	dmaBufSync(t.fd, dmaBufSyncStart|dmaBufSyncRead)
	s.presentInternal(t.pix, t.stride, w, h, damage, whole, true)
	dmaBufSync(t.fd, dmaBufSyncEnd|dmaBufSyncRead)
	return nil
}

// releaseGLTarget frees only the OH_NativeBuffer FBO while keeping s.glCtx warm
// so gl.Renderer retains compiled shaders, VAOs, and glyph/image textures.
func (s *Surface) releaseGLTarget() {
	if s.gl == nil {
		return
	}
	drv, err := getGPUDriver()
	if err == nil && drv != nil {
		if s.glCtx != 0 {
			_ = drv.makeCurrent(s.glCtx)
		}
		drv.destroyTarget(s.glCtx, s.gl)
	}
	s.gl = nil
}

// ReleaseGL frees this surface's OH_NativeBuffer render target and destroys its
// surfaceless OpenGL ES context.
func (s *Surface) ReleaseGL() {
	s.releaseGLTarget()
	if s.glCtx == 0 {
		return
	}
	drv, err := getGPUDriver()
	if err == nil && drv != nil {
		if drv.eglGetCurrentContext() == s.glCtx {
			drv.eglMakeCurrent(drv.dpy, 0, 0, 0)
		}
		drv.eglDestroyContext(drv.dpy, s.glCtx)
	}
	s.glCtx = 0
}
