package index

import (
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// Type is what an expression evaluates to: a struct, trait or module (or,
// for a function used as a value, its def), either the type itself
// (`List[Int]`) or an instance of it (`List[Int]()`), with its
// compile-time parameters bound to the arguments it was given.
type Type struct {
	T    Target
	Inst bool
	Env  *Env
}

// Env binds compile-time parameters (of a struct, a parameterized
// comptime alias) to the argument expressions given for them, each to be
// evaluated in its own environment: `Pointer[Node]` binds Pointer's
// `type` to `Node`, so `node[]` yields a Node.
type Env struct {
	binds map[*syntax.Decl]boundArg
}

type boundArg struct {
	f   *File
	sp  syntax.Span
	env *Env
}

func (e *Env) lookup(d *syntax.Decl) (boundArg, bool) {
	if e == nil {
		return boundArg{}, false
	}
	a, ok := e.binds[d]
	return a, ok
}

// stepBudget bounds the work of one resolution, whatever the code looks
// like: generic code can branch a lot, and every answer must stay fast.
const stepBudget = 20000

func (ix *Index) step() bool {
	ix.budget--
	return ix.budget > 0
}

// valueType is the type of the value occurrence i of f stands for. env
// binds parameters when evaluating code inside an instantiated generic.
func (ix *Index) valueType(f *File, i int, env *Env, depth int) (Type, bool) {
	if f == nil || i < 0 || i >= len(f.Syn.Refs) || depth > maxDepth || !ix.step() {
		return Type{}, false
	}
	if env == nil {
		ix.memo(f)
		if t, ok := f.typeMemo[i]; ok {
			return t.t, t.ok
		}
		f.typeMemo[i] = typeResult{} // in progress: a cycle has no type
	}
	t, ok := ix.valueTypeUncached(f, i, env, depth)
	if env == nil {
		if ix.budget <= 0 {
			delete(f.typeMemo, i)
		} else {
			f.typeMemo[i] = typeResult{t, ok}
		}
	}
	return t, ok
}

type typeResult struct {
	t  Type
	ok bool
}

func (ix *Index) valueTypeUncached(f *File, i int, env *Env, depth int) (Type, bool) {
	r := &f.Syn.Refs[i]
	if r.Kind == syntax.RMember {
		bt, ok := ix.baseType(f, i, env, depth+1)
		if !ok {
			return Type{}, false
		}
		m, ok := Final(ix.memberIn(bt, r.Name, depth+1))
		if !ok {
			return Type{}, false
		}
		return ix.typeOfDecl(m, depth+1, bt.Env, env)
	}
	t, ok := Final(ix.resolveRef(f, i, depth+1))
	if !ok {
		return Type{}, false
	}
	return ix.typeOfDecl(t, depth+1, env)
}

// baseType is the type of the expression a member hangs off: a name
// chain, a call or subscript, a literal, a parenthesized expression, or,
// for a contextual member, the type its context expects.
func (ix *Index) baseType(f *File, i int, env *Env, depth int) (Type, bool) {
	r := &f.Syn.Refs[i]
	switch {
	case r.Base >= 0:
		return ix.valueType(f, r.Base, env, depth+1)
	case r.Call >= 0:
		return ix.postfixType(f, r.Call, r.Span.Start, env, depth+1)
	}
	c, ok := f.Syn.Ctx[i]
	if !ok {
		return Type{}, false
	}
	switch {
	case c.Lit != 0:
		return ix.literalType(f, c.Lit, depth+1)
	case c.Inner.End > c.Inner.Start:
		return ix.typeOfSpan(f, c.Inner, env, nil, depth+1)
	case c.Expect.End > c.Expect.Start:
		return ix.typeOfSpan(f, c.Expect, env, nil, depth+1)
	case c.HasPeer:
		return ix.valueType(f, c.Peer, env, depth+1)
	case c.Return:
		if fs := r.Scope.Func(); fs != nil && fs.Owner != nil {
			return ix.resultType(Target{File: f, Decl: fs.Owner}, nil, depth+1)
		}
	case c.Callee >= 0:
		callee, ok := Final(ix.resolveRef(f, c.Callee, depth+1))
		if !ok {
			return Type{}, false
		}
		param, pf := ix.paramFor(callee, c, depth+1)
		if param == nil {
			return Type{}, false
		}
		return ix.typeOfDecl(Target{File: pf, Decl: param}, depth+1)
	}
	return Type{}, false
}

// memberIn finds a member of a type or instance.
func (ix *Index) memberIn(t Type, name string, depth int) []Target {
	if t.T.IsModule() {
		return ix.lookupModule(t.T.File, name, depth+1)
	}
	switch t.T.Decl.Kind {
	case syntax.KStruct, syntax.KTrait:
		return ix.memberOfType(t.T.File, t.T.Decl, name, depth+1, nil)
	}
	return nil
}

// typeOfDecl is the type of what a declaration names, used as a value.
// envs are searched in order for the binding of a compile-time parameter.
func (ix *Index) typeOfDecl(t Target, depth int, envs ...*Env) (Type, bool) {
	if depth > maxDepth || !ix.step() {
		return Type{}, false
	}
	if t.IsModule() {
		return Type{T: t}, true
	}
	d := t.Decl
	var env *Env
	for _, e := range envs {
		if e != nil {
			env = e
			break
		}
	}
	switch d.Kind {
	case syntax.KStruct, syntax.KTrait:
		return Type{T: t}, true
	case syntax.KFunc:
		return Type{T: t, Env: env}, true
	case syntax.KParam:
		for _, e := range envs {
			if a, ok := e.lookup(d); ok {
				return ix.typeOfSpan(a.f, a.sp, a.env, nil, depth+1)
			}
		}
		// Unbound: its constraint (a trait) stands for it.
		if d.Type.End > d.Type.Start {
			ty, ok := ix.typeOfSpan(t.File, d.Type, env, nil, depth+1)
			ty.Inst = true
			return ty, ok
		}
		return Type{}, false
	case syntax.KArg:
		if d.Type.End <= d.Type.Start {
			// An unannotated `self` is the enclosing struct.
			if d.Name == "self" && d.Parent != nil && d.Parent.Scope != nil && d.Parent.Scope.Kind == syntax.SStruct && d.Parent.Scope.Owner != nil {
				return Type{T: Target{File: t.File, Decl: d.Parent.Scope.Owner}, Inst: true, Env: env}, true
			}
			return Type{}, false
		}
	}
	if d.Loop {
		return ix.elementType(t, env, depth+1)
	}
	if d.Type.End > d.Type.Start {
		ty, ok := ix.typeOfSpan(t.File, d.Type, env, d, depth+1)
		ty.Inst = true
		return ty, ok
	}
	if d.Value.End > d.Value.Start {
		return ix.typeOfSpan(t.File, d.Value, env, d, depth+1)
	}
	return Type{}, false
}

// elementType is the type of a loop variable: what the iterator of the
// iterated value yields (`__iter__`, then `__next__`). Only a single loop
// variable is typed; tuple unpacking is not.
func (ix *Index) elementType(v Target, env *Env, depth int) (Type, bool) {
	d := v.Decl
	if d.Value.End <= d.Value.Start || strings.Contains(string(v.File.Syn.Src[d.Span.Start:d.Span.End]), ",") {
		return Type{}, false
	}
	c, ok := ix.typeOfSpan(v.File, d.Value, env, d, depth+1)
	if !ok || c.T.IsModule() || !c.Inst {
		return Type{}, false
	}
	it, ok := ix.callMethod(c, "__iter__", depth+1)
	if !ok || it.T.IsModule() {
		return Type{}, false
	}
	return ix.callMethod(it, "__next__", depth+1)
}

// callMethod is the result type of calling method name on a value of
// type t.
func (ix *Index) callMethod(t Type, name string, depth int) (Type, bool) {
	if t.T.IsModule() {
		return Type{}, false
	}
	m, ok := Final(ix.memberOfType(t.T.File, t.T.Decl, name, depth+1, nil))
	if !ok || m.IsModule() || m.Decl.Kind != syntax.KFunc {
		return Type{}, false
	}
	return ix.resultTypeFor(m, t.Env, nil, depth+1)
}

// typeOfSpan is the type of an expression: its leading name chain, then
// any calls and subscripts right after it. For a compound expression
// (`a + b`) it is the type of the leading operand.
func (ix *Index) typeOfSpan(f *File, sp syntax.Span, env *Env, self *syntax.Decl, depth int) (Type, bool) {
	if depth > maxDepth || !ix.step() || sp.End <= sp.Start {
		return Type{}, false
	}
	src := f.Syn.Src
	p := skipBlank(src, sp.Start, sp.End)
	for p < sp.End && src[p] == '(' {
		p = skipBlank(src, p+1, sp.End)
	}
	if p < sp.End {
		if lit := literalAt(src, p); lit != 0 {
			return ix.literalType(f, lit, depth+1)
		}
	}
	j := ix.chainEnd(f, p, sp.End)
	if j < 0 {
		return Type{}, false
	}
	if t, ok := Final(ix.resolveRef(f, j, depth+1)); ok && !t.IsModule() && t.Decl == self {
		return Type{}, false // `var x = x`
	}
	t, ok := ix.valueType(f, j, env, depth+1)
	if !ok {
		return Type{}, false
	}
	for _, g := range bracketGroups(src, f.Syn.Refs[j].Span.End, sp.End) {
		if t, ok = ix.applyPostfix(t, g, f, env, depth+1); !ok {
			return Type{}, false
		}
	}
	return t, true
}

// chainEnd finds the last name of the leading chain of an expression
// starting at p (`c` in `a.b(x).c`), or -1. `Some[Trait]` is read as
// Trait; conventions before a type are skipped.
func (ix *Index) chainEnd(f *File, p, end int) int {
	refs := f.Syn.Refs
	lo, hi := 0, len(refs)
	for lo < hi {
		m := (lo + hi) / 2
		if refs[m].Span.Start < p {
			lo = m + 1
		} else {
			hi = m
		}
	}
	i := lo
	for i < len(refs) && refs[i].Span.Start < end && (refs[i].Name == "mut" || refs[i].Name == "owned" || refs[i].Name == "read") {
		i++
	}
	if i >= len(refs) || refs[i].Span.Start >= end || refs[i].Span.Start != skipBlankRef(f.Syn.Src, p, refs[i].Span.Start) {
		return -1
	}
	if refs[i].Name == "Some" && i+1 < len(refs) && refs[i+1].Span.Start < end {
		i++
	}
	for j := i + 1; j < len(refs) && refs[j].Span.Start < end; j++ {
		if refs[j].Kind == syntax.RMember && (refs[j].Base == i || refs[j].Call == i) {
			i = j
		}
	}
	return i
}

// skipBlankRef returns where a name starting at or after p really
// begins once blanks (and a leading `mut`-like word handled by the
// caller) are skipped; used to check that nothing else precedes it.
func skipBlankRef(src []byte, p, at int) int {
	q := skipBlank(src, p, at)
	if q == at {
		return at
	}
	return -1
}

func skipBlank(src []byte, p, end int) int {
	for p < end && (src[p] == ' ' || src[p] == '\t' || src[p] == '\n' || src[p] == '\r' || src[p] == '\\') {
		p++
	}
	return p
}

// literalAt reports the kind of literal starting at p, if any.
func literalAt(src []byte, p int) byte {
	c := src[p]
	switch {
	case c == '"' || c == '\'':
		return 's'
	case (c == 't' || c == 'f' || c == 'T' || c == 'F') && p+1 < len(src) && (src[p+1] == '"' || src[p+1] == '\''):
		return 't'
	case (c == 'r' || c == 'b') && p+1 < len(src) && (src[p+1] == '"' || src[p+1] == '\''):
		return 's'
	case c >= '0' && c <= '9':
		for q := p; q < len(src) && (isNumByte(src[q])); q++ {
			if src[q] == '.' || ((src[q] == 'e' || src[q] == 'E') && !(q > p+1 && (src[p+1] == 'x' || src[p+1] == 'X'))) {
				return 'f'
			}
		}
		return 'i'
	}
	if hasWord(src, p, "True") || hasWord(src, p, "False") {
		return 'b'
	}
	return 0
}

func isNumByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '.'
}

func hasWord(src []byte, p int, w string) bool {
	if len(src)-p < len(w) || string(src[p:p+len(w)]) != w {
		return false
	}
	return p+len(w) == len(src) || !isNumByte(src[p+len(w)])
}

// literalType is the struct of a literal: StringLiteral, TString,
// IntLiteral, FloatLiteral or Bool, when the stdlib sources are known.
func (ix *Index) literalType(f *File, lit byte, depth int) (Type, bool) {
	var chain []Target
	switch lit {
	case 's':
		chain = ix.lookupScope(f, f.Syn.Module, "StringLiteral", 0, depth+1)
	case 't':
		chain = ix.lookupModule(ix.resolveModule(f.Path, "std.format.tstring"), "TString", depth+1)
	case 'i':
		chain = ix.lookupScope(f, f.Syn.Module, "IntLiteral", 0, depth+1)
	case 'f':
		chain = ix.lookupScope(f, f.Syn.Module, "FloatLiteral", 0, depth+1)
	case 'b':
		chain = ix.lookupScope(f, f.Syn.Module, "Bool", 0, depth+1)
	}
	t, ok := Final(chain)
	if !ok || t.IsModule() {
		return Type{}, false
	}
	return Type{T: t, Inst: true}, true
}

// group is one bracket group after a name: `[Int]` or `(x, y)`.
type group struct {
	op   byte
	args []syntax.Span
}

// bracketGroups reads the bracket groups directly after a name, up to the
// next thing that is not one (a dot, an operator, the end): `[Int](3)`
// gives two. A transfer sigil `^` in between is skipped.
func bracketGroups(src []byte, p, end int) []group {
	var out []group
	for {
		p = skipBlank(src, p, end)
		for p < end && src[p] == '^' {
			p = skipBlank(src, p+1, end)
		}
		if p >= end || (src[p] != '(' && src[p] != '[') {
			return out
		}
		g := group{op: src[p]}
		depth := 0
		argStart := p + 1
		q := p
		for ; q < end; q++ {
			c := src[q]
			switch c {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
				if depth == 0 {
					if a := trimSpan(src, argStart, q); a.End > a.Start {
						g.args = append(g.args, a)
					}
					out = append(out, g)
					p = q + 1
					goto next
				}
			case ',':
				if depth == 1 {
					g.args = append(g.args, trimSpan(src, argStart, q))
					argStart = q + 1
				}
			case '"', '\'':
				for q++; q < end && src[q] != c; q++ {
					if src[q] == '\\' {
						q++
					}
				}
			}
		}
		return out // unclosed
	next:
	}
}

func trimSpan(src []byte, a, b int) syntax.Span {
	for a < b && (src[a] == ' ' || src[a] == '\t' || src[a] == '\n' || src[a] == '\r') {
		a++
	}
	for b > a && (src[b-1] == ' ' || src[b-1] == '\t' || src[b-1] == '\n' || src[b-1] == '\r') {
		b--
	}
	return syntax.Span{Start: a, End: b}
}

// postfixType is the type of `callee(...)`, `callee[...]`, or a chain of
// them, ending before offset end (where the member's dot is).
func (ix *Index) postfixType(f *File, callee int, end int, env *Env, depth int) (Type, bool) {
	groups := bracketGroups(f.Syn.Src, f.Syn.Refs[callee].Span.End, end)
	r := f.Syn.Refs[callee]
	if r.Kind == syntax.RName && r.Name == "type_of" && len(groups) > 0 && groups[0].op == '(' && len(groups[0].args) == 1 {
		// `type_of(x)` is the type of x itself (a compiler builtin).
		t, ok := ix.typeOfSpan(f, groups[0].args[0], env, nil, depth+1)
		t.Inst = false
		for _, g := range groups[1:] {
			if !ok {
				break
			}
			t, ok = ix.applyPostfix(t, g, f, env, depth+1)
		}
		return t, ok
	}
	t, ok := ix.valueType(f, callee, env, depth+1)
	if !ok {
		return Type{}, false
	}
	for _, g := range groups {
		if t, ok = ix.applyPostfix(t, g, f, env, depth+1); !ok {
			return Type{}, false
		}
	}
	return t, true
}

// applyPostfix is the type of calling or subscripting a value of type t:
// a type's instantiation (`List[Int]`) or construction (`List[Int]()`), a
// function's return type, an instance's __getitem__ or __call__ result.
// args are evaluated in env, in file f.
func (ix *Index) applyPostfix(t Type, g group, f *File, env *Env, depth int) (Type, bool) {
	if depth > maxDepth || !ix.step() || t.T.IsModule() {
		return Type{}, false
	}
	d := t.T.Decl
	switch {
	case d.Kind == syntax.KFunc:
		if g.op == '[' {
			return t, true // explicit parameters; the call comes next
		}
		if d.Name == "type_of" && len(g.args) == 1 {
			ty, ok := ix.typeOfSpan(f, g.args[0], env, nil, depth+1)
			ty.Inst = false // the type itself: `type_of(x).method`
			return ty, ok
		}
		return ix.resultType(t.T, t.Env, depth+1)
	case (d.Kind == syntax.KStruct || d.Kind == syntax.KTrait) && !t.Inst:
		if g.op == '[' {
			return Type{T: t.T, Env: ix.bindParams(t.T, d.Body, g.args, f, env)}, true
		}
		return Type{T: t.T, Inst: true, Env: t.Env}, true
	case d.Kind == syntax.KComptime && !t.Inst:
		if d.Body != nil && g.op == '[' {
			inner := ix.bindParams(t.T, d.Body, g.args, f, env)
			return ix.typeOfSpan(t.T.File, d.Value, inner, d, depth+1)
		}
	case d.Kind != syntax.KStruct && d.Kind != syntax.KTrait:
		return Type{}, false
	}
	if !t.Inst {
		return Type{}, false
	}
	method := "__getitem__"
	if g.op == '(' {
		method = "__call__"
	}
	m, ok := Final(ix.memberOfType(t.T.File, d, method, depth+1, nil))
	if !ok || m.IsModule() || m.Decl.Kind != syntax.KFunc {
		return Type{}, false
	}
	// `x[a:b]` wants the slice overload of __getitem__, `x[i]` another.
	slicing := false
	for _, a := range g.args {
		if topLevelColon(f.Syn.Src, a) {
			slicing = true
		}
	}
	return ix.resultTypeFor(m, t.Env, func(o *syntax.Decl) bool {
		return method != "__getitem__" || mentionsSlice(m.File, o) == slicing
	}, depth+1)
}

func topLevelColon(src []byte, a syntax.Span) bool {
	depth := 0
	for p := a.Start; p < a.End; p++ {
		switch src[p] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// mentionsSlice reports whether an overload takes a slice argument.
func mentionsSlice(f *File, d *syntax.Decl) bool {
	if d.Body == nil {
		return false
	}
	for _, ds := range d.Body.Names {
		for _, a := range ds {
			if a.Kind == syntax.KArg && strings.Contains(string(f.Syn.Src[a.Type.Start:a.Type.End]), "Slice") {
				return true
			}
		}
	}
	return false
}

// bindParams binds the compile-time parameters declared in body (of a
// struct or parameterized comptime) to bracket arguments, by position
// (skipping infer-only parameters) or by keyword.
func (ix *Index) bindParams(owner Target, body *syntax.Scope, args []syntax.Span, f *File, env *Env) *Env {
	if body == nil {
		return nil
	}
	var params []*syntax.Decl
	for _, ds := range body.Names {
		for _, d := range ds {
			if d.Kind == syntax.KParam && d.Scope == body {
				params = append(params, d)
			}
		}
	}
	sortByIndex(params)
	e := &Env{binds: map[*syntax.Decl]boundArg{}}
	pos := 0
	for _, a := range args {
		if kw, val, ok := keywordArg(f.Syn.Src, a); ok {
			for _, p := range params {
				if p.Name == kw {
					e.binds[p] = boundArg{f: f, sp: val, env: env}
				}
			}
			continue
		}
		for pos < len(params) && params[pos].InferOnly {
			pos++
		}
		if pos >= len(params) {
			break
		}
		if text := string(f.Syn.Src[a.Start:a.End]); text != "_" && text != "*_" && text != "..." {
			e.binds[params[pos]] = boundArg{f: f, sp: a, env: env}
		}
		pos++
	}
	return e
}

// keywordArg splits `name = value` (not `==`).
func keywordArg(src []byte, a syntax.Span) (string, syntax.Span, bool) {
	p := a.Start
	for p < a.End && (src[p] == '_' || src[p] >= 'a' && src[p] <= 'z' || src[p] >= 'A' && src[p] <= 'Z' || p > a.Start && src[p] >= '0' && src[p] <= '9') {
		p++
	}
	if p == a.Start {
		return "", syntax.Span{}, false
	}
	name := string(src[a.Start:p])
	q := skipBlank(src, p, a.End)
	if q >= a.End || src[q] != '=' || (q+1 < a.End && src[q+1] == '=') {
		return "", syntax.Span{}, false
	}
	return name, trimSpan(src, q+1, a.End), true
}

// resultType is a function's return type, an instance; overloads are
// tried in order. env binds the parameters of the struct the method was
// reached through.
func (ix *Index) resultType(fn Target, env *Env, depth int) (Type, bool) {
	return ix.resultTypeFor(fn, env, nil, depth)
}

// resultTypeFor is resultType over the overloads accepted by want (all,
// if want is nil or accepts none).
func (ix *Index) resultTypeFor(fn Target, env *Env, want func(*syntax.Decl) bool, depth int) (Type, bool) {
	group := []*syntax.Decl{fn.Decl}
	if fn.Decl.Scope != nil {
		group = append(group, fn.Decl.Scope.Names[fn.Decl.Name]...)
	}
	if want != nil {
		var keep []*syntax.Decl
		for _, g := range group {
			if want(g) {
				keep = append(keep, g)
			}
		}
		if len(keep) > 0 {
			group = keep
		}
	}
	for _, g := range group {
		if g.Kind == syntax.KFunc && g.Result.End > g.Result.Start {
			t, ok := ix.typeOfSpan(fn.File, g.Result, env, nil, depth+1)
			if ok {
				t.Inst = true
			}
			return t, ok
		}
	}
	return Type{}, false
}

// member resolves `.name` on a resolved name (kept for kwarg callers).
func (ix *Index) member(base Target, name string, depth int) []Target {
	t, ok := ix.typeOfDecl(base, depth+1)
	if !ok {
		return nil
	}
	return ix.memberIn(t, name, depth+1)
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
		} else if t, ok := ix.typeOfDecl(callee, depth+1); ok && !t.T.IsModule() {
			return ix.paramFor(t.T, c, depth+1)
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
