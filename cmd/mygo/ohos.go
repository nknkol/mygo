package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// OHOS configures HarmonyOS PC (.hap) packaging.
type OHOS struct {
	// Vendor names the application publisher in AppScope/app.json5
	// (default: publisher, else the second segment of identifier, else "mygo").
	Vendor string `json:"vendor"`
	// VersionCode is the positive integer bundle version in AppScope/app.json5
	// (default: derived from version as major*1_000_000 + minor*1_000 + patch).
	VersionCode int `json:"versionCode"`
	// DeviceTypes lists target HarmonyOS form factors in module.json5
	// (default: ["2in1", "tablet"]).
	DeviceTypes []string `json:"deviceTypes"`
	// Permissions adds extra requestPermissions names to module.json5.
	// User-grant permissions automatically receive a localized reason and
	// EntryAbility usedScene; system-grant and sandbox permissions (such as
	// ohos.permission.CUSTOM_SANDBOX) are emitted as plain name entries.
	Permissions []string `json:"permissions"`
	// PermissionReasons overrides or supplies usage-description text for
	// permissions in requestPermissions, keyed by permission name.
	PermissionReasons map[string]string `json:"permissionReasons"`
	// IconForeground and IconBackground optionally supply dedicated 1024x1024
	// square PNG layers for HarmonyOS's $media:layered_image icon. When unset,
	// MyGo derives compliant 1024x1024 layers from Config.Icon.
	IconForeground string `json:"iconForeground"`
	IconBackground string `json:"iconBackground"`
	// BackgroundModes lists EntryAbility backgroundModes in module.json5
	// (default: ["audioPlayback"]).
	BackgroundModes []string `json:"backgroundModes"`
	// SignWithHDA signs the assembled .hap with `hda install --keep-signed`
	// when true (or when MYGO_OHOS_SIGN=hda).
	SignWithHDA bool `json:"signWithHda"`
}

func (o *OHOS) validate() error {
	if o.VersionCode < 0 {
		return fmt.Errorf("ohos.versionCode %d must be positive", o.VersionCode)
	}
	for k, v := range o.PermissionReasons {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("ohos.permissionReasons maps a permission name to a non-empty reason, not %q to %q", k, v)
		}
	}
	return nil
}

// isOhosPlatform reports whether a -platform target builds a HarmonyOS .hap.
func isOhosPlatform(p string) bool {
	return p == "ohos" || strings.HasPrefix(p, "ohos/")
}

// ohosArch returns the Go architecture of an ohos platform target.
func ohosArch(p string) string {
	if _, arch, ok := strings.Cut(p, "/"); ok && arch != "" {
		return arch
	}
	return "arm64"
}

// isOhosHost reports whether the CLI is running inside a HarmonyOS PC container
// without a traditional glibc/GTK ldconfig environment.
func isOhosHost() bool {
	if !isDir("/data/service/hnp") {
		return false
	}
	_, err := exec.LookPath("ldconfig")
	return err != nil
}

// ohosTempDir returns a writable temporary directory, preferring the terminal
// sandbox's /data/storage/el2/base/haps/entry/temp on HarmonyOS PC when TMPDIR
// is unset (since /tmp on HarmonyOS PC is owned by system:shell).
func ohosTempDir() string {
	if env := strings.TrimSpace(os.Getenv("TMPDIR")); env != "" {
		return env
	}
	if isOhosHost() && isDir("/data/storage/el2/base/haps/entry/temp") {
		return "/data/storage/el2/base/haps/entry/temp"
	}
	return os.TempDir()
}

// hapFileName returns "<slug>-<version>-<arch>.hap".
func hapFileName(c *Config, goarch string) string {
	return c.slug() + "-" + fsName(c.Version) + "-" + goarch + ".hap"
}

// ohosVersionCode returns the positive integer versionCode for AppScope/app.json5.
func (c *Config) ohosVersionCode() int {
	if c.OHOS.VersionCode > 0 {
		return c.OHOS.VersionCode
	}
	v := strings.TrimPrefix(strings.TrimSpace(c.Version), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	parse := func(idx int) (int, bool) {
		if idx >= len(parts) {
			return 0, true
		}
		n, err := strconv.Atoi(parts[idx])
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	maj, ok1 := parse(0)
	minN, ok2 := parse(1)
	pat, ok3 := parse(2)
	if !ok1 || !ok2 || !ok3 {
		return 1000
	}
	code := min(maj, 2147)*1_000_000 + min(minN, 999)*1_000 + min(pat, 999)
	if code <= 0 {
		return 1000
	}
	return code
}

// ohosSemVer normalizes version into strict MAJOR.MINOR.PATCH SemVer for oh-package.json5.
func ohosSemVer(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	suffix := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		suffix = v[i:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := [3]int{0, 1, 0}
	okAll := len(parts) >= 1 && len(parts) <= 3
	for i := 0; i < 3 && okAll; i++ {
		if i < len(parts) {
			n, err := strconv.Atoi(parts[i])
			if err != nil || n < 0 {
				okAll = false
				break
			}
			nums[i] = n
		} else {
			nums[i] = 0
		}
	}
	if !okAll {
		return "1.0.0"
	}
	return fmt.Sprintf("%d.%d.%d%s", nums[0], nums[1], nums[2], suffix)
}

// ohosVendor returns the vendor string for AppScope/app.json5.
func (c *Config) ohosVendor() string {
	if v := strings.TrimSpace(c.OHOS.Vendor); v != "" {
		return v
	}
	if p := c.publisher(); p != "" {
		return p
	}
	if segs := strings.Split(c.Identifier, "."); len(segs) >= 2 && segs[1] != "" {
		return segs[1]
	}
	return "mygo"
}

// ohosBuildTags returns the single -tags argument for compiling a HarmonyOS Go engine.
// All build tags must be combined into one -tags flag because go build lets later
// -tags flags override earlier ones.
func ohosBuildTags(debug bool) []string {
	tags := []string{"mygo_ohos"}
	add := func(t string) {
		for _, part := range strings.Split(t, ",") {
			if part = strings.TrimSpace(part); part != "" && !slices.Contains(tags, part) {
				tags = append(tags, part)
			}
		}
	}
	for _, f := range strings.Fields(os.Getenv("GOFLAGS")) {
		if v, ok := strings.CutPrefix(strings.TrimLeft(f, "-"), "tags="); ok && v != "" {
			add(v)
		}
	}
	if !debug {
		add(noDownloadTag)
		if !keepInspector() {
			add(inspectorTag)
		}
	}
	return []string{"-tags", strings.Join(tags, ",")}
}

// ohosQualifier maps a BCP 47 language tag to a HarmonyOS resource qualifier
// directory name (such as "zh_CN", "zh_TW", "pt_BR", "de"), or "" for English base.
func ohosQualifier(tag string) string {
	lang, script, region := tagParts(tag)
	if lang == "" || lang == "en" && region == "" && script == "" {
		return ""
	}
	if lang == "zh" {
		switch {
		case region == "HK":
			return "zh_HK"
		case script == "Hant" || region == "TW" || region == "MO":
			return "zh_TW"
		default:
			return "zh_CN"
		}
	}
	if region != "" {
		return lang + "_" + region
	}
	return lang
}

// pangoRoots are the shared libraries internal/text/pango_linux.go opens with purego.
var ohosPangoRoots = []string{
	"libgobject-2.0.so.0",
	"libpango-1.0.so.0",
	"libglib-2.0.so.0",
	"libpangocairo-1.0.so.0",
	"libcairo.so.2",
	"libharfbuzz.so.0",
	"libpangoft2-1.0.so.0",
	"libfontconfig.so.1",
}

func isOhosSystemLib(name string) bool {
	switch name {
	case "libc.so", "libm.so", "libdl.so", "libpthread.so",
		"ld-musl-aarch64.so.1", "ld-musl-x86_64.so.1":
		return true
	}
	return false
}

// resolveOhosLibs walks the transitive DT_NEEDED closure starting from roots inside
// libDir and returns a sorted slice of [soname, resolvedHostPath] pairs.
func resolveOhosLibs(libDir string, roots []string) ([][2]string, error) {
	seen := make(map[string]string)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := seen[name]; ok || isOhosSystemLib(name) {
			continue
		}
		cand := filepath.Join(libDir, name)
		real, err := filepath.EvalSymlinks(cand)
		if err != nil {
			return nil, fmt.Errorf("resolve %s in %s: %w", name, libDir, err)
		}
		ef, err := elf.Open(real)
		if err != nil {
			return nil, fmt.Errorf("open elf %s: %w", real, err)
		}
		machine := ef.Machine
		deps, err := ef.ImportedLibraries()
		ef.Close()
		if err != nil {
			return nil, fmt.Errorf("read DT_NEEDED of %s: %w", real, err)
		}
		if machine != elf.EM_AARCH64 {
			return nil, fmt.Errorf("%s is %s, want EM_AARCH64 musl shared library", real, machine)
		}
		seen[name] = real
		for _, dep := range deps {
			if _, ok := seen[dep]; !ok && !isOhosSystemLib(dep) {
				queue = append(queue, dep)
			}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, name := range names {
		out = append(out, [2]string{name, seen[name]})
	}
	return out, nil
}

// findOhosLibDir locates the musl aarch64 shared library directory containing Pango.
func findOhosLibDir() string {
	if env := strings.TrimSpace(os.Getenv("MYGO_OHOS_LIB_DIR")); env != "" {
		if hasAarch64Lib(filepath.Join(env, "libpango-1.0.so.0")) {
			return env
		}
		return ""
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".harmonybrew", "lib"))
	}
	candidates = append(candidates,
		"/storage/Users/currentUser/.harmonybrew/lib",
		"/usr/local/lib/ohos-arm64",
	)
	for _, dir := range candidates {
		if hasAarch64Lib(filepath.Join(dir, "libpango-1.0.so.0")) {
			return dir
		}
	}
	return ""
}

func hasAarch64Lib(path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	ef, err := elf.Open(real)
	if err != nil {
		return false
	}
	defer ef.Close()
	return ef.Machine == elf.EM_AARCH64
}

// findOhosToolchain discovers hvigorw and DEVECO_SDK_HOME from the environment
// or standard HarmonyOS Command Line Tools installations.
func findOhosToolchain() (hvigorw, sdkHome string, extraEnv []string) {
	sdkHome = strings.TrimSpace(os.Getenv("DEVECO_SDK_HOME"))
	if p, err := exec.LookPath("hvigorw"); err == nil {
		hvigorw = p
	}
	var cltDirs []string
	if envCLT := strings.TrimSpace(os.Getenv("OHOS_CLT_HOME")); envCLT != "" {
		cltDirs = append(cltDirs, envCLT)
	}
	if matches, _ := filepath.Glob("/data/service/hnp/hmos-clt.org/hmos-clt_*"); len(matches) > 0 {
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		cltDirs = append(cltDirs, matches...)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cltDirs = append(cltDirs,
			filepath.Join(home, "command-line-tools"),
			filepath.Join(home, "Library", "Huawei", "command-line-tools"),
		)
	}
	cltDirs = append(cltDirs, "/opt/command-line-tools")

	var prependPaths []string
	for _, clt := range cltDirs {
		if !isDir(clt) {
			continue
		}
		if sdkHome == "" && isDir(filepath.Join(clt, "sdk")) {
			sdkHome = filepath.Join(clt, "sdk")
		}
		if hvigorw == "" && fileExists(filepath.Join(clt, "hvigor", "bin", "hvigorw")) {
			hvigorw = filepath.Join(clt, "hvigor", "bin", "hvigorw")
		}
		for _, sub := range []string{
			filepath.Join(clt, "node", "bin"),
			filepath.Join(clt, "ohpm", "bin"),
			filepath.Join(clt, "hvigor", "bin"),
			filepath.Join(clt, "sdk", "default", "openharmony", "toolchains"),
		} {
			if isDir(sub) {
				prependPaths = append(prependPaths, sub)
			}
		}
		break
	}
	if isDir("/data/service/hnp/bin") {
		prependPaths = append(prependPaths, "/data/service/hnp/bin")
	}
	if sdkHome != "" {
		extraEnv = append(extraEnv, "DEVECO_SDK_HOME="+sdkHome)
	}
	if strings.TrimSpace(os.Getenv("TMPDIR")) == "" {
		if td := ohosTempDir(); td != "" {
			extraEnv = append(extraEnv, "TMPDIR="+td)
		}
	}
	if len(prependPaths) > 0 {
		curPath := os.Getenv("PATH")
		if curPath != "" {
			prependPaths = append(prependPaths, curPath)
		}
		extraEnv = append(extraEnv, "PATH="+strings.Join(prependPaths, string(os.PathListSeparator)))
	}
	return hvigorw, sdkHome, extraEnv
}

// findOhosHostDir locates the MyGo HarmonyOS host project template (internal/ohos/host).
func findOhosHostDir(c *Config) (string, error) {
	check := func(dir string) bool {
		return fileExists(filepath.Join(dir, "AppScope", "app.json5")) &&
			fileExists(filepath.Join(dir, "entry", "src", "main", "cpp", "host.cpp"))
	}
	if env := strings.TrimSpace(os.Getenv("MYGO_OHOS_HOST_DIR")); env != "" {
		if check(env) {
			return env, nil
		}
		return "", fmt.Errorf("MYGO_OHOS_HOST_DIR %s does not contain a valid HarmonyOS host template", env)
	}
	var out bytes.Buffer
	cmd := goCommand(c.root, nil, "list", "-m", "-f", "{{.Dir}}", "github.com/egoist/mygo")
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() == nil {
		if modDir := strings.TrimSpace(out.String()); modDir != "" {
			cand := filepath.Join(modDir, "internal", "ohos", "host")
			if check(cand) {
				return cand, nil
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		for d, i := filepath.Dir(exe), 0; i < 5; d, i = filepath.Dir(d), i+1 {
			cand := filepath.Join(d, "internal", "ohos", "host")
			if check(cand) {
				return cand, nil
			}
		}
	}
	return "", errors.New("cannot find github.com/egoist/mygo/internal/ohos/host; ensure github.com/egoist/mygo is in go.mod or set MYGO_OHOS_HOST_DIR")
}

// copyOhosHostTemplate copies hostTemplateDir into stageHostDir, skipping transient
// build directories and ensuring every copied file is owner-writable even when
// sourced from a read-only Go module cache ($GOMODCACHE).
func copyOhosHostTemplate(hostTemplateDir, stageHostDir string) error {
	skipDirs := map[string]bool{
		".cxx":        true,
		".hvigor":     true,
		".git":        true,
		"build":       true,
		"hnp":         true,
		"oh_modules":  true,
		"node_modules": true,
	}
	return filepath.WalkDir(hostTemplateDir, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(hostTemplateDir, src)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(stageHostDir, 0o755)
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		dst := filepath.Join(stageHostDir, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(src)
			if err != nil {
				return err
			}
			return os.Symlink(link, dst)
		case mode.IsDir():
			return os.MkdirAll(dst, 0o755)
		case mode.IsRegular():
			perm := mode.Perm() | 0o644
			if mode.Perm()&0o111 != 0 {
				perm = 0o755
			}
			return copyFile(src, dst, perm)
		}
		return nil
	})
}

// writeOhosJSON writes v formatted with 2-space indentation and a trailing newline.
func writeOhosJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

type ohosStringEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func writeOhosStringTable(path string, entries []ohosStringEntry) error {
	return writeOhosJSON(path, map[string]any{"string": entries})
}

// writeOhosHostProject stages the HarmonyOS ArkTS/C++ host project for c into
// stageHostDir and generates app.json5, module.json5, string catalogs and PNG icons.
func writeOhosHostProject(c *Config, hostTemplateDir, stageHostDir string, debug bool) error {
	if err := copyOhosHostTemplate(hostTemplateDir, stageHostDir); err != nil {
		return err
	}

	// Link entry/oh_modules/libmygo_host.so -> ../src/main/cpp/types/libmygo_host
	// so ArkTS N-API type checking resolves offline without running ohpm.
	ohModDir := filepath.Join(stageHostDir, "entry", "oh_modules")
	if err := os.MkdirAll(ohModDir, 0o755); err != nil {
		return err
	}
	_ = os.Symlink(filepath.FromSlash("../src/main/cpp/types/libmygo_host"), filepath.Join(ohModDir, "libmygo_host.so"))

	semver := ohosSemVer(c.Version)
	desc := strings.TrimSpace(c.Linux.Comment)
	if desc == "" {
		desc = c.Name
	}

	// 1. AppScope/app.json5
	if err := writeOhosJSON(filepath.Join(stageHostDir, "AppScope", "app.json5"), map[string]any{
		"app": map[string]any{
			"bundleName":  c.Identifier,
			"vendor":      c.ohosVendor(),
			"versionCode": c.ohosVersionCode(),
			"versionName": c.Version,
			"icon":        "$media:layered_image",
			"label":       "$string:app_name",
		},
	}); err != nil {
		return err
	}

	// 2. Root oh-package.json5 and entry/oh-package.json5
	if err := writeOhosJSON(filepath.Join(stageHostDir, "oh-package.json5"), map[string]any{
		"modelVersion":    "5.0.0",
		"name":            c.slug(),
		"version":         semver,
		"description":     desc,
		"main":            "",
		"author":          c.ohosVendor(),
		"license":         c.License,
		"dependencies":    map[string]any{},
		"devDependencies": map[string]any{},
	}); err != nil {
		return err
	}
	if err := writeOhosJSON(filepath.Join(stageHostDir, "entry", "oh-package.json5"), map[string]any{
		"name":        "entry",
		"version":     semver,
		"description": desc,
		"main":        "",
		"author":      c.ohosVendor(),
		"license":     c.License,
		"dependencies": map[string]any{
			"libmygo_host.so": "file:./src/main/cpp/types/libmygo_host",
		},
	}); err != nil {
		return err
	}

	// 3. entry/build-profile.json5 (optimize native host with -O2 in release builds)
	cppFlags := ""
	if !debug {
		cppFlags = "-O2"
	}
	if err := writeOhosJSON(filepath.Join(stageHostDir, "entry", "build-profile.json5"), map[string]any{
		"apiType": "stageMode",
		"buildOption": map[string]any{
			"externalNativeOptions": map[string]any{
				"path":      "./src/main/cpp/CMakeLists.txt",
				"arguments": "",
				"cppFlags":  cppFlags,
			},
		},
		"buildOptionSet": []any{
			map[string]any{
				"name": "release",
				"arkOptions": map[string]any{
					"obfuscation": map[string]any{
						"ruleOptions": map[string]any{
							"enable": true,
							"files":  []string{"./obfuscation-rules.txt"},
						},
					},
				},
			},
		},
		"targets": []any{
			map[string]any{"name": "default"},
		},
	}); err != nil {
		return err
	}

	// 4. Base & localized string resources (including auto-classified permission reasons)
	modCfg, permStrings := ohosModuleAndPermStrings(c)
	baseStrings := append([]ohosStringEntry{
		{Name: "module_desc", Value: desc},
		{Name: "EntryAbility_desc", Value: desc},
		{Name: "EntryAbility_label", Value: c.Name},
	}, permStrings...)
	if err := writeOhosStringTable(
		filepath.Join(stageHostDir, "AppScope", "resources", "base", "element", "string.json"),
		[]ohosStringEntry{{Name: "app_name", Value: c.Name}},
	); err != nil {
		return err
	}
	if err := writeOhosStringTable(
		filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "element", "string.json"),
		baseStrings,
	); err != nil {
		return err
	}

	if langs, err := c.languages(); err == nil {
		seenQual := map[string]bool{}
		for _, l := range langs {
			qual := ohosQualifier(l.Tag)
			if qual == "" || seenQual[qual] || (l.Name == "" && l.Description == "") {
				continue
			}
			seenQual[qual] = true
			locName := c.Name
			if l.Name != "" {
				locName = l.Name
			}
			locDesc := desc
			if l.Description != "" {
				locDesc = l.Description
			}
			if err := writeOhosStringTable(
				filepath.Join(stageHostDir, "AppScope", "resources", qual, "element", "string.json"),
				[]ohosStringEntry{{Name: "app_name", Value: locName}},
			); err != nil {
				return err
			}
			locStrings := append([]ohosStringEntry{
				{Name: "module_desc", Value: locDesc},
				{Name: "EntryAbility_desc", Value: locDesc},
				{Name: "EntryAbility_label", Value: locName},
			}, permStrings...)
			if err := writeOhosStringTable(
				filepath.Join(stageHostDir, "entry", "src", "main", "resources", qual, "element", "string.json"),
				locStrings,
			); err != nil {
				return err
			}
		}
	}

	// 5. entry/src/main/module.json5
	if err := writeOhosJSON(
		filepath.Join(stageHostDir, "entry", "src", "main", "module.json5"),
		modCfg,
	); err != nil {
		return err
	}

	// 6. Layered icons ($media:layered_image: 1024x1024 background.png + foreground.png, startIcon.png, app_icon.png)
	if err := writeOhosIcons(c, stageHostDir); err != nil {
		return err
	}
	return nil
}

// ohosKnownReasonPermissions maps HarmonyOS permissions that require (or benefit
// from) reason + EntryAbility usedScene in module.json5 to their string key and
// default English usage description. System-grant and sandbox permissions such
// as ohos.permission.CUSTOM_SANDBOX intentionally stay out of this table so
// hvigor packages them as clean {"name": "..."} entries.
var ohosKnownReasonPermissions = map[string][2]string{
	"ohos.permission.READ_PASTEBOARD": {
		"read_pasteboard_reason",
		"Used to paste text, links, files, and images from the system clipboard into the application.",
	},
	"ohos.permission.KEEP_BACKGROUND_RUNNING": {
		"keep_background_reason",
		"Used to keep audio playing smoothly while windows are minimized or in the background.",
	},
	"ohos.permission.MICROPHONE": {
		"microphone_reason",
		"Used to capture audio input from the microphone when requested by the user.",
	},
	"ohos.permission.CAMERA": {
		"camera_reason",
		"Used to capture photos or video from the camera when requested by the user.",
	},
	"ohos.permission.READ_WRITE_DOWNLOAD_DIRECTORY": {
		"read_write_download_directory_reason",
		"Used to read and save files in the user's Downloads folder.",
	},
	"ohos.permission.READ_WRITE_DOCUMENTS_DIRECTORY": {
		"read_write_documents_directory_reason",
		"Used to read and save files in the user's Documents folder.",
	},
	"ohos.permission.READ_WRITE_DESKTOP_DIRECTORY": {
		"read_write_desktop_directory_reason",
		"Used to read and save files on the user's Desktop.",
	},
	"ohos.permission.READ_WRITE_IMAGES": {
		"read_write_images_reason",
		"Used to read and save images in the user's media library.",
	},
	"ohos.permission.READ_WRITE_VIDEO": {
		"read_write_video_reason",
		"Used to read and save videos in the user's media library.",
	},
	"ohos.permission.READ_WRITE_AUDIO": {
		"read_write_audio_reason",
		"Used to read and save audio files in the user's media library.",
	},
	"ohos.permission.READ_IMAGEVIDEO": {
		"read_imagevideo_reason",
		"Used to read images and videos selected by the user.",
	},
	"ohos.permission.WRITE_IMAGEVIDEO": {
		"write_imagevideo_reason",
		"Used to save images and videos to the user's media library.",
	},
	"ohos.permission.SHORT_TERM_WRITE_IMAGEVIDEO": {
		"short_term_write_imagevideo_reason",
		"Used to save images and videos to the user's media library.",
	},
	"ohos.permission.READ_AUDIO": {
		"read_audio_reason",
		"Used to read audio files from the user's media library.",
	},
	"ohos.permission.WRITE_AUDIO": {
		"write_audio_reason",
		"Used to save audio files to the user's media library.",
	},
	"ohos.permission.MEDIA_LOCATION": {
		"media_location_reason",
		"Used to read location metadata attached to user media files.",
	},
	"ohos.permission.LOCATION": {
		"location_reason",
		"Used to provide location-aware features when requested by the user.",
	},
	"ohos.permission.APPROXIMATELY_LOCATION": {
		"approximately_location_reason",
		"Used to provide approximate location features when requested by the user.",
	},
	"ohos.permission.LOCATION_IN_BACKGROUND": {
		"location_in_background_reason",
		"Used to update location while the application is in the background.",
	},
	"ohos.permission.ACCESS_BLUETOOTH": {
		"access_bluetooth_reason",
		"Used to discover and connect to nearby Bluetooth devices.",
	},
	"ohos.permission.READ_CALENDAR": {
		"read_calendar_reason",
		"Used to read calendar events when requested by the user.",
	},
	"ohos.permission.WRITE_CALENDAR": {
		"write_calendar_reason",
		"Used to create or update calendar events when requested by the user.",
	},
	"ohos.permission.READ_WHOLE_CALENDAR": {
		"read_whole_calendar_reason",
		"Used to read calendar events when requested by the user.",
	},
	"ohos.permission.WRITE_WHOLE_CALENDAR": {
		"write_whole_calendar_reason",
		"Used to create or update calendar events when requested by the user.",
	},
	"ohos.permission.READ_CONTACTS": {
		"read_contacts_reason",
		"Used to read contacts when requested by the user.",
	},
	"ohos.permission.WRITE_CONTACTS": {
		"write_contacts_reason",
		"Used to save contacts when requested by the user.",
	},
	"ohos.permission.ACTIVITY_MOTION": {
		"activity_motion_reason",
		"Used to read motion and fitness activity when requested by the user.",
	},
	"ohos.permission.READ_HEALTH_DATA": {
		"read_health_data_reason",
		"Used to read health data when requested by the user.",
	},
	"ohos.permission.DISTRIBUTED_DATASYNC": {
		"distributed_datasync_reason",
		"Used to synchronize application data across the user's HarmonyOS devices.",
	},
	"ohos.permission.APP_TRACKING_CONSENT": {
		"app_tracking_consent_reason",
		"Used to request user consent for cross-application tracking.",
	},
}

func ohosPermReasonKey(perm string) string {
	trimmed := strings.TrimPrefix(perm, "ohos.permission.")
	var b strings.Builder
	for _, r := range strings.ToLower(trimmed) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		s = "custom"
	}
	return s + "_reason"
}

func ohosPermissionEntry(c *Config, name string) (map[string]any, *ohosStringEntry) {
	customReason := ""
	if c.OHOS.PermissionReasons != nil {
		customReason = strings.TrimSpace(c.OHOS.PermissionReasons[name])
	}
	if known, ok := ohosKnownReasonPermissions[name]; ok {
		reasonText := known[1]
		if customReason != "" {
			reasonText = customReason
		}
		return map[string]any{
			"name":   name,
			"reason": "$string:" + known[0],
			"usedScene": map[string]any{
				"abilities": []string{"EntryAbility"},
				"when":      "inuse",
			},
		}, &ohosStringEntry{Name: known[0], Value: reasonText}
	}
	if customReason != "" {
		key := ohosPermReasonKey(name)
		return map[string]any{
			"name":   name,
			"reason": "$string:" + key,
			"usedScene": map[string]any{
				"abilities": []string{"EntryAbility"},
				"when":      "inuse",
			},
		}, &ohosStringEntry{Name: key, Value: customReason}
	}
	return map[string]any{"name": name}, nil
}

func ohosModuleConfig(c *Config) map[string]any {
	cfg, _ := ohosModuleAndPermStrings(c)
	return cfg
}

func ohosModuleAndPermStrings(c *Config) (map[string]any, []ohosStringEntry) {
	deviceTypes := c.OHOS.DeviceTypes
	if len(deviceTypes) == 0 {
		deviceTypes = []string{"2in1", "tablet"}
	}
	bgModes := c.OHOS.BackgroundModes
	if bgModes == nil {
		bgModes = []string{"audioPlayback"}
	}

	defaultPerms := []string{
		"ohos.permission.INTERNET",
		"ohos.permission.GET_NETWORK_INFO",
		"ohos.permission.PREPARE_APP_TERMINATE",
		"ohos.permission.KEEP_BACKGROUND_RUNNING",
		"ohos.permission.FILE_ACCESS_PERSIST",
		"ohos.permission.READ_PASTEBOARD",
	}
	var perms []map[string]any
	var reasonStrings []ohosStringEntry
	seenPerm := map[string]bool{}
	seenKey := map[string]bool{}
	addPerm := func(raw string) {
		name := strings.TrimSpace(raw)
		if name == "" || seenPerm[name] {
			return
		}
		seenPerm[name] = true
		entry, strEntry := ohosPermissionEntry(c, name)
		perms = append(perms, entry)
		if strEntry != nil && !seenKey[strEntry.Name] {
			seenKey[strEntry.Name] = true
			reasonStrings = append(reasonStrings, *strEntry)
		}
	}
	for _, p := range defaultPerms {
		addPerm(p)
	}
	for _, extra := range c.OHOS.Permissions {
		addPerm(extra)
	}

	skills := []any{
		map[string]any{
			"entities": []string{"entity.system.home"},
			"actions":  []string{"action.system.home"},
		},
	}
	if len(c.URLSchemes) > 0 {
		var uris []any
		seen := map[string]bool{}
		for _, s := range c.URLSchemes {
			if s != "" && !seen[s] {
				seen[s] = true
				uris = append(uris, map[string]any{"scheme": s})
			}
		}
		if len(uris) > 0 {
			skills = append(skills, map[string]any{
				"entities": []string{"entity.system.default", "entity.system.browsable"},
				"actions":  []string{"ohos.want.action.viewData", "action.system.view"},
				"uris":     uris,
			})
		}
	}
	fileURIs := []any{map[string]any{"scheme": "file"}}
	seenMime := map[string]bool{}
	for _, fa := range c.FileAssociations {
		if mt := strings.TrimSpace(fa.MimeType); mt != "" && !seenMime[mt] {
			seenMime[mt] = true
			fileURIs = append(fileURIs, map[string]any{"scheme": "file", "type": mt})
		}
	}
	skills = append(skills, map[string]any{
		"entities": []string{"entity.system.default", "entity.system.browsable"},
		"actions":  []string{"ohos.want.action.viewData", "action.system.view"},
		"uris":     fileURIs,
	})

	ability := map[string]any{
		"name":                  "EntryAbility",
		"srcEntry":              "./ets/entryability/EntryAbility.ets",
		"description":           "$string:EntryAbility_desc",
		"icon":                  "$media:layered_image",
		"label":                 "$string:EntryAbility_label",
		"startWindowIcon":       "$media:startIcon",
		"startWindowBackground": "$color:start_window_background",
		"launchType":            "singleton",
		"exported":              true,
		"skills":                skills,
	}
	if len(bgModes) > 0 {
		ability["backgroundModes"] = bgModes
	}

	return map[string]any{
		"module": map[string]any{
			"name":                "entry",
			"type":                "entry",
			"description":         "$string:module_desc",
			"mainElement":         "EntryAbility",
			"deviceTypes":         deviceTypes,
			"deliveryWithInstall": true,
			"installationFree":    false,
			"hnpPackages": []any{
				map[string]any{
					"package": "mygo.hnp",
					"type":    "private",
				},
			},
			"pages":              "$profile:main_pages",
			"requestPermissions": perms,
			"abilities":          []any{ability},
		},
	}, reasonStrings
}

func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok && n.Bounds().Min == (image.Point{}) {
		return n
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			out.SetNRGBA(x, y, color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA))
		}
	}
	return out
}

// solidIconCrop trims macOS-style transparent squircle margins and outer drop
// shadows so the solid icon body fills HarmonyOS's 1024x1024 layered canvas.
func solidIconCrop(src *image.NRGBA) image.Image {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	findBox := func(minAlpha uint8) (image.Rectangle, bool) {
		minX, minY := w, h
		maxX, maxY := -1, -1
		for y := range h {
			row := src.Pix[y*src.Stride : y*src.Stride+w*4]
			for x := range w {
				if row[x*4+3] >= minAlpha {
					if x < minX {
						minX = x
					}
					if x > maxX {
						maxX = x
					}
					if y < minY {
						minY = y
					}
					if y > maxY {
						maxY = y
					}
				}
			}
		}
		if maxX < minX || maxY < minY {
			return image.Rectangle{}, false
		}
		return image.Rect(minX, minY, maxX+1, maxY+1), true
	}
	box, ok := findBox(224)
	if !ok {
		box, ok = findBox(16)
	}
	if !ok {
		return src
	}
	side := max(box.Dx(), box.Dy())
	// When the icon has a macOS transparent inset (>= 8% total margin), shave a
	// thin outer rim to drop baked-in bevel/shadow pixels before corner fill.
	if side <= w*92/100 && side > 32 {
		trim := max(2, side/180)
		if side-2*trim >= 16 {
			side -= 2 * trim
		}
	}
	cx := (box.Min.X + box.Max.X) / 2
	cy := (box.Min.Y + box.Max.Y) / 2
	x0 := min(max(cx-side/2, 0), max(0, w-side))
	y0 := min(max(cy-side/2, 0), max(0, h-side))
	return src.SubImage(image.Rect(x0, y0, min(w, x0+side), min(h, y0+side)))
}

func isFilledSquircleIcon(cropped *image.NRGBA) bool {
	w, h := cropped.Bounds().Dx(), cropped.Bounds().Dy()
	if w < 8 || h < 8 {
		return true
	}
	d := max(2, min(w, h)*3/100)
	mx, my := w/2, h/2
	opaqueEdges := 0
	for _, pt := range [4][2]int{{mx, d}, {mx, h - 1 - d}, {d, my}, {w - 1 - d, my}} {
		if cropped.NRGBAAt(pt[0], pt[1]).A >= 224 {
			opaqueEdges++
		}
	}
	return opaqueEdges >= 3
}

// extrapolateOpaqueBackground produces a 1024x1024 100%-opaque HarmonyOS
// background layer by extending interior opaque colors outward into rounded
// corner cutouts via a two-pass Chamfer nearest-seed transform.
func extrapolateOpaqueBackground(src *image.NRGBA) *image.NRGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	n := w * h
	const inf = int32(1 << 29)
	dist := make([]int32, n)
	seed := make([]int32, n)
	hasSeed := false
	seedAlpha := uint8(248)
	for pass := range 2 {
		if pass == 1 && hasSeed {
			break
		}
		if pass == 1 {
			seedAlpha = 128
		}
		for i := range n {
			if src.Pix[i*4+3] >= seedAlpha {
				dist[i] = 0
				seed[i] = int32(i)
				hasSeed = true
			} else {
				dist[i] = inf
				seed[i] = -1
			}
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	if !hasSeed {
		for i := range n {
			c := src.Pix[i*4 : i*4+4]
			a := uint16(c[3])
			out.Pix[i*4+0] = uint8((uint16(c[0])*a + 255*(255-a) + 127) / 255)
			out.Pix[i*4+1] = uint8((uint16(c[1])*a + 255*(255-a) + 127) / 255)
			out.Pix[i*4+2] = uint8((uint16(c[2])*a + 255*(255-a) + 127) / 255)
			out.Pix[i*4+3] = 255
		}
		return out
	}

	relax := func(idx, nb int, step int32) {
		if d := dist[nb] + step; d < dist[idx] {
			dist[idx] = d
			seed[idx] = seed[nb]
		}
	}
	for y := range h {
		row := y * w
		for x := range w {
			idx := row + x
			if dist[idx] == 0 {
				continue
			}
			if y > 0 {
				if x > 0 {
					relax(idx, idx-w-1, 4)
				}
				relax(idx, idx-w, 3)
				if x+1 < w {
					relax(idx, idx-w+1, 4)
				}
			}
			if x > 0 {
				relax(idx, idx-1, 3)
			}
		}
	}
	for y := h - 1; y >= 0; y-- {
		row := y * w
		for x := w - 1; x >= 0; x-- {
			idx := row + x
			if dist[idx] == 0 {
				continue
			}
			if y+1 < h {
				if x+1 < w {
					relax(idx, idx+w+1, 4)
				}
				relax(idx, idx+w, 3)
				if x > 0 {
					relax(idx, idx+w-1, 4)
				}
			}
			if x+1 < w {
				relax(idx, idx+1, 3)
			}
		}
	}

	for i := range n {
		p := src.Pix[i*4 : i*4+4]
		if p[3] >= seedAlpha || seed[i] < 0 {
			out.Pix[i*4+0] = p[0]
			out.Pix[i*4+1] = p[1]
			out.Pix[i*4+2] = p[2]
			out.Pix[i*4+3] = 255
			continue
		}
		sp := src.Pix[int(seed[i])*4 : int(seed[i])*4+4]
		out.Pix[i*4+0] = sp[0]
		out.Pix[i*4+1] = sp[1]
		out.Pix[i*4+2] = sp[2]
		out.Pix[i*4+3] = 255
	}
	return out
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeSquareIconFile(path, label string) (*image.NRGBA, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := validateIcon(raw); err != nil {
		return nil, fmt.Errorf("%s %s: %w", label, path, err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", label, path, err)
	}
	return toNRGBA(img), nil
}

func writeOhosIcons(c *Config, stageHostDir string) error {
	const layerSide = 1024

	var baseImg *image.NRGBA
	if c.Icon != "" {
		var err error
		baseImg, err = decodeSquareIconFile(c.path(c.Icon), "icon")
		if err != nil {
			return err
		}
	} else if c.OHOS.IconForeground == "" && c.OHOS.IconBackground == "" {
		// If the host template already carries prebuilt background/foreground
		// layers, keep them; otherwise synthesize layers from defaultIcon().
		appBg := filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "background.png")
		entBg := filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "background.png")
		if fileExists(appBg) && fileExists(entBg) {
			return nil
		}
		img, err := png.Decode(bytes.NewReader(defaultIcon()))
		if err != nil {
			return err
		}
		baseImg = toNRGBA(img)
	}

	var bgImg, fgImg *image.NRGBA
	var croppedForAux image.Image

	if c.OHOS.IconBackground != "" || c.OHOS.IconForeground != "" {
		if c.OHOS.IconBackground != "" {
			rawBg, err := decodeSquareIconFile(c.path(c.OHOS.IconBackground), "ohos.iconBackground")
			if err != nil {
				return err
			}
			bgImg = extrapolateOpaqueBackground(resize(rawBg, layerSide))
		} else {
			bgImg = image.NewNRGBA(image.Rect(0, 0, layerSide, layerSide))
			for i := 0; i < len(bgImg.Pix); i += 4 {
				bgImg.Pix[i+0], bgImg.Pix[i+1], bgImg.Pix[i+2], bgImg.Pix[i+3] = 255, 255, 255, 255
			}
		}
		if c.OHOS.IconForeground != "" {
			rawFg, err := decodeSquareIconFile(c.path(c.OHOS.IconForeground), "ohos.iconForeground")
			if err != nil {
				return err
			}
			fgImg = resize(rawFg, layerSide)
		} else {
			fgImg = image.NewNRGBA(image.Rect(0, 0, layerSide, layerSide))
		}
		if baseImg != nil {
			croppedForAux = solidIconCrop(baseImg)
		} else {
			croppedForAux = bgImg
		}
	} else {
		cropped := solidIconCrop(baseImg)
		croppedForAux = cropped
		scaled := resize(cropped, layerSide)
		if isFilledSquircleIcon(scaled) {
			bgImg = extrapolateOpaqueBackground(scaled)
			fgImg = image.NewNRGBA(image.Rect(0, 0, layerSide, layerSide))
		} else {
			bgImg = image.NewNRGBA(image.Rect(0, 0, layerSide, layerSide))
			for i := 0; i < len(bgImg.Pix); i += 4 {
				bgImg.Pix[i+0], bgImg.Pix[i+1], bgImg.Pix[i+2], bgImg.Pix[i+3] = 255, 255, 255, 255
			}
			fgImg = image.NewNRGBA(image.Rect(0, 0, layerSide, layerSide))
			const glyphSide = 680
			glyph := resize(cropped, glyphSide)
			pad := (layerSide - glyphSide) / 2
			for y := range glyphSide {
				copy(
					fgImg.Pix[(pad+y)*fgImg.Stride+pad*4:(pad+y)*fgImg.Stride+(pad+glyphSide)*4],
					glyph.Pix[y*glyph.Stride:y*glyph.Stride+glyphSide*4],
				)
			}
		}
	}

	bgPNG, err := encodePNG(bgImg)
	if err != nil {
		return err
	}
	fgPNG, err := encodePNG(fgImg)
	if err != nil {
		return err
	}
	icon512, err := encodePNG(resize(croppedForAux, 512))
	if err != nil {
		return err
	}
	icon256, err := encodePNG(resize(croppedForAux, 256))
	if err != nil {
		return err
	}
	layeredJSON := []byte("{\n  \"layered-image\": {\n    \"background\": \"$media:background\",\n    \"foreground\": \"$media:foreground\"\n  }\n}\n")

	targets := []struct {
		path string
		data []byte
	}{
		{filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "layered_image.json"), layeredJSON},
		{filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "background.png"), bgPNG},
		{filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "foreground.png"), fgPNG},
		{filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "app_icon.png"), icon512},
		{filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "layered_image.json"), layeredJSON},
		{filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "background.png"), bgPNG},
		{filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "foreground.png"), fgPNG},
		{filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "app_icon.png"), icon512},
		{filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "media", "startIcon.png"), icon256},
	}
	for _, t := range targets {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(t.path, t.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// packOhosHNP creates a HarmonyOS Native Package (.hnp) zip archive containing
// bin/mygo-engine (0755), optional app resources beside bin/mygo-engine, the
// transitive musl Pango shared library closure in lib/*.so (0755), fonts.conf
// in etc/fonts/fonts.conf (0644), and hnp.json (0644).
func packOhosHNP(c *Config, engineBin, libDir, fontsConf string, res []resource, outPath string) error {
	return packOhosHNPWithEnv(c, engineBin, libDir, fontsConf, res, nil, outPath)
}

func packOhosHNPWithEnv(c *Config, engineBin, libDir, fontsConf string, res []resource, devEnv []string, outPath string) error {
	root := "mygo_" + fsName(c.Version)

	var libs [][2]string
	if libDir != "" {
		var err error
		libs, err = resolveOhosLibs(libDir, ohosPangoRoots)
		if err != nil {
			return err
		}
	}

	conf, err := json.MarshalIndent(map[string]any{
		"type":    "hnp-config",
		"name":    "mygo",
		"version": fsName(c.Version),
		"install": map[string]any{
			"links": []any{
				map[string]any{"source": "bin/mygo-engine", "target": "mygo-engine"},
			},
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	conf = append(conf, '\n')

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}

	when := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	addedDirs := map[string]bool{}
	var addDir func(d string) error
	addDir = func(d string) error {
		d = strings.Trim(filepath.ToSlash(d), "/")
		if d == "" || addedDirs[d] {
			return nil
		}
		if i := strings.LastIndexByte(d, '/'); i > 0 {
			if err := addDir(d[:i]); err != nil {
				return err
			}
		}
		addedDirs[d] = true
		h := &zip.FileHeader{Name: d + "/", Method: zip.Store, Modified: when}
		h.SetMode(0o755 | os.ModeDir)
		_, err := zw.CreateHeader(h)
		return err
	}
	addFile := func(dst, srcPath string, mode os.FileMode) error {
		dst = filepath.ToSlash(dst)
		if i := strings.LastIndexByte(dst, '/'); i > 0 {
			if err := addDir(dst[:i]); err != nil {
				return err
			}
		}
		sf, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer sf.Close()
		h := &zip.FileHeader{Name: dst, Method: zip.Deflate, Modified: when}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, sf)
		return err
	}
	addBytes := func(dst string, data []byte, mode os.FileMode) error {
		dst = filepath.ToSlash(dst)
		if i := strings.LastIndexByte(dst, '/'); i > 0 {
			if err := addDir(dst[:i]); err != nil {
				return err
			}
		}
		h := &zip.FileHeader{Name: dst, Method: zip.Deflate, Modified: when}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	if err := addDir(root + "/bin"); err != nil {
		return fail(err)
	}
	if err := addFile(root+"/bin/mygo-engine", engineBin, 0o755); err != nil {
		return fail(err)
	}

	// Pack app resources into bin/ next to mygo-engine so mygo.PathResources
	// finds them via os.Executable() without extra configuration.
	if len(res) > 0 {
		resStage, err := os.MkdirTemp(filepath.Dir(outPath), ".res-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(resStage)
		for _, r := range res {
			if dir := filepath.Dir(filepath.Join(resStage, filepath.FromSlash(r.name))); dir != resStage {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fail(err)
				}
			}
		}
		if err := copyResources(res, resStage); err != nil {
			return fail(err)
		}
		err = filepath.WalkDir(resStage, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(resStage, p)
			if err != nil || rel == "." {
				return err
			}
			dst := root + "/bin/" + filepath.ToSlash(rel)
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			switch mode := info.Mode(); {
			case mode&fs.ModeSymlink != 0:
				link, err := os.Readlink(p)
				if err != nil {
					return err
				}
				if i := strings.LastIndexByte(dst, '/'); i > 0 {
					if err := addDir(dst[:i]); err != nil {
						return err
					}
				}
				h := &zip.FileHeader{Name: dst, Method: zip.Store, Modified: when}
				h.SetMode(0o777 | os.ModeSymlink)
				w, err := zw.CreateHeader(h)
				if err != nil {
					return err
				}
				_, err = w.Write([]byte(link))
				return err
			case mode.IsDir():
				return addDir(dst)
			case mode.IsRegular():
				perm := os.FileMode(0o644)
				if mode.Perm()&0o111 != 0 {
					perm = 0o755
				}
				return addFile(dst, p, perm)
			}
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}

	for _, lib := range libs {
		if err := addFile(root+"/lib/"+lib[0], lib[1], 0o755); err != nil {
			return fail(err)
		}
	}
	if fontsConf != "" {
		if err := addFile(root+"/etc/fonts/fonts.conf", fontsConf, 0o644); err != nil {
			return fail(err)
		}
	}
	if len(devEnv) > 0 {
		var envBuf strings.Builder
		for _, kv := range devEnv {
			kv = strings.TrimSpace(kv)
			if strings.IndexByte(kv, '=') > 0 {
				envBuf.WriteString(kv)
				envBuf.WriteByte('\n')
			}
		}
		if envBuf.Len() > 0 {
			if err := addBytes(root+"/etc/mygo.env", []byte(envBuf.String()), 0o644); err != nil {
				return fail(err)
			}
		}
	}
	if err := addBytes(root+"/hnp.json", conf, 0o644); err != nil {
		return fail(err)
	}

	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, outPath)
}

// injectHNPIntoHAP writes hnpPath into hapPath at hnpEntryName (such as
// "hnp/arm64-v8a/mygo.hnp") using zip.Store (uncompressed), preserving all
// other entries without re-compressing them.
func injectHNPIntoHAP(hapPath, hnpEntryName, hnpPath string) error {
	zr, err := zip.OpenReader(hapPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	hnpFile, err := os.Open(hnpPath)
	if err != nil {
		return err
	}
	defer hnpFile.Close()
	hnpInfo, err := hnpFile.Stat()
	if err != nil {
		return err
	}

	tmp := hapPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	fail := func(err error) error {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}

	hnpEntryName = strings.TrimLeft(filepath.ToSlash(hnpEntryName), "/")
	for _, zf := range zr.File {
		if zf.Name == hnpEntryName {
			continue
		}
		if err := zw.Copy(zf); err != nil {
			return fail(err)
		}
	}

	hdr, err := zip.FileInfoHeader(hnpInfo)
	if err != nil {
		return fail(err)
	}
	hdr.Name = hnpEntryName
	hdr.Method = zip.Store
	hdr.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	hdr.SetMode(0o644)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fail(err)
	}
	if _, err := io.Copy(w, hnpFile); err != nil {
		return fail(err)
	}

	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = zr.Close()
	return os.Rename(tmp, hapPath)
}

// buildOhos builds and packages the app for HarmonyOS PC into
// <out>/ohos-<goarch>/<slug>-<version>-<goarch>.hap.
func buildOhos(c *Config, goarch string, opts buildOptions) ([]string, error) {
	if goarch != "arm64" {
		return nil, fmt.Errorf("unsupported HarmonyOS architecture %q: want arm64", goarch)
	}
	hvigorw, sdkHome, toolEnv := findOhosToolchain()
	if hvigorw == "" || sdkHome == "" {
		return nil, errors.New("HarmonyOS Command Line Tools not found: set DEVECO_SDK_HOME and add hvigorw to PATH")
	}
	libDir := findOhosLibDir()
	if libDir == "" && os.Getenv("MYGO_OHOS_SKIP_LIBS") != "1" {
		return nil, errors.New("HarmonyOS musl Pango libraries not found: set MYGO_OHOS_LIB_DIR or install Pango into ~/.harmonybrew/lib")
	}
	hostTemplateDir, err := findOhosHostDir(c)
	if err != nil {
		return nil, err
	}

	out := c.path(c.Out)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(out, ".staging-ohos-"+goarch+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o755); err != nil {
		return nil, err
	}

	res, err := c.resources("ohos", goarch, reservedNames(c, "ohos")...)
	if err != nil {
		return nil, err
	}

	target := "ohos-" + goarch
	if !opts.debug && keepInspector() {
		logf("Keeping the inspector of native UI (MYGO_INSPECTOR=1)")
	}
	ldflags := "-s -w" + packageFlags(c) + updateFlags(c, target)
	if !opts.debug {
		ldflags += " -X github.com/egoist/mygo.production=1"
	}
	ldflags += extraLDFlags()

	ohosWork := filepath.Join(opts.work, "ohos-"+goarch)
	if err := os.MkdirAll(ohosWork, 0o755); err != nil {
		return nil, err
	}
	engineBin := filepath.Join(ohosWork, "mygo-engine")

	tCompile := con.start("Compiling ohos/" + goarch)
	err = func() error {
		overlay, err := writeOverlay(filepath.Join(opts.work, "overlay.json"), opts.overlay)
		if err != nil {
			return err
		}
		flags := []string{"-trimpath", "-ldflags", ldflags}
		flags = append(flags, ohosBuildTags(opts.debug)...)
		if overlay != "" {
			flags = append(flags, "-overlay", overlay)
		}
		return goBuild(context.Background(), c, tCompile, engineBin, []string{"GOOS=linux", "GOARCH=" + goarch}, flags...)
	}()
	if err := tCompile.end(err, "Compiled ohos/"+goarch); err != nil {
		return nil, err
	}

	hapName := hapFileName(c, goarch)
	tPack := con.start("Packaging " + hapName)
	err = func() error {
		tPack.set("packing mygo.hnp")
		fontsConf := filepath.Join(hostTemplateDir, "fonts.conf")
		if !fileExists(fontsConf) {
			fontsConf = ""
		}
		hnpPath := filepath.Join(ohosWork, "mygo.hnp")
		if err := packOhosHNP(c, engineBin, libDir, fontsConf, res, hnpPath); err != nil {
			return err
		}

		tPack.set("staging HarmonyOS host")
		stageHost := filepath.Join(ohosWork, "host")
		if err := writeOhosHostProject(c, hostTemplateDir, stageHost, opts.debug); err != nil {
			return err
		}

		tPack.set("hvigorw assembleHap")
		cmd := exec.Command(hvigorw, "assembleHap", "--no-daemon")
		cmd.Dir = stageHost
		cmd.Env = append(os.Environ(), toolEnv...)
		var hvigorOut bytes.Buffer
		cmd.Stdout = &hvigorOut
		cmd.Stderr = &hvigorOut
		cmd.WaitDelay = waitDelay
		if err := waited(cmd.Run()); err != nil {
			return fmt.Errorf("hvigorw assembleHap: %w\n%s", err, strings.TrimSpace(hvigorOut.String()))
		}

		builtHap := filepath.Join(stageHost, "entry", "build", "default", "outputs", "default", "entry-default-unsigned.hap")
		if signed := filepath.Join(stageHost, "entry", "build", "default", "outputs", "default", "entry-default-signed.hap"); fileExists(signed) {
			builtHap = signed
		}
		dstHap := filepath.Join(stage, hapName)
		if err := copyFile(builtHap, dstHap, 0o644); err != nil {
			return err
		}

		tPack.set("embedding HNP archive")
		if err := injectHNPIntoHAP(dstHap, "hnp/arm64-v8a/mygo.hnp", hnpPath); err != nil {
			return err
		}

		if c.OHOS.SignWithHDA || os.Getenv("MYGO_OHOS_SIGN") == "hda" {
			tPack.set("signing with hda")
			if err := signOhosHapWithHDA(dstHap, toolEnv); err != nil {
				return err
			}
		}
		return nil
	}()
	if err := tPack.end(err, "Packaged "+hapName); err != nil {
		return nil, err
	}

	final := filepath.Join(out, target)
	if err := replacePath(stage, final); err != nil {
		return nil, err
	}
	return []string{filepath.Join(final, hapName)}, nil
}

// signOhosHapWithHDA signs hapPath in place using `hda install --keep-signed`
// run from ohosTempDir() so hdc never leaves hdc.log in the project directory.
func signOhosHapWithHDA(hapPath string, extraEnv []string) error {
	hda := ""
	if p, err := exec.LookPath("hda"); err == nil {
		hda = p
	} else if fileExists("/data/service/hnp/bin/hda") {
		hda = "/data/service/hnp/bin/hda"
	} else {
		return errors.New("hda not found: install HarmonyOS Debug Assistant CLI or pass -sign - when using an unsigned emulator/device")
	}
	signedTmp := strings.TrimSuffix(hapPath, ".hap") + ".signed.hap"
	defer os.Remove(signedTmp)
	cmd := exec.Command(hda, "install", hapPath, "--keep-signed", signedTmp)
	cmd.Dir = ohosTempDir()
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hda --keep-signed: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	if !fileExists(signedTmp) {
		return fmt.Errorf("hda did not write signed HAP %s", signedTmp)
	}
	return os.Rename(signedTmp, hapPath)
}

// hashOhosStageDir computes a deterministic SHA-256 over all files and symlinks
// in a staged HarmonyOS host project directory.
func hashOhosStageDir(stageDir string) ([32]byte, error) {
	var sum [32]byte
	h := sha256.New()
	err := filepath.WalkDir(stageDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(stageDir, path)
		if err != nil || rel == "." {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "L:%s->%s\x00", relSlash, filepath.ToSlash(target))
		case mode.IsDir():
			fmt.Fprintf(h, "D:%s\x00", relSlash)
		case mode.IsRegular():
			fmt.Fprintf(h, "F:%s:%d\x00", relSlash, info.Size())
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(h, f)
			_ = f.Close()
			if copyErr != nil {
				return copyErr
			}
			h.Write([]byte{0})
		}
		return nil
	})
	if err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// ensureOhosDevHostHap returns a cached unsigned host .hap matching dc and
// hostTemplateDir, running `hvigorw assembleHap` only when the staged host
// project's SHA-256 changes.
func ensureOhosDevHostHap(ctx context.Context, t *task, dc *Config, hostTemplateDir, cacheDir, hvigorw string, toolEnv []string) (string, [32]byte, error) {
	var hostSum [32]byte
	if err := os.MkdirAll(ohosTempDir(), 0o755); err != nil {
		return "", hostSum, err
	}
	stageHost, err := os.MkdirTemp(ohosTempDir(), "mygo-ohos-host-*")
	if err != nil {
		return "", hostSum, err
	}
	defer os.RemoveAll(stageHost)
	if err := os.Chmod(stageHost, 0o755); err != nil {
		return "", hostSum, err
	}
	if err := writeOhosHostProject(dc, hostTemplateDir, stageHost, true); err != nil {
		return "", hostSum, err
	}
	hostSum, err = hashOhosStageDir(stageHost)
	if err != nil {
		return "", hostSum, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", hostSum, err
	}
	cachedHap := filepath.Join(cacheDir, hex.EncodeToString(hostSum[:16])+".hap")
	if fileExists(cachedHap) {
		now := time.Now()
		_ = os.Chtimes(cachedHap, now, now)
		return cachedHap, hostSum, nil
	}

	if t != nil {
		t.set("hvigorw assembleHap (cached for subsequent rebuilds)")
	}
	cmd := exec.CommandContext(ctx, hvigorw, "assembleHap", "--no-daemon")
	cmd.Dir = stageHost
	cmd.Env = append(os.Environ(), toolEnv...)
	var hvigorOut bytes.Buffer
	cmd.Stdout = &hvigorOut
	cmd.Stderr = &hvigorOut
	cmd.WaitDelay = waitDelay
	if err := waited(cmd.Run()); err != nil {
		if ctx.Err() != nil {
			return "", hostSum, ctx.Err()
		}
		return "", hostSum, fmt.Errorf("hvigorw assembleHap: %w\n%s", err, strings.TrimSpace(hvigorOut.String()))
	}

	builtHap := filepath.Join(stageHost, "entry", "build", "default", "outputs", "default", "entry-default-unsigned.hap")
	if !fileExists(builtHap) {
		signed := filepath.Join(stageHost, "entry", "build", "default", "outputs", "default", "entry-default-signed.hap")
		if fileExists(signed) {
			builtHap = signed
		}
	}
	tmpHap := cachedHap + ".tmp"
	if err := copyFile(builtHap, tmpHap, 0o644); err != nil {
		return "", hostSum, err
	}
	if err := os.Rename(tmpHap, cachedHap); err != nil {
		_ = os.Remove(tmpHap)
		return "", hostSum, err
	}
	pruneOhosHostCache(cacheDir, 4)
	return cachedHap, hostSum, nil
}

func pruneOhosHostCache(cacheDir string, keep int) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) <= keep {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	var haps []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".hap") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		haps = append(haps, item{path: filepath.Join(cacheDir, e.Name()), mod: info.ModTime()})
	}
	if len(haps) <= keep {
		return
	}
	sort.Slice(haps, func(i, j int) bool { return haps[i].mod.After(haps[j].mod) })
	for _, old := range haps[keep:] {
		_ = os.Remove(old.path)
	}
}

func findOhosHDC(extraEnv []string) (string, error) {
	if p, err := exec.LookPath("hdc"); err == nil {
		return p, nil
	}
	for _, kv := range extraEnv {
		if pathVal, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(pathVal) {
				cand := filepath.Join(dir, "hdc")
				if runtime.GOOS == "windows" {
					cand += ".exe"
				}
				if fileExists(cand) {
					return cand, nil
				}
			}
		}
	}
	for _, cand := range []string{
		"/storage/Users/currentUser/.harmonybrew/bin/hdc",
		"/data/service/hnp/bin/hdc",
	} {
		if fileExists(cand) {
			return cand, nil
		}
	}
	return "", errors.New("hdc not found: install HarmonyOS Command Line Tools or add hdc to PATH")
}

func ensureOhosHDCTarget(hdc string, extraEnv []string) {
	cmd := exec.Command(hdc, "list", "targets")
	cmd.Dir = ohosTempDir()
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	trimmed := strings.TrimSpace(string(out))
	if err == nil && trimmed != "" && !strings.Contains(trimmed, "[Empty]") {
		return
	}
	if !isOhosHost() || !fileExists("/system/bin/param") {
		return
	}
	portOut, err := exec.Command("/system/bin/param", "get", "persist.hdc.port").Output()
	if err != nil {
		return
	}
	port := strings.TrimSpace(string(portOut))
	if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
		connCmd := exec.Command(hdc, "tconn", "127.0.0.1:"+port)
		connCmd.Dir = ohosTempDir()
		connCmd.Env = append(os.Environ(), extraEnv...)
		_ = connCmd.Run()
	}
}

func installOhosHapWithHDC(ctx context.Context, hdc, hapPath string, extraEnv []string) error {
	ensureOhosHDCTarget(hdc, extraEnv)
	cmd := exec.CommandContext(ctx, hdc, "install", "-r", hapPath)
	cmd.Dir = ohosTempDir()
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	lower := strings.ToLower(text)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("hdc install: %w\n%s", err, text)
	}
	if strings.Contains(lower, "error:") || strings.Contains(lower, "failed to install") {
		return fmt.Errorf("hdc install failed:\n%s", text)
	}
	return nil
}

// formatOhosHilogLine extracts clean application output from a HarmonyOS hilog
// line formatted as "<date> <time> <pid> <tid> <lvl> <dom>/<pkg>/<tag>: <msg>".
func formatOhosHilogLine(line string) (msg string, isErr, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return "", false, false
	}
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return "", false, false
	}
	level := fields[4]
	header := fields[5]
	colonIdx := strings.Index(line, header)
	if colonIdx < 0 {
		return "", false, false
	}
	rest := line[colonIdx+len(header):]
	rest = strings.TrimPrefix(rest, " ")
	tag := strings.TrimSuffix(header, ":")
	if slash := strings.LastIndexByte(tag, '/'); slash >= 0 {
		tag = tag[slash+1:]
	}
	errLevel := level == "E" || level == "F"
	switch tag {
	case "mygo-host":
		if strings.HasPrefix(rest, "engine started pid=") {
			return "", false, false
		}
		return rest, errLevel, true
	case "mygo", "mygo-bridge":
		if errLevel {
			return "[host] " + rest, true, true
		}
		return "", false, false
	default:
		return "", false, false
	}
}

func isSafeOhosBundleName(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// buildAndLaunchOhos compiles the HarmonyOS Go engine, reuses a cached host
// .hap unless host configuration changed, repacks mygo.hnp, signs with hda,
// installs over hdc, launches EntryAbility, and streams hilog output.
func (s *devSession) buildAndLaunchOhos(ctx context.Context, t *task, running [32]byte) (p *devProcess, sum [32]byte, err error) {
	const goarch = "arm64"
	c, err := loadConfig(s.root)
	if err != nil {
		return nil, sum, err
	}
	hvigorw, sdkHome, toolEnv := findOhosToolchain()
	if hvigorw == "" || sdkHome == "" {
		return nil, sum, errors.New("HarmonyOS Command Line Tools not found: set DEVECO_SDK_HOME and add hvigorw to PATH")
	}
	hdc, err := findOhosHDC(toolEnv)
	if err != nil {
		return nil, sum, err
	}
	libDir := findOhosLibDir()
	if libDir == "" && os.Getenv("MYGO_OHOS_SKIP_LIBS") != "1" {
		return nil, sum, errors.New("HarmonyOS musl Pango libraries not found: set MYGO_OHOS_LIB_DIR or install Pango into ~/.harmonybrew/lib")
	}
	hostTemplateDir, err := findOhosHostDir(c)
	if err != nil {
		return nil, sum, err
	}

	dir := filepath.Join(s.root, ".mygo", "dev", "ohos-"+goarch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, sum, err
	}
	stage, err := os.MkdirTemp(dir, ".staging-")
	if err != nil {
		return nil, sum, err
	}
	defer os.RemoveAll(stage)

	if err := s.updateI18n(c); err != nil {
		return nil, sum, err
	}

	dc := devConfig(c)
	engineBin := filepath.Join(stage, "mygo-engine")
	buildFlags := []string{"-ldflags", strings.TrimSpace(packageFlags(dc))}
	buildFlags = append(buildFlags, ohosBuildTags(true)...)
	if err := goBuild(ctx, c, t, engineBin, []string{"GOOS=linux", "GOARCH=" + goarch}, buildFlags...); err != nil {
		return nil, sum, err
	}

	res, err := dc.resources("ohos", goarch, reservedNames(dc, "ohos")...)
	if err != nil {
		return nil, sum, err
	}

	cacheDir := filepath.Join(dir, "host-cache")
	cachedHostHap, hostSum, err := ensureOhosDevHostHap(ctx, t, dc, hostTemplateDir, cacheDir, hvigorw, toolEnv)
	if err != nil {
		return nil, sum, err
	}

	devEnv := append([]string(nil), s.env...)
	devEnv = append(devEnv, "MYGO_DEV=1")

	h := sha256.New()
	ef, err := os.Open(engineBin)
	if err != nil {
		return nil, sum, err
	}
	_, copyErr := io.Copy(h, ef)
	_ = ef.Close()
	if copyErr != nil {
		return nil, sum, copyErr
	}
	if err := hashResources(h, res); err != nil {
		return nil, sum, err
	}
	h.Write(hostSum[:])
	for _, kv := range devEnv {
		fmt.Fprintf(h, "E:%s\x00", kv)
	}
	fmt.Fprintf(h, "S:%s\x00", s.sign)
	copy(sum[:], h.Sum(nil))
	if sum == running {
		return nil, sum, errUnchanged
	}

	if c.Bindings != "" {
		t.set("generating the TypeScript client")
		genBin := tempBinary(dc.slug())
		defer os.Remove(genBin)
		var genErr error
		if runtime.GOOS == "linux" && runtime.GOARCH == goarch {
			genErr = copyFile(engineBin, genBin, 0o755)
		} else {
			genErr = goBuild(ctx, c, t, genBin, nil)
		}
		if genErr != nil {
			return nil, sum, genErr
		}
		changed, err := generateBindings(c, genBin)
		if err != nil {
			return nil, sum, err
		}
		if changed {
			logf("Wrote %s", cyan(relPathTo(c.root, c.path(c.Bindings))))
		}
	}

	t.set("packing mygo.hnp")
	fontsConf := filepath.Join(hostTemplateDir, "fonts.conf")
	if !fileExists(fontsConf) {
		fontsConf = ""
	}
	hnpPath := filepath.Join(stage, "mygo.hnp")
	if err := packOhosHNPWithEnv(dc, engineBin, libDir, fontsConf, res, devEnv, hnpPath); err != nil {
		return nil, sum, err
	}

	dstHap := filepath.Join(stage, dc.slug()+".hap")
	if err := copyFile(cachedHostHap, dstHap, 0o644); err != nil {
		return nil, sum, err
	}
	if err := injectHNPIntoHAP(dstHap, "hnp/arm64-v8a/mygo.hnp", hnpPath); err != nil {
		return nil, sum, err
	}

	if s.sign != "-" {
		t.set("signing with hda")
		if err := signOhosHapWithHDA(dstHap, toolEnv); err != nil {
			return nil, sum, err
		}
	}

	finalHap := filepath.Join(dir, dc.slug()+".hap")
	if err := replacePath(dstHap, finalHap); err != nil {
		return nil, sum, err
	}

	s.stopLive()
	t.set("installing on HarmonyOS")
	if err := installOhosHapWithHDC(ctx, hdc, finalHap, toolEnv); err != nil {
		return nil, sum, err
	}

	t.set("launching " + dc.Name)
	p, err = s.launchOhos(hdc, dc.Identifier, toolEnv)
	if err != nil {
		return nil, sum, err
	}
	return p, sum, nil
}

func (s *devSession) launchOhos(hdc, bundleName string, extraEnv []string) (*devProcess, error) {
	if !isSafeOhosBundleName(bundleName) {
		return nil, fmt.Errorf("invalid HarmonyOS bundle identifier %q", bundleName)
	}
	script := fmt.Sprintf(
		"aa force-stop '%[1]s' >/dev/null 2>&1 || true; "+
			"out=$(aa start -a EntryAbility -b '%[1]s' 2>&1); "+
			"if [ $? -ne 0 ]; then printf '%%s\\n' \"$out\" >&2; exit 1; fi; "+
			"case \"$out\" in *error*|*failed*) printf '%%s\\n' \"$out\" >&2; exit 1 ;; esac; "+
			"pid=''; i=0; "+
			"while [ $i -lt 50 ]; do "+
			"  pid=$(pidof '%[1]s' 2>/dev/null | awk '{print $1}'); "+
			"  [ -n \"$pid\" ] && break; "+
			"  usleep 100000 2>/dev/null || sleep 1; "+
			"  i=$((i + 1)); "+
			"done; "+
			"if [ -z \"$pid\" ]; then printf 'EntryAbility did not start for %[1]s: %%s\\n' \"$out\" >&2; exit 1; fi; "+
			"hilog -P \"$pid\" -T mygo,mygo-host,mygo-bridge & "+
			"hpid=$!; "+
			"trap 'kill $hpid 2>/dev/null; aa force-stop '\\''%[1]s'\\'' >/dev/null 2>&1; exit 0' INT TERM HUP; "+
			"while [ -d \"/proc/$pid\" ]; do sleep 1; done; "+
			"usleep 200000 2>/dev/null || true; "+
			"kill $hpid 2>/dev/null || true; "+
			"wait $hpid 2>/dev/null || true; "+
			"exit 0",
		bundleName,
	)

	stdoutR, stdoutW := io.Pipe()
	stderr, errDone, err := pipe(con.output("app", os.Stderr))
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, err
	}

	cmd := exec.Command(hdc, "shell", script)
	cmd.Dir = ohosTempDir()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), extraEnv...)
	setProcessGroup(cmd)
	err = cmd.Start()
	_ = stdoutW.Close()
	_ = stderr.Close()
	if err != nil {
		_ = stdoutR.Close()
		return nil, err
	}

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		defer stdoutR.Close()
		appOut := con.output("app", os.Stdout)
		appErr := con.output("app", os.Stderr)
		defer appOut.flush()
		defer appErr.flush()
		sc := bufio.NewScanner(stdoutR)
		sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for sc.Scan() {
			msg, isErr, ok := formatOhosHilogLine(sc.Text())
			if !ok {
				continue
			}
			if isErr {
				_, _ = io.WriteString(appErr, msg+"\n")
			} else {
				_, _ = io.WriteString(appOut, msg+"\n")
			}
		}
	}()

	stopAbility := func() {
		stopCmd := exec.Command(hdc, "shell", "aa", "force-stop", bundleName)
		stopCmd.Dir = ohosTempDir()
		stopCmd.Env = append(os.Environ(), extraEnv...)
		_ = stopCmd.Run()
	}

	p := &devProcess{
		exe:    bundleName,
		cmd:    cmd,
		stopFn: stopAbility,
		done:   make(chan struct{}),
	}
	go func() {
		p.err = cmd.Wait()
		timeout := time.After(250 * time.Millisecond)
	drain:
		for _, copied := range []<-chan struct{}{outDone, errDone} {
			select {
			case <-copied:
			case <-timeout:
				break drain
			}
		}
		close(p.done)
	}()
	return p, nil
}
