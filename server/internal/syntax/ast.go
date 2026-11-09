package syntax

// DeclKind says what a declaration declares.
type DeclKind uint8

const (
	KStruct   DeclKind = iota // struct S
	KTrait                    // trait T
	KFunc                     // def f (a method when its Parent is a struct or trait)
	KComptime                 // comptime X = ... (alias X = ... in older code)
	KField                    // var x: T inside a struct or trait
	KVar                      // var, ref, loop, with/except target, or implicit local
	KParam                    // compile-time parameter: def f[T: AnyType], struct S[T]
	KArg                      // run-time argument: def f(x: Int)
	KImport                   // a name bound by an import statement
)

var declKindNames = [...]string{"struct", "trait", "def", "comptime", "field", "var", "param", "arg", "import"}

func (k DeclKind) String() string {
	if int(k) < len(declKindNames) {
		return declKindNames[k]
	}
	return "?"
}

// Decl is one declaration.
type Decl struct {
	Kind     DeclKind
	Name     string
	NameSpan Span
	// Span covers the whole declaration: for a def, struct or trait its
	// header through the end of its body.
	Span   Span
	Parent *Decl  // enclosing struct, trait or def; nil at module level
	Scope  *Scope // the scope the name is declared in
	Body   *Scope // the scope a struct, trait or def opens
	// Type is the annotation (`var x: List[Int]` → `List[Int]`), Value the
	// initializer (`= List[Int]()`), Header the signature line for a def,
	// struct or trait. All may be empty.
	Type, Value, Header Span
	Doc                 Span // docstring, if any
	Decorators          []string
	// Result, for a def, is its return type (`-> List[T]`), without a
	// leading `ref [origin]`.
	Result Span
	// Capture marks a name in a closure's capture list (`{mut x}`); it
	// stands for the outer variable of that name.
	Capture bool
	// InferOnly marks a compile-time parameter before `//`, which callers
	// never pass by position.
	InferOnly bool
	// Bases, for a struct or trait, are the name spans of the traits it
	// lists (`struct S(Copyable, Writable where ...)`: Copyable, Writable;
	// for a dotted base, its last name).
	Bases []Span
	// Visible is the offset from which the name can be used in a def or
	// block: after the whole statement for `var x = x + 1`, so the right
	// side still means the outer x; the body for a loop variable.
	Visible int
	// Implicit marks a local created by plain assignment (`x = 1` in a def).
	Implicit bool
	// Import is set for KImport: the statement that bound the name.
	Import *Import
	// Index is the position in File.Decls.
	Index int
}

// ScopeKind classifies a scope.
type ScopeKind uint8

const (
	SModule ScopeKind = iota
	SStruct           // struct or trait body
	SFunc             // a def: its parameters, arguments and implicit locals
	SBlock            // an indented block inside a def (if, for, while, with, ...)
	SComp             // a comprehension
)

// Scope is a region where names can be declared.
type Scope struct {
	Kind   ScopeKind
	Span   Span
	Parent *Scope
	Owner  *Decl // the struct, trait or def that opened it
	Names  map[string][]*Decl
}

// Func returns the innermost SFunc scope enclosing s (s itself included),
// or nil at module or struct level.
func (s *Scope) Func() *Scope {
	for ; s != nil; s = s.Parent {
		switch s.Kind {
		case SFunc:
			return s
		case SModule, SStruct:
			return nil
		}
	}
	return nil
}

func (s *Scope) declare(d *Decl) {
	if s.Names == nil {
		s.Names = map[string][]*Decl{}
	}
	s.Names[d.Name] = append(s.Names[d.Name], d)
	d.Scope = s
}

// RefKind classifies an identifier occurrence.
type RefKind uint8

const (
	RName     RefKind = iota // a plain use: `x`, `List`, `Self`
	RMember                  // after a dot: the `y` in `x.y`
	RKwarg                   // a keyword argument or parameter: the `x` in `f(x=1)`
	RDecl                    // the name in a declaration; Decl is set
	RModule                  // a module path component in an import: `std`, `collections`
	RImported                // the imported name in `from M import name as alias`
)

var refKindNames = [...]string{"name", "member", "kwarg", "decl", "module", "imported"}

func (k RefKind) String() string {
	if int(k) < len(refKindNames) {
		return refKindNames[k]
	}
	return "?"
}

// Ref is one identifier occurrence.
type Ref struct {
	Name  string
	Span  Span
	Kind  RefKind
	Scope *Scope // innermost scope at the occurrence
	Decl  *Decl  // RDecl: what it declares
	// Base, for RMember, is the index in File.Refs of the identifier the
	// member hangs off when the chain is plain names (`a.b.c`: c's Base is
	// b, b's is a), or -1 when it follows a call, subscript or literal.
	Base int
	// Callee, for RKwarg, is the index in File.Refs of the identifier just
	// before the opening bracket (`f` in `f(x=1)`, `T` in `T[n=3]`), or -1.
	Callee int
	// Call, for an RMember following a call or subscript (`f(x).y`,
	// `a[i].y`, `List[Int]().y`), is the index of the identifier being
	// called or subscripted, with Post the bracket ('(' or '['); else -1.
	Call int
	Post byte
}

// Context is what a contextual member (`.uint8` in `SIMD[.uint8, 4]`, or a
// parameter default `dtype: DType = .float32`) is a member of: the
// parameter or argument it is passed as, or an expected type.
type Context struct {
	Callee int    // index of the identifier called or subscripted, or -1
	Op     byte   // '(' or '['
	Pos    int    // positional index among the arguments
	Name   string // the keyword, for `name=.member`
	Expect Span   // an expected type's source, when known directly
}

// Import is one import statement.
type Import struct {
	// Module is the dotted module path: "std.collections" in both
	// `from std.collections import List` and `import std.collections`, and
	// ".anytype" for a relative import.
	Module     string
	ModuleSpan Span
	From       bool // a `from M import ...` statement
	Star       bool // from M import *
	Names      []ImportName
	// Alias is set for `import a.b as c`.
	Alias     string
	AliasSpan Span
	Span      Span
}

// ImportName is one name in a from-import.
type ImportName struct {
	Name      string
	NameSpan  Span
	Alias     string
	AliasSpan Span
	Decl      *Decl // the KImport declaration it creates
}

// File is the result of parsing one file.
type File struct {
	Src     []byte
	Decls   []*Decl
	Module  *Scope
	Scopes  []*Scope
	Refs    []Ref
	Imports []*Import
	// Ctx holds the context of contextual members, by occurrence index.
	Ctx map[int]Context
	// Panic is set if parsing hit a bug and stopped early; the rest of the
	// File holds what was read before it.
	Panic any
}

// Text returns the source text of sp.
func (f *File) Text(sp Span) string {
	if sp.End <= sp.Start || sp.Start < 0 || sp.End > len(f.Src) {
		return ""
	}
	return string(f.Src[sp.Start:sp.End])
}

// ScopeAt returns the innermost scope containing offset.
func (f *File) ScopeAt(offset int) *Scope {
	best := f.Module
	for _, s := range f.Scopes {
		if s.Span.Start <= offset && offset < s.Span.End && s.Span.End-s.Span.Start <= best.Span.End-best.Span.Start {
			best = s
		}
	}
	return best
}

// RefAt returns the index of the identifier occurrence covering offset
// (its end included, so a cursor just after a name finds it), or -1.
func (f *File) RefAt(offset int) int {
	lo, hi := 0, len(f.Refs)
	for lo < hi {
		m := (lo + hi) / 2
		if f.Refs[m].Span.End < offset {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(f.Refs) && f.Refs[lo].Span.Start <= offset && offset <= f.Refs[lo].Span.End {
		return lo
	}
	return -1
}
