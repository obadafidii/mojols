package proxy

// Cross-file invalidation.
//
// mojo-lsp-server gives every open document its own parser context, and
// that context reads imported modules from disk. Nothing tells it when an
// imported file changes, so after `ops.mojo` is saved, `main.mojo` (which
// imports it through `mathlib`) keeps showing diagnostics computed against
// the old `ops.mojo` until main.mojo itself is edited.
//
// mojols keeps a dependency graph of the files open documents import,
// transitively, and when a file is saved or changes on disk it re-opens
// every open document that depends on it, so the backend parses it again.
// Unsaved edits are not a trigger: the backend reads imports from disk, so
// they are invisible to dependents anyway.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tdadadavid/mojols/server/internal/discover"
	"github.com/tdadadavid/mojols/server/internal/imports"
	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
)

const (
	maxSourceBytes = 4 << 20 // files larger than this are not scanned for imports
	maxGraphWalk   = 5000    // files visited per dependency query
)

type depEntry struct {
	key  string // content hash (open doc) or size+mtime (disk)
	deps []string
}

// depGraph maps a file to the files it imports. Entries are validated
// against the file's current content on every lookup, so it never serves
// a stale edge.
type depGraph struct {
	mu       sync.Mutex
	resolver imports.Resolver
	cache    map[string]depEntry
	// openText returns an open document's text by path.
	openText func(path string) (string, bool)
}

func newDepGraph(openText func(string) (string, bool)) *depGraph {
	return &depGraph{cache: map[string]depEntry{}, openText: openText}
}

func (g *depGraph) setRoots(roots []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.Join(roots, "\x00") != strings.Join(g.resolver.Roots, "\x00") {
		g.resolver.Roots = append([]string(nil), roots...)
		g.cache = map[string]depEntry{} // resolution depends on the roots
	}
}

func (g *depGraph) invalidate(path string) {
	g.mu.Lock()
	delete(g.cache, path)
	g.mu.Unlock()
}

// source returns a file's text and a key that changes when it does.
func (g *depGraph) source(path string) (string, string, bool) {
	if text, ok := g.openText(path); ok {
		return text, "open:" + contentHash(text), true
	}
	if !strings.HasSuffix(path, ".mojo") && !strings.HasSuffix(path, ".🔥") {
		return "", "", false // .mojopkg: a leaf
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() > maxSourceBytes {
		return "", "", false
	}
	key := fmt.Sprintf("disk:%d:%d", st.Size(), st.ModTime().UnixNano())
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	return string(b), key, true
}

func (g *depGraph) direct(path string) []string {
	text, key, ok := g.source(path)
	if !ok {
		return nil
	}
	g.mu.Lock()
	e, hit := g.cache[path]
	resolver := g.resolver
	g.mu.Unlock()
	if hit && e.key == key {
		return e.deps
	}
	seen := map[string]bool{path: true}
	var deps []string
	for _, imp := range imports.Scan(text) {
		for _, d := range resolver.Resolve(path, imp) {
			if !seen[d] {
				seen[d] = true
				deps = append(deps, d)
			}
		}
	}
	g.mu.Lock()
	g.cache[path] = depEntry{key: key, deps: deps}
	g.mu.Unlock()
	return deps
}

// reaches reports whether path imports any of targets, directly or not.
func (g *depGraph) reaches(path string, targets map[string]bool) bool {
	visited := map[string]bool{path: true}
	stack := []string{path}
	for len(stack) > 0 && len(visited) < maxGraphWalk {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range g.direct(cur) {
			if targets[d] {
				return true
			}
			if !visited[d] {
				visited[d] = true
				stack = append(stack, d)
			}
		}
	}
	return false
}

func uriToPath(uri string) (string, bool) { return discover.URIToPath(uri) }

// noteChangedLocked records that a file changed on disk and schedules a
// refresh of its dependents.
func (s *Server) noteChangedLocked(uri string) {
	if s.opts.NoRefresh {
		return
	}
	path, ok := uriToPath(uri)
	if !ok {
		return
	}
	s.changed[path] = true
	s.deps.invalidate(path)
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
	s.refreshTimer = time.AfterFunc(s.opts.RefreshDebounce, s.refreshDependents)
}

// refreshDependents re-opens every open document that imports a changed file.
func (s *Server) refreshDependents() {
	s.mu.Lock()
	changed := s.changed
	s.changed = map[string]bool{}
	if !s.ready || s.exited || len(changed) == 0 {
		// Not ready: the replay after the restart re-opens everything anyway.
		s.mu.Unlock()
		return
	}
	var candidates []string
	for _, d := range s.docs.Snapshot() {
		if q, p := s.blockedLocked(d.URI); !q && !p {
			candidates = append(candidates, d.URI)
		}
	}
	s.mu.Unlock()

	start := time.Now()
	var stale []string
	for _, uri := range candidates {
		path, ok := uriToPath(uri)
		if !ok || changed[path] {
			continue // the changed file itself is already current in the backend
		}
		if s.deps.reaches(path, changed) {
			stale = append(stale, uri)
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Strings(stale)

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return
	}
	n := 0
	for _, uri := range stale {
		d, ok := s.docs.Get(uri)
		if !ok {
			continue
		}
		if q, p := s.blockedLocked(uri); q || p {
			continue
		}
		s.sendBackendLocked(&jsonrpc.Message{Method: "textDocument/didClose", Params: mustJSON(map[string]any{"textDocument": map[string]any{"uri": uri}})})
		s.sendDidOpenLocked(d)
		n++
	}
	s.refreshes += n
	s.log.Info("refreshed documents whose imports changed", "count", n, "changed", sortedKeys(changed), "took", time.Since(start).Round(time.Microsecond))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// clientWatchesFiles reports whether the editor accepts dynamic
// registration of workspace/didChangeWatchedFiles.
func clientWatchesFiles(initParams []byte) bool {
	var p struct {
		Capabilities struct {
			Workspace struct {
				DidChangeWatchedFiles struct {
					DynamicRegistration bool `json:"dynamicRegistration"`
				} `json:"didChangeWatchedFiles"`
			} `json:"workspace"`
		} `json:"capabilities"`
	}
	return json.Unmarshal(initParams, &p) == nil && p.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
}

// registerWatchersLocked asks the editor to report changes to Mojo files
// on disk (git checkout, generators, other editors).
func (s *Server) registerWatchersLocked() *jsonrpc.Message {
	if s.watching || s.opts.NoRefresh || !clientWatchesFiles(s.initParams) {
		return nil
	}
	s.watching = true
	s.nextID++
	id := jsonrpc.NumberID(s.nextID)
	return &jsonrpc.Message{ID: &id, Method: "client/registerCapability", Params: mustJSON(map[string]any{
		"registrations": []any{map[string]any{
			"id":     "mojols-watch-mojo-files",
			"method": "workspace/didChangeWatchedFiles",
			"registerOptions": map[string]any{"watchers": []any{
				map[string]any{"globPattern": "**/*.mojo"},
				map[string]any{"globPattern": "**/*.🔥"},
				map[string]any{"globPattern": "**/*.mojopkg"},
			}},
		}},
	})}
}
