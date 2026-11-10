package index

import (
	"sort"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// TypeAt returns the struct or trait for a type hierarchy at offset in f:
// the one named there, or the type of the variable, argument or field
// named there.
func (ix *Index) TypeAt(f *File, offset int) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	i := f.Syn.RefAt(offset)
	if i < 0 {
		return Target{}, false
	}
	ix.budget = stepBudget
	t, ok := Final(ix.resolveRef(f, i, 0))
	if !ok || t.IsModule() {
		return Target{}, false
	}
	if isType(t.Decl) {
		return t, true
	}
	ix.budget = stepBudget
	ty, ok := ix.typeOfDecl(t, 0)
	if !ok || ty.T.IsModule() || !isType(ty.T.Decl) {
		return Target{}, false
	}
	return ty.T, true
}

func isType(d *syntax.Decl) bool {
	return d.Kind == syntax.KStruct || d.Kind == syntax.KTrait
}

// DeclAt finds the struct or trait named name whose name starts at, or
// nearest to, offset in path (a hierarchy item from an earlier request;
// the file may have changed since).
func (ix *Index) DeclAt(path, name string, offset int) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	f := ix.file(path)
	if f == nil {
		return Target{}, false
	}
	var best *syntax.Decl
	for _, d := range f.Syn.Decls {
		if d.Name != name || !isType(d) {
			continue
		}
		if best == nil || abs(d.NameSpan.Start-offset) < abs(best.NameSpan.Start-offset) {
			best = d
		}
	}
	if best == nil {
		return Target{}, false
	}
	return Target{File: f, Decl: best}, true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Supertypes lists the traits (or structs) t declares it conforms to or
// inherits from, in order, resolved across files. Names that cannot be
// resolved (from a binary package) are left out.
func (ix *Index) Supertypes(t Target) []Target {
	ix.lock()
	defer ix.mu.Unlock()
	var out []Target
	seen := map[*syntax.Decl]bool{}
	for _, b := range t.Decl.Bases {
		i := t.File.Syn.RefAt(b.Start)
		if i < 0 {
			continue
		}
		ix.budget = stepBudget
		s, ok := Final(ix.resolveRef(t.File, i, 0))
		if !ok || s.IsModule() || !isType(s.Decl) || seen[s.Decl] {
			continue
		}
		seen[s.Decl] = true
		out = append(out, s)
	}
	return out
}

// Subtypes lists the structs and traits in the workspace, open documents
// and library sources that declare they conform to, or inherit from, t.
func (ix *Index) Subtypes(t Target) []Target {
	ix.lock()
	defer ix.mu.Unlock()
	var out []Target
	for _, f := range ix.searchFiles() {
		for _, d := range f.Syn.Decls {
			if !isType(d) {
				continue
			}
			for _, b := range d.Bases {
				i := f.Syn.RefAt(b.Start)
				if i < 0 {
					continue
				}
				// Cheap filter first: the base must be spelled like t or
				// be an import alias.
				if name := f.Syn.Refs[i].Name; name != t.Decl.Name && !ix.isAliasName(f, name) {
					continue
				}
				ix.budget = stepBudget
				s, ok := Final(ix.resolveRef(f, i, 0))
				if ok && !s.IsModule() && s.Decl == t.Decl && s.File == t.File {
					out = append(out, Target{File: f, Decl: d})
					break
				}
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Decl.Name != out[b].Decl.Name {
			return out[a].Decl.Name < out[b].Decl.Name
		}
		return out[a].File.Path < out[b].File.Path
	})
	return out
}

// isAliasName reports whether name is bound by an aliased import in f.
func (ix *Index) isAliasName(f *File, name string) bool {
	for _, d := range f.Syn.Module.Names[name] {
		if d.Kind == syntax.KImport && d.Import != nil {
			for _, n := range d.Import.Names {
				if n.Decl == d && n.Alias != "" {
					return true
				}
			}
			if d.Import.Alias != "" {
				return true
			}
		}
	}
	return false
}
