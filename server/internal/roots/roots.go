// Package roots infers the include directories (-I) a Mojo workspace needs.
//
// mojo-lsp-server resolves `from mathlib import x` only if the directory
// containing the package `mathlib/` is an include dir. Measured on a real
// toolchain: without it, every import of a sibling package is an error and
// definition returns nothing. Editors have no way to know these directories,
// so mojols finds them: the parent of every top-level package (a directory
// with __init__.mojo whose parent has none) and every directory holding a
// .mojopkg file.
package roots

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Options bounds the scan. Zero values take the defaults.
type Options struct {
	MaxDepth   int           // directories below a workspace root: 8
	MaxEntries int           // files and directories visited, all roots: 50,000
	Timeout    time.Duration // 2s
	MaxRoots   int           // 32
}

// Skip is a directory that looked like a root but was rejected.
type Skip struct {
	Dir    string
	Reason string
}

// Result is the outcome of a scan.
type Result struct {
	Roots     []string // include dirs, shallowest first
	Skipped   []Skip
	Visited   int
	Truncated bool   // a cap was hit; Roots may be incomplete
	Limit     string // which cap: "entries", "time" or "roots"
	Fixtures  int    // test-fixture directories not scanned
	Elapsed   time.Duration
}

// stdlibPackages are top-level packages the installed toolchain provides.
// A workspace copy of them (a modular checkout, a stdlib snapshot) must
// never shadow the toolchain's own: a 1.1.0 server type-checking 1.3 stdlib
// sources segfaults.
var stdlibPackages = map[string]bool{"std": true, "stdlib": true, "max": true}

// ignoredDirs are never scanned: environments, build output, VCS data.
var ignoredDirs = map[string]bool{
	"node_modules": true, "target": true, "build": true, "dist": true, "out": true,
	"__pycache__": true, "site-packages": true, "venv": true,
}

func ignored(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") || ignoredDirs[name]
}

// fixtureDirs hold test inputs: packages built to exercise the compiler,
// often with deliberately clashing names. Code never imports them, so
// they are not scanned. Measured on modular/modular: most candidate roots
// were fixtures (inputs/, testdata/, Inputs/).
var fixtureDirs = map[string]bool{"inputs": true, "testdata": true, "test_data": true, "fixtures": true}

var testDirs = map[string]bool{"test": true, "tests": true, "unittests": true}

// fixture reports whether dir (relative to the workspace) holds test inputs.
func fixture(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	name := strings.ToLower(parts[len(parts)-1])
	if fixtureDirs[name] {
		return true
	}
	if name == "data" {
		for _, p := range parts[:len(parts)-1] {
			if testDirs[strings.ToLower(p)] {
				return true
			}
		}
	}
	return false
}

var errStop = errors.New("stop")

func isPackage(dir string) bool {
	for _, init := range []string{"__init__.mojo", "__init__.🔥"} {
		if st, err := os.Stat(filepath.Join(dir, init)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// Infer scans the workspace roots and returns the include dirs they need.
func Infer(workspaces []string, opts Options) Result {
	if opts.MaxDepth == 0 {
		opts.MaxDepth = 8
	}
	if opts.MaxEntries == 0 {
		opts.MaxEntries = 50_000
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	if opts.MaxRoots == 0 {
		opts.MaxRoots = 32
	}
	start := time.Now()
	deadline := start.Add(opts.Timeout)
	var res Result
	found := map[string][]string{} // include dir -> packages it provides
	var sources []string           // .mojo files seen, for the import check
	add := func(dir, pkg string) {
		if !contains(found[dir], pkg) {
			found[dir] = append(found[dir], pkg)
		}
	}

	for _, ws := range workspaces {
		ws = filepath.Clean(ws)
		// A workspace that is itself a package (the user opened src/mathlib)
		// needs the directory above its outermost package.
		if isPackage(ws) {
			top := ws
			for isPackage(filepath.Dir(top)) && filepath.Dir(top) != top {
				top = filepath.Dir(top)
			}
			add(filepath.Dir(top), filepath.Base(top))
		}
		err := filepath.WalkDir(ws, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable: skip, keep scanning the rest
			}
			res.Visited++
			if res.Visited > opts.MaxEntries {
				res.Truncated, res.Limit = true, "entries"
				return errStop
			}
			if time.Now().After(deadline) {
				res.Truncated, res.Limit = true, "time"
				return errStop
			}
			if !d.IsDir() {
				if strings.HasSuffix(d.Name(), ".mojopkg") {
					add(filepath.Dir(path), strings.TrimSuffix(d.Name(), ".mojopkg"))
				}
				if strings.HasSuffix(d.Name(), ".mojo") && len(sources) < maxImportScan {
					sources = append(sources, path)
				}
				return nil
			}
			if path == ws {
				return nil
			}
			if ignored(d.Name()) {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(ws, path)
			if fixture(rel) {
				res.Fixtures++
				return filepath.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator))+1 > opts.MaxDepth {
				return filepath.SkipDir
			}
			if isPackage(path) {
				if !isPackage(filepath.Dir(path)) {
					add(filepath.Dir(path), d.Name())
				}
				// Subpackages resolve through their top-level package.
				return filepath.SkipDir
			}
			return nil
		})
		if errors.Is(err, errStop) {
			break
		}
	}

	// Shallowest first: when two roots provide the same package, the
	// earlier include dir wins, so prefer the one nearest the top.
	candidates := make([]string, 0, len(found))
	for dir := range found {
		candidates = append(candidates, dir)
	}
	sort.Slice(candidates, func(i, j int) bool {
		di, dj := depth(candidates[i]), depth(candidates[j])
		if di != dj {
			return di < dj
		}
		return candidates[i] < candidates[j]
	})
	// A root inside a test directory is usually compiler test inputs; keep
	// it only if a file outside it imports one of its packages.
	unused := unimportedTestRoots(candidates, found, sources)
	provider := map[string]string{} // package -> include dir providing it
	for _, dir := range candidates {
		pkgs := found[dir]
		sort.Strings(pkgs)
		var shadow, fresh []string
		for _, p := range pkgs {
			switch {
			case stdlibPackages[p]:
				shadow = append(shadow, p)
			case provider[p] == "":
				fresh = append(fresh, p)
			}
		}
		switch {
		case unused[dir]:
			res.Skipped = append(res.Skipped, Skip{Dir: dir, Reason: "test inputs: nothing imports " + strings.Join(pkgs, ", ")})
		case len(shadow) > 0:
			res.Skipped = append(res.Skipped, Skip{Dir: dir, Reason: "would shadow the installed toolchain's " + strings.Join(shadow, ", ") + " package"})
		case len(fresh) == 0:
			res.Skipped = append(res.Skipped, Skip{Dir: dir, Reason: "its package " + pkgs[0] + " is already provided by " + provider[pkgs[0]]})
		case len(res.Roots) >= opts.MaxRoots:
			res.Skipped = append(res.Skipped, Skip{Dir: dir, Reason: "too many include dirs"})
			res.Truncated, res.Limit = true, "roots"
		default:
			res.Roots = append(res.Roots, dir)
			for _, p := range fresh {
				provider[p] = dir
			}
		}
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Dir < res.Skipped[j].Dir })
	res.Elapsed = time.Since(start)
	return res
}

func depth(p string) int { return strings.Count(filepath.Clean(p), string(filepath.Separator)) }

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

const (
	maxImportScan  = 20_000
	importScanTime = 400 * time.Millisecond
	importScanHead = 8 << 10 // imports come first: read this much of a file
)

var importRe = regexp.MustCompile(`(?m)^[ \t]*(?:from|import)[ \t]+([A-Za-z_][A-Za-z0-9_]*)`)

// unimportedTestRoots returns the candidate roots under a test directory
// whose packages no file outside them imports. If the check runs out of
// time it reports nothing, and every root is kept.
func unimportedTestRoots(candidates []string, found map[string][]string, sources []string) map[string]bool {
	want := map[string][]string{} // package -> test roots providing it
	for _, dir := range candidates {
		if !underTestDir(dir) {
			continue
		}
		for _, p := range found[dir] {
			want[p] = append(want[p], dir)
		}
	}
	if len(want) == 0 {
		return nil
	}
	used := map[string]bool{} // root dir -> a package of it is imported from outside
	deadline := time.Now().Add(importScanTime)
	buf := make([]byte, importScanHead)
	for _, path := range sources {
		if time.Now().After(deadline) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		n, _ := io.ReadFull(f, buf)
		f.Close()
		for _, m := range importRe.FindAllSubmatch(buf[:n], -1) {
			pkg := string(m[1])
			for _, dir := range want[pkg] {
				if !strings.HasPrefix(path, filepath.Join(dir, pkg)+string(filepath.Separator)) {
					used[dir] = true
				}
			}
		}
	}
	unused := map[string]bool{}
	for _, dir := range candidates {
		if underTestDir(dir) && !used[dir] {
			unused[dir] = true
		}
	}
	return unused
}

func underTestDir(dir string) bool {
	for _, p := range strings.Split(filepath.ToSlash(dir), "/") {
		if testDirs[strings.ToLower(p)] {
			return true
		}
	}
	return false
}
