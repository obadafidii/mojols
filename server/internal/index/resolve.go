package index

import (
	"github.com/tdadadavid/mojols/server/internal/syntax"
)

// maxDepth bounds every chain of resolution steps (imports through
// re-exports, aliases of aliases, members of members), so cyclic or
// pathological code costs a bounded amount of work.
const maxDepth = 24

// Resolve returns what occurrence i of f names, as a chain: the hops on
// the way (an import, a package re-export) and, last, the declaration
// itself. It returns nil when the name cannot be resolved statically.
func (ix *Index) Resolve(f *File, i int) []Target {
	ix.lock()
	defer ix.mu.Unlock()
	return ix.resolveRef(f, i, 0)
}

// Final is the last element of a chain: the declaration itself.
func Final(chain []Target) (Target, bool) {
	if len(chain) == 0 {
		return Target{}, false
	}
	return chain[len(chain)-1], true
}

func (ix *Index) resolveRef(f *File, i int, depth int) []Target {
	if f == nil || i < 0 || i >= len(f.Syn.Refs) || depth > maxDepth {
		return nil
	}
	if depth == 0 {
		ix.budget = stepBudget
	}
	ix.memo(f)
	if chain, ok := f.refMemo[i]; ok {
		return chain
	}
	f.refMemo[i] = nil // in progress: a cycle resolves to nothing
	chain := ix.resolveRefUncached(f, i, depth)
	if ix.budget <= 0 {
		// Out of budget: the answer may be incomplete; do not keep it.
		delete(f.refMemo, i)
		return chain
	}
	f.refMemo[i] = chain
	return chain
}

func (ix *Index) resolveRefUncached(f *File, i int, depth int) []Target {
	r := &f.Syn.Refs[i]
	switch r.Kind {
	case syntax.RDecl:
		t := Target{File: f, Decl: r.Decl}
		if r.Decl.Kind == syntax.KImport {
			return append([]Target{t}, ix.followImport(f, r.Decl, depth+1)...)
		}
		if r.Decl.Capture && r.Decl.Scope != nil && r.Decl.Scope.Parent != nil {
			// A capture-list entry stands for the outer variable.
			return append([]Target{t}, ix.lookupScope(f, r.Decl.Scope.Parent, r.Name, r.Decl.Scope.Span.Start, depth+1)...)
		}
		return []Target{t}
	case syntax.RName:
		return ix.lookupScope(f, r.Scope, r.Name, r.Span.Start, depth+1)
	case syntax.RMember:
		// A member of whatever its base evaluates to: a name chain, a
		// call or subscript, a literal, a parenthesized expression, or
		// the type a contextual member's context expects.
		bt, ok := ix.baseType(f, i, nil, depth+1)
		if !ok {
			return nil
		}
		return ix.memberIn(bt, r.Name, depth+1)
	case syntax.RImported:
		// `from M import name as alias`: name is looked up in M.
		for _, im := range f.Syn.Imports {
			for _, n := range im.Names {
				if n.NameSpan == r.Span {
					return ix.lookupModule(ix.resolveModule(f.Path, im.Module), n.Name, depth+1)
				}
			}
		}
		return nil
	case syntax.RKwarg:
		if r.Callee < 0 {
			return nil
		}
		callee, ok := Final(ix.resolveRef(f, r.Callee, depth+1))
		if !ok {
			return nil
		}
		return ix.kwarg(callee, r.Name, depth+1)
	}
	return nil
}

// withImport extends a found declaration: an import is followed to what
// it imports.
func (ix *Index) withImport(f *File, d *syntax.Decl, depth int) []Target {
	t := Target{File: f, Decl: d}
	if d.Kind != syntax.KImport {
		return []Target{t}
	}
	rest := ix.followImport(f, d, depth+1)
	if rest == nil {
		return nil
	}
	return append([]Target{t}, rest...)
}

// followImport resolves the name an import statement binds.
func (ix *Index) followImport(f *File, d *syntax.Decl, depth int) []Target {
	if depth > maxDepth || d.Import == nil {
		return nil
	}
	im := d.Import
	if !im.From {
		mod := im.Module
		if im.Alias == "" {
			// `import a.b` binds `a`.
			if k := indexByte(mod, '.'); k > 0 {
				mod = mod[:k]
			}
		}
		if m := ix.resolveModule(f.Path, mod); m != nil {
			return []Target{{File: m}}
		}
		return nil
	}
	m := ix.resolveModule(f.Path, im.Module)
	if m == nil {
		return nil
	}
	name := ""
	for _, n := range im.Names {
		if n.Decl == d {
			name = n.Name
		}
	}
	if name == "" {
		return nil
	}
	if m == f {
		// `from . import path` inside a package's own __init__: a submodule.
		if sub := ix.submodule(m, name); sub != nil {
			return []Target{{File: sub}}
		}
		return nil
	}
	if chain := ix.lookupModule(m, name, depth+1); chain != nil {
		return chain
	}
	if sub := ix.submodule(m, name); sub != nil {
		return []Target{{File: sub}}
	}
	return nil
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// lookupModule finds a top-level name in module m: its own declarations
// (an import there is a re-export and is followed), then its star imports,
// then, for a package, a submodule of that name.
func (ix *Index) lookupModule(m *File, name string, depth int) []Target {
	if m == nil || depth > maxDepth {
		return nil
	}
	ix.memo(m)
	if chain, ok := m.nameMemo[name]; ok {
		return chain
	}
	m.nameMemo[name] = nil
	chain := ix.lookupModuleUncached(m, name, depth)
	m.nameMemo[name] = chain
	return chain
}

func (ix *Index) lookupModuleUncached(m *File, name string, depth int) []Target {
	if d := pick(m.Syn.Module.Names[name]); d != nil {
		return ix.withImport(m, d, depth+1)
	}
	for _, im := range m.Syn.Imports {
		if !im.Star {
			continue
		}
		if chain := ix.lookupModule(ix.resolveModule(m.Path, im.Module), name, depth+1); chain != nil {
			return chain
		}
	}
	if sub := ix.submodule(m, name); sub != nil {
		return []Target{{File: sub}}
	}
	return nil
}

// pick chooses among same-named declarations in one scope: the first real
// declaration, else the first import. Overloads share a name; the first
// stands for the set.
func pick(ds []*syntax.Decl) *syntax.Decl {
	for _, d := range ds {
		if d.Kind != syntax.KImport {
			return d
		}
	}
	if len(ds) > 0 {
		return ds[0]
	}
	return nil
}

// pickOutside is pick, ignoring declarations whose own statement contains
// pos.
func pickOutside(ds []*syntax.Decl, pos int) *syntax.Decl {
	var keep []*syntax.Decl
	for _, d := range ds {
		if !(d.NameSpan.Start <= pos && pos < d.Visible) {
			keep = append(keep, d)
		}
	}
	return pick(keep)
}

// lookupScope resolves a plain name used at pos in scope s.
func (ix *Index) lookupScope(f *File, s *syntax.Scope, name string, pos int, depth int) []Target {
	if name == "_" {
		return nil // a placeholder (`MutUnsafePointer[_]`), never a name
	}
	if name == "Self" {
		for sc := s; sc != nil; sc = sc.Parent {
			if sc.Kind == syntax.SStruct && sc.Owner != nil {
				return []Target{{File: f, Decl: sc.Owner}}
			}
		}
		return nil
	}
	var prev *syntax.Scope
	for sc := s; sc != nil; prev, sc = sc, sc.Parent {
		ds := sc.Names[name]
		if len(ds) == 0 {
			continue
		}
		var found *syntax.Decl
		switch sc.Kind {
		case syntax.SBlock, syntax.SFunc:
			// The latest declaration before the use; parameters and
			// arguments are always in view.
			for _, d := range ds {
				if d.Visible <= pos || d.Kind == syntax.KParam || d.Kind == syntax.KArg {
					found = d
				}
			}
		case syntax.SStruct:
			if prev == nil || prev.Kind == syntax.SComp {
				// Used in the struct's own header or members; a member
				// is not in view inside its own declaration
				// (`comptime WeakPointer = WeakPointer[Self.T]`).
				// Methods and fields need `Self.`; only parameters and
				// comptime members are in view by bare name.
				var typeLevel []*syntax.Decl
				for _, d := range ds {
					if d.Kind == syntax.KParam || d.Kind == syntax.KComptime {
						typeLevel = append(typeLevel, d)
					}
				}
				found = pickOutside(typeLevel, pos)
			} else {
				// From inside a method: only compile-time parameters and
				// comptime members are in view without `Self.`.
				for _, d := range ds {
					if d.Kind == syntax.KParam || d.Kind == syntax.KComptime {
						found = d
						break
					}
				}
			}
		case syntax.SModule:
			found = pickOutside(ds, pos)
		default: // SComp
			found = pick(ds)
		}
		if found != nil {
			if found.Capture && found.Scope != nil && found.Scope.Parent != nil {
				// A captured name stands for the outer variable.
				outer := ix.lookupScope(f, found.Scope.Parent, name, found.Scope.Span.Start, depth+1)
				return append([]Target{{File: f, Decl: found}}, outer...)
			}
			return ix.withImport(f, found, depth+1)
		}
	}
	for _, im := range f.Syn.Imports {
		if !im.Star {
			continue
		}
		if chain := ix.lookupModule(ix.resolveModule(f.Path, im.Module), name, depth+1); chain != nil {
			return chain
		}
	}
	if prelude := ix.resolveModule(f.Path, "std.prelude"); prelude != nil && prelude != f {
		return ix.lookupModule(prelude, name, depth+1)
	}
	return nil
}

// memberOfType finds a member of a struct or trait, then in the traits it
// lists (default methods, associated comptime members).
func (ix *Index) memberOfType(f *File, d *syntax.Decl, name string, depth int, seen map[*syntax.Decl]bool) []Target {
	if depth > maxDepth || d.Body == nil {
		return nil
	}
	if found := pick(d.Body.Names[name]); found != nil {
		return []Target{{File: f, Decl: found}}
	}
	if seen == nil {
		seen = map[*syntax.Decl]bool{}
	}
	seen[d] = true
	for _, b := range d.Bases {
		i := f.Syn.RefAt(b.Start)
		if i < 0 {
			continue
		}
		bt, ok := Final(ix.resolveRef(f, i, depth+1))
		if !ok || bt.IsModule() || seen[bt.Decl] || (bt.Decl.Kind != syntax.KTrait && bt.Decl.Kind != syntax.KStruct) {
			continue
		}
		if chain := ix.memberOfType(bt.File, bt.Decl, name, depth+1, seen); chain != nil {
			return chain
		}
	}
	return nil
}

// kwarg resolves `name=` in a call or parameter list to the callee's
// argument or parameter. Overloads are searched in order; a struct is
// searched for parameters, then its __init__ arguments.
func (ix *Index) kwarg(callee Target, name string, depth int) []Target {
	if callee.IsModule() || depth > maxDepth {
		return nil
	}
	d := callee.Decl
	switch d.Kind {
	case syntax.KFunc, syntax.KComptime:
		var group []*syntax.Decl
		if d.Scope != nil {
			group = d.Scope.Names[d.Name]
		}
		if len(group) == 0 {
			group = []*syntax.Decl{d}
		}
		for _, g := range group {
			if g.Body != nil {
				if a := argNamed(g.Body, name); a != nil {
					return []Target{{File: callee.File, Decl: a}}
				}
			}
		}
	case syntax.KStruct:
		if d.Body == nil {
			return nil
		}
		if a := argNamed(d.Body, name); a != nil {
			return []Target{{File: callee.File, Decl: a}}
		}
		for _, init := range d.Body.Names["__init__"] {
			if init.Body != nil {
				if a := argNamed(init.Body, name); a != nil {
					return []Target{{File: callee.File, Decl: a}}
				}
			}
		}
	}
	return nil
}

func argNamed(s *syntax.Scope, name string) *syntax.Decl {
	for _, d := range s.Names[name] {
		if d.Kind == syntax.KArg || d.Kind == syntax.KParam {
			return d
		}
	}
	return nil
}
