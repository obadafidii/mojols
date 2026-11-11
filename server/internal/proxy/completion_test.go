package proxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportContext(t *testing.T) {
	type want struct {
		ok             bool
		names, keyword bool
		module, part   string
	}
	cases := []struct {
		src string // | marks the cursor
		w   want
	}{
		{"from std.|", want{ok: true, module: "std"}},
		{"from std.coll|", want{ok: true, module: "std", part: "coll"}},
		{"from |", want{ok: true}},
		{"from ge|", want{ok: true, part: "ge"}},
		{"from .|", want{ok: true, module: "."}},
		{"from ..sh|", want{ok: true, module: "..", part: "sh"}},
		{"from .a.b|", want{ok: true, module: ".a", part: "b"}},
		{"import std.|", want{ok: true, module: "std"}},
		{"import os, std.coll|", want{ok: true, module: "std", part: "coll"}},
		{"import std.sys as |", want{}},
		{"from std.collections |", want{ok: true, keyword: true, module: "std.collections"}},
		{"from std.collections imp|", want{ok: true, keyword: true, module: "std.collections", part: "imp"}},
		{"from std.collections import |", want{ok: true, names: true, module: "std.collections"}},
		{"from std.collections import Li|", want{ok: true, names: true, module: "std.collections", part: "Li"}},
		{"from std.collections import List, Di|", want{ok: true, names: true, module: "std.collections", part: "Di"}},
		{"from std.collections import List as L, |", want{ok: true, names: true, module: "std.collections"}},
		{"from std.collections import List as |", want{}},
		{"from std.collections import (\n    List,\n    Di|", want{ok: true, names: true, module: "std.collections", part: "Di"}},
		{"from .shapes import (Circle, a|", want{ok: true, names: true, module: ".shapes", part: "a"}},
		{"from x import (\n    a,\n)\nb|", want{}},
		{"def main():\n    var x = fro|", want{}},
		{"# from std.|", want{}},
		{"    from std.|", want{ok: true, module: "std"}},
		{"from std.collections import *|", want{}},
	}
	for _, c := range cases {
		off := strings.Index(c.src, "|")
		src := []byte(strings.Replace(c.src, "|", "", 1))
		ctx, ok := importContext(src, off)
		got := want{ok: ok}
		if ok {
			got = want{ok: true, names: ctx.names, keyword: ctx.keyword, module: ctx.module, part: ctx.partial}
		}
		if got != c.w {
			t.Errorf("%q: got %+v, want %+v", c.src, got, c.w)
		}
	}
}

type testCompletion struct {
	IsIncomplete bool `json:"isIncomplete"`
	Items        []struct {
		Label               string                    `json:"label"`
		Kind                int                       `json:"kind"`
		Detail              string                    `json:"detail"`
		SortText            string                    `json:"sortText"`
		TextEdit            *struct{ NewText string } `json:"textEdit"`
		AdditionalTextEdits []struct {
			Range struct {
				Start struct{ Line, Character int }
			} `json:"range"`
			NewText string `json:"newText"`
		} `json:"additionalTextEdits"`
		LabelDetails struct{ Description string } `json:"labelDetails"`
	} `json:"items"`
}

func (c testCompletion) labels() []string {
	var out []string
	for _, it := range c.Items {
		out = append(out, it.Label)
	}
	return out
}

func (h *harness) completeAt(uri string, line, char int) testCompletion {
	h.t.Helper()
	var out testCompletion
	r := h.call("textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": pos(line, char)})
	if r.Error != nil {
		h.t.Fatalf("completion: %v", r.Error)
	}
	if len(r.Result) > 0 && r.Result[0] == '[' {
		json.Unmarshal(r.Result, &out.Items)
	} else {
		json.Unmarshal(r.Result, &out)
	}
	return out
}

func TestImportCompletionFromIndex(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/imp.mojo"))
	h.openURI(uri, "from \n")
	c := h.completeAt(uri, 0, 5)
	if !contains(c.labels(), "geo") || !contains(c.labels(), "main") || contains(c.labels(), "imp") {
		t.Fatalf("top-level modules: %v", c.labels())
	}
	h.replaceText(uri, 2, "from geo.\n")
	if c = h.completeAt(uri, 0, 9); strings.Join(c.labels(), ",") != "shapes" {
		t.Fatalf("submodules: %v", c.labels())
	}
	h.replaceText(uri, 3, "from geo import \n")
	c = h.completeAt(uri, 0, 16)
	if strings.Join(c.labels(), ",") != "Circle,area,shapes" || c.Items[0].Kind != 22 || c.Items[1].Kind != 3 || c.Items[1].Detail != "def area(c: Circle) -> Float64" {
		t.Fatalf("package names: %+v", c.Items)
	}
	h.replaceText(uri, 4, "from geo import (\n    Circle,\n    a\n")
	c = h.completeAt(uri, 2, 5)
	if strings.Join(c.labels(), ",") != "area,shapes" || c.Items[0].TextEdit.NewText != "area" {
		t.Fatalf("parenthesized, Circle excluded: %+v", c.labels())
	}
	inPkg := pathToURI(filepath.Join(dir, "pkg/geo/extra.mojo"))
	h.openURI(inPkg, "from .shapes import \nfrom .\n")
	if c = h.completeAt(inPkg, 0, 20); strings.Join(c.labels(), ",") != "Circle,area" {
		t.Fatalf("relative module names: %v", c.labels())
	}
	if c = h.completeAt(inPkg, 1, 6); strings.Join(c.labels(), ",") != "shapes" {
		t.Fatalf("relative modules: %v", c.labels())
	}
	if fromBackend(c) {
		t.Error("import completion went to the backend")
	}
	// A module mojols cannot see: the backend answers.
	h.replaceText(uri, 6, "from max.gpu import \n")
	if c = h.completeAt(uri, 0, 20); !fromBackend(c) {
		t.Errorf("unknown module not passed to the backend: %v", c.labels())
	}
}

func TestUnimportedNamesInCompletion(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/use.mojo"))
	h.openURI(uri, "from geo import area\n\ndef main():\n    var c = Cir\n")
	c := h.completeAt(uri, 3, 15)
	var found bool
	for _, it := range c.Items {
		if it.Label != "Circle" {
			continue
		}
		found = true
		if it.LabelDetails.Description != "geo" || it.TextEdit.NewText != "Circle" || len(it.AdditionalTextEdits) != 1 ||
			it.AdditionalTextEdits[0].NewText != "from geo import Circle, area" || it.SortText[0] != '~' {
			t.Fatalf("auto-import item: %+v", it)
		}
	}
	if !found {
		t.Fatalf("Circle not offered: %v", c.labels())
	}
	// One letter: nothing looked up, but the editor is asked to come back.
	h.replaceText(uri, 9, "from geo import area\n\ndef main():\n    var c = C\n")
	if c := h.completeAt(uri, 3, 13); !c.IsIncomplete || contains(c.labels(), "Circle") {
		t.Errorf("one letter: incomplete=%v %v", c.IsIncomplete, c.labels())
	}
	// Already imported names, members and short prefixes get nothing.
	h.replaceText(uri, 2, "from geo import area\n\ndef main():\n    var c = ar\n")
	if c := h.completeAt(uri, 3, 14); contains(c.labels(), "area") && len(c.Items) > 0 && c.Items[len(c.Items)-1].SortText != "" && c.Items[len(c.Items)-1].SortText[0] == '~' {
		t.Errorf("imported name offered again: %v", c.labels())
	}
	h.replaceText(uri, 3, "from geo import area\n\ndef main():\n    var c = x.Cir\n")
	for _, it := range h.completeAt(uri, 3, 17).Items {
		if it.Label == "Circle" {
			t.Errorf("auto-import after a dot: %+v", it)
		}
	}
}

func fromBackend(c testCompletion) bool {
	for _, l := range c.labels() {
		if strings.HasPrefix(l, "fake:") {
			return true
		}
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
