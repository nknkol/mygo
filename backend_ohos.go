//go:build linux && arm64 && mygo_ohos

package mygo

import (
	"github.com/egoist/mygo/internal/ohos"
	"github.com/egoist/mygo/internal/platform"
)

func newBackend() platform.Backend { return ohos.New() }

// The linking hooks the Linux backend has are about widgets and web views
// of Gtk, which a surface has none of.
func linkContentRules()       {}
func linkNowPlaying()         {}
func linkNotificationImages() {}
func linkMenuIcons()          {}
func linkContextMenus()       {}
func linkDMABUFProbe()        {}
