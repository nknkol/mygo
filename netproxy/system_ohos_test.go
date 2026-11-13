//go:build linux && (amd64 || arm64) && mygo_ohos

package netproxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/egoist/mygo/internal/ohosproxy"
)

// TestOhosStatic checks that HarmonyOS proxy configs build authenticated URLs
// and honor domain-suffix, wildcard, CIDR, and <local> exclusion lists.
func TestOhosStatic(t *testing.T) {
	u := func(s string) *url.URL {
		p, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		cfg    ohosproxy.Config
		target string
		want   string
	}{
		{ohosproxy.Config{}, "https://example.com/", ""},
		{ohosproxy.Config{Host: "127.0.0.1", Port: 7890}, "https://example.com/", "http://127.0.0.1:7890"},
		{ohosproxy.Config{Host: "::1", Port: 7890}, "https://example.com/", "http://[::1]:7890"},
		{ohosproxy.Config{Host: "socks5://127.0.0.1", Port: 1080}, "https://example.com/", "socks5://127.0.0.1:1080"},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Username: "alice", Password: "p@ss:1"},
			"https://example.com/",
			"http://alice:p%40ss%3A1@proxy.corp:8080",
		},
		// HarmonyOS domain suffix exclusions match both the domain and subdomains.
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"ample.com"}},
			"https://ample.com/",
			"",
		},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"ample.com"}},
			"https://www.ample.com/",
			"",
		},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"ample.com"}},
			"https://example.com/",
			"http://proxy.corp:8080",
		},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"*"}},
			"https://example.com/",
			"",
		},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"10.0.0.0/8, <local>"}},
			"http://10.2.3.4/",
			"",
		},
		{
			ohosproxy.Config{Host: "proxy.corp", Port: 8080, Exclusions: []string{"10.0.0.0/8, <local>"}},
			"http://intranet/",
			"",
		},
	} {
		if got := urlString(ohosStatic(tc.cfg, u(tc.target))); got != tc.want {
			t.Errorf("ohosStatic(%+v, %q) = %q, want %q", tc.cfg, tc.target, got, tc.want)
		}
	}
}

// resetProxyCache flushes netproxy's 5-second per-host lookup cache in tests.
func resetProxyCache() {
	cache.Lock()
	cache.m = nil
	cache.Unlock()
}

// TestOhosProxyFallbackOrder checks that Proxy uses host-pushed ohosproxy state,
// falls back to MYGO_OHOS_HTTP_PROXY or the NDK lookup when unset, and still
// yields to standard HTTP_PROXY environment variables.
func TestOhosProxyFallbackOrder(t *testing.T) {
	clearEnv(t)
	t.Setenv(ohosproxy.EnvVar, "")
	ohosproxy.Clear()
	defer func() {
		ohosproxy.Clear()
		resetProxyCache()
	}()

	prevHook := queryNativeProxyHook
	defer func() { queryNativeProxyHook = prevHook }()

	nativeCalls := 0
	queryNativeProxyHook = func() (ohosproxy.Config, bool) {
		nativeCalls++
		return ohosproxy.Config{Host: "ndk.local", Port: 8888, Exclusions: []string{"bypass.local"}}, true
	}

	req, err := http.NewRequest("GET", "https://api.example.com/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resetProxyCache()
	got, err := Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if urlString(got) != "http://ndk.local:8888" || nativeCalls != 1 {
		t.Fatalf("NDK fallback got %q (calls=%d), want http://ndk.local:8888 (calls=1)", urlString(got), nativeCalls)
	}

	// Startup environment variable overrides NDK fallback before host IPC connects.
	t.Setenv(ohosproxy.EnvVar, ohosproxy.EncodeEnv(ohosproxy.Config{
		Host:     "env.local",
		Port:     8081,
		Username: "dev",
		Password: "secret",
	}))
	resetProxyCache()
	got, err = Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if urlString(got) != "http://dev:secret@env.local:8081" {
		t.Fatalf("EnvVar fallback got %q, want http://dev:secret@env.local:8081", urlString(got))
	}

	// Live host-bridge update takes precedence over startup EnvVar, including
	// clearing to direct connection when Host is empty.
	ohosproxy.Set(ohosproxy.Config{Host: "bridge.local", Port: 7890})
	resetProxyCache()
	got, err = Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if urlString(got) != "http://bridge.local:7890" {
		t.Fatalf("host bridge got %q, want http://bridge.local:7890", urlString(got))
	}

	// Standard HTTPS_PROXY environment variable still overrides system proxy.
	t.Setenv("HTTPS_PROXY", "http://override.local:3128")
	resetProxyCache()
	got, err = Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if urlString(got) != "http://override.local:3128" {
		t.Fatalf("HTTPS_PROXY override got %q, want http://override.local:3128", urlString(got))
	}
}

// TestSystemOhosNative queries libnet_connection.so when present on HarmonyOS
// and checks that system returns either nil or a valid proxy URL with a host.
func TestSystemOhosNative(t *testing.T) {
	ohosproxy.Clear()
	t.Setenv(ohosproxy.EnvVar, "")
	if netConn.get(loadNetConn) == nil {
		t.Log("no libnet_connection.so: requests go direct")
	}
	p := system(&url.URL{Scheme: "https", Host: "example.com", Path: "/"})
	if p != nil && p.Host == "" {
		t.Errorf("system = %v, a proxy without a host", p)
	}
	t.Logf("HarmonyOS native proxy: %v", p)
}
