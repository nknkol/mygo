package main

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	Permissions []string `json:"permissions"`
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
			"icon":        "$media:app_icon",
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

	// 4. Base & localized string resources
	const (
		pasteboardReason = "Used to paste text, links, files, and images from the system clipboard into the application."
		backgroundReason = "Used to keep audio playing smoothly while windows are minimized or in the background."
	)
	if err := writeOhosStringTable(
		filepath.Join(stageHostDir, "AppScope", "resources", "base", "element", "string.json"),
		[]ohosStringEntry{{Name: "app_name", Value: c.Name}},
	); err != nil {
		return err
	}
	if err := writeOhosStringTable(
		filepath.Join(stageHostDir, "entry", "src", "main", "resources", "base", "element", "string.json"),
		[]ohosStringEntry{
			{Name: "module_desc", Value: desc},
			{Name: "EntryAbility_desc", Value: desc},
			{Name: "EntryAbility_label", Value: c.Name},
			{Name: "read_pasteboard_reason", Value: pasteboardReason},
			{Name: "keep_background_reason", Value: backgroundReason},
		},
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
			if err := writeOhosStringTable(
				filepath.Join(stageHostDir, "entry", "src", "main", "resources", qual, "element", "string.json"),
				[]ohosStringEntry{
					{Name: "module_desc", Value: locDesc},
					{Name: "EntryAbility_desc", Value: locDesc},
					{Name: "EntryAbility_label", Value: locName},
					{Name: "read_pasteboard_reason", Value: pasteboardReason},
					{Name: "keep_background_reason", Value: backgroundReason},
				},
			); err != nil {
				return err
			}
		}
	}

	// 5. entry/src/main/module.json5
	if err := writeOhosJSON(
		filepath.Join(stageHostDir, "entry", "src", "main", "module.json5"),
		ohosModuleConfig(c),
	); err != nil {
		return err
	}

	// 6. Icons (AppScope app_icon.png, entry app_icon.png, entry startIcon.png)
	if c.Icon != "" {
		if err := writeOhosIcons(c, stageHostDir); err != nil {
			return err
		}
	}
	return nil
}

func ohosModuleConfig(c *Config) map[string]any {
	deviceTypes := c.OHOS.DeviceTypes
	if len(deviceTypes) == 0 {
		deviceTypes = []string{"2in1", "tablet"}
	}
	bgModes := c.OHOS.BackgroundModes
	if bgModes == nil {
		bgModes = []string{"audioPlayback"}
	}

	perms := []map[string]any{
		{"name": "ohos.permission.INTERNET"},
		{"name": "ohos.permission.GET_NETWORK_INFO"},
		{"name": "ohos.permission.PREPARE_APP_TERMINATE"},
		{
			"name":   "ohos.permission.KEEP_BACKGROUND_RUNNING",
			"reason": "$string:keep_background_reason",
			"usedScene": map[string]any{
				"abilities": []string{"EntryAbility"},
				"when":      "inuse",
			},
		},
		{"name": "ohos.permission.FILE_ACCESS_PERSIST"},
		{
			"name":   "ohos.permission.READ_PASTEBOARD",
			"reason": "$string:read_pasteboard_reason",
			"usedScene": map[string]any{
				"abilities": []string{"EntryAbility"},
				"when":      "inuse",
			},
		},
	}
	havePerm := func(name string) bool {
		return slices.ContainsFunc(perms, func(p map[string]any) bool { return p["name"] == name })
	}
	for _, extra := range c.OHOS.Permissions {
		if extra = strings.TrimSpace(extra); extra != "" && !havePerm(extra) {
			perms = append(perms, map[string]any{"name": extra})
		}
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
		"icon":                  "$media:app_icon",
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
	}
}

func writeOhosIcons(c *Config, stageHostDir string) error {
	src, err := os.ReadFile(c.path(c.Icon))
	if err != nil {
		return err
	}
	if err := validateIcon(src); err != nil {
		return fmt.Errorf("icon %s: %w", c.Icon, err)
	}
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return fmt.Errorf("icon %s: %w", c.Icon, err)
	}
	encodeSize := func(size int) ([]byte, error) {
		if img.Bounds().Dx() == size && img.Bounds().Dy() == size {
			return src, nil
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, resize(img, size)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	icon512, err := encodeSize(512)
	if err != nil {
		return err
	}
	icon256, err := encodeSize(256)
	if err != nil {
		return err
	}
	targets := []struct {
		path string
		data []byte
	}{
		{filepath.Join(stageHostDir, "AppScope", "resources", "base", "media", "app_icon.png"), icon512},
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
// run from os.TempDir() so hdc never leaves hdc.log in the project directory.
func signOhosHapWithHDA(hapPath string, extraEnv []string) error {
	hda := ""
	if p, err := exec.LookPath("hda"); err == nil {
		hda = p
	} else if fileExists("/data/service/hnp/bin/hda") {
		hda = "/data/service/hnp/bin/hda"
	} else {
		return errors.New("hda not found: install HarmonyOS Debug Assistant CLI or disable ohos.signWithHda")
	}
	signedTmp := hapPath + ".signed"
	defer os.Remove(signedTmp)
	cmd := exec.Command(hda, "install", hapPath, "--keep-signed", signedTmp)
	cmd.Dir = os.TempDir()
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
