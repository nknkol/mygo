//go:build linux && (amd64 || arm64)

package ohos

import (
	"fmt"
	"image"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

const (
	eglOpenGLESAPI              = 0x30A0
	eglRenderableType           = 0x3040
	eglOpenGLES3Bit             = 0x0040
	eglRedSize                  = 0x3024
	eglGreenSize                = 0x3023
	eglBlueSize                 = 0x3022
	eglAlphaSize                = 0x3021
	eglContextClientVersion     = 0x3098
	eglNone                     = 0x3038
	eglNativeBufferOHOS         = 0x34E1
	eglSyncNativeFenceANDROID   = 0x3144
	eglSyncNativeFenceFDANDROID = 0x3145
	eglNoSyncKHR                = 0

	glScissorTest         = 0x0C11
	glTex2D               = 0x0DE1
	glRGBA                = 0x1908
	glUnsignedByte        = 0x1401
	glNearestFilter       = 0x2600
	glLinearFilter        = 0x2601
	glTexMagFilter        = 0x2800
	glTexMinFilter        = 0x2801
	glTexWrapS            = 0x2802
	glTexWrapT            = 0x2803
	glColorBufferBit      = 0x00004000
	glRGBA8               = 0x8058
	glClampEdge           = 0x812F
	glReadFBO             = 0x8CA8
	glDrawFBO             = 0x8CA9
	glFramebufferComplete = 0x8CD5
	glColorAttach0        = 0x8CE0
	glFBO                 = 0x8D40

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
	dpy            uintptr
	cfg            uintptr
	hasNativeFence bool
	hasParcel      bool

	eglBindAPI                  func(api uint32) uint32
	eglCreateContext            func(dpy, cfg, share uintptr, attribs *int32) uintptr
	eglDestroyContext           func(dpy, ctx uintptr) uint32
	eglMakeCurrent              func(dpy, draw, read, ctx uintptr) uint32
	eglGetCurrentContext        func() uintptr
	eglCreateImageKHR           func(dpy, ctx uintptr, target uint32, buf uintptr, attribs *int32) uintptr
	eglDestroyImageKHR          func(dpy, img uintptr) uint32
	eglCreateSyncKHR            func(dpy uintptr, typ uint32, attribs *int32) uintptr
	eglDestroySyncKHR           func(dpy, sync uintptr) uint32
	eglWaitSyncKHR              func(dpy, sync uintptr, flags int32) int32
	eglDupNativeFenceFDANDROID  func(dpy, sync uintptr) int32

	nbAlloc          func(cfg *ohNativeBufferConfig) uintptr
	nbMap            func(nb uintptr, addr *uintptr) int32
	nbUnmap          func(nb uintptr) int32
	nbUnref          func(nb uintptr) int32
	nbWriteToParcel  func(nb, parcel uintptr) int32
	nbReadFromParcel func(parcel uintptr, outNB *uintptr) int32
	nwFromNB         func(nb uintptr) uintptr
	nwFreeNB         func(nwb uintptr)
	nwGetBH          func(nwb uintptr) uintptr

	parcelCreate      func() uintptr
	parcelDestroy     func(p uintptr)
	parcelGetDataSize func(p uintptr) int32
	parcelGetReadPos  func(p uintptr) int32
	parcelRewindPos   func(p uintptr, pos uint32) int32
	parcelReadInt32   func(p uintptr, v *int32) int32
	parcelWriteInt32  func(p uintptr, v int32) int32
	parcelReadFD      func(p uintptr, fd *int32) int32
	parcelWriteFD     func(p uintptr, fd int32) int32

	glGenFramebuffers            func(n int32, out *uint32)
	glDeleteFramebuffers         func(n int32, fbs *uint32)
	glBindFramebuffer            func(target, fb uint32)
	glCheckFramebufferStatus     func(target uint32) uint32
	glFramebufferTexture2D       func(target, attachment, textarget, tex uint32, level int32)
	glBlitFramebuffer            func(srcX0, srcY0, srcX1, srcY1, dstX0, dstY0, dstX1, dstY1 int32, mask, filter uint32)
	glGenTextures                func(n int32, out *uint32)
	glDeleteTextures             func(n int32, texs *uint32)
	glBindTexture                func(target, tex uint32)
	glTexImage2D                 func(target uint32, level, internalFormat, width, height, border int32, format, typ uint32, pixels uintptr)
	glTexParameteri              func(target, pname uint32, param int32)
	glEGLImageTargetTexture2DOES func(target uint32, img uintptr)
	glDisable                    func(cap uint32)
	glFlush                      func()
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
		{glesLib, &drv.glBlitFramebuffer, "glBlitFramebuffer"},
		{glesLib, &drv.glGenTextures, "glGenTextures"},
		{glesLib, &drv.glDeleteTextures, "glDeleteTextures"},
		{glesLib, &drv.glBindTexture, "glBindTexture"},
		{glesLib, &drv.glTexImage2D, "glTexImage2D"},
		{glesLib, &drv.glTexParameteri, "glTexParameteri"},
		{glesLib, &drv.glDisable, "glDisable"},
		{glesLib, &drv.glFlush, "glFlush"},
		{glesLib, &drv.glFinish, "glFinish"},
	} {
		if err := reg(item.lib, item.dst, item.name); err != nil {
			return nil, err
		}
	}

	if ipcLib, err := purego.Dlopen("libipc_capi.so", purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil && ipcLib != 0 {
		parcelOK := true
		for _, item := range []struct {
			lib  uintptr
			dst  any
			name string
		}{
			{nbLib, &drv.nbWriteToParcel, "OH_NativeBuffer_WriteToParcel"},
			{nbLib, &drv.nbReadFromParcel, "OH_NativeBuffer_ReadFromParcel"},
			{ipcLib, &drv.parcelCreate, "OH_IPCParcel_Create"},
			{ipcLib, &drv.parcelDestroy, "OH_IPCParcel_Destroy"},
			{ipcLib, &drv.parcelGetDataSize, "OH_IPCParcel_GetDataSize"},
			{ipcLib, &drv.parcelGetReadPos, "OH_IPCParcel_GetReadPosition"},
			{ipcLib, &drv.parcelRewindPos, "OH_IPCParcel_RewindReadPosition"},
			{ipcLib, &drv.parcelReadInt32, "OH_IPCParcel_ReadInt32"},
			{ipcLib, &drv.parcelWriteInt32, "OH_IPCParcel_WriteInt32"},
			{ipcLib, &drv.parcelReadFD, "OH_IPCParcel_ReadFileDescriptor"},
			{ipcLib, &drv.parcelWriteFD, "OH_IPCParcel_WriteFileDescriptor"},
		} {
			if reg(item.lib, item.dst, item.name) != nil {
				parcelOK = false
				break
			}
		}
		drv.hasParcel = parcelOK
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

	createSyncSym := eglGetProcAddress("eglCreateSyncKHR")
	destroySyncSym := eglGetProcAddress("eglDestroySyncKHR")
	waitSyncSym := eglGetProcAddress("eglWaitSyncKHR")
	dupFenceSym := eglGetProcAddress("eglDupNativeFenceFDANDROID")
	if createSyncSym != 0 && destroySyncSym != 0 && waitSyncSym != 0 && dupFenceSym != 0 {
		purego.RegisterFunc(&drv.eglCreateSyncKHR, createSyncSym)
		purego.RegisterFunc(&drv.eglDestroySyncKHR, destroySyncSym)
		purego.RegisterFunc(&drv.eglWaitSyncKHR, waitSyncSym)
		purego.RegisterFunc(&drv.eglDupNativeFenceFDANDROID, dupFenceSym)
		drv.hasNativeFence = true
	}

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
	w, h     int
	pureVRAM bool
	stride   int
	fd       int
	pix      []byte
	nb       uintptr
	nwb      uintptr
	img      uintptr
	tex      uint32
	fb       uint32
}

type gpuSwapTarget struct {
	id       uint32
	w, h     int
	nb       uintptr
	nwb      uintptr
	img      uintptr
	tex      uint32
	fb       uint32
	lastUsed uint64
}

// newTextureTarget allocates a pure GPU VRAM GL_RGBA8 framebuffer so the OpenGL
// ES driver can keep internal tiled/AFBC compression without CPU-readback stride
// or snooping constraints.
func (d *gpuDriver) newTextureTarget(w, h int) (*glTarget, error) {
	t := &glTarget{w: w, h: h, pureVRAM: true, fd: -1}
	d.glGenTextures(1, &t.tex)
	d.glBindTexture(glTex2D, t.tex)
	d.glTexParameteri(glTex2D, glTexMinFilter, glNearestFilter)
	d.glTexParameteri(glTex2D, glTexMagFilter, glNearestFilter)
	d.glTexParameteri(glTex2D, glTexWrapS, glClampEdge)
	d.glTexParameteri(glTex2D, glTexWrapT, glClampEdge)
	d.glTexImage2D(glTex2D, 0, glRGBA8, int32(w), int32(h), 0, glRGBA, glUnsignedByte, 0)
	d.glBindTexture(glTex2D, 0)

	d.glGenFramebuffers(1, &t.fb)
	d.glBindFramebuffer(glFBO, t.fb)
	d.glFramebufferTexture2D(glFBO, glColorAttach0, glTex2D, t.tex, 0)
	if status := d.glCheckFramebufferStatus(glFBO); status != glFramebufferComplete {
		d.glBindFramebuffer(glFBO, 0)
		d.destroyTarget(d.eglGetCurrentContext(), t)
		return nil, fmt.Errorf("ohos: incomplete VRAM framebuffer (%#x)", status)
	}
	return t, nil
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

func (d *gpuDriver) bindSwapTarget(st *gpuSwapTarget) error {
	if st == nil || st.nb == 0 {
		return fmt.Errorf("ohos: nil swap target")
	}
	if st.fb != 0 {
		return nil
	}
	st.nwb = d.nwFromNB(st.nb)
	if st.nwb == 0 {
		return fmt.Errorf("ohos: CreateNativeWindowBufferFromNativeBuffer failed")
	}
	none := int32(eglNone)
	st.img = d.eglCreateImageKHR(d.dpy, 0, eglNativeBufferOHOS, st.nwb, &none)
	if st.img == 0 {
		d.nwFreeNB(st.nwb)
		st.nwb = 0
		return fmt.Errorf("ohos: eglCreateImageKHR(swap) failed")
	}
	d.glGenTextures(1, &st.tex)
	d.glBindTexture(glTex2D, st.tex)
	d.glTexParameteri(glTex2D, glTexMinFilter, glNearestFilter)
	d.glTexParameteri(glTex2D, glTexMagFilter, glNearestFilter)
	d.glTexParameteri(glTex2D, glTexWrapS, glClampEdge)
	d.glTexParameteri(glTex2D, glTexWrapT, glClampEdge)
	d.glEGLImageTargetTexture2DOES(glTex2D, st.img)
	d.glBindTexture(glTex2D, 0)

	d.glGenFramebuffers(1, &st.fb)
	d.glBindFramebuffer(glFBO, st.fb)
	d.glFramebufferTexture2D(glFBO, glColorAttach0, glTex2D, st.tex, 0)
	if status := d.glCheckFramebufferStatus(glFBO); status != glFramebufferComplete {
		d.glBindFramebuffer(glFBO, 0)
		d.unbindSwapTargetGL(d.eglGetCurrentContext(), st, false)
		return fmt.Errorf("ohos: incomplete swap framebuffer (%#x)", status)
	}
	return nil
}

func (d *gpuDriver) unbindSwapTargetGL(ctx uintptr, st *gpuSwapTarget, dropNB bool) {
	if st == nil {
		return
	}
	if ctx != 0 && d.eglGetCurrentContext() == ctx {
		if st.fb != 0 {
			d.glDeleteFramebuffers(1, &st.fb)
			st.fb = 0
		}
		if st.tex != 0 {
			d.glDeleteTextures(1, &st.tex)
			st.tex = 0
		}
	}
	if st.img != 0 {
		d.eglDestroyImageKHR(d.dpy, st.img)
		st.img = 0
	}
	if st.nwb != 0 {
		d.nwFreeNB(st.nwb)
		st.nwb = 0
	}
	if dropNB && st.nb != 0 {
		d.nbUnref(st.nb)
		st.nb = 0
	}
}

func (d *gpuDriver) serializeNativeBuffer(nb uintptr) (isFD []bool, ints []int32, fds []int, err error) {
	if !d.hasParcel || nb == 0 {
		return nil, nil, nil, fmt.Errorf("ohos: IPC parcel serialization unavailable")
	}
	p := d.parcelCreate()
	if p == 0 {
		return nil, nil, nil, fmt.Errorf("ohos: OH_IPCParcel_Create failed")
	}
	defer d.parcelDestroy(p)
	if rc := d.nbWriteToParcel(nb, p); rc != 0 {
		return nil, nil, nil, fmt.Errorf("ohos: OH_NativeBuffer_WriteToParcel rc=%d", rc)
	}
	dataSize := d.parcelGetDataSize(p)
	if dataSize <= 0 || dataSize > 4096 {
		return nil, nil, nil, fmt.Errorf("ohos: unexpected parcel size %d", dataSize)
	}
	for d.parcelGetReadPos(p) < dataSize {
		pos := d.parcelGetReadPos(p)
		var v int32
		if d.parcelReadInt32(p, &v) == 0 {
			isFD = append(isFD, false)
			ints = append(ints, v)
			continue
		}
		d.parcelRewindPos(p, uint32(pos))
		var fd int32 = -1
		if d.parcelReadFD(p, &fd) == 0 && fd >= 0 {
			isFD = append(isFD, true)
			fds = append(fds, int(fd))
			continue
		}
		closeFDs(fds)
		return nil, nil, nil, fmt.Errorf("ohos: failed reading parcel token at offset %d/%d", pos, dataSize)
	}
	return isFD, ints, fds, nil
}

func (d *gpuDriver) deserializeNativeBuffer(isFD []bool, ints []int32, fds []int) (uintptr, error) {
	if !d.hasParcel {
		return 0, fmt.Errorf("ohos: IPC parcel deserialization unavailable")
	}
	p := d.parcelCreate()
	if p == 0 {
		return 0, fmt.Errorf("ohos: OH_IPCParcel_Create failed")
	}
	defer d.parcelDestroy(p)
	intIdx, fdIdx := 0, 0
	for _, fdTok := range isFD {
		if fdTok {
			if fdIdx >= len(fds) || fds[fdIdx] < 0 {
				return 0, fmt.Errorf("ohos: missing parcel fd %d", fdIdx)
			}
			if rc := d.parcelWriteFD(p, int32(fds[fdIdx])); rc != 0 {
				return 0, fmt.Errorf("ohos: OH_IPCParcel_WriteFileDescriptor rc=%d", rc)
			}
			fdIdx++
		} else {
			if intIdx >= len(ints) {
				return 0, fmt.Errorf("ohos: missing parcel int %d", intIdx)
			}
			if rc := d.parcelWriteInt32(p, ints[intIdx]); rc != 0 {
				return 0, fmt.Errorf("ohos: OH_IPCParcel_WriteInt32 rc=%d", rc)
			}
			intIdx++
		}
	}
	var nb uintptr
	if rc := d.nbReadFromParcel(p, &nb); rc != 0 || nb == 0 {
		return 0, fmt.Errorf("ohos: OH_NativeBuffer_ReadFromParcel rc=%d", rc)
	}
	return nb, nil
}

func pollAndCloseFD(fd int) {
	if fd < 0 {
		return
	}
	pfd := struct {
		fd      int32
		events  int16
		revents int16
	}{fd: int32(fd), events: 1} // POLLIN = 1
	ts := syscall.Timespec{Sec: 3, Nsec: 0}
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&pfd)), 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
		if e != syscall.EINTR && e != syscall.EAGAIN {
			break
		}
	}
	_ = syscall.Close(fd)
}

func dmaBufSync(fd int, flags uint64) {
	if fd < 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), dmaBufIoctlSync, uintptr(unsafe.Pointer(&flags)))
}

// BeginGL makes this surface's surfaceless OpenGL ES 3.x context current and
// binds its persistent scene framebuffer for w×h device pixels.
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
	s.mu.Lock()
	wantPureVRAM := s.cur != nil && s.cur.gpuSwap
	s.mu.Unlock()

	if s.gl != nil && s.gl.w == w && s.gl.h == h && s.gl.pureVRAM == wantPureVRAM {
		drv.glBindFramebuffer(glFBO, s.gl.fb)
		return false, nil
	}
	if s.gl != nil {
		drv.destroyTarget(s.glCtx, s.gl)
		s.gl = nil
	}
	var target *glTarget
	if wantPureVRAM {
		target, err = drv.newTextureTarget(w, h)
	} else {
		target, err = drv.newTarget(w, h)
	}
	if err != nil {
		return false, err
	}
	s.gl = target
	drv.glBindFramebuffer(glFBO, target.fb)
	return true, nil
}

// EndGL presents OpenGL ES rendering either via a zero-copy GPU hardware Y-flip
// blit + native fence into the imported OHNativeWindow swapchain buffer, or via
// the CPU-mapped fallback when running against software/memfd swapchain buffers.
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
	t0 := time.Now()
	var uiGLUs uint32
	if !s.frameStart.IsZero() {
		uiGLUs = uint32(max(0, t0.Sub(s.frameStart).Microseconds()))
	}
	s.lastUIGLUs = uiGLUs

	s.mu.Lock()
	isZeroCopy := s.cur != nil && s.cur.gpuSwap
	s.mu.Unlock()
	if !isZeroCopy {
		drv.glFinish()
		t1 := time.Now()
		s.lastGPUFinishUs = uint32(max(0, t1.Sub(t0).Microseconds()))
		s.dmaSyncStart = t1
		dmaBufSync(t.fd, dmaBufSyncStart|dmaBufSyncRead)
		s.presentInternal(t.pix, t.stride, w, h, damage, whole, true, t.fd)
		return nil
	}

	s.mu.Lock()
	b := s.cur
	if b == nil || !b.gpuSwap {
		s.mu.Unlock()
		return nil
	}
	st := s.gpuTargets[b.id]
	if st == nil || st.nb == 0 || st.w != w || st.h != h {
		s.mu.Unlock()
		return fmt.Errorf("ohos: missing imported GPU swapchain buffer id=%d (%dx%d)", b.id, w, h)
	}
	s.cur = nil
	acquireFD := b.acquireFD
	b.acquireFD = -1
	paintWhole, acc, rects := s.accumulateDamageLocked(b, w, h, damage, whole)
	st.lastUsed = b.lastUsed
	curSeq := s.curSeq
	seq := s.seq
	s.seq++
	s.mu.Unlock()

	if err := drv.bindSwapTarget(st); err != nil {
		pollAndCloseFD(acquireFD)
		if s.w != nil && s.w.b != nil {
			s.w.b.send(kindAbort, abortMessage(s.w.id, curSeq))
		}
		return err
	}

	if acquireFD >= 0 {
		if drv.hasNativeFence {
			attribs := [...]int32{eglSyncNativeFenceFDANDROID, int32(acquireFD), eglNone}
			sync := drv.eglCreateSyncKHR(drv.dpy, eglSyncNativeFenceANDROID, &attribs[0])
			if sync != eglNoSyncKHR {
				drv.eglWaitSyncKHR(drv.dpy, sync, 0)
				drv.eglDestroySyncKHR(drv.dpy, sync)
			} else {
				pollAndCloseFD(acquireFD)
			}
		} else {
			pollAndCloseFD(acquireFD)
		}
	}

	drv.glDisable(glScissorTest)
	drv.glBindFramebuffer(glReadFBO, t.fb)
	drv.glBindFramebuffer(glDrawFBO, st.fb)
	if paintWhole {
		drv.glBlitFramebuffer(0, int32(h), int32(w), 0, 0, 0, int32(w), int32(h), glColorBufferBit, glNearestFilter)
	} else {
		for _, r := range acc {
			drv.glBlitFramebuffer(
				int32(r.Min.X), int32(h-r.Min.Y), int32(r.Max.X), int32(h-r.Max.Y),
				int32(r.Min.X), int32(r.Min.Y), int32(r.Max.X), int32(r.Max.Y),
				glColorBufferBit, glNearestFilter,
			)
		}
	}
	drv.glBindFramebuffer(glFBO, t.fb)

	releaseFD := -1
	var gpuFinishUs uint32
	if drv.hasNativeFence {
		none := int32(eglNone)
		sync := drv.eglCreateSyncKHR(drv.dpy, eglSyncNativeFenceANDROID, &none)
		if sync != eglNoSyncKHR {
			drv.glFlush()
			releaseFD = int(drv.eglDupNativeFenceFDANDROID(drv.dpy, sync))
			drv.eglDestroySyncKHR(drv.dpy, sync)
		} else {
			f0 := time.Now()
			drv.glFinish()
			gpuFinishUs = uint32(max(0, time.Since(f0).Microseconds()))
		}
	} else {
		f0 := time.Now()
		drv.glFinish()
		gpuFinishUs = uint32(max(0, time.Since(f0).Microseconds()))
	}
	blitSubmitUs := uint32(max(0, time.Since(t0).Microseconds()))
	if blitSubmitUs > gpuFinishUs {
		blitSubmitUs -= gpuFinishUs
	} else {
		blitSubmitUs = 0
	}

	coal := min(255, s.coalescedScrolls.Swap(0))
	scEv := min(255, s.scrollEvents.Swap(0))
	meta := presentPerfFromGPU | presentPerfZeroCopy
	if paintWhole {
		meta |= presentPerfWholeCopy
	}
	meta |= (coal & 0xFF) << 8
	meta |= (scEv & 0xFF) << 16

	if s.w != nil && s.w.b != nil {
		s.w.b.sendWithFD(kindPresent, presentMessageWithPerf(s.w.id, seq, rects, presentPerf{
			uiGLUs:      uiGLUs + blitSubmitUs,
			gpuFinishUs: gpuFinishUs,
			copyUs:      0,
			meta:        meta,
		}), releaseFD)
	}
	if releaseFD >= 0 {
		_ = syscall.Close(releaseFD)
	}
	return nil
}

// releaseGLTarget frees the scene FBO and unbinds swapchain GL wrappers while
// keeping s.glCtx and imported OH_NativeBuffer references warm across Idle.
func (s *Surface) releaseGLTarget() {
	drv, err := getGPUDriver()
	if err == nil && drv != nil && s.glCtx != 0 {
		_ = drv.makeCurrent(s.glCtx)
	}
	if s.gl != nil {
		if drv != nil {
			drv.destroyTarget(s.glCtx, s.gl)
		}
		s.gl = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if drv != nil {
		for _, st := range s.gpuTargets {
			drv.unbindSwapTargetGL(s.glCtx, st, false)
		}
	}
}

func (s *Surface) evictAllGPUTargets() {
	s.mu.Lock()
	if len(s.gpuTargets) == 0 {
		s.mu.Unlock()
		return
	}
	old := s.gpuTargets
	s.gpuTargets = nil
	for id, b := range s.buffers {
		if b.gpuSwap && b != s.cur {
			if b.acquireFD >= 0 {
				_ = syscall.Close(b.acquireFD)
				b.acquireFD = -1
			}
			delete(s.buffers, id)
		}
	}
	s.mu.Unlock()

	drv, _ := getGPUDriver()
	if drv != nil {
		if s.glCtx != 0 {
			_ = drv.makeCurrent(s.glCtx)
		}
		for _, st := range old {
			drv.unbindSwapTargetGL(s.glCtx, st, true)
		}
	}
}

// ReleaseGL frees this surface's render targets, imported swapchain buffers,
// and surfaceless OpenGL ES context.
func (s *Surface) ReleaseGL() {
	s.releaseGLTarget()
	s.evictAllGPUTargets()
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
