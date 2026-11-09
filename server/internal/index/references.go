package index

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tdadadavid/mojols/server/internal/syntax"
)

// Workspace limits. A references search parses every file under the
// workspace roots once; these bound the cost on huge or odd trees.
const (
	MaxWorkspaceFiles = 20000
	MaxFileBytes      = 4 << 20
)

// skipDirs are never scanned for sources.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, ".pixi": true, ".magic": true,
	".venv": true, "venv": true, "__pycache__": true, "build": true, "dist": true, "target": true,
	".mojo_cache": true, "bazel-bin": true, "bazel-out": true, "bazel-testlogs": true,
}

// SetWorkspace sets the directories whose .mojo files a references search
// covers.
func (ix *Index) SetWorkspace(dirs []string) {
	ix.lock()
	defer ix.mu.Unlock()
	ix.workspace = append([]string(nil), dirs...)
	ix.scanned = false
}

// SetLibraries sets directories of library sources (such as the stdlib
// matching the installed Mojo): imports resolve against them after the
// package roots, and type hierarchies search them, but they are never
// edited and references search only the workspace.
func (ix *Index) SetLibraries(dirs []string) {
	ix.lock()
	defer ix.mu.Unlock()
	ix.libs = append([]string(nil), dirs...)
	ix.libPaths = nil
	ix.stat = map[string]bool{}
	ix.gen++
}

// listSources lists the .mojo files under dirs, within the workspace
// limits.
func listSources(dirs []string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, root := range dirs {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if len(paths) >= MaxWorkspaceFiles {
				return filepath.SkipAll
			}
			if strings.HasSuffix(path, ".mojo") || strings.HasSuffix(path, ".🔥") {
				if info, err := d.Info(); err == nil && info.Size() <= MaxFileBytes && !seen[path] {
					seen[path] = true
					paths = append(paths, path)
				}
			}
			return nil
		})
	}
	return paths
}

// searchFiles is workspaceFiles plus the library sources.
func (ix *Index) searchFiles() []*File {
	files := ix.workspaceFiles()
	if ix.libPaths == nil && len(ix.libs) > 0 {
		ix.libPaths = listSources(ix.libs)
	}
	for _, p := range ix.libPaths {
		if f := ix.file(p); f != nil {
			files = append(files, f)
		}
	}
	return files
}

// workspaceFiles lists (once per change of workspace) and parses every
// .mojo file under the workspace directories.
func (ix *Index) workspaceFiles() []*File {
	if !ix.scanned {
		ix.paths = listSources(ix.workspace)
		ix.scanned = true
	}
	files := make([]*File, 0, len(ix.paths)+len(ix.overlay))
	seen := map[string]bool{}
	for _, p := range ix.paths {
		if f := ix.file(p); f != nil {
			files = append(files, f)
			seen[p] = true
		}
	}
	// Open documents outside the workspace directories still count.
	for p := range ix.overlay {
		if !seen[p] {
			if f := ix.file(p); f != nil {
				files = append(files, f)
			}
		}
	}
	return files
}

// Occurrence is one place a symbol appears.
type Occurrence struct {
	File *File
	Span syntax.Span
	// Decl marks the declaration itself (or an import binding it).
	Decl bool
	// ViaAlias marks a use through an alias (`from m import f as g; g()`):
	// a reference to f, but spelled g, so a rename leaves it alone.
	ViaAlias bool
}

// Symbol is what a references search is about: a declaration, standing
// for its whole overload set.
type Symbol struct {
	File *File
	Decl *syntax.Decl
}

// SymbolAt returns the symbol named at offset in f: the final declaration
// its occurrence resolves to.
func (ix *Index) SymbolAt(f *File, offset int) (Symbol, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	i := f.Syn.RefAt(offset)
	if i < 0 {
		return Symbol{}, false
	}
	t, ok := Final(ix.resolveRef(f, i, 0))
	if !ok || t.IsModule() {
		return Symbol{}, false
	}
	return Symbol{File: t.File, Decl: t.Decl}, true
}

// same reports whether t is sym, or an overload of it.
func (sym Symbol) same(t Target) bool {
	if t.IsModule() || t.File != sym.File {
		return false
	}
	if t.Decl == sym.Decl {
		return true
	}
	return sym.Decl.Kind == syntax.KFunc && t.Decl.Kind == syntax.KFunc && t.Decl.Scope == sym.Decl.Scope && t.Decl.Name == sym.Decl.Name
}

// References finds every occurrence of sym across the workspace and open
// documents, in file and position order.
func (ix *Index) References(sym Symbol) []Occurrence {
	occs, _ := ix.ReferencesAndUnknown(sym)
	return occs
}

// ReferencesAndUnknown is References, plus the occurrences of sym's name
// that mojols cannot resolve statically but that could be sym: members of
// expressions whose type it cannot infer, when sym is a member of a
// struct or trait. The official server can settle those (internal/verify).
func (ix *Index) ReferencesAndUnknown(sym Symbol) ([]Occurrence, []Occurrence) {
	ix.lock()
	defer ix.mu.Unlock()
	var unknown []Occurrence
	isMember := sym.Decl.Parent != nil && (sym.Decl.Parent.Kind == syntax.KStruct || sym.Decl.Parent.Kind == syntax.KTrait) &&
		sym.Decl.Scope == sym.Decl.Parent.Body
	files := ix.workspaceFiles()
	type key struct {
		f    *File
		name string
	}
	var work []key
	queued := map[key]bool{}
	push := func(f *File, name string) {
		k := key{f, name}
		if !queued[k] {
			queued[k] = true
			work = append(work, k)
		}
	}
	// A local can only be referenced from its own file.
	local := sym.Decl.Scope != nil && sym.Decl.Scope.Kind != syntax.SModule && sym.Decl.Scope.Kind != syntax.SStruct
	for _, f := range files {
		if local && f != sym.File {
			continue
		}
		push(f, sym.Decl.Name)
	}
	if local {
		push(sym.File, sym.Decl.Name)
	}
	var out []Occurrence
	seen := map[*File]map[int]bool{}
	for len(work) > 0 {
		k := work[0]
		work = work[1:]
		for _, i := range k.f.ByName[k.name] {
			if seen[k.f][i] {
				continue
			}
			chain := ix.resolveRef(k.f, i, 0)
			if len(chain) == 0 && isMember && k.name == sym.Decl.Name && k.f.Syn.Refs[i].Kind == syntax.RMember {
				unknown = append(unknown, Occurrence{File: k.f, Span: k.f.Syn.Refs[i].Span})
				continue
			}
			hit, viaAlias := false, false
			for n, t := range chain {
				if sym.same(t) {
					hit = true
					break
				}
				// An alias on the way: `g` in `from m import f as g`.
				if !t.IsModule() && t.Decl.Kind == syntax.KImport && n+1 < len(chain) && !chain[n+1].IsModule() && t.Decl.Name != chain[n+1].Decl.Name {
					viaAlias = true
				}
			}
			if !hit {
				continue
			}
			if seen[k.f] == nil {
				seen[k.f] = map[int]bool{}
			}
			seen[k.f][i] = true
			r := k.f.Syn.Refs[i]
			occ := Occurrence{File: k.f, Span: r.Span, Decl: r.Kind == syntax.RDecl, ViaAlias: viaAlias}
			out = append(out, occ)
			// Uses of an alias bound here are references too.
			if r.Kind == syntax.RDecl && r.Decl.Kind == syntax.KImport && r.Decl.Name != sym.Decl.Name {
				push(k.f, r.Decl.Name)
			}
			if r.Kind == syntax.RImported {
				for _, im := range k.f.Syn.Imports {
					for _, n := range im.Names {
						if n.NameSpan == r.Span && n.Alias != "" {
							push(k.f, n.Alias)
						}
					}
				}
			}
		}
	}
	sortOccurrences(out)
	sortOccurrences(unknown)
	return out, unknown
}

func sortOccurrences(out []Occurrence) {
	sort.Slice(out, func(a, b int) bool {
		if out[a].File.Path != out[b].File.Path {
			return out[a].File.Path < out[b].File.Path
		}
		return out[a].Span.Start < out[b].Span.Start
	})
}

// InWorkspace reports whether path is under one of the workspace
// directories (so a rename may edit it).
func (ix *Index) InWorkspace(path string) bool {
	ix.lock()
	defer ix.mu.Unlock()
	if _, open := ix.overlay[path]; open {
		return true
	}
	for _, root := range ix.workspace {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}
