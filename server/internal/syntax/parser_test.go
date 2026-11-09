package syntax

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func trim(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\t", "")) }

func TestOutline(t *testing.T) {
	src := `
from std.collections import List, Dict as D
import std.math as m

comptime N: Int = 3
comptime Pair[T: AnyType] = Tuple[T, T]

@fieldwise_init
struct Point[dtype: DType](Copyable, Writable where conforms_to(dtype, Writable)):
    """A point."""
    comptime Zero = Self(0, 0)
    var x: Int
    var y: Int

    def __init__(out self, x: Int, *, y: Int = 0):
        self.x = x
        self.y = y

    def norm[p: Int = 2](self) raises -> Float64 where p > 0:
        var total = 0
        for i in range(p):
            total += i
        return total

trait Shape(Movable):
    def area(self) -> Float64: ...

def main():
    var p = Point[DType.int32](1, y=2)
    count = 0
`
	f := Parse([]byte(src))
	want := `
import List
import D
import m
comptime N: Int
comptime Pair
  param T: AnyType
struct Point
  param dtype: DType
  comptime Zero
  field x: Int
  field y: Int
  def __init__
    arg self
    arg x: Int
    arg y: Int
  def norm
    param p: Int
    arg self
trait Shape
  def area
    arg self
def main
`
	if got := f.Outline(false); trim(got) != trim(want) {
		t.Errorf("outline:\n%s\nwant:\n%s", got, want)
	}
	withLocals := f.Outline(true)
	for _, w := range []string{"var total", "var i", "var p", "var count (implicit)"} {
		if !strings.Contains(withLocals, w) {
			t.Errorf("locals missing %q:\n%s", w, withLocals)
		}
	}
	point := f.Decls[findDecl(f, "Point")]
	if f.Text(point.Doc) != `"""A point."""` {
		t.Errorf("doc: %q", f.Text(point.Doc))
	}
	if len(point.Decorators) != 1 || point.Decorators[0] != "fieldwise_init" {
		t.Errorf("decorators: %v", point.Decorators)
	}
	var bases []string
	for _, b := range point.Bases {
		bases = append(bases, f.Text(b))
	}
	if strings.Join(bases, ",") != "Copyable,Writable" {
		t.Errorf("bases: %v", bases)
	}
	if !strings.HasPrefix(f.Text(point.Header), "struct Point[dtype: DType](Copyable") {
		t.Errorf("header: %q", f.Text(point.Header))
	}
}

func findDecl(f *File, name string) int {
	for i, d := range f.Decls {
		if d.Name == name {
			return i
		}
	}
	return -1
}

func TestRefs(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"member chain", "a.b.c", "a a.b b.c"},
		{"member after call", "f(x).y", "f x .y"},
		{"member after subscript", "a[0].y", "a .y"},
		{"kwarg", "f(a, b=1, c=d)", "f a f:b= f:c= d"},
		{"param kwarg", "Origin[mut=m]", "Origin Origin:mut= m"},
		{"comparison is not kwarg", "f(a == b)", "f a b"},
		{"nested kwarg callee", "f(g(x=1), y=2)", "f g g:x= f:y="},
		{"t-string", `print(t"{self.x} and {y!r}")`, "print self self.x y"},
		{"nested t-string", `t"a {rt"b {x}"}"`, "x"},
		{"keywords skipped", "if a and not b: pass", "a b"},
		{"soft keywords are names", "file.read(); out = mut", "file file.read +out mut"},
		{"Self", "Self.T(x)", "Self Self.T x"},
		{"backtick", "var `def` = 1", "+def"},
		{"import refs", "from std.collections import List as L, Dict", "@std @collections <List +L +Dict"},
		{"plain import", "import std.math", "@std @math"},
		{"relative import", "from .math import exp", "@math +exp"},
		{"comprehension", "y = [x * k for x in xs if x]", "+y x k +x xs x"},
		{"walrus in def", "def f():\n    if (n := g()):\n        n", "+f +n g n"},
		{"signature", "def f[T: Copyable](x: T, *args: Int, out self) raises -> List[T] where T > 0:\n    pass",
			"+f +T Copyable +x T +args Int +self List T T"},
		{"capture list", "def f() {mut x, read y}:\n    pass", "+f +x +y"},
		{"capture mode only", "def f() {mut}:\n    pass", "+f"},
		{"call result member", "a.b(x).c", "a a.b x .c"},
		{"transfer then member", "self^.close()", "self self.close"},
		{"ref origin", "def f(ref [o] self, ref x: Int):\n    pass", "+f o +self +x Int"},
		{"annotated assign", "def f():\n    x: Int = 1\n    x = 2", "+f +x Int x"},
		{"tuple assign", "def f():\n    a, b = g()\n    a", "+f +a +b g a"},
		{"attribute assign", "def f():\n    self.x = 1", "+f self self.x"},
		{"augmented assign", "def f():\n    n += 1", "+f n"},
		{"decorator", "@deprecated(use=memmove)\ndef f(): pass", "deprecated deprecated:use= memmove +f"},
		{"for targets", "def f():\n    for ref k, v in d.items():\n        k", "+f +k +v d d.items k"},
		{"with as", "def f():\n    with open(p) as h:\n        h", "+f open p +h h"},
		{"except as", "def f():\n    try:\n        pass\n    except e as err:\n        err", "+f e +err err"},
		{"except binds", "def f():\n    try:\n        pass\n    except e:\n        print(e)", "+f +e print e"},
		{"unnamed function-type param", "comptime F = def[Int]() -> T", "+F Int T"},
		{"lambda params", "g(lambda [i: Int]() {}: i)", "g +i Int i"},
		{"comptime for", "def f():\n    comptime for i in range(3):\n        i", "+f +i range i"},
		{"comptime assert", "comptime assert x > 0, \"msg\"", "x"},
		{"async def", "async def f(x: Int) -> Int:\n    return await g(x)", "+f +x Int Int g x"},
		{"function type", "comptime F = def[d: DType, w: Int](SIMD[d, w], x: Int) capturing -> SIMD[d, w]", "+F +d DType +w Int SIMD d w +x Int SIMD d w"},
		{"function type param", "def f(g: def(Int) -> Int, h: Int): pass", "+f +g Int Int +h Int"},
		{"lambda", "f(lambda (k: Int) {imm key} -> Bool: (k == key), 1)", "f +k Int +key Bool k key"},
		{"bare lambda", "g(lambda x: x + y)", "g +x x y"},
		{"discard", "def f():\n    _ = g()\n    _ = h()", "+f g h"},
		{"semicolons", "a = 1; b = a", "+a +b a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse([]byte(c.src)).RefsString(); got != c.want {
				t.Errorf("\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func TestScopes(t *testing.T) {
	src := `
def f(a: Int):
    var x = 1
    if a:
        var x = 2
        y = x
    for i in range(a):
        x
    z = 1
    z = 2
`
	f := Parse([]byte(src))
	// Two `x` declarations: one in the def scope, one in the if block.
	var xs []*Decl
	for _, d := range f.Decls {
		if d.Name == "x" {
			xs = append(xs, d)
		}
	}
	if len(xs) != 2 || xs[0].Scope.Kind != SFunc || xs[1].Scope.Kind != SBlock {
		t.Fatalf("x decls: %d", len(xs))
	}
	// `y = x` inside the if declares y in the def, implicitly.
	y := f.Decls[findDecl(f, "y")]
	if !y.Implicit || y.Scope.Kind != SFunc {
		t.Errorf("y: implicit=%v scope=%v", y.Implicit, y.Scope.Kind)
	}
	// The loop variable belongs to the loop body.
	i := f.Decls[findDecl(f, "i")]
	if i.Scope.Kind != SBlock {
		t.Errorf("i scope: %v", i.Scope.Kind)
	}
	// `z = 2` reassigns: only one z.
	n := 0
	for _, d := range f.Decls {
		if d.Name == "z" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("z declared %d times", n)
	}
}

func TestRecovery(t *testing.T) {
	// A broken signature must not take the rest of the file with it.
	src := "def broken(a: Int\n\ndef good(b: Int) -> Int:\n    return b\n\nstruct S:\n    var x: Int\n"
	f := Parse([]byte(src))
	out := f.Outline(false)
	for _, w := range []string{"def good", "arg b: Int", "struct S", "field x: Int"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q after a broken line:\n%s", w, out)
		}
	}
}

func TestRefAt(t *testing.T) {
	src := "var abc = d.efg"
	f := Parse([]byte(src))
	for _, c := range []struct {
		off  int
		want string
	}{{4, "abc"}, {7, "abc"}, {10, "d"}, {12, "efg"}, {15, "efg"}, {8, ""}} {
		i := f.RefAt(c.off)
		got := ""
		if i >= 0 {
			got = f.Refs[i].Name
		}
		if got != c.want {
			t.Errorf("RefAt(%d) = %q, want %q", c.off, got, c.want)
		}
	}
}

func checkParse(t *testing.T, src []byte) *File {
	t.Helper()
	f := Parse(src)
	if f.Panic != nil {
		t.Fatalf("parser panicked: %v", f.Panic)
	}
	prev := -1
	for i, r := range f.Refs {
		if r.Span.Start < prev || r.Span.End > len(src) || r.Span.End <= r.Span.Start {
			t.Fatalf("ref %d %q at %v out of order or bounds", i, r.Name, r.Span)
		}
		prev = r.Span.Start
		if r.Base >= len(f.Refs) || r.Callee >= len(f.Refs) || r.Scope == nil {
			t.Fatalf("ref %d %q: base %d callee %d scope %v", i, r.Name, r.Base, r.Callee, r.Scope)
		}
		if r.Base >= 0 && f.Refs[r.Base].Span.Start >= r.Span.Start {
			t.Fatalf("ref %d %q: base after it", i, r.Name)
		}
		if (r.Kind == RDecl) != (r.Decl != nil) {
			t.Fatalf("ref %d %q: kind %v decl %v", i, r.Name, r.Kind, r.Decl)
		}
		if r.Kind != RDecl && string(src[r.Span.Start:r.Span.End]) != r.Name && string(src[r.Span.Start:r.Span.End]) != "`"+r.Name+"`" {
			t.Fatalf("ref %d %q does not match source %q", i, r.Name, src[r.Span.Start:r.Span.End])
		}
	}
	for _, d := range f.Decls {
		if d.Scope == nil || d.NameSpan.End > len(src) {
			t.Fatalf("decl %q: scope %v span %v", d.Name, d.Scope, d.NameSpan)
		}
	}
	return f
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"def f[T: Copyable](x: T, *, y: Int = 0) raises -> T where T > 0 {mut x}:\n    var z = [a for a in x]\n    return t\"{z}\"\n",
		"struct S[T: AnyType](C where conforms_to(T, C)):\n    comptime X[n: Int] = n\n    var a: T\n",
		"from . import (a as b, c)\nimport a.b as c\n",
		"def f(\n",
		"x = y = (a, b) = 1\n",
		"for ref a in b:\n    with c as d, e as f:\n        try:\n            pass\n        except g as h:\n            pass\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkParse(t, []byte(s))
	})
}

// TestParseCorpus parses every .mojo file under $MOJOLS_CORPUS.
func TestParseCorpus(t *testing.T) {
	root := os.Getenv("MOJOLS_CORPUS")
	if root == "" {
		t.Skip("set MOJOLS_CORPUS to a directory of .mojo files")
	}
	files, refs, decls := 0, 0, 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".mojo") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files++
		f := checkParse(t, src)
		refs += len(f.Refs)
		decls += len(f.Decls)
		return nil
	})
	t.Logf("%d files, %d decls, %d refs", files, decls, refs)
}

func BenchmarkParseCorpus(b *testing.B) {
	root := os.Getenv("MOJOLS_CORPUS")
	if root == "" {
		b.Skip("set MOJOLS_CORPUS")
	}
	var srcs [][]byte
	total := 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".mojo") {
			if src, err := os.ReadFile(path); err == nil {
				srcs = append(srcs, src)
				total += len(src)
			}
		}
		return nil
	})
	b.SetBytes(int64(total))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, s := range srcs {
			Parse(s)
		}
	}
}

func refNamed(f *File, name string, kind RefKind, nth int) int {
	for i, r := range f.Refs {
		if r.Name == name && r.Kind == kind {
			if nth == 0 {
				return i
			}
			nth--
		}
	}
	return -1
}

func TestPostfixAndContext(t *testing.T) {
	f := Parse([]byte("a.b(x).c\nf[T](y).d\nSIMD[.uint8, 4]\ng(1, dtype=.float32)\ndef h(d: DType = .int8): pass\n"))
	c := f.Refs[refNamed(f, "c", RMember, 0)]
	if c.Call < 0 || f.Refs[c.Call].Name != "b" || c.Post != '(' {
		t.Errorf("c: call %d post %q", c.Call, c.Post)
	}
	d := f.Refs[refNamed(f, "d", RMember, 0)]
	if d.Call < 0 || f.Refs[d.Call].Name != "f" || d.Post != '(' {
		t.Errorf("d: call %d post %q (a postfix chain f[T](y) calls f)", d.Call, d.Post)
	}
	u := refNamed(f, "uint8", RMember, 0)
	if cx, ok := f.Ctx[u]; !ok || f.Refs[cx.Callee].Name != "SIMD" || cx.Op != '[' || cx.Pos != 0 {
		t.Errorf("uint8 context: %+v %v", cx, ok)
	}
	fl := refNamed(f, "float32", RMember, 0)
	if cx, ok := f.Ctx[fl]; !ok || f.Refs[cx.Callee].Name != "g" || cx.Pos != 1 || cx.Name != "dtype" {
		t.Errorf("float32 context: %+v %v", cx, ok)
	}
	i8 := refNamed(f, "int8", RMember, 0)
	if cx, ok := f.Ctx[i8]; !ok || f.Text(cx.Expect) != "DType" {
		t.Errorf("int8 context: %+v %v", cx, ok)
	}
}

func TestSignatureDetails(t *testing.T) {
	f := Parse([]byte("def f[a: Bool, //, T: AnyType](x: T) raises -> ref [origin] List[T] where a:\n    pass\n"))
	fn := f.Decls[findDecl(f, "f")]
	if got := f.Text(fn.Result); got != "List[T]" {
		t.Errorf("result %q", got)
	}
	a, T := f.Decls[findDecl(f, "a")], f.Decls[findDecl(f, "T")]
	if !a.InferOnly || T.InferOnly {
		t.Errorf("infer-only: a=%v T=%v", a.InferOnly, T.InferOnly)
	}
}
