// Command hnp packs a HarmonyOS native package: a zip whose entries carry
// their own permission bits. That is the whole trick — the mode a file gets
// on the device comes from the zip entry, not from the file that was packed,
// so the packing tool sets it and nothing depends on the build machine's
// filesystem keeping a chmod.
package main

import (
	"archive/zip"
	"debug/elf"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// pangoRoots are the shared libraries internal/text/pango_linux.go opens
// directly with purego. Their transitive DT_NEEDED closure (excluding the
// system musl libc) is bundled into lib/ so pango_linux.go runs unchanged.
var pangoRoots = []string{
	"libgobject-2.0.so.0",
	"libpango-1.0.so.0",
	"libglib-2.0.so.0",
	"libpangocairo-1.0.so.0",
	"libcairo.so.2",
	"libharfbuzz.so.0",
	"libpangoft2-1.0.so.0",
	"libfontconfig.so.1",
}

func isSystemLib(name string) bool {
	switch name {
	case "libc.so", "libm.so", "libdl.so", "libpthread.so", "ld-musl-aarch64.so.1":
		return true
	}
	return false
}

// resolveLibs walks the DT_NEEDED closure starting from roots inside libDir,
// returning a sorted list of (soname, resolvedHostPath) pairs.
func resolveLibs(libDir string, roots []string) ([][2]string, error) {
	seen := make(map[string]string)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, ok := seen[name]; ok || isSystemLib(name) {
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
		deps, err := ef.ImportedLibraries()
		ef.Close()
		if err != nil {
			return nil, fmt.Errorf("read DT_NEEDED of %s: %w", real, err)
		}
		seen[name] = real
		for _, dep := range deps {
			if _, ok := seen[dep]; !ok && !isSystemLib(dep) {
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

func main() {
	version := flag.String("version", "", "the version the package installs under, e.g. 1.0.7")
	name := flag.String("name", "mygo", "the package name hnp.json declares")
	bin := flag.String("bin", "", "the binary to pack")
	target := flag.String("target", "", "the name the binary takes inside the package, e.g. mygo-engine")
	libDir := flag.String("lib-dir", "", "optional musl shared library directory to bundle Pango stack from")
	fontsConf := flag.String("fonts-conf", "", "optional fonts.conf to pack at etc/fonts/fonts.conf")
	out := flag.String("out", "", "the .hnp to write")
	flag.Parse()

	if *version == "" || *bin == "" || *target == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "hnp: want -version, -bin, -target and -out")
		os.Exit(2)
	}
	if err := pack(*version, *name, *bin, *target, *libDir, *fontsConf, *out); err != nil {
		fmt.Fprintln(os.Stderr, "hnp:", err)
		os.Exit(1)
	}
}

// pack writes the package from nothing: a zip that already holds a version
// is not a package the installer accepts, and it is worth not being able to
// make one by accident.
func pack(version, name, bin, target, libDir, fontsConf, out string) error {
	root := name + "_" + version

	var libs [][2]string
	if libDir != "" {
		var err error
		libs, err = resolveLibs(libDir, pangoRoots)
		if err != nil {
			return err
		}
	}

	conf, err := json.MarshalIndent(map[string]any{
		"type":    "hnp-config",
		"name":    name,
		"version": version,
		"install": map[string]any{
			"links": []any{
				map[string]any{"source": "bin/" + target, "target": target},
			},
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	conf = append(conf, '\n')

	tmp := out + ".tmp"
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}

	when := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	addDir := func(d string) error {
		h := &zip.FileHeader{Name: d + "/", Method: zip.Store, Modified: when}
		h.SetMode(0o755 | os.ModeDir)
		_, err := zw.CreateHeader(h)
		return err
	}
	addFile := func(dst, srcPath string, mode os.FileMode) error {
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

	dirs := []string{root, root + "/bin"}
	if len(libs) > 0 {
		dirs = append(dirs, root+"/lib")
	}
	if fontsConf != "" {
		dirs = append(dirs, root+"/etc", root+"/etc/fonts")
	}
	for _, d := range dirs {
		if err := addDir(d); err != nil {
			return fail(err)
		}
	}

	// The binary is executable, which is the reason this tool exists.
	if err := addFile(root+"/bin/"+target, bin, 0o755); err != nil {
		return fail(err)
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

	h := &zip.FileHeader{Name: root + "/hnp.json", Method: zip.Deflate, Modified: when}
	h.SetMode(0o644)
	w, err := zw.CreateHeader(h)
	if err != nil {
		return fail(err)
	}
	if _, err := w.Write(conf); err != nil {
		return fail(err)
	}

	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	fmt.Printf("hnp: %s\n  %s/bin/%s  0755\n", filepath.Clean(out), root, target)
	if len(libs) > 0 {
		fmt.Printf("  %s/lib/*.so  (%d libraries, 0755)\n", root, len(libs))
	}
	if fontsConf != "" {
		fmt.Printf("  %s/etc/fonts/fonts.conf  0644\n", root)
	}
	fmt.Printf("  %s/hnp.json  0644\n", root)
	return nil
}
