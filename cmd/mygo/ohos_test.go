package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	// Draw an inset squircle-like 1024x1024 icon (transparent 100px outer margin
	// and transparent corners) to verify full-bleed 1024x1024 opaque background extrapolation.
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 100; y < 924; y++ {
		for x := 100; x < 924; x++ {
			// Leave 80x80 corners of the 824x824 box transparent like a squircle.
			inTL := x < 180 && y < 180 && (x-180)*(x-180)+(y-180)*(y-180) > 80*80
			inTR := x >= 844 && y < 180 && (x-844)*(x-844)+(y-180)*(y-180) > 80*80
			inBL := x < 180 && y >= 844 && (x-180)*(x-180)+(y-844)*(y-844) > 80*80
			inBR := x >= 844 && y >= 844 && (x-844)*(x-844)+(y-844)*(y-844) > 80*80
			if inTL || inTR || inBL || inBR {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{R: 220, G: 50, B: 70, A: 255})
		}
	}
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
		"ohos": {
			"permissions": [
				"ohos.permission.MICROPHONE",
				"ohos.permission.READ_WRITE_DOWNLOAD_DIRECTORY",
				"ohos.permission.CUSTOM_SANDBOX"
			]
		}
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
			Icon        string `json:"icon"`
		} `json:"app"`
	}
	if err := json.Unmarshal(appRaw, &appDoc); err != nil {
		t.Fatal(err)
	}
	if appDoc.App.BundleName != "dev.egoist.music" || appDoc.App.Vendor != "egoist" || appDoc.App.VersionCode != 1022 || appDoc.App.VersionName != "0.1.22" || appDoc.App.Icon != "$media:layered_image" {
		t.Fatalf("unexpected app.json5: %+v", appDoc.App)
	}

	// Verify module.json5 URL schemes, file association MIME types, layered icon, and permission classification.
	modRaw, err := os.ReadFile(filepath.Join(stageHost, "entry", "src", "main", "module.json5"))
	if err != nil {
		t.Fatal(err)
	}
	modText := string(modRaw)
	for _, want := range []string{
		`"scheme": "mygo-music"`,
		`"scheme": "mygo-music-plugin"`,
		`"type": "audio/flac"`,
		`"icon": "$media:layered_image"`,
		`"$string:microphone_reason"`,
		`"$string:read_write_download_directory_reason"`,
	} {
		if !strings.Contains(modText, want) {
			t.Fatalf("module.json5 missing %s:\n%s", want, modText)
		}
	}
	var modDoc struct {
		Module struct {
			RequestPermissions []struct {
				Name      string         `json:"name"`
				Reason    string         `json:"reason"`
				UsedScene map[string]any `json:"usedScene"`
			} `json:"requestPermissions"`
		} `json:"module"`
	}
	if err := json.Unmarshal(modRaw, &modDoc); err != nil {
		t.Fatal(err)
	}
	permMap := map[string]struct {
		Reason    string
		UsedScene map[string]any
	}{}
	for _, p := range modDoc.Module.RequestPermissions {
		permMap[p.Name] = struct {
			Reason    string
			UsedScene map[string]any
		}{p.Reason, p.UsedScene}
	}
	if got := permMap["ohos.permission.MICROPHONE"]; got.Reason != "$string:microphone_reason" || got.UsedScene == nil {
		t.Fatalf("MICROPHONE perm = %+v, want reason + usedScene", got)
	}
	if got := permMap["ohos.permission.READ_WRITE_DOWNLOAD_DIRECTORY"]; got.Reason != "$string:read_write_download_directory_reason" || got.UsedScene == nil {
		t.Fatalf("READ_WRITE_DOWNLOAD_DIRECTORY perm = %+v, want reason + usedScene", got)
	}
	if got, ok := permMap["ohos.permission.CUSTOM_SANDBOX"]; !ok || got.Reason != "" || got.UsedScene != nil {
		t.Fatalf("CUSTOM_SANDBOX perm = %+v (ok=%v), want plain system/sandbox permission without reason/usedScene", got, ok)
	}

	// Verify localized zh_CN string tables include both app label and user_grant permission reasons.
	zhRaw, err := os.ReadFile(filepath.Join(stageHost, "AppScope", "resources", "zh_CN", "element", "string.json"))
	if err != nil || !strings.Contains(string(zhRaw), "云音乐") {
		t.Fatalf("zh_CN string.json = %q, %v", zhRaw, err)
	}
	for _, strPath := range []string{
		"entry/src/main/resources/base/element/string.json",
		"entry/src/main/resources/zh_CN/element/string.json",
	} {
		raw, err := os.ReadFile(filepath.Join(stageHost, filepath.FromSlash(strPath)))
		if err != nil {
			t.Fatal(err)
		}
		for _, wantKey := range []string{"microphone_reason", "read_write_download_directory_reason"} {
			if !strings.Contains(string(raw), wantKey) {
				t.Fatalf("%s missing %s:\n%s", strPath, wantKey, raw)
			}
		}
	}

	// Verify 1024x1024 layered icons and 512/256 auxiliary icons were generated,
	// and that background.png extrapolated opaque color into all four corners.
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
	checkDim("AppScope/resources/base/media/background.png", 1024)
	checkDim("AppScope/resources/base/media/foreground.png", 1024)
	checkDim("entry/src/main/resources/base/media/background.png", 1024)
	checkDim("entry/src/main/resources/base/media/foreground.png", 1024)
	checkDim("AppScope/resources/base/media/app_icon.png", 512)
	checkDim("entry/src/main/resources/base/media/app_icon.png", 512)
	checkDim("entry/src/main/resources/base/media/startIcon.png", 256)

	bgFile, err := os.Open(filepath.Join(stageHost, "AppScope/resources/base/media/background.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer bgFile.Close()
	bgDecoded, err := png.Decode(bgFile)
	if err != nil {
		t.Fatal(err)
	}
	bgNRGBA := toNRGBA(bgDecoded)
	for _, pt := range [][2]int{{0, 0}, {1023, 0}, {0, 1023}, {1023, 1023}, {512, 512}} {
		px := bgNRGBA.NRGBAAt(pt[0], pt[1])
		if px != (color.NRGBA{R: 220, G: 50, B: 70, A: 255}) {
			t.Fatalf("background.png pixel (%d,%d) = %+v, want opaque extrapolated coral {220 50 70 255}", pt[0], pt[1], px)
		}
	}
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

// ensureOhosDevHostHap reuses the cached unsigned host HAP until host inputs change, and packOhosHNPWithEnv embeds etc/mygo.env.
func TestOhosDevHostCacheAndEnvHNP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake hvigorw requires POSIX sh")
	}
	workDir, err := os.MkdirTemp(ohosTempDir(), "mygo-ohos-dev-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	tmpl := filepath.Join(workDir, "tmpl")
	for _, rel := range []string{
		"AppScope/app.json5",
		"entry/src/main/cpp/host.cpp",
		"entry/src/main/cpp/types/libmygo_host/Index.d.ts",
	} {
		p := filepath.Join(tmpl, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("template-v1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	countFile := filepath.Join(workDir, "hvigor-count")
	fakeHvigor := filepath.Join(workDir, "fake-hvigorw.sh")
	script := "#!/bin/sh\n" +
		"echo 1 >> '" + countFile + "'\n" +
		"mkdir -p entry/build/default/outputs/default\n" +
		"printf 'PK\\005\\006\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000\\000' > entry/build/default/outputs/default/entry-default-unsigned.hap\n"
	if err := os.WriteFile(fakeHvigor, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	proj := filepath.Join(workDir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "mygo.json"), []byte(`{"name":"Demo","identifier":"dev.egoist.demo","version":"0.2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(proj)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	dc := devConfig(c)
	cacheDir := filepath.Join(workDir, "host-cache")

	hap1, sum1, err := ensureOhosDevHostHap(context.Background(), nil, dc, tmpl, cacheDir, fakeHvigor, nil)
	if err != nil {
		t.Fatalf("first ensureOhosDevHostHap: %v", err)
	}
	hap2, sum2, err := ensureOhosDevHostHap(context.Background(), nil, dc, tmpl, cacheDir, fakeHvigor, nil)
	if err != nil {
		t.Fatalf("second ensureOhosDevHostHap: %v", err)
	}
	if hap1 != hap2 || sum1 != sum2 {
		t.Fatalf("cache miss on identical config: (%q, %x) vs (%q, %x)", hap1, sum1, hap2, sum2)
	}
	runs, _ := os.ReadFile(countFile)
	if got := len(bytes.Split(bytes.TrimSpace(runs), []byte("\n"))); got != 1 {
		t.Fatalf("hvigorw invoked %d times on cached config, want 1", got)
	}

	// Changing a host-level setting (permissions) invalidates the host HAP cache.
	dc.OHOS.Permissions = []string{"ohos.permission.MICROPHONE"}
	hap3, sum3, err := ensureOhosDevHostHap(context.Background(), nil, dc, tmpl, cacheDir, fakeHvigor, nil)
	if err != nil {
		t.Fatalf("third ensureOhosDevHostHap: %v", err)
	}
	if hap3 == hap1 || sum3 == sum1 {
		t.Fatal("expected new cached HAP after permission change")
	}
	runs, _ = os.ReadFile(countFile)
	if got := len(bytes.Split(bytes.TrimSpace(runs), []byte("\n"))); got != 2 {
		t.Fatalf("hvigorw invoked %d times after config change, want 2", got)
	}

	// Verify packOhosHNPWithEnv writes etc/mygo.env inside the HNP zip.
	engine := filepath.Join(workDir, "mygo-engine")
	if err := os.WriteFile(engine, []byte("ELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	hnpPath := filepath.Join(workDir, "mygo.hnp")
	wantEnv := []string{"MYGO_ENV=development", "MYGO_DEV=1", "MYGO_LOCALE=zh-CN"}
	if err := packOhosHNPWithEnv(dc, engine, "", "", nil, wantEnv, hnpPath); err != nil {
		t.Fatalf("packOhosHNPWithEnv: %v", err)
	}
	zr, err := zip.OpenReader(hnpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var gotEnv string
	for _, zf := range zr.File {
		if zf.Name == "mygo_0.2.0/etc/mygo.env" {
			rc, err := zf.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			gotEnv = string(b)
		}
	}
	if gotEnv != "MYGO_ENV=development\nMYGO_DEV=1\nMYGO_LOCALE=zh-CN\n" {
		t.Fatalf("etc/mygo.env = %q", gotEnv)
	}
}

// formatOhosHilogLine strips hilog prefixes from Go engine logs while surfacing host error lines and suppressing routine bridge info logs.
func TestFormatOhosHilogLine(t *testing.T) {
	msg, isErr, ok := formatOhosHilogLine("11-13 18:05:50.294  4120  4128 I A00000/dev.egoist.demo.dev/mygo-host: listening on :8080")
	if !ok || isErr || msg != "listening on :8080" {
		t.Fatalf("mygo-host info = (%q, %v, %v), want ('listening on :8080', false, true)", msg, isErr, ok)
	}
	if _, _, ok := formatOhosHilogLine("11-13 18:05:50.294  4120  4121 I A00000/dev.egoist.demo.dev/mygo-host: engine started pid=4129"); ok {
		t.Fatal("expected internal engine started pid line to be suppressed")
	}
	if _, _, ok := formatOhosHilogLine("11-13 18:05:50.294  4120  4120 I A00000/dev.egoist.demo.dev/mygo: onCreate uri="); ok {
		t.Fatal("expected routine EntryAbility info log to be suppressed")
	}
	msg, isErr, ok = formatOhosHilogLine("11-13 18:05:50.294  4120  4120 E A00000/dev.egoist.demo.dev/mygo-bridge: loadContent failed: bad page")
	if !ok || !isErr || msg != "[host] loadContent failed: bad page" {
		t.Fatalf("mygo-bridge error = (%q, %v, %v)", msg, isErr, ok)
	}
}
