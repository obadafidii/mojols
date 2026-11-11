package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/obadafidii/mojols/server/internal/stdlib"
)

// setupStdlib gives the index the stdlib's sources, once per session, in
// the background: from a configured directory, or (auto) matching the
// installed Mojo from the cache, downloading them the first time.
func (s *Server) setupStdlib(serverPath string) {
	if s.ix == nil || s.opts.StdlibSources == "off" {
		s.setStdlibState("off")
		return
	}
	var root string
	var err error
	switch {
	case s.opts.StdlibFetch != nil:
		root, err = s.opts.StdlibFetch(context.Background(), serverPath)
	case s.opts.StdlibSources != "" && s.opts.StdlibSources != "auto":
		root, err = stdlibDir(s.opts.StdlibSources)
	case s.workspaceStdlib() != "":
		// The workspace has the stdlib's sources (a modular checkout): they
		// are what the user is editing, so go to definition lands there.
		root = s.workspaceStdlib()
	default:
		s.setStdlibState("detecting the Mojo version")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var v stdlib.Version
		if v, err = stdlib.DetectVersion(ctx, serverPath); err == nil {
			s.setStdlibState("fetching the stdlib sources for Mojo " + v.Number)
			var r stdlib.Root
			r, err = stdlib.Ensure(ctx, v, stdlib.Options{Logger: s.log, Token: os.Getenv("GITHUB_TOKEN")})
			root = string(r)
		}
	}
	if err != nil {
		s.log.Warn("stdlib sources unavailable; names from std will not resolve", "err", err)
		s.setStdlibState("unavailable: " + err.Error())
		return
	}
	s.ix.SetLibraries([]string{root})
	s.log.Info("stdlib sources ready", "dir", root)
	go s.ix.Warm(nil)
	s.setStdlibState("ready: " + root)
}

func (s *Server) setStdlibState(st string) {
	s.mu.Lock()
	s.stdlibState = st
	s.mu.Unlock()
}

// stdlibDir accepts a directory containing std/ or the std package itself
// and returns the directory containing it.
func stdlibDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, "std", "__init__.mojo")); err == nil {
		return abs, nil
	}
	if filepath.Base(abs) == "std" {
		if _, err := os.Stat(filepath.Join(abs, "__init__.mojo")); err == nil {
			return filepath.Dir(abs), nil
		}
	}
	return "", fmt.Errorf("%s does not contain the std package (std/__init__.mojo)", dir)
}

// workspaceStdlib is a stdlib checkout inside the workspace, if any.
func (s *Server) workspaceStdlib() string {
	s.mu.Lock()
	roots := append([]string(nil), s.roots...)
	s.mu.Unlock()
	return stdlib.InWorkspace(roots)
}
