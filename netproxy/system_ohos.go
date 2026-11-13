//go:build linux && (amd64 || arm64) && mygo_ohos

package netproxy

import (
	"bytes"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/egoist/mygo/internal/ohosproxy"
)

// HarmonyOS reads the default network's HTTP proxy either from the Stage-model
// host bridge (internal/ohosproxy, which receives live updates and optional
// proxy credentials from @ohos.net.connection) or, in standalone CLI binaries
// and before the host bridge connects, directly from libnet_connection.so's
// OH_NetConn_GetDefaultHttpProxy.

const (
	netConnMaxStrLen        = 256
	netConnMaxExclusionSize = 256
)

// netConnHttpProxy matches NetConn_HttpProxy in
// <network/netmanager/net_connection_type.h>.
type netConnHttpProxy struct {
	host              [netConnMaxStrLen]byte
	exclusionList     [netConnMaxExclusionSize][netConnMaxStrLen]byte
	exclusionListSize int32
	port              uint16
	_                 [2]byte
}

type netConnAPI struct {
	getDefaultHttpProxy func(proxy *netConnHttpProxy) int32
}

var netConn lazy[*netConnAPI]

// queryNativeProxyHook is overridden in tests to supply NDK proxy results
// without depending on the host device's live network settings.
var queryNativeProxyHook func() (ohosproxy.Config, bool)

func loadNetConn() *netConnAPI {
	var lib uintptr
	for _, soname := range []string{"libnet_connection.so", "/system/lib64/ndk/libnet_connection.so"} {
		h, err := purego.Dlopen(soname, purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err == nil && h != 0 {
			lib = h
			break
		}
	}
	if lib == 0 {
		return nil
	}
	p, err := purego.Dlsym(lib, "OH_NetConn_GetDefaultHttpProxy")
	if err != nil || p == 0 {
		return nil
	}
	api := &netConnAPI{}
	purego.RegisterFunc(&api.getDefaultHttpProxy, p)
	return api
}

func queryNativeProxy() (ohosproxy.Config, bool) {
	if fn := queryNativeProxyHook; fn != nil {
		return fn()
	}
	api := netConn.get(loadNetConn)
	if api == nil {
		return ohosproxy.Config{}, false
	}
	var raw netConnHttpProxy
	if rc := api.getDefaultHttpProxy(&raw); rc != 0 {
		return ohosproxy.Config{}, false
	}
	cfg := ohosproxy.Config{
		Host: cBytesString(raw.host[:]),
		Port: int(raw.port),
	}
	n := int(raw.exclusionListSize)
	if n < 0 {
		n = 0
	}
	if n > netConnMaxExclusionSize {
		n = netConnMaxExclusionSize
	}
	if n > 0 {
		cfg.Exclusions = make([]string, 0, n)
		for i := range n {
			if s := cBytesString(raw.exclusionList[i][:]); s != "" {
				cfg.Exclusions = append(cfg.Exclusions, s)
			}
		}
	}
	// Ensure raw stays live across the C call under all compiler optimizations.
	_ = unsafe.Pointer(&raw)
	return cfg, true
}

func cBytesString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func system(target *url.URL) *url.URL {
	cfg, ok := ohosproxy.Get()
	if !ok {
		cfg, ok = queryNativeProxy()
	}
	if !ok {
		return nil
	}
	return ohosStatic(cfg, target)
}

func ohosStatic(cfg ohosproxy.Config, target *url.URL) *url.URL {
	host := strings.TrimSpace(cfg.Host)
	if host == "" || cfg.Port <= 0 || cfg.Port > 65535 || target == nil {
		return nil
	}
	if ohosBypassed(cfg.Exclusions, target.Hostname()) {
		return nil
	}
	var raw string
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Host == "" {
			return nil
		}
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(cfg.Port))
		}
		raw = u.String()
	} else {
		h := strings.Trim(host, "[]")
		raw = "http://" + net.JoinHostPort(h, strconv.Itoa(cfg.Port))
	}
	p, direct := proxyURL(raw)
	if direct || p == nil {
		return nil
	}
	if cfg.Username != "" || cfg.Password != "" {
		if cfg.Password != "" {
			p.User = url.UserPassword(cfg.Username, cfg.Password)
		} else {
			p.User = url.User(cfg.Username)
		}
	}
	return p
}

// ohosBypassed reports whether HarmonyOS's HttpProxy.exclusionList sends host
// direct. Per @ohos.net.connection.HttpProxy, entries are case-insensitive,
// "*" bypasses all hosts, domain suffixes such as "example.com" match both
// "example.com" and "sub.example.com", and Windows/PAC-style globs ("*.local",
// "10.*"), CIDRs ("10.0.0.0/8"), and "<local>" are also honored.
func ohosBypassed(exclusions []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || len(exclusions) == 0 {
		return false
	}
	for _, rawEntry := range exclusions {
		for _, part := range strings.FieldsFunc(rawEntry, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
		}) {
			e := strings.ToLower(strings.TrimSpace(part))
			if e == "" {
				continue
			}
			if e == "*" {
				return true
			}
			if winBypassed(e, host) {
				return true
			}
			clean := e
			if _, rest, ok := strings.Cut(clean, "://"); ok {
				clean = rest
			}
			if h, _, err := net.SplitHostPort(clean); err == nil {
				clean = h
			}
			clean = strings.TrimPrefix(strings.Trim(clean, "[]"), ".")
			if clean != "" && !strings.ContainsAny(clean, "*/") {
				if host == clean || strings.HasSuffix(host, "."+clean) {
					return true
				}
			}
		}
	}
	return false
}
