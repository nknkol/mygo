//go:build linux && (amd64 || arm64)

package ui

import (
	"github.com/egoist/mygo/internal/gpu/gl"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/scene"
)

// newGPURenderer returns an OpenGL renderer for a surface drawing in a
// GtkGLArea's render signal or a HarmonyOS OH_NativeBuffer target. Surfaces
// draw in memory where OpenGL does not run on a GPU, which the backend tells.
func newGPURenderer(n platform.SurfaceNative) (gpuRenderer, error) {
	if n.OHOSSurface != nil {
		if _, err := n.OHOSSurface.BeginGL(0, 0); err != nil {
			return nil, err
		}
		r, err := gl.New()
		if err != nil {
			n.OHOSSurface.ReleaseGL()
			return nil, err
		}
		r.Partial = true
		return &ohosRenderer{Renderer: r, surf: n.OHOSSurface}, nil
	}
	if n.GLArea == 0 {
		return nil, nil
	}
	// The renderer reads the version of the context current.
	gl.MakeAreaCurrent(n.GLArea)
	r, err := gl.New()
	if err != nil {
		return nil, err
	}
	// GTK keeps what the area's framebuffer holds from render to render,
	// but as it allocates the area (SurfaceResize, see invalidator).
	r.Partial = true
	return &areaRenderer{Renderer: r, area: n.GLArea}, nil
}

// areaRenderer draws into a GtkGLArea with the area's context current: the
// frame, as a window a button's action creates or hides, may have left
// another current, or none, since GTK made it current (gl.MakeAreaCurrent).
type areaRenderer struct {
	*gl.Renderer
	area uintptr
}

func (r *areaRenderer) Render(s *scene.Scene) error {
	gl.MakeAreaCurrent(r.area)
	return r.Renderer.Render(s)
}

type ohosRenderer struct {
	*gl.Renderer
	surf platform.OHOSSurface
}

func (r *ohosRenderer) Render(s *scene.Scene) error {
	reallocated, err := r.surf.BeginGL(s.Width, s.Height)
	if err != nil {
		return err
	}
	if reallocated {
		r.Invalidate()
	}
	if err := r.Renderer.Render(s); err != nil {
		return err
	}
	whole, unchanged, damage := r.LastRedraw()
	return r.surf.EndGL(s.Width, s.Height, damage, whole, unchanged)
}

func (r *ohosRenderer) Release() {
	_, _ = r.surf.BeginGL(0, 0)
	r.Renderer.Release()
	r.surf.ReleaseGL()
}
