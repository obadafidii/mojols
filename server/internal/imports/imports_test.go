package imports

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mods(imps []Import) []string {
	var out []string
	for _, i := range imps {
		s := i.Module
		if len(i.Names) > 0 {
			s += ":" + strings.Join(i.Names, ",")
		}
		out = append(out, s)
	}
	return out
}

func TestScan(t *testing.T) {
	src := `"""Module docstring.
from fake import nothing   # inside a docstring: ignored
"""
from mathlib import double
import os.path, collections as c
from .ops import (
    triple,   # comment inside the parens
    quad,
)
from ..shared.util import helper; import json
x = "from strings import ignored"
# from comments import ignored

def f():
    from lazy.mod import thing  # function-level import counts
    from std.collections import List
from long.line \
    import continued
from . import sibling
from 9bad import x
`
	got := mods(Scan(src))
	want := []string{
		"mathlib:double", "os.path", "collections", ".ops:triple,quad", "..shared.util:helper", "json",
		"lazy.mod:thing", "std.collections:List", "long.line:continued", ".:sibling",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if imps := Scan(src); imps[0].Line != 3 {
		t.Fatalf("line numbers: first import on line %d, want 3", imps[0].Line)
	}
}

func FuzzScan(f *testing.F) {
	f.Add("from a import (b,\n c)\nimport d")
	f.Add(`"""unterminated`)
	f.Add("'\\\nfrom x import y")
	f.Fuzz(func(t *testing.T, src string) {
		for _, imp := range Scan(src) {
			if !validModule(imp.Module) {
				t.Fatalf("invalid module %q from %q", imp.Module, src)
			}
		}
	})
}

func write(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolve(t *testing.T) {
	ws := t.TempDir()
	write(t, ws,
		"src/mathlib/__init__.mojo",
		"src/mathlib/ops.mojo",
		"src/mathlib/geo/__init__.mojo",
		"src/mathlib/geo/shapes.mojo",
		"src/prebuilt.mojopkg",
		"app/main.mojo",
		"app/helper.mojo",
	)
	r := Resolver{Roots: []string{filepath.Join(ws, "src")}}
	noRoots := Resolver{}
	main := filepath.Join(ws, "app/main.mojo")
	initF := filepath.Join(ws, "src/mathlib/__init__.mojo")
	rel := func(ps []string) []string {
		out := []string{}
		for _, p := range ps {
			x, _ := filepath.Rel(ws, p)
			out = append(out, filepath.ToSlash(x))
		}
		return out
	}
	cases := []struct {
		from string
		imp  Import
		want []string
	}{
		{main, Import{Module: "mathlib", Names: []string{"double"}}, []string{"src/mathlib/__init__.mojo"}},
		{main, Import{Module: "mathlib", Names: []string{"ops"}}, []string{"src/mathlib/__init__.mojo", "src/mathlib/ops.mojo"}},
		{main, Import{Module: "mathlib.geo.shapes"}, []string{"src/mathlib/__init__.mojo", "src/mathlib/geo/__init__.mojo", "src/mathlib/geo/shapes.mojo"}},
		{main, Import{Module: "helper"}, []string{"app/helper.mojo"}}, // the file's own directory
		{main, Import{Module: "prebuilt"}, []string{"src/prebuilt.mojopkg"}},
		{main, Import{Module: "std.collections"}, []string{}}, // toolchain: not in the workspace
		{initF, Import{Module: ".ops", Names: []string{"double"}}, []string{"src/mathlib/__init__.mojo", "src/mathlib/ops.mojo"}},
		{filepath.Join(ws, "src/mathlib/geo/shapes.mojo"), Import{Module: "..ops"}, []string{"src/mathlib/__init__.mojo", "src/mathlib/ops.mojo"}},
		{initF, Import{Module: ".", Names: []string{"geo"}}, []string{"src/mathlib/__init__.mojo", "src/mathlib/geo/__init__.mojo"}},
		// A package importing itself absolutely, with no include dirs: found
		// by walking up to the outermost package (the stdlib does this).
		{filepath.Join(ws, "src/mathlib/geo/shapes.mojo"), Import{Module: "mathlib.ops"}, []string{"src/mathlib/__init__.mojo", "src/mathlib/ops.mojo"}},
	}
	for i, c := range cases {
		res := r
		if i == len(cases)-1 {
			res = noRoots
		}
		if got := rel(res.Resolve(c.from, c.imp)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s from %s: got %v, want %v", c.imp.Module, filepath.Base(c.from), got, c.want)
		}
	}
}
