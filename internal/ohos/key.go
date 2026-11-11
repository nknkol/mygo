//go:build linux && (amd64 || arm64)

package ohos

import (
	"github.com/egoist/mygo/internal/platform"
)

// ArkUI_KeyCode values from <arkui/native_key_event.h>.
const (
	arkKeyBack         = 2
	arkKey0            = 2000
	arkKey9            = 2009
	arkKeyDpadUp       = 2012
	arkKeyDpadDown     = 2013
	arkKeyDpadLeft     = 2014
	arkKeyDpadRight    = 2015
	arkKeyA            = 2017
	arkKeyZ            = 2042
	arkKeyComma        = 2043
	arkKeyPeriod       = 2044
	arkKeyAltLeft      = 2045
	arkKeyAltRight     = 2046
	arkKeyShiftLeft    = 2047
	arkKeyShiftRight   = 2048
	arkKeyTab          = 2049
	arkKeySpace        = 2050
	arkKeyEnter        = 2054
	arkKeyDel          = 2055 // Backspace
	arkKeyGrave        = 2056
	arkKeyMinus        = 2057
	arkKeyEquals       = 2058
	arkKeyLeftBracket  = 2059
	arkKeyRightBracket = 2060
	arkKeyBackslash    = 2061
	arkKeySemicolon    = 2062
	arkKeyApostrophe   = 2063
	arkKeySlash        = 2064
	arkKeyMenu         = 2067
	arkKeyPageUp       = 2068
	arkKeyPageDown     = 2069
	arkKeyEscape       = 2070
	arkKeyForwardDel   = 2071 // Delete
	arkKeyCtrlLeft     = 2072
	arkKeyCtrlRight    = 2073
	arkKeyCapsLock     = 2074
	arkKeyScrollLock   = 2075
	arkKeyMetaLeft     = 2076
	arkKeyMetaRight    = 2077
	arkKeySysrq        = 2079
	arkKeyBreak        = 2080
	arkKeyMoveHome     = 2081
	arkKeyMoveEnd      = 2082
	arkKeyInsert       = 2083
	arkKeyForward      = 2084
	arkKeyF1           = 2090
	arkKeyF12          = 2101
	arkKeyNumLock      = 2102
	arkKeyNumpad0      = 2103
	arkKeyNumpad9      = 2112
	arkKeyNumpadDivide = 2113
	arkKeyNumpadMul    = 2114
	arkKeyNumpadSub    = 2115
	arkKeyNumpadAdd    = 2116
	arkKeyNumpadDot    = 2117
	arkKeyNumpadComma  = 2118
	arkKeyNumpadEnter  = 2119
	arkKeyNumpadEquals = 2120
	arkKeyVolumeUp     = 16
	arkKeyVolumeDown   = 17
	arkKeyVolumeMute   = 22
)

// pressedKeys tracks ArkUI keycodes currently down so auto-repeats and
// left/right modifier pairs behave identically to Linux's surface.down.
type pressedKeys struct {
	down map[int32]bool
}

func (p *pressedKeys) press(code int32) (repeat bool) {
	if p.down == nil {
		p.down = make(map[int32]bool)
	}
	repeat = p.down[code]
	p.down[code] = true
	return repeat
}

func (p *pressedKeys) release(code int32) {
	delete(p.down, code)
}

func (p *pressedKeys) clear() {
	clear(p.down)
}

func (p *pressedKeys) otherSideHeld(code int32) bool {
	var other int32
	switch code {
	case arkKeyShiftLeft:
		other = arkKeyShiftRight
	case arkKeyShiftRight:
		other = arkKeyShiftLeft
	case arkKeyCtrlLeft:
		other = arkKeyCtrlRight
	case arkKeyCtrlRight:
		other = arkKeyCtrlLeft
	case arkKeyAltLeft:
		other = arkKeyAltRight
	case arkKeyAltRight:
		other = arkKeyAltLeft
	case arkKeyMetaLeft:
		other = arkKeyMetaRight
	case arkKeyMetaRight:
		other = arkKeyMetaLeft
	default:
		return false
	}
	return p.down[other]
}

func (p *pressedKeys) heldMods() platform.Modifiers {
	var m platform.Modifiers
	if p.down[arkKeyShiftLeft] || p.down[arkKeyShiftRight] {
		m |= platform.ModShift
	}
	if p.down[arkKeyCtrlLeft] || p.down[arkKeyCtrlRight] {
		m |= platform.ModCtrl
	}
	if p.down[arkKeyAltLeft] || p.down[arkKeyAltRight] {
		m |= platform.ModAlt
	}
	if p.down[arkKeyMetaLeft] || p.down[arkKeyMetaRight] {
		m |= platform.ModSuper
	}
	return m
}

func modifierForArkKey(code int32) platform.Modifiers {
	switch code {
	case arkKeyShiftLeft, arkKeyShiftRight:
		return platform.ModShift
	case arkKeyCtrlLeft, arkKeyCtrlRight:
		return platform.ModCtrl
	case arkKeyAltLeft, arkKeyAltRight:
		return platform.ModAlt
	case arkKeyMetaLeft, arkKeyMetaRight:
		return platform.ModSuper
	}
	return 0
}

// mapArkKey maps an ArkUI_KeyCode and its layout Unicode code point to
// mygo's logical Key, physical ANSI Key, and positional KeyCode.
func mapArkKey(arkCode int32, unicode uint32) (key, physical platform.Key, code platform.KeyCode) {
	switch {
	case arkCode >= arkKeyA && arkCode <= arkKeyZ:
		off := arkCode - arkKeyA
		physical = platform.KeyA + platform.Key(off)
		code = platform.CodeKeyA + platform.KeyCode(off)
		key = physical
	case arkCode >= arkKey0 && arkCode <= arkKey9:
		d := arkCode - arkKey0
		physical = platform.Key0 + platform.Key(d)
		if d == 0 {
			code = platform.CodeDigit0
		} else {
			code = platform.CodeDigit1 + platform.KeyCode(d-1)
		}
		key = physical
	case arkCode >= arkKeyF1 && arkCode <= arkKeyF12:
		off := arkCode - arkKeyF1
		key = platform.KeyF1 + platform.Key(off)
		physical = key
		code = platform.CodeF1 + platform.KeyCode(off)
	case arkCode >= arkKeyNumpad0 && arkCode <= arkKeyNumpad9:
		d := arkCode - arkKeyNumpad0
		key = platform.Key0 + platform.Key(d)
		if d == 0 {
			code = platform.CodeNumpad0
		} else {
			code = platform.CodeNumpad1 + platform.KeyCode(d-1)
		}
	default:
		switch arkCode {
		case arkKeyEnter:
			key, physical, code = platform.KeyEnter, platform.KeyEnter, platform.CodeEnter
		case arkKeyNumpadEnter:
			key, code = platform.KeyEnter, platform.CodeNumpadEnter
		case arkKeyEscape:
			key, physical, code = platform.KeyEscape, platform.KeyEscape, platform.CodeEscape
		case arkKeyDel:
			key, physical, code = platform.KeyBackspace, platform.KeyBackspace, platform.CodeBackspace
		case arkKeyForwardDel:
			key, physical, code = platform.KeyDelete, platform.KeyDelete, platform.CodeDelete
		case arkKeyTab:
			key, physical, code = platform.KeyTab, platform.KeyTab, platform.CodeTab
		case arkKeySpace:
			key, physical, code = platform.KeySpace, platform.KeySpace, platform.CodeSpace
		case arkKeyInsert:
			key, physical, code = platform.KeyInsert, platform.KeyInsert, platform.CodeInsert
		case arkKeyMoveHome:
			key, physical, code = platform.KeyHome, platform.KeyHome, platform.CodeHome
		case arkKeyMoveEnd:
			key, physical, code = platform.KeyEnd, platform.KeyEnd, platform.CodeEnd
		case arkKeyPageUp:
			key, physical, code = platform.KeyPageUp, platform.KeyPageUp, platform.CodePageUp
		case arkKeyPageDown:
			key, physical, code = platform.KeyPageDown, platform.KeyPageDown, platform.CodePageDown
		case arkKeyDpadUp:
			key, physical, code = platform.KeyUp, platform.KeyUp, platform.CodeArrowUp
		case arkKeyDpadDown:
			key, physical, code = platform.KeyDown, platform.KeyDown, platform.CodeArrowDown
		case arkKeyDpadLeft:
			key, physical, code = platform.KeyLeft, platform.KeyLeft, platform.CodeArrowLeft
		case arkKeyDpadRight:
			key, physical, code = platform.KeyRight, platform.KeyRight, platform.CodeArrowRight
		case arkKeyMinus:
			key, physical, code = platform.KeyMinus, platform.KeyMinus, platform.CodeMinus
		case arkKeyEquals:
			key, physical, code = platform.KeyEqual, platform.KeyEqual, platform.CodeEqual
		case arkKeyComma:
			key, physical, code = platform.KeyComma, platform.KeyComma, platform.CodeComma
		case arkKeyPeriod:
			key, physical, code = platform.KeyPeriod, platform.KeyPeriod, platform.CodePeriod
		case arkKeySlash:
			key, physical, code = platform.KeySlash, platform.KeySlash, platform.CodeSlash
		case arkKeySemicolon:
			key, physical, code = platform.KeySemicolon, platform.KeySemicolon, platform.CodeSemicolon
		case arkKeyApostrophe:
			key, physical, code = platform.KeyQuote, platform.KeyQuote, platform.CodeQuote
		case arkKeyLeftBracket:
			key, physical, code = platform.KeyBracketLeft, platform.KeyBracketLeft, platform.CodeBracketLeft
		case arkKeyRightBracket:
			key, physical, code = platform.KeyBracketRight, platform.KeyBracketRight, platform.CodeBracketRight
		case arkKeyBackslash:
			key, physical, code = platform.KeyBackslash, platform.KeyBackslash, platform.CodeBackslash
		case arkKeyGrave:
			key, physical, code = platform.KeyBackquote, platform.KeyBackquote, platform.CodeBackquote
		case arkKeyMenu:
			key, physical, code = platform.KeyContextMenu, platform.KeyContextMenu, platform.CodeContextMenu
		case arkKeyBack:
			key = platform.KeyBack
		case arkKeyForward:
			key = platform.KeyForward
		case arkKeyShiftLeft:
			code = platform.CodeShiftLeft
		case arkKeyShiftRight:
			code = platform.CodeShiftRight
		case arkKeyCtrlLeft:
			code = platform.CodeControlLeft
		case arkKeyCtrlRight:
			code = platform.CodeControlRight
		case arkKeyAltLeft:
			code = platform.CodeAltLeft
		case arkKeyAltRight:
			code = platform.CodeAltRight
		case arkKeyMetaLeft:
			code = platform.CodeSuperLeft
		case arkKeyMetaRight:
			code = platform.CodeSuperRight
		case arkKeyCapsLock:
			code = platform.CodeCapsLock
		case arkKeyScrollLock:
			code = platform.CodeScrollLock
		case arkKeyNumLock:
			code = platform.CodeNumLock
		case arkKeySysrq:
			code = platform.CodePrintScreen
		case arkKeyBreak:
			code = platform.CodePause
		case arkKeyNumpadDivide:
			key, code = platform.KeySlash, platform.CodeNumpadDivide
		case arkKeyNumpadMul:
			code = platform.CodeNumpadMultiply
		case arkKeyNumpadSub:
			key, code = platform.KeyMinus, platform.CodeNumpadSubtract
		case arkKeyNumpadAdd:
			code = platform.CodeNumpadAdd
		case arkKeyNumpadDot:
			key, code = platform.KeyPeriod, platform.CodeNumpadDecimal
		case arkKeyNumpadComma:
			key, code = platform.KeyComma, platform.CodeNumpadComma
		case arkKeyNumpadEquals:
			key, code = platform.KeyEqual, platform.CodeNumpadEqual
		case arkKeyVolumeUp:
			code = platform.CodeAudioVolumeUp
		case arkKeyVolumeDown:
			code = platform.CodeAudioVolumeDown
		case arkKeyVolumeMute:
			code = platform.CodeAudioVolumeMute
		}
	}
	// Layout-aware logical key override when unicode identifies a non-US letter/symbol.
	if unicode != 0 && key == platform.KeyUnknown {
		key = platform.KeyForRune(rune(unicode))
	}
	return key, physical, code
}
