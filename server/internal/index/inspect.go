package index

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// UnusedImport is one imported name (or module) a file never uses.
type UnusedImport struct {
	Import *syntax.Import
	// Name is the index into Import.Names, or -1 for a plain
	// `import a.b [as c]`.
	Name int
	// Bound is the name the import introduces (the module path for a plain
	// `import a.b`); Span covers it (with its
	// `as alias`), or the whole statement when nothing else is imported.
	Bound string
	Span  syntax.Span
	// Whole reports that no name of the statement is used.
	Whole bool
}

// UnusedImports lists the imports of f that nothing uses. It is
// conservative: a name spelled anywhere else in the file counts as used,
// star imports are skipped, and so are names another workspace file
// imports through f. reexports says whether f's imports may be public API
// (a package module): then nothing is reported.
func (ix *Index) UnusedImports(f *File, reexports bool) []UnusedImport {
	if f == nil || reexports || filepath.Base(f.Path) == "__init__.mojo" || f.Syn.Panic != nil {
		return nil
	}
	// Uses: every occurrence outside import statements.
	used := map[string]bool{}
	in := func(off int) bool {
		for _, im := range f.Syn.Imports {
			if off >= im.Span.Start && off < im.Span.End {
				return true
			}
		}
		return false
	}
	for _, r := range f.Syn.Refs {
		switch r.Kind {
		case syntax.RName, syntax.RKwarg, syntax.RMember:
			if !in(r.Span.Start) {
				used[r.Name] = true
			}
		}
	}
	var out []UnusedImport
	for _, im := range f.Syn.Imports {
		if im.Module == "" || im.Star || im.Span.Start < 0 {
			continue
		}
		topLevel := lineStartOf(f.Syn.Src, im.Span.Start) == im.Span.Start
		elsewhere := func(bound string) bool { return topLevel && ix.ImportedElsewhere(f, bound) }
		if !im.From {
			bound := im.Alias
			if bound == "" {
				bound = strings.Split(im.Module, ".")[0]
			}
			if !used[bound] && !elsewhere(bound) {
				if im.Alias == "" {
					bound = im.Module // what the reader sees: `import a.b`
				}
				out = append(out, UnusedImport{Import: im, Name: -1, Bound: bound, Span: im.Span, Whole: true})
			}
			continue
		}
		var dead []UnusedImport
		for i, n := range im.Names {
			bound, sp := n.Name, n.NameSpan
			if n.Alias != "" {
				bound = n.Alias
				sp.End = n.AliasSpan.End
			}
			if bound == "" || used[bound] || elsewhere(bound) {
				continue
			}
			dead = append(dead, UnusedImport{Import: im, Name: i, Bound: bound, Span: sp})
		}
		if len(dead) > 0 && len(dead) == len(im.Names) {
			for i := range dead {
				dead[i].Whole = true
			}
		}
		out = append(out, dead...)
	}
	return out
}

func lineStartOf(src []byte, off int) int {
	for off > 0 && src[off-1] != '\n' {
		off--
	}
	return off
}

// Unresolved reports whether the plain name at occurrence i of f resolves
// to nothing: no local, no import, no prelude name.
func (ix *Index) Unresolved(f *File, i int) bool {
	if i < 0 || i >= len(f.Syn.Refs) || f.Syn.Refs[i].Kind != syntax.RName {
		return false
	}
	ix.lock()
	defer ix.mu.Unlock()
	ix.budget = stepBudget
	return len(ix.resolveRef(f, i, 0)) == 0 && ix.budget > 0
}

// ImportCandidate is a way to import a name: `from Module import Name`.
type ImportCandidate struct {
	Module string
	Name   string
	Target Target // the declaration it brings in
}

// ImportCandidates finds the modules that `from` could import name from:
// top-level declarations in the workspace and library sources, and
// packages that re-export one. Each declaration is offered once, through
// its shortest module path; candidates in from's own package come first,
// then the standard library, then the rest.
func (ix *Index) ImportCandidates(from *File, name string, max int) []ImportCandidate {
	if name == "" || strings.HasPrefix(name, "_") {
		return nil
	}
	ix.lock()
	defer ix.mu.Unlock()
	type cand struct {
		ImportCandidate
		depth int
		// foreign marks a package that imports the name from somewhere
		// outside itself: importable there, but more likely that package's
		// own dependency than its API.
		foreign bool
	}
	better := func(a, b cand) bool {
		if a.foreign != b.foreign {
			return !a.foreign
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return a.Module < b.Module
	}
	best := map[*syntax.Decl]cand{}
	for _, g := range ix.searchFiles() {
		if g == from {
			continue
		}
		ds := g.Syn.Module.Names[name]
		if len(ds) == 0 {
			continue
		}
		d := pick(ds)
		if d == nil {
			continue
		}
		isInit := filepath.Base(g.Path) == "__init__.mojo"
		var t Target
		switch d.Kind {
		case syntax.KStruct, syntax.KTrait, syntax.KFunc, syntax.KComptime, syntax.KVar:
			t = Target{File: g, Decl: d}
		case syntax.KImport:
			if !isInit {
				continue // only packages re-export
			}
			ix.budget = stepBudget
			ft, ok := Final(ix.lookupModule(g, name, 0))
			if !ok || ft.IsModule() || ft.Decl.Kind == syntax.KImport {
				continue
			}
			t = ft
		default:
			continue
		}
		module := ix.moduleName(g)
		if module == "" || ix.resolveModule(from.Path, module) != g {
			continue // not importable from here under that name
		}
		c := cand{ImportCandidate: ImportCandidate{Module: module, Name: name, Target: t}, depth: strings.Count(module, ".")}
		if t.File != g {
			rel, err := filepath.Rel(filepath.Dir(g.Path), t.File.Path)
			c.foreign = err != nil || strings.HasPrefix(rel, "..")
		}
		if old, ok := best[t.Decl]; !ok || better(c, old) {
			best[t.Decl] = c
		}
	}
	out := make([]cand, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	own := ""
	if top := ix.outermostPackage(filepath.Dir(from.Path)); top != "" {
		own = filepath.Base(top)
	}
	rank := func(c cand) int {
		first := strings.Split(c.Module, ".")[0]
		switch {
		case own != "" && first == own:
			return 0
		case first == "std":
			return 1
		}
		return 2
	}
	sort.Slice(out, func(a, b int) bool {
		if ra, rb := rank(out[a]), rank(out[b]); ra != rb {
			return ra < rb
		}
		return better(out[a], out[b])
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	res := make([]ImportCandidate, len(out))
	for i, c := range out {
		res[i] = c.ImportCandidate
	}
	return res
}

// moduleName is the absolute dotted name of g: its path below the
// directory holding its outermost package, or its file name alone when it
// is in no package.
func (ix *Index) moduleName(g *File) string {
	dir := filepath.Dir(g.Path)
	stem := strings.TrimSuffix(filepath.Base(g.Path), ".mojo")
	top := ix.outermostPackage(dir)
	if top == "" {
		if stem == "__init__" {
			return ""
		}
		return stem
	}
	rel, err := filepath.Rel(filepath.Dir(top), dir)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if stem != "__init__" {
		parts = append(parts, stem)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || !syntax.IsIdent(p) {
			return ""
		}
	}
	return strings.Join(parts, ".")
}
