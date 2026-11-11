// Package discover finds the mojo-lsp-server binary that mojols supervises.
//
// mojols never bundles Modular's binaries. It uses the toolchain the user
// already has, searched in a fixed, explainable order.
package discover

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Binary is the official server's executable name.
const Binary = "mojo-lsp-server"

// EnvBackend overrides discovery with an explicit path.
const EnvBackend = "MOJOLS_BACKEND"

// Candidate is one place discovery looked.
type Candidate struct {
	Path   string
	Source string
	Found  bool
}

// Result is the outcome of discovery.
type Result struct {
	Path   string
	Source string
	Tried  []Candidate
}

// Env abstracts the environment for tests.
type Env struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
	IsExec   func(string) bool
	// ReadDir lists a directory, for finding project environments below
	// the workspace root; nil skips that step.
	ReadDir func(string) ([]os.DirEntry, error)
}

// OSEnv is the real environment.
func OSEnv() Env {
	return Env{Getenv: os.Getenv, LookPath: exec.LookPath, IsExec: isExecutable, ReadDir: os.ReadDir}
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// ErrNotFound means no candidate existed.
var ErrNotFound = errors.New("mojo-lsp-server not found")

// Find returns the backend to run. explicit is the --backend flag; roots are
// workspace folders from initialize, most specific first.
func Find(explicit string, roots []string, env Env) (Result, error) {
	var res Result
	try := func(path, source string) bool {
		if path == "" {
			return false
		}
		path = expandHome(path, env)
		ok := env.IsExec(path)
		res.Tried = append(res.Tried, Candidate{Path: path, Source: source, Found: ok})
		if ok {
			res.Path, res.Source = path, source
		}
		return ok
	}
	if explicit != "" {
		if strings.ContainsRune(explicit, filepath.Separator) {
			if try(explicit, "--backend flag") {
				return res, nil
			}
		} else if p, err := env.LookPath(explicit); err == nil && try(p, "--backend flag (PATH)") {
			return res, nil
		}
		return res, fmt.Errorf("%w: --backend %q is not an executable file", ErrNotFound, explicit)
	}
	if v := env.Getenv(EnvBackend); v != "" {
		if try(v, "$"+EnvBackend) {
			return res, nil
		}
		return res, fmt.Errorf("%w: $%s=%q is not an executable file", ErrNotFound, EnvBackend, v)
	}
	for _, root := range roots {
		for _, rel := range envDirs {
			if try(filepath.Join(root, rel, Binary), "workspace "+rel) {
				return res, nil
			}
		}
	}
	// A project inside the workspace with its own environment (modular's
	// Mojo/ in a checkout of the whole repository): the shallowest wins.
	for _, root := range roots {
		for _, dir := range projectDirs(root, env) {
			rel, _ := filepath.Rel(root, dir)
			for _, envDir := range envDirs {
				if try(filepath.Join(dir, envDir, Binary), "workspace "+filepath.ToSlash(filepath.Join(rel, envDir))) {
					return res, nil
				}
			}
		}
	}
	// An activated conda or virtual environment.
	for _, v := range []string{"CONDA_PREFIX", "VIRTUAL_ENV"} {
		if dir := env.Getenv(v); dir != "" && try(filepath.Join(dir, "bin", Binary), "$"+v) {
			return res, nil
		}
	}
	if p, err := env.LookPath(Binary); err == nil {
		if try(p, "PATH") {
			return res, nil
		}
	} else {
		res.Tried = append(res.Tried, Candidate{Path: Binary, Source: "PATH"})
	}
	for _, p := range []string{
		"~/.pixi/bin/" + Binary,
		"~/.mojo/bin/" + Binary,
		"~/.modular/bin/" + Binary,
		"~/.local/bin/" + Binary,
		"/opt/homebrew/bin/" + Binary,
		"/usr/local/bin/" + Binary,
	} {
		if try(p, "standard location") {
			return res, nil
		}
	}
	return res, ErrNotFound
}

var envDirs = []string{".pixi/envs/default/bin", ".magic/envs/default/bin", ".venv/bin"}

// projectMarkers name the files that make a directory a Mojo project with
// its own environment.
var projectMarkers = []string{"pixi.toml", "mojoproject.toml", "pyproject.toml"}

// projectDirs lists directories below root (up to three levels, shallowest
// first, then by name) that hold a project file and an environment.
func projectDirs(root string, env Env) []string {
	if env.ReadDir == nil {
		return nil
	}
	var out []string
	level := []string{root}
	for depth := 1; depth <= 3 && len(level) > 0; depth++ {
		var next []string
		for _, dir := range level {
			ents, err := env.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				n := e.Name()
				if !e.IsDir() || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "bazel-") || skipProjectDirs[n] {
					continue
				}
				sub := filepath.Join(dir, n)
				next = append(next, sub)
				if hasEnv(sub, env) {
					out = append(out, sub)
				}
			}
			if len(next) > 2000 {
				return out // a huge tree: stop looking
			}
		}
		level = next
	}
	return out
}

var skipProjectDirs = map[string]bool{"node_modules": true, "build": true, "dist": true, "target": true, "__pycache__": true, "venv": true}

func hasEnv(dir string, env Env) bool {
	ents, err := env.ReadDir(dir)
	if err != nil {
		return false
	}
	marker, envDir := false, false
	for _, e := range ents {
		for _, m := range projectMarkers {
			if e.Name() == m {
				marker = true
			}
		}
		if e.Name() == ".pixi" || e.Name() == ".magic" || e.Name() == ".venv" {
			envDir = true
		}
	}
	return marker && envDir
}

func expandHome(p string, env Env) string {
	if strings.HasPrefix(p, "~/") {
		if home := env.Getenv("HOME"); home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// URIToPath converts a file:// URI to a filesystem path.
func URIToPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
	}
	return filepath.FromSlash(p), true
}

// Report renders a human-readable account of a discovery run.
func Report(res Result, err error) string {
	var b strings.Builder
	for _, c := range res.Tried {
		mark := "  "
		if c.Found {
			mark = "✓ "
		}
		fmt.Fprintf(&b, "%s%-34s %s\n", mark, c.Source, c.Path)
	}
	if err != nil {
		fmt.Fprintf(&b, "\nerror: %v\n", err)
		b.WriteString("Install Mojo (https://docs.modular.com/mojo/manual/install), or pass --backend /path/to/mojo-lsp-server.\n")
	} else {
		fmt.Fprintf(&b, "\nusing %s (%s)\n", res.Path, res.Source)
	}
	return b.String()
}
