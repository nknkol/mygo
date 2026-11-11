//go:build linux && (amd64 || arm64) && mygo_ohos

package audio

import (
	"time"

	"github.com/egoist/mygo/internal/ohosaudio"
)

// HarmonyOS plays through an OH_AudioRenderer owned by the Stage-model Ability
// process and fed over a shared-memory SPSC ringbuffer.

type ohosDevice struct{ s *ohosaudio.Stream }

func openDevice(ahead time.Duration) (device, int, error) {
	s, frames, err := ohosaudio.Open(sampleRate, channels, ahead)
	if err != nil {
		return nil, 0, err
	}
	return &ohosDevice{s: s}, frames, nil
}

func (d *ohosDevice) write(samples []float32) error { return d.s.Write(samples) }
func (d *ohosDevice) close()                        { d.s.Close() }
