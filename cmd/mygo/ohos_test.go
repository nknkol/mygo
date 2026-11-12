package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// buildPlatforms accepts ohos and ohos/arm64 and rejects unsupported HarmonyOS architectures before project work.
func TestOhosBuildPlatforms(t *testing.T) {
	targets, err := buildPlatforms("ohos, ohos/arm64, linux/amd64")
	if err != nil {
		t.Fatalf("buildPlatforms: %v", err)
	}
	if !slices.Equal(targets, []string{"ohos", "ohos/arm64", "linux/amd64"}) {
		t.Fatalf("targets = %v", targets)
	}
	if ohosArch("ohos") != "arm64" || ohosArch("ohos/arm64") != "arm64" {
		t.Fatalf("unexpected ohosArch mapping")
	}

	for _, bad := range []string{"ohos/", "ohos/amd64", "ohos/386", "ohos/arm64/extra"} {
		if err := runBuild([]string{"-platform", bad, t.TempDir()}); err == nil || !strings.Contains(err.Error(), "platform") {
			t.Fatalf("runBuild(%q) = %v; want platform error", bad, err)
		}
	}
}

// ohosBuildTags merges mygo_ohos, GOFLAGS tags and production tags into a single -tags flag so go build never overrides mygo_ohos.
func TestOhosBuildTagsSingleFlag(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=customtag")
	t.Setenv("MYGO_INSPECTOR", "")

	prod := ohosBuildTags(false)
	if !slices.Equal(prod, []string{"-tags", "mygo_ohos,customtag,mygo_nodownload,mygo_noinspector"}) {
		t.Fatalf("prod tags = %v", prod)
	}

	t.Setenv("MYGO_INSPECTOR", "1")
	withInsp := ohosBuildTags(false)
	if !slices.Equal(withInsp, []string{"-tags", "mygo_ohos,customtag,mygo_nodownload"}) {
		t.Fatalf("inspector tags = %v", withInsp)
	}

	dbg := ohosBuildTags(true)
	if !slices.Equal(dbg, []string{"-tags", "mygo_ohos,customtag"}) {
		t.Fatalf("debug tags = %v", dbg)
	}
}

// writeOhosHostProject copies read-only module-cache templates writably and generates app.json5, module.json5 skills, localized strings and resized icons.
func TestOhosHostProjectGeneration(t *testing.T) {
	tmpl := t.TempDir()
	for _, rel := range []string{
		"AppScope/app.json5",
		"entry/src/main/cpp/host.cpp",
		"entry/src/main/cpp/types/libmygo_host/Index.d.ts",
		"entry/.cxx/stale.o",
		"entry/build/stale.hap",
	} {
		p := filepath.Join(tmpl, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("template"), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	proj := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 120, B: 40, A: 255})
	var iconBuf bytes.Buffer
	if err := png.Encode(&iconBuf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "resources", "icon.png"), iconBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, "locales"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "locales", "en.json"), []byte(`{"hello": "Hello"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "locales", "zh-Hans.json"), []byte(`{"mygo.appName": "云音乐", "mygo.appDescription": "本地与在线音乐播放器"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{
		"name": "MyGo Music",
		"identifier": "dev.egoist.music",
		"slug": "mygo-music",
		"version": "0.1.22",
		"urlSchemes": ["mygo-music", "mygo-music-plugin"],
		"fileAssociations": [{"ext": ["flac", "mp3"], "mimeType": "audio/flac"}],
		"i18n": {"dir": "locales", "source": "en"},
		"linux": {"comment": "Music player"},
		"ohos": {"permissions": ["ohos.permission.MICROPHONE"]}
	}`
	if err := os.WriteFile(filepath.Join(proj, "mygo.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := loadConfig(proj)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := c.ohosVersionCode(); got != 1022 {
		t.Fatalf("ohosVersionCode = %d, want 1022", got)
	}
	if got := c.ohosVendor(); got != "egoist" {
		t.Fatalf("ohosVendor = %q, want egoist", got)
	}

	stageHost := filepath.Join(t.TempDir(), "host")
	if err := writeOhosHostProject(c, tmpl, stageHost, false); err != nil {
		t.Fatalf("writeOhosHostProject: %v", err)
	}

	// Transient build directories in the template must not be copied.
	for _, skipped := range []string{"entry/.cxx", "entry/build"} {
		if _, err := os.Stat(filepath.Join(stageHost, filepath.FromSlash(skipped))); err == nil {
			t.Fatalf("expected %s to be skipped", skipped)
		}
	}

	// Verify AppScope/app.json5 metadata.
	appRaw, err := os.ReadFile(filepath.Join(stageHost, "AppScope", "app.json5"))
	if err != nil {
		t.Fatal(err)
	}
	var appDoc struct {
		App struct {
			BundleName  string `json:"bundleName"`
			Vendor      string `json:"vendor"`
			VersionCode int    `json:"versionCode"`
			VersionName string `json:"versionName"`
		} `json:"app"`
	}
	if err := json.Unmarshal(appRaw, &appDoc); err != nil {
		t.Fatal(err)
	}
	if appDoc.App.BundleName != "dev.egoist.music" || appDoc.App.Vendor != "egoist" || appDoc.App.VersionCode != 1022 || appDoc.App.VersionName != "0.1.22" {
		t.Fatalf("unexpected app.json5: %+v", appDoc.App)
	}

	// Verify module.json5 URL schemes, file association MIME types, and extra permission.
	modRaw, err := os.ReadFile(filepath.Join(stageHost, "entry", "src", "main", "module.json5"))
	if err != nil {
		t.Fatal(err)
	}
	modText := string(modRaw)
	for _, want := range []string{`"scheme": "mygo-music"`, `"scheme": "mygo-music-plugin"`, `"type": "audio/flac"`, `"ohos.permission.MICROPHONE"`} {
		if !strings.Contains(modText, want) {
			t.Fatalf("module.json5 missing %s:\n%s", want, modText)
		}
	}

	// Verify localized zh_CN string table was generated.
	zhRaw, err := os.ReadFile(filepath.Join(stageHost, "AppScope", "resources", "zh_CN", "element", "string.json"))
	if err != nil || !strings.Contains(string(zhRaw), "云音乐") {
		t.Fatalf("zh_CN string.json = %q, %v", zhRaw, err)
	}

	// Verify icons were resized to 512x512 and 256x256.
	checkDim := func(rel string, want int) {
		t.Helper()
		f, err := os.Open(filepath.Join(stageHost, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cfg, err := png.DecodeConfig(f)
		if err != nil || cfg.Width != want || cfg.Height != want {
			t.Fatalf("%s dimensions = %dx%d (%v), want %dx%d", rel, cfg.Width, cfg.Height, err, want, want)
		}
	}
	checkDim("AppScope/resources/base/media/app_icon.png", 512)
	checkDim("entry/src/main/resources/base/media/app_icon.png", 512)
	checkDim("entry/src/main/resources/base/media/startIcon.png", 256)
}

// packOhosHNP sets 0755 permissions on the engine and executable resources, and injectHNPIntoHAP embeds the HNP archive uncompressed via zip.Store.
func TestOhosPackHNPAndInjectIntoHAP(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "mygo-engine")
	if err := os.WriteFile(engine, []byte("ELF-ARM64-ENGINE"), 0o600); err != nil {
		t.Fatal(err)
	}
	fonts := filepath.Join(dir, "fonts.conf")
	if err := os.WriteFile(fonts, []byte("<fontconfig/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	helperTool := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helperTool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataFile := filepath.Join(dir, "words.txt")
	if err := os.WriteFile(dataFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Config{Name: "MyGo Music", Slug: "mygo-music", Version: "0.1.22"}
	res := []resource{
		{name: "bin/helper.sh", src: helperTool},
		{name: "data/words.txt", src: dataFile},
	}
	hnpPath := filepath.Join(dir, "mygo.hnp")
	if err := packOhosHNP(c, engine, "", fonts, res, hnpPath); err != nil {
		t.Fatalf("packOhosHNP: %v", err)
	}

	hnpZip, err := zip.OpenReader(hnpPath)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]os.FileMode{}
	for _, zf := range hnpZip.File {
		modes[zf.Name] = zf.Mode()
	}
	_ = hnpZip.Close()

	if modes["mygo_0.1.22/bin/mygo-engine"].Perm() != 0o755 {
		t.Fatalf("mygo-engine mode = %v, want 0755", modes["mygo_0.1.22/bin/mygo-engine"])
	}
	if modes["mygo_0.1.22/bin/bin/helper.sh"].Perm() != 0o755 {
		t.Fatalf("helper.sh mode = %v, want 0755", modes["mygo_0.1.22/bin/bin/helper.sh"])
	}
	if modes["mygo_0.1.22/bin/data/words.txt"].Perm() != 0o644 {
		t.Fatalf("words.txt mode = %v, want 0644", modes["mygo_0.1.22/bin/data/words.txt"])
	}
	if modes["mygo_0.1.22/etc/fonts/fonts.conf"].Perm() != 0o644 || modes["mygo_0.1.22/hnp.json"].Perm() != 0o644 {
		t.Fatalf("unexpected config modes: %v", modes)
	}

	// Build a synthetic HAP with a deflated entry and a stale HNP entry, then inject.
	hapPath := filepath.Join(dir, hapFileName(c, "arm64"))
	f, err := os.Create(hapPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w1, err := zw.CreateHeader(&zip.FileHeader{Name: "module.json", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w1.Write([]byte(`{"module":{"name":"entry"}}`))
	w2, err := zw.CreateHeader(&zip.FileHeader{Name: "hnp/arm64-v8a/mygo.hnp", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.Write([]byte("stale"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if err := injectHNPIntoHAP(hapPath, "hnp/arm64-v8a/mygo.hnp", hnpPath); err != nil {
		t.Fatalf("injectHNPIntoHAP: %v", err)
	}

	hapZip, err := zip.OpenReader(hapPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hapZip.Close()

	hnpCount := 0
	for _, zf := range hapZip.File {
		if zf.Name == "hnp/arm64-v8a/mygo.hnp" {
			hnpCount++
			if zf.Method != zip.Store {
				t.Fatalf("HNP zip method = %d, want zip.Store (0)", zf.Method)
			}
			rc, err := zf.Open()
			if err != nil {
				t.Fatal(err)
			}
			gotBytes, _ := io.ReadAll(rc)
			_ = rc.Close()
			wantBytes, _ := os.ReadFile(hnpPath)
			if !bytes.Equal(gotBytes, wantBytes) {
				t.Fatalf("embedded HNP bytes mismatch")
			}
		}
	}
	if hnpCount != 1 {
		t.Fatalf("hnp entry count = %d, want 1", hnpCount)
	}
}
