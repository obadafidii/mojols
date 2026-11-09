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
	ix.memo(f)
	if chain, ok := f.refMemo[i]; ok {
		return chain
	}
	f.refMemo[i] = nil // in progress: a cycle resolves to nothing
	chain := ix.resolveRefUncached(f, i, depth)
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
		switch {
		case r.Base >= 0:
			base, ok := Final(ix.resolveRef(f, r.Base, depth+1))
			if !ok {
				return nil
			}
			return ix.member(base, r.Name, depth+1)
		case r.Call >= 0:
			// `f(x).y`: a member of what the call or subscript yields.
			callee, ok := Final(ix.resolveRef(f, r.Call, depth+1))
			if !ok {
				return nil
			}
			t, ok := ix.applyPostfix(callee, r.Post, depth+1)
			if !ok {
				return nil
			}
			return ix.member(t, r.Name, depth+1)
		}
		if c, ok := f.Syn.Ctx[i]; ok {
			return ix.contextual(f, c, r.Name, depth+1)
		}
		return nil
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
	return ix.lookupModule(m, name, depth+1)
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

// member resolves `.name` on a resolved base.
func (ix *Index) member(base Target, name string, depth int) []Target {
	if depth > maxDepth {
		return nil
	}
	if base.IsModule() {
		return ix.lookupModule(base.File, name, depth+1)
	}
	switch base.Decl.Kind {
	case syntax.KStruct, syntax.KTrait:
		return ix.memberOfType(base.File, base.Decl, name, depth+1, nil)
	case syntax.KFunc:
		return nil
	}
	t, ok := ix.typeOf(base.File, base.Decl, depth+1)
	if !ok {
		return nil
	}
	if t.IsModule() {
		return ix.lookupModule(t.File, name, depth+1)
	}
	return ix.memberOfType(t.File, t.Decl, name, depth+1, nil)
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

// typeOf infers the struct or trait a variable, argument, field, parameter
// or comptime alias stands for, from its annotation or its initializer.
func (ix *Index) typeOf(f *File, d *syntax.Decl, depth int) (Target, bool) {
	if depth > maxDepth {
		return Target{}, false
	}
	if d.Kind == syntax.KArg && d.Type.End <= d.Type.Start {
		// An unannotated `self` is the enclosing struct.
		if d.Name == "self" && d.Parent != nil && d.Parent.Scope != nil && d.Parent.Scope.Kind == syntax.SStruct {
			return Target{File: f, Decl: d.Parent.Scope.Owner}, d.Parent.Scope.Owner != nil
		}
		return Target{}, false
	}
	if d.Type.End > d.Type.Start {
		return ix.typeOfSpan(f, d.Type, d, depth+1)
	}
	if d.Value.End > d.Value.Start {
		return ix.typeOfSpan(f, d.Value, d, depth+1)
	}
	return Target{}, false
}

// typeOfSpan infers the type an expression names or yields: its leading
// postfix chain (`a.b(x).c`), then a call or subscript right after it.
// self is the declaration being typed, to stop `var x = x`.
func (ix *Index) typeOfSpan(f *File, sp syntax.Span, self *syntax.Decl, depth int) (Target, bool) {
	if depth > maxDepth {
		return Target{}, false
	}
	i, post := ix.headChain(f, sp)
	if i < 0 {
		return Target{}, false
	}
	t, ok := Final(ix.resolveRef(f, i, depth+1))
	if !ok || (!t.IsModule() && t.Decl == self) {
		return Target{}, false
	}
	if post != 0 {
		return ix.applyPostfix(t, post, depth+1)
	}
	return ix.asType(t, depth+1)
}

// asType turns a resolved name into the type it denotes or holds.
func (ix *Index) asType(t Target, depth int) (Target, bool) {
	if t.IsModule() {
		return t, true
	}
	switch t.Decl.Kind {
	case syntax.KStruct, syntax.KTrait:
		return t, true
	case syntax.KComptime, syntax.KParam, syntax.KVar, syntax.KArg, syntax.KField:
		return ix.typeOf(t.File, t.Decl, depth+1)
	}
	return Target{}, false
}

// applyPostfix gives the type of calling (post '(') or subscripting ('[')
// what t names: a function's return type, a struct's instance, an
// instance's __call__ or __getitem__ result.
func (ix *Index) applyPostfix(t Target, post byte, depth int) (Target, bool) {
	if depth > maxDepth || t.IsModule() {
		return Target{}, false
	}
	switch t.Decl.Kind {
	case syntax.KFunc:
		// `f(x)`, and `f[T]` which is then called.
		return ix.resultType(t, depth+1)
	case syntax.KStruct, syntax.KTrait:
		return t, true
	case syntax.KComptime:
		if t.Decl.Body != nil && post == '[' || t.Decl.Type.End <= t.Decl.Type.Start {
			// A (parameterized) alias: applying it gives what it aliases.
			return ix.typeOf(t.File, t.Decl, depth+1)
		}
	}
	base, ok := ix.asType(t, depth+1)
	if !ok || base.IsModule() {
		return Target{}, false
	}
	method := "__getitem__"
	if post == '(' {
		method = "__call__"
	}
	m, ok := Final(ix.memberOfType(base.File, base.Decl, method, depth+1, nil))
	if !ok || m.Decl.Kind != syntax.KFunc {
		return Target{}, false
	}
	return ix.resultType(m, depth+1)
}

// resultType is a function's return type; overloads are tried in order.
func (ix *Index) resultType(fn Target, depth int) (Target, bool) {
	group := []*syntax.Decl{fn.Decl}
	if fn.Decl.Scope != nil {
		group = append(group, fn.Decl.Scope.Names[fn.Decl.Name]...)
	}
	for _, g := range group {
		if g.Kind == syntax.KFunc && g.Result.End > g.Result.Start {
			return ix.typeOfSpan(fn.File, g.Result, nil, depth+1)
		}
	}
	return Target{}, false
}

// headChain finds the leading postfix chain of an expression: the index
// of its last name (`c` in `a.b(x).c`, `List` in `List[Int]()`) and the
// bracket right after that name, if any. Conventions before a type are
// skipped.
func (ix *Index) headChain(f *File, sp syntax.Span) (int, byte) {
	refs := f.Syn.Refs
	lo, hi := 0, len(refs)
	for lo < hi {
		m := (lo + hi) / 2
		if refs[m].Span.Start < sp.Start {
			lo = m + 1
		} else {
			hi = m
		}
	}
	i := lo
	for i < len(refs) && refs[i].Span.Start < sp.End && (refs[i].Name == "mut" || refs[i].Name == "owned" || refs[i].Name == "read") {
		i++
	}
	if i >= len(refs) || refs[i].Span.Start >= sp.End {
		return -1, 0
	}
	// Only a chain that starts the expression counts (`(a + b)` does not).
	if !blank(f.Syn.Src[sp.Start:refs[i].Span.Start]) {
		return -1, 0
	}
	if refs[i].Name == "Some" && i+1 < len(refs) && refs[i+1].Span.Start < sp.End {
		i++ // Some[Trait]
	}
	for j := i + 1; j < len(refs) && refs[j].Span.Start < sp.End; j++ {
		if refs[j].Kind == syntax.RMember && (refs[j].Base == i || refs[j].Call == i) {
			i = j
		}
	}
	p := refs[i].Span.End
	for p < sp.End && (f.Syn.Src[p] == ' ' || f.Syn.Src[p] == '\t') {
		p++
	}
	if p < sp.End && (f.Syn.Src[p] == '(' || f.Syn.Src[p] == '[') {
		return i, f.Syn.Src[p]
	}
	return i, 0
}

func blank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

// contextual resolves `.name` whose type comes from where it is used: the
// parameter or argument it is passed as, or an expected type.
func (ix *Index) contextual(f *File, c syntax.Context, name string, depth int) []Target {
	var typ Target
	var ok bool
	if c.Expect.End > c.Expect.Start {
		typ, ok = ix.typeOfSpan(f, c.Expect, nil, depth+1)
	} else if c.Callee >= 0 {
		callee, found := Final(ix.resolveRef(f, c.Callee, depth+1))
		if !found {
			return nil
		}
		param, pf := ix.paramFor(callee, c, depth+1)
		if param == nil {
			return nil
		}
		typ, ok = ix.typeOf(pf, param, depth+1)
	}
	if !ok || typ.IsModule() {
		return nil
	}
	return ix.memberOfType(typ.File, typ.Decl, name, depth+1, nil)
}

// paramFor finds the parameter (for '[') or argument (for '(') of callee
// that an argument at position c.Pos, or keyword c.Name, binds to.
func (ix *Index) paramFor(callee Target, c syntax.Context, depth int) (*syntax.Decl, *File) {
	if callee.IsModule() || depth > maxDepth {
		return nil, nil
	}
	want := syntax.KArg
	if c.Op == '[' {
		want = syntax.KParam
	}
	d := callee.Decl
	var bodies []*syntax.Scope
	switch d.Kind {
	case syntax.KFunc:
		group := []*syntax.Decl{d}
		if d.Scope != nil {
			group = append(group, d.Scope.Names[d.Name]...)
		}
		for _, g := range group {
			if g.Body != nil {
				bodies = append(bodies, g.Body)
			}
		}
	case syntax.KStruct:
		if want == syntax.KParam {
			bodies = append(bodies, d.Body)
		} else if d.Body != nil {
			for _, init := range d.Body.Names["__init__"] {
				if init.Body != nil {
					bodies = append(bodies, init.Body)
				}
			}
		}
	case syntax.KComptime:
		if d.Body != nil && want == syntax.KParam {
			bodies = append(bodies, d.Body)
		} else if t, ok := ix.typeOf(callee.File, d, depth+1); ok {
			return ix.paramFor(t, c, depth+1)
		}
	}
	for _, b := range bodies {
		if b == nil {
			continue
		}
		var list []*syntax.Decl
		for _, ds := range b.Names {
			for _, x := range ds {
				if x.Kind == want && x.Scope == b {
					list = append(list, x)
				}
			}
		}
		sortByIndex(list)
		if c.Name != "" {
			for _, x := range list {
				if x.Name == c.Name {
					return x, callee.File
				}
			}
			continue
		}
		var positional []*syntax.Decl
		for k, x := range list {
			if x.InferOnly || (want == syntax.KArg && k == 0 && x.Name == "self") {
				continue
			}
			positional = append(positional, x)
		}
		if c.Pos < len(positional) && positional[c.Pos].Type.End > positional[c.Pos].Type.Start {
			return positional[c.Pos], callee.File
		}
	}
	return nil, nil
}

func sortByIndex(ds []*syntax.Decl) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j].Index < ds[j-1].Index; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
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
