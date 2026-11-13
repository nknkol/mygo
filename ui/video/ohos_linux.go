//go:build linux && (amd64 || arm64) && mygo_ohos

package video

import (
	"errors"
	"fmt"

	"github.com/egoist/mygo/internal/media"
	"github.com/egoist/mygo/internal/ohosvideo"
)

func newEngine(s source) (media.Engine, error) {
	var (
		e   media.Engine
		err error
	)
	if s.data != nil {
		e, err = ohosvideo.OpenData(s.data)
	} else {
		e, err = ohosvideo.OpenURL(s.url)
	}
	if errors.Is(err, ohosvideo.ErrNoBridge) {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return e, err
}
