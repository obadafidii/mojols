package organize

import (
	"strings"
	"testing"

	"github.com/tdadadavid/mojols/server/internal/syntax"
)

func apply(src string, path string) (string, bool) {
	f := syntax.Parse([]byte(src))
	e, ok := Organize(f, path, func(string) bool { return false })
	if !ok {
		return src, false
	}
	return src[:e.Start] + e.Text + src[e.End:], true
}

func TestOrganize(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"sort, merge, group, drop unused",
			"from .util import helper\nfrom std.collections import List\nimport mypkg\nfrom std.collections import Dict, List\nfrom std.math import sqrt, floor\n\ndef f():\n    var d = Dict[Int, Int]()\n    var l = List[Int]()\n    helper(sqrt(2.0), mypkg.x)\n",
			"from std.collections import Dict, List\nfrom std.math import sqrt\n\nimport mypkg\n\nfrom .util import helper\n\ndef f():\n    var d = Dict[Int, Int]()\n    var l = List[Int]()\n    helper(sqrt(2.0), mypkg.x)\n",
		},
		{
			"aliases keyed by the alias",
			"from std.math import max as _max, min\nimport std.os as os_mod\n\n_max(1, 2)\n",
			"from std.math import max as _max\n\n_max(1, 2)\n",
		},
		{
			"constants, classes, functions",
			"from m import zeta, Alpha, BETA, beta\n\nprint(zeta, Alpha, BETA, beta)\n",
			"from m import BETA, Alpha, beta, zeta\n\nprint(zeta, Alpha, BETA, beta)\n",
		},
		{
			"long lines wrap like mojo format",
			"from std.collections.string.string_slice import StringSlice, CodepointsIter, CodepointSliceIter\n\nprint(StringSlice, CodepointsIter, CodepointSliceIter)\n",
			"from std.collections.string.string_slice import (\n    CodepointSliceIter,\n    CodepointsIter,\n    StringSlice,\n)\n\nprint(StringSlice, CodepointsIter, CodepointSliceIter)\n",
		},
		{
			"all unused",
			"from std.math import sqrt\nimport os\n\ndef f():\n    pass\n",
			"\ndef f():\n    pass\n",
		},
		{
			"star imports kept",
			"from b import *\nfrom a import x\n\nx\n",
			"from a import x\nfrom b import *\n\nx\n",
		},
		{
			"use in a t-string counts",
			"from m import name\n\nprint(t\"{name}\")\n",
			"from m import name\n\nprint(t\"{name}\")\n",
		},
		{
			"crlf kept",
			"from b import y\r\nfrom a import x\r\n\r\nx, y\r\n",
			"from a import x\r\nfrom b import y\r\n\r\nx, y\r\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := apply(c.src, "/p/m.mojo")
			if got != c.want {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestOrganizeLeavesAlone(t *testing.T) {
	for name, src := range map[string]string{
		"already organized":  "from a import x\nfrom b import y\n\nx, y\n",
		"comment in block":   "from b import y  # keep\nfrom a import x\n\nx, y\n",
		"no imports":         "def f():\n    pass\n",
		"imports after code": "x = 1\nfrom a import b\n",
	} {
		if got, changed := apply(src, "/p/m.mojo"); changed {
			t.Errorf("%s: changed to %q", name, got)
		}
	}
	// __init__.mojo keeps everything (re-exports), but still sorts.
	got, _ := apply("from .b import y\nfrom .a import x\n", "/p/pkg/__init__.mojo")
	if got != "from .a import x\nfrom .b import y\n" {
		t.Errorf("__init__: %q", got)
	}
}

// The block stops at a comment line; imports after it are not touched.
func TestOrganizeStopsAtComment(t *testing.T) {
	src := "from b import y\nfrom a import x\n# section\nfrom d import w\nfrom c import v\n\nx, y, v, w\n"
	got, _ := apply(src, "/p/m.mojo")
	if !strings.HasPrefix(got, "from a import x\nfrom b import y\n# section\nfrom d import w\nfrom c import v\n") {
		t.Errorf("got %q", got)
	}
}

// Organizing is idempotent: organizing the result changes nothing.
func TestOrganizeIdempotent(t *testing.T) {
	src := "from .util import helper\nfrom std.collections import List\nfrom std.collections import Dict\n\nhelper(List, Dict)\n"
	once, _ := apply(src, "/p/m.mojo")
	twice, changed := apply(once, "/p/m.mojo")
	if changed || twice != once {
		t.Errorf("not idempotent:\n%q\n%q", once, twice)
	}
}

// A name re-exported to another module is kept even if unused here.
func TestOrganizeKeepsReexports(t *testing.T) {
	src := "from .backend import tile, vectorize\n\ndef f():\n    pass\n"
	f := syntax.Parse([]byte(src))
	e, ok := Organize(f, "/p/m.mojo", func(name string) bool { return name == "vectorize" })
	if !ok || e.Text != "from .backend import vectorize" {
		t.Errorf("got %+v %v", e, ok)
	}
	// Without the workspace answer, nothing is removed.
	if _, ok := Organize(f, "/p/m.mojo", nil); ok {
		t.Error("nil importedElsewhere must not remove anything")
	}
}
