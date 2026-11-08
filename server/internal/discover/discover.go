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
}

// OSEnv is the real environment.
func OSEnv() Env {
	return Env{Getenv: os.Getenv, LookPath: exec.LookPath, IsExec: isExecutable}
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
		for _, rel := range []string{".pixi/envs/default/bin", ".magic/envs/default/bin", ".venv/bin"} {
			if try(filepath.Join(root, rel, Binary), "workspace "+rel) {
				return res, nil
			}
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
