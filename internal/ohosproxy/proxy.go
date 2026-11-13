// Package ohosproxy shares the HarmonyOS system HTTP proxy configuration
// reported by the Stage-model host process with package netproxy without
// coupling netproxy to internal/ohos.
package ohosproxy

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
)

// EnvVar carries a JSON-encoded Config from the HarmonyOS host process to the
// Go engine at fork/exec time.
const EnvVar = "MYGO_OHOS_HTTP_PROXY"

// Config is the HTTP proxy configuration reported by @ohos.net.connection.
type Config struct {
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	Username   string   `json:"username,omitempty"`
	Password   string   `json:"password,omitempty"`
	Exclusions []string `json:"exclusions,omitempty"`
}

var (
	mu  sync.RWMutex
	has bool
	cur Config
)

func cloneConfig(c Config) Config {
	c.Exclusions = slices.Clone(c.Exclusions)
	return c
}

// Set stores the latest proxy configuration pushed by the HarmonyOS host.
// An empty Host or non-positive Port indicates a direct connection.
func Set(c Config) {
	mu.Lock()
	cur = cloneConfig(c)
	has = true
	mu.Unlock()
}

// Clear resets any host-supplied proxy configuration, returning Get to
// environment/NDK fallback mode. Intended for tests.
func Clear() {
	mu.Lock()
	cur = Config{}
	has = false
	mu.Unlock()
}

// Get returns the active host-supplied proxy configuration if one has been set
// via Set or EnvVar.
func Get() (Config, bool) {
	mu.RLock()
	if has {
		c := cloneConfig(cur)
		mu.RUnlock()
		return c, true
	}
	mu.RUnlock()

	if raw := strings.TrimSpace(os.Getenv(EnvVar)); raw != "" {
		if c, ok := DecodeEnv(raw); ok {
			return c, true
		}
	}
	return Config{}, false
}

// EncodeEnv serializes c for EnvVar.
func EncodeEnv(c Config) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeEnv parses a JSON value stored in EnvVar.
func DecodeEnv(raw string) (Config, bool) {
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, false
	}
	return c, true
}
