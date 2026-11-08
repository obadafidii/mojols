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
	"io/fs"
	"os"
	"path/filepath"
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
	Truncated bool // a cap was hit; Roots may be incomplete
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
			if res.Visited > opts.MaxEntries || time.Now().After(deadline) {
				res.Truncated = true
				return errStop
			}
			if !d.IsDir() {
				if strings.HasSuffix(d.Name(), ".mojopkg") {
					add(filepath.Dir(path), strings.TrimSuffix(d.Name(), ".mojopkg"))
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

	for dir, pkgs := range found {
		var shadow []string
		for _, p := range pkgs {
			if stdlibPackages[p] {
				shadow = append(shadow, p)
			}
		}
		if len(shadow) > 0 {
			sort.Strings(shadow)
			res.Skipped = append(res.Skipped, Skip{Dir: dir, Reason: "would shadow the installed toolchain's " + strings.Join(shadow, ", ") + " package"})
			continue
		}
		res.Roots = append(res.Roots, dir)
	}
	sort.Slice(res.Roots, func(i, j int) bool {
		di, dj := depth(res.Roots[i]), depth(res.Roots[j])
		if di != dj {
			return di < dj
		}
		return res.Roots[i] < res.Roots[j]
	})
	if len(res.Roots) > opts.MaxRoots {
		for _, r := range res.Roots[opts.MaxRoots:] {
			res.Skipped = append(res.Skipped, Skip{Dir: r, Reason: "too many include dirs"})
		}
		res.Roots = res.Roots[:opts.MaxRoots]
		res.Truncated = true
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
