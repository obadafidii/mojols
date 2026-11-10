package index

import (
	"sort"
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// OccurrenceAt returns the index of the identifier occurrence at offset in
// f, or -1.
func (ix *Index) OccurrenceAt(f *File, offset int) int {
	return f.Syn.RefAt(offset)
}

// DefinitionAt resolves the identifier at offset in f to its declaration
// (or module). ok is false when there is no identifier there or it cannot
// be resolved statically.
func (ix *Index) DefinitionAt(f *File, offset int) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	i := f.Syn.RefAt(offset)
	if i < 0 {
		return Target{}, false
	}
	return Final(ix.resolveRef(f, i, 0))
}

// LookupFrom resolves name as if it were written in place of occurrence i
// of f (same scope and position), for rename collision checks.
func (ix *Index) LookupFrom(f *File, i int, name string) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	if i < 0 || i >= len(f.Syn.Refs) {
		return Target{}, false
	}
	r := f.Syn.Refs[i]
	return Final(ix.lookupScope(f, r.Scope, name, r.Span.Start, 0))
}

// SymbolMatch is one workspace symbol search result.
type SymbolMatch struct {
	File *File
	Decl *syntax.Decl
	rank int
}

// Symbols searches the declarations of the workspace and open documents
// (structs, traits, defs, comptime members, fields at module or struct
// level) for query: exact, then prefix, then substring, then subsequence
// matches, case-insensitively. An empty query lists everything up to max.
func (ix *Index) Symbols(query string, max int) []SymbolMatch {
	ix.lock()
	defer ix.mu.Unlock()
	q := strings.ToLower(query)
	var out []SymbolMatch
	for _, f := range ix.workspaceFiles() {
		for _, d := range f.Syn.Decls {
			switch d.Kind {
			case syntax.KStruct, syntax.KTrait, syntax.KFunc, syntax.KComptime, syntax.KField:
			default:
				continue
			}
			if d.Scope == nil || (d.Scope.Kind != syntax.SModule && d.Scope.Kind != syntax.SStruct) {
				continue
			}
			rank, ok := matchRank(strings.ToLower(d.Name), q)
			if !ok {
				continue
			}
			out = append(out, SymbolMatch{File: f, Decl: d, rank: rank})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].rank != out[b].rank {
			return out[a].rank < out[b].rank
		}
		if len(out[a].Decl.Name) != len(out[b].Decl.Name) {
			return len(out[a].Decl.Name) < len(out[b].Decl.Name)
		}
		return out[a].Decl.Name < out[b].Decl.Name
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

func matchRank(name, q string) (int, bool) {
	switch {
	case q == "":
		return 4, true
	case name == q:
		return 0, true
	case strings.HasPrefix(name, q):
		return 1, true
	case strings.Contains(name, q):
		return 2, true
	}
	// Subsequence: every query character in order.
	i := 0
	for j := 0; j < len(name) && i < len(q); j++ {
		if name[j] == q[i] {
			i++
		}
	}
	return 3, i == len(q)
}

// ImportedElsewhere reports whether some other workspace file or open
// document imports the name that f's top-level import binds as bound,
// through f (a re-export). Organize imports keeps such names.
func (ix *Index) ImportedElsewhere(f *File, bound string) bool {
	ix.lock()
	defer ix.mu.Unlock()
	var decls []*syntax.Decl
	for _, d := range f.Syn.Module.Names[bound] {
		if d.Kind == syntax.KImport {
			decls = append(decls, d)
		}
	}
	if len(decls) == 0 {
		return false
	}
	for _, g := range ix.workspaceFiles() {
		if g == f {
			continue
		}
		for _, i := range g.ByName[bound] {
			ix.budget = stepBudget
			for _, t := range ix.resolveRef(g, i, 0) {
				if t.File != f || t.IsModule() {
					continue
				}
				for _, d := range decls {
					if t.Decl == d {
						return true
					}
				}
			}
		}
	}
	return false
}
