package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// workspace writes files (path -> source; "|" marks nothing, it is plain
// text) under a temp dir and returns an index rooted at <dir>/pkg with the
// whole dir as workspace.
func workspace(t *testing.T, files map[string]string) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	for p, src := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix := New([]string{filepath.Join(dir, "pkg")})
	ix.SetWorkspace([]string{dir})
	return ix, dir
}

var files = map[string]string{
	"pkg/mathlib/__init__.mojo": "from .ops import double, Vec\n",
	"pkg/mathlib/ops.mojo": `
def double(x: Int) -> Int:
    return x * 2

def double(x: Float64) -> Float64:
    return x * 2

struct Vec(Normed):
    var x: Int

    def __init__(out self, x: Int):
        self.x = x

    def scaled(self, k: Int) -> Vec:
        return Vec(self.x * k)

trait Normed:
    def norm(self) -> Int:
        return 0
`,
	"app/main.mojo": `
from mathlib import double, Vec
from mathlib.ops import double as twice
import mathlib.ops as o

def main():
    var v = Vec(2)
    print(v.x, v.scaled(3).x, v.norm())
    print(double(1), twice(2), o.double(x=3))
    var total = 0
    def add() {mut total}:
        total += double(total)
    var x = 1
    if x:
        var x = 2
        print(x)
    print(x)
`,
}

// at returns the offset of the n-th (0-based) occurrence of word in src
// that is a whole word.
func at(src, word string, n int) int {
	off := 0
	for {
		i := strings.Index(src[off:], word)
		if i < 0 {
			return -1
		}
		p := off + i
		before := p == 0 || !isWord(src[p-1])
		after := p+len(word) >= len(src) || !isWord(src[p+len(word)])
		if before && after {
			if n == 0 {
				return p
			}
			n--
		}
		off = p + len(word)
	}
}

func isWord(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// resolveAt resolves the occurrence of word (its n-th) in file and
// describes the final declaration as "file:line kind name".
func resolveAt(t *testing.T, ix *Index, dir, file, word string, n int) string {
	t.Helper()
	f := ix.File(filepath.Join(dir, file))
	off := at(string(f.Syn.Src), word, n)
	if off < 0 {
		t.Fatalf("%s: no %q #%d", file, word, n)
	}
	i := f.Syn.RefAt(off)
	if i < 0 {
		t.Fatalf("%s: no occurrence at %q #%d", file, word, n)
	}
	last, ok := Final(ix.Resolve(f, i))
	if !ok {
		return "-"
	}
	if last.IsModule() {
		rel, _ := filepath.Rel(dir, last.File.Path)
		return "module " + rel
	}
	return describe(dir, last.File, last.Decl)
}

func describe(dir string, f *File, d *syntax.Decl) string {
	rel, _ := filepath.Rel(dir, f.Path)
	line := strings.Count(string(f.Syn.Src[:d.NameSpan.Start]), "\n") + 1
	return fmt.Sprintf("%s:%d %s %s", rel, line, d.Kind, d.Name)
}

func TestResolveAcrossFiles(t *testing.T) {
	ix, dir := workspace(t, files)
	main := "app/main.mojo"
	cases := []struct {
		word string
		n    int
		want string
	}{
		{"Vec", 1, "pkg/mathlib/ops.mojo:8 struct Vec"},     // Vec(2), through the package re-export
		{"x", 0, "pkg/mathlib/ops.mojo:9 field x"},          // v.x, from var v = Vec(2)
		{"scaled", 0, "pkg/mathlib/ops.mojo:14 def scaled"}, // v.scaled
		{"x", 1, "pkg/mathlib/ops.mojo:9 field x"},          // v.scaled(3).x via the return type
		{"norm", 0, "pkg/mathlib/ops.mojo:18 def norm"},     // inherited from the trait Normed
		{"double", 2, "pkg/mathlib/ops.mojo:2 def double"},  // double(1)
		{"twice", 1, "pkg/mathlib/ops.mojo:2 def double"},   // twice(2), an alias
		{"o", 1, "module pkg/mathlib/ops.mojo"},             // o.double
		{"double", 3, "pkg/mathlib/ops.mojo:2 def double"},  // o.double
		{"x", 2, "pkg/mathlib/ops.mojo:2 arg x"},            // o.double(x=3), keyword
		{"total", 3, "app/main.mojo:10 var total"},          // inside the closure, via the capture
		{"x", 6, "app/main.mojo:15 var x"},                  // the inner x
		{"x", 7, "app/main.mojo:13 var x"},                  // the outer x again
	}
	for _, c := range cases {
		if got := resolveAt(t, ix, dir, main, c.word, c.n); got != c.want {
			t.Errorf("%s #%d: got %q, want %q", c.word, c.n, got, c.want)
		}
	}
}

func refsOf(t *testing.T, ix *Index, dir, file, word string, n int) []string {
	t.Helper()
	f := ix.File(filepath.Join(dir, file))
	sym, ok := ix.SymbolAt(f, at(string(f.Syn.Src), word, n))
	if !ok {
		t.Fatalf("no symbol at %s #%d", word, n)
	}
	var out []string
	for _, o := range ix.References(sym) {
		rel, _ := filepath.Rel(dir, o.File.Path)
		line := strings.Count(string(o.File.Syn.Src[:o.Span.Start]), "\n") + 1
		s := fmt.Sprintf("%s:%d", rel, line)
		if o.Decl {
			s += "+"
		}
		if o.ViaAlias {
			s += "~"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestReferences(t *testing.T) {
	ix, dir := workspace(t, files)
	// double: both overloads, the re-export, the imports (one aliased),
	// the module-qualified call, and the alias's use (marked ~).
	got := strings.Join(refsOf(t, ix, dir, "pkg/mathlib/ops.mojo", "double", 0), " ")
	for _, w := range []string{"app/main.mojo:2+", "app/main.mojo:3", "pkg/mathlib/__init__.mojo:1+", "pkg/mathlib/ops.mojo:2+", "pkg/mathlib/ops.mojo:5+", "app/main.mojo:12"} {
		if !strings.Contains(" "+got+" ", " "+w+" ") {
			t.Errorf("double refs missing %s:\n%s", w, got)
		}
	}
	if strings.Count(got, "app/main.mojo:9") != 3 || strings.Count(got, "app/main.mojo:9~") != 1 || !strings.Contains(got, "app/main.mojo:3+~") {
		t.Errorf("line 9 should have double, o.double and twice~; line 3 the alias binding: %s", got)
	}

	// The field x: its declaration, self.x, v.x, v.scaled(3).x, self.x * k.
	got = strings.Join(refsOf(t, ix, dir, "pkg/mathlib/ops.mojo", "x", 4), " ")
	if !strings.Contains(got, "pkg/mathlib/ops.mojo:9+") || strings.Count(got, "app/main.mojo:8") != 2 || strings.Count(got, "pkg/mathlib/ops.mojo:12") != 1 {
		t.Errorf("field x refs: %s", got)
	}

	// A local stays local: the outer x in main is not the inner one.
	got = strings.Join(refsOf(t, ix, dir, "app/main.mojo", "x", 3), " ")
	if got != "app/main.mojo:13+ app/main.mojo:14 app/main.mojo:17" {
		t.Errorf("outer x refs: %s", got)
	}

	// total: declaration, capture, and both uses in the closure.
	got = strings.Join(refsOf(t, ix, dir, "app/main.mojo", "total", 0), " ")
	if got != "app/main.mojo:10+ app/main.mojo:11+ app/main.mojo:12 app/main.mojo:12" {
		t.Errorf("total refs: %s", got)
	}
}

func TestEditsInvalidate(t *testing.T) {
	ix, dir := workspace(t, files)
	ops := filepath.Join(dir, "pkg/mathlib/ops.mojo")
	if got := resolveAt(t, ix, dir, "app/main.mojo", "scaled", 0); !strings.HasSuffix(got, "def scaled") {
		t.Fatalf("before: %s", got)
	}
	// Rename the method in the editor: the use no longer resolves.
	src, _ := os.ReadFile(ops)
	ix.Update(ops, []byte(strings.Replace(string(src), "def scaled", "def scale_by", 1)))
	if got := resolveAt(t, ix, dir, "app/main.mojo", "scaled", 0); got != "-" {
		t.Errorf("after rename in editor: %s", got)
	}
	// Close without saving: back to the disk version.
	ix.Close(ops)
	if got := resolveAt(t, ix, dir, "app/main.mojo", "scaled", 0); !strings.HasSuffix(got, "def scaled") {
		t.Errorf("after close: %s", got)
	}
}

func TestSelfImportOfSubmodule(t *testing.T) {
	ix, dir := workspace(t, map[string]string{
		"pkg/os/__init__.mojo":      "from . import path\n",
		"pkg/os/path/__init__.mojo": "from .impl import exists\n",
		"pkg/os/path/impl.mojo":     "def exists(p: String) -> Bool:\n    return True\n",
		"app/m.mojo":                "import os\nos.path.exists(\"x\")\n",
	})
	if got := resolveAt(t, ix, dir, "app/m.mojo", "exists", 0); got != "pkg/os/path/impl.mojo:1 def exists" {
		t.Errorf("os.path.exists: %s", got)
	}
}

func TestCyclesTerminate(t *testing.T) {
	ix, dir := workspace(t, map[string]string{
		"pkg/a/__init__.mojo": "from .b import X\nfrom .b import *\ncomptime Y = Y\n",
		"pkg/a/b.mojo":        "from . import X\nfrom .__init__ import *\ncomptime Z = Z.W\n",
		"app/m.mojo":          "from a import X, Y\nX.q\nY.r\n",
	})
	for _, w := range []string{"X", "Y"} {
		_ = resolveAt(t, ix, dir, "app/m.mojo", w, 1)
	}
	for _, w := range []string{"q", "r"} {
		_ = resolveAt(t, ix, dir, "app/m.mojo", w, 0)
	}
	_ = resolveAt(t, ix, dir, "pkg/a/b.mojo", "W", 0)
}

func TestTypeHierarchy(t *testing.T) {
	ix, dir := workspace(t, map[string]string{
		"pkg/shapes/__init__.mojo": "from .base import Shape, Named\n",
		"pkg/shapes/base.mojo":     "trait Named:\n    def name(self) -> String: ...\n\ntrait Shape(Named):\n    def area(self) -> Float64: ...\n",
		"app/circle.mojo":          "from shapes import Shape\n\nstruct Circle(Shape, Copyable):\n    var r: Float64\n",
		"app/square.mojo":          "from shapes import Shape as S\n\nstruct Square(S):\n    var side: Float64\n\ndef main():\n    var sq = Square(1.0)\n",
	})
	base := filepath.Join(dir, "pkg/shapes/base.mojo")
	shape, ok := ix.DeclAt(base, "Shape", 0)
	if !ok {
		t.Fatal("no Shape")
	}
	names := func(ts []Target) string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Decl.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(ix.Subtypes(shape)); got != "Circle,Square" {
		t.Errorf("subtypes of Shape: %s (the alias S must count)", got)
	}
	if got := names(ix.Supertypes(shape)); got != "Named" {
		t.Errorf("supertypes of Shape: %s", got)
	}
	circle, _ := ix.DeclAt(filepath.Join(dir, "app/circle.mojo"), "Circle", 0)
	// Copyable is not in the workspace (a binary std): left out.
	if got := names(ix.Supertypes(circle)); got != "Shape" {
		t.Errorf("supertypes of Circle: %s", got)
	}
	named, _ := ix.DeclAt(base, "Named", 0)
	if got := names(ix.Subtypes(named)); got != "Shape" {
		t.Errorf("subtypes of Named: %s", got)
	}
	// From a variable, its type.
	sq := ix.File(filepath.Join(dir, "app/square.mojo"))
	if tt, ok := ix.TypeAt(sq, at(string(sq.Syn.Src), "sq", 0)); !ok || tt.Decl.Name != "Square" {
		t.Errorf("TypeAt(sq): %v %v", tt.Decl, ok)
	}
}
