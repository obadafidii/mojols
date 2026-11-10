package proxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
)

// indexedWorkspace writes a two-package workspace and returns its root.
func indexedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/geo/__init__.mojo", "from .shapes import area, Circle\n")
	write("pkg/geo/shapes.mojo", "struct Circle:\n    var r: Float64\n\ndef area(c: Circle) -> Float64:\n    return c.r * c.r\n")
	write("app/main.mojo", "from geo import area, Circle\n\ndef main():\n    var c = Circle(2.0)\n    print(area(c), c.r)\n")
	return dir
}

// newIndexedHarness starts a harness whose backend gets <dir>/pkg as an
// include directory, and initializes it with dir as the workspace.
func newIndexedHarness(t *testing.T, dir string, fakeEnv []string, documentChanges bool) *harness {
	t.Helper()
	h := newHarness(t, fakeEnv, func(o *Options) {
		resolve := o.Resolve
		o.Resolve = func(roots []string) (Command, error) {
			c, err := resolve(roots)
			c.IncludeDirs = []string{filepath.Join(dir, "pkg")}
			return c, err
		}
	})
	caps := map[string]any{}
	if documentChanges {
		caps["workspace"] = map[string]any{"workspaceEdit": map[string]any{"documentChanges": true}}
	}
	m := h.call("initialize", map[string]any{"processId": nil, "rootUri": pathToURI(dir), "capabilities": caps})
	if m.Error != nil {
		t.Fatalf("initialize: %v", m.Error)
	}
	h.notify("initialized", map[string]any{})
	var init struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	json.Unmarshal(m.Result, &init)
	if string(init.Capabilities["renameProvider"]) != `{"prepareProvider":true}` || string(init.Capabilities["referencesProvider"]) != "true" ||
		string(init.Capabilities["workspaceSymbolProvider"]) != "true" {
		t.Fatalf("index capabilities not advertised: %s", m.Result)
	}
	return h
}

type lspLoc struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct{ Line, Character int } `json:"start"`
	} `json:"range"`
}

func locStrings(dir string, locs []lspLoc) []string {
	var out []string
	for _, l := range locs {
		p := strings.TrimPrefix(l.URI, "file://")
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel+":"+itoa(l.Range.Start.Line+1)+":"+itoa(l.Range.Start.Character+1))
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func pos(line, char int) map[string]any { return map[string]any{"line": line, "character": char} }

func TestIndexReferencesAcrossFiles(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src))

	var locs []lspLoc
	h.callOK("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 11), // area( in print(area(c)...)
		"context": map[string]any{"includeDeclaration": true},
	}, &locs)
	got := strings.Join(locStrings(dir, locs), " ")
	want := "app/main.mojo:1:17 app/main.mojo:5:11 pkg/geo/__init__.mojo:1:21 pkg/geo/shapes.mojo:4:5"
	if got != want {
		t.Errorf("references:\n got %s\nwant %s", got, want)
	}

	// Without the declaration.
	h.callOK("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 11),
		"context": map[string]any{"includeDeclaration": false},
	}, &locs)
	if got := strings.Join(locStrings(dir, locs), " "); strings.Contains(got, "shapes.mojo:4:5") || !strings.Contains(got, "__init__.mojo") {
		t.Errorf("without declaration: %s", got)
	}

	// An unsaved edit adding a use is seen at once.
	h.replaceText(mainURI, 2, string(src)+"    area(c)\n")
	h.callOK("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 11),
		"context": map[string]any{"includeDeclaration": true},
	}, &locs)
	if got := strings.Join(locStrings(dir, locs), " "); !strings.Contains(got, "app/main.mojo:6:5") {
		t.Errorf("unsaved use missing: %s", got)
	}
}

func TestIndexDefinition(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src))
	var locs []lspLoc
	// c.r: a field, through `var c = Circle(2.0)`.
	h.callOK("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 22)}, &locs)
	if got := strings.Join(locStrings(dir, locs), " "); got != "pkg/geo/shapes.mojo:2:9" {
		t.Errorf("definition of c.r: %s", got)
	}
	// An unresolvable name goes to the backend.
	before := h.fakeState().Definitions
	m := h.call("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 4)}) // print
	if m.Error != nil || h.fakeState().Definitions != before+1 {
		t.Errorf("fallback definition did not reach the backend: %v %s", m.Error, m.Result)
	}
}

type workspaceEdit struct {
	Changes         map[string][]struct{ NewText string } `json:"changes"`
	DocumentChanges []struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version *int   `json:"version"`
		} `json:"textDocument"`
		Edits []struct{ NewText string } `json:"edits"`
	} `json:"documentChanges"`
}

func TestIndexRename(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, true)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src))
	at := map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(3, 13)} // Circle(2.0)

	var prep struct {
		Placeholder string `json:"placeholder"`
	}
	h.callOK("textDocument/prepareRename", at, &prep)
	if prep.Placeholder != "Circle" {
		t.Errorf("prepareRename placeholder %q", prep.Placeholder)
	}

	var edit workspaceEdit
	h.callOK("textDocument/rename", map[string]any{"textDocument": at["textDocument"], "position": at["position"], "newName": "Disk"}, &edit)
	counts := map[string]int{}
	for _, dc := range edit.DocumentChanges {
		rel, _ := filepath.Rel(dir, strings.TrimPrefix(dc.TextDocument.URI, "file://"))
		counts[rel] += len(dc.Edits)
		for _, e := range dc.Edits {
			if e.NewText != "Disk" {
				t.Errorf("edit text %q", e.NewText)
			}
		}
		if dc.TextDocument.URI == mainURI && (dc.TextDocument.Version == nil || *dc.TextDocument.Version != 1) {
			t.Errorf("open document edit should carry its version")
		}
	}
	// shapes: the struct and the annotation `c: Circle`; __init__: the
	// re-export; main: the import and the call.
	if counts["pkg/geo/shapes.mojo"] != 2 || counts["pkg/geo/__init__.mojo"] != 1 || counts["app/main.mojo"] != 2 {
		t.Errorf("rename edits per file: %v", counts)
	}

	for _, bad := range []struct{ name, why string }{
		{"def", "keyword"},
		{"2fast", "valid Mojo identifier"},
		{"area", "already declared"},
		{"__init__", "special"},
	} {
		m := h.call("textDocument/rename", map[string]any{"textDocument": at["textDocument"], "position": at["position"], "newName": bad.name})
		if m.Error == nil || !strings.Contains(m.Error.Message, bad.why) {
			t.Errorf("rename to %q: want error mentioning %q, got %v %s", bad.name, bad.why, m.Error, m.Result)
		}
	}
	// `print` comes from outside the workspace: not renamable.
	m := h.call("textDocument/prepareRename", map[string]any{"textDocument": at["textDocument"], "position": pos(4, 5)})
	if m.Error == nil {
		t.Errorf("prepareRename on print: want an error, got %s", m.Result)
	}
}

func TestIndexWorkspaceSymbol(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	var syms []struct {
		Name string `json:"name"`
		Kind int    `json:"kind"`
	}
	h.callOK("workspace/symbol", map[string]any{"query": "circ"}, &syms)
	if len(syms) == 0 || syms[0].Name != "Circle" || syms[0].Kind != symStruct {
		t.Errorf("workspace/symbol circ: %+v", syms)
	}
}

func TestIndexWorksWithoutBackend(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, []string{"MOJOLS_FAKE_EXIT_ON_START=1"}, false)
	h.expect("degraded", 10*time.Second, func(m *jsonrpc.Message) bool { return m.Method == "window/showMessage" })
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src))
	var locs []lspLoc
	h.callOK("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 11),
		"context": map[string]any{"includeDeclaration": true},
	}, &locs)
	if len(locs) != 4 {
		t.Errorf("references with the backend down: %d", len(locs))
	}
}

func TestIndexReferencesConfirmedByServer(t *testing.T) {
	dir := indexedWorkspace(t)
	shapes := filepath.Join(dir, "pkg/geo/shapes.mojo")
	// The fake server knows `r` is the field declared at shapes.mojo:2:9.
	h := newIndexedHarness(t, dir, []string{"MOJOLS_FAKE_DEFS=r=" + shapes + ":1:8"}, false)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	// mystery() is unknown to mojols, so `.r` on its result is unresolved.
	h.openURI(mainURI, string(src)+"    print(mystery().r)\n")
	var locs []lspLoc
	h.callOK("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(shapes)}, "position": pos(1, 8),
		"context": map[string]any{"includeDeclaration": true},
	}, &locs)
	got := strings.Join(locStrings(dir, locs), " ")
	if got != "app/main.mojo:5:22 app/main.mojo:6:21 pkg/geo/shapes.mojo:2:9 pkg/geo/shapes.mojo:5:14 pkg/geo/shapes.mojo:5:20" {
		t.Errorf("references to r: %s", got)
	}
	var st Status
	h.callOK("mojols/status", nil, &st)
	if st.Verifier == nil || st.Verifier.Files != 1 {
		t.Errorf("verifier status: %+v", st.Verifier)
	}

	// Rename covers the confirmed use too.
	var edit workspaceEdit
	h.callOK("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": pathToURI(shapes)}, "position": pos(1, 8), "newName": "radius"}, &edit)
	n := 0
	for _, e := range edit.Changes {
		n += len(e)
	}
	if n != 5 { // shapes: the field and c.r * c.r; main: c.r and mystery().r
		t.Errorf("rename edits: %d (%+v)", n, edit.Changes)
	}
}

func TestIndexRenameRefusesUncheckedUses(t *testing.T) {
	dir := indexedWorkspace(t)
	shapes := filepath.Join(dir, "pkg/geo/shapes.mojo")
	// The server crashes on every definition request: unknown uses stay
	// unknown, and renaming would risk missing them.
	h := newIndexedHarness(t, dir, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/definition"}, false)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src)+"    print(mystery().r)\n")
	m := h.call("textDocument/rename", map[string]any{"textDocument": map[string]any{"uri": pathToURI(shapes)}, "position": pos(1, 8), "newName": "radius"})
	if m.Error == nil || !strings.Contains(m.Error.Message, "could not be checked") {
		t.Errorf("rename with unchecked uses: %v %s", m.Error, m.Result)
	}
}

func TestIndexTypeHierarchy(t *testing.T) {
	dir := indexedWorkspace(t)
	os.WriteFile(filepath.Join(dir, "pkg/geo/shapes.mojo"), []byte("trait Shape:\n    def area(self) -> Float64: ...\n\nstruct Circle(Shape):\n    var r: Float64\n\ndef area(c: Circle) -> Float64:\n    return c.r * c.r\n"), 0o644)
	h := newIndexedHarness(t, dir, nil, false)
	var items []map[string]any
	h.callOK("textDocument/prepareTypeHierarchy", map[string]any{"textDocument": map[string]any{"uri": pathToURI(filepath.Join(dir, "pkg/geo/shapes.mojo"))}, "position": pos(0, 7)}, &items)
	if len(items) != 1 || items[0]["name"] != "Shape" || items[0]["kind"].(float64) != symInterface {
		t.Fatalf("prepare: %+v", items)
	}
	var subs []map[string]any
	h.callOK("typeHierarchy/subtypes", map[string]any{"item": items[0]}, &subs)
	if len(subs) != 1 || subs[0]["name"] != "Circle" || subs[0]["detail"] != "struct Circle(Shape)" {
		t.Errorf("subtypes: %+v", subs)
	}
	var sups []map[string]any
	h.callOK("typeHierarchy/supertypes", map[string]any{"item": subs[0]}, &sups)
	if len(sups) != 1 || sups[0]["name"] != "Shape" {
		t.Errorf("supertypes: %+v", sups)
	}
}

func TestStdlibSources(t *testing.T) {
	dir := indexedWorkspace(t)
	lib := t.TempDir()
	os.MkdirAll(filepath.Join(lib, "std", "collections"), 0o755)
	os.WriteFile(filepath.Join(lib, "std", "__init__.mojo"), nil, 0o644)
	os.WriteFile(filepath.Join(lib, "std", "collections", "__init__.mojo"), []byte("from .list import List\n"), 0o644)
	os.WriteFile(filepath.Join(lib, "std", "collections", "list.mojo"), []byte("trait Sized:\n    def __len__(self) -> Int: ...\n\nstruct List[T: AnyType](Sized):\n    def append(mut self, value: T):\n        pass\n"), 0o644)
	user := "from std.collections import List\nfrom std.collections.list import Sized\n\nstruct Bag(Sized):\n    var items: List[Int]\n\ndef main():\n    var b = List[Int]()\n    b.append(1)\n"
	os.WriteFile(filepath.Join(dir, "app/bag.mojo"), []byte(user), 0o644)

	h := newHarness(t, nil, func(o *Options) {
		o.StdlibSources = "auto"
		o.StdlibFetch = func(context.Context, string) (string, error) { return lib, nil }
	})
	h.call("initialize", map[string]any{"processId": nil, "rootUri": pathToURI(dir), "capabilities": map[string]any{}})
	h.notify("initialized", map[string]any{})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(h.status().StdlibSources, "ready") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := h.status().StdlibSources; !strings.HasPrefix(st, "ready") {
		t.Fatalf("stdlib sources: %q", st)
	}
	bag := pathToURI(filepath.Join(dir, "app/bag.mojo"))
	var locs []lspLoc
	h.callOK("textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": bag}, "position": pos(8, 6)}, &locs) // b.append
	if len(locs) != 1 || !strings.HasSuffix(locs[0].URI, "std/collections/list.mojo") || locs[0].Range.Start.Line != 4 {
		t.Errorf("definition of append into the stdlib: %+v", locs)
	}
	var items []map[string]any
	h.callOK("textDocument/prepareTypeHierarchy", map[string]any{"textDocument": map[string]any{"uri": bag}, "position": pos(3, 8)}, &items)
	var sups []map[string]any
	h.callOK("typeHierarchy/supertypes", map[string]any{"item": items[0]}, &sups)
	if len(sups) != 1 || sups[0]["name"] != "Sized" {
		t.Errorf("supertypes of Bag: %+v", sups)
	}
	var subs []map[string]any
	h.callOK("typeHierarchy/subtypes", map[string]any{"item": sups[0]}, &subs)
	names := []string{}
	for _, x := range subs {
		names = append(names, x["name"].(string))
	}
	if strings.Join(names, ",") != "Bag,List" {
		t.Errorf("subtypes of Sized (workspace and library): %v", names)
	}
	// Library code is never renamed.
	m := h.call("textDocument/prepareRename", map[string]any{"textDocument": map[string]any{"uri": bag}, "position": pos(8, 6)})
	if m.Error == nil || !strings.Contains(m.Error.Message, "outside the workspace") {
		t.Errorf("rename of a stdlib method: %v %s", m.Error, m.Result)
	}
}

func TestIndexCallHierarchy(t *testing.T) {
	dir := indexedWorkspace(t)
	os.WriteFile(filepath.Join(dir, "pkg/geo/shapes.mojo"), []byte(`struct Circle:
    var r: Float64

    def __init__(out self, r: Float64):
        self.r = r

    def scaled(self, k: Float64) -> Circle:
        return Circle(self.r * k)

def area(c: Circle) -> Float64:
    return c.r * c.r
`), 0o644)
	os.WriteFile(filepath.Join(dir, "app/main.mojo"), []byte(`from geo import area, Circle

def total(cs: List[Circle]) -> Float64:
    var t = 0.0
    for c in cs:
        t += area(c) + area(Circle(3.0).scaled(2))
    return t

def main():
    var c = Circle(2.0)
    print(area(c), total([c]))

area(Circle(1.0))
`), 0o644)
	h := newIndexedHarness(t, dir, nil, false)
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	shapesURI := pathToURI(filepath.Join(dir, "pkg/geo/shapes.mojo"))

	var items []map[string]any
	h.callOK("textDocument/prepareCallHierarchy", map[string]any{"textDocument": map[string]any{"uri": shapesURI}, "position": pos(9, 4)}, &items)
	if len(items) != 1 || items[0]["name"] != "area" {
		t.Fatalf("prepare on area: %+v", items)
	}
	var in []struct {
		From       map[string]any `json:"from"`
		FromRanges []any          `json:"fromRanges"`
	}
	h.callOK("callHierarchy/incomingCalls", map[string]any{"item": items[0]}, &in)
	got := map[string]int{}
	for _, c := range in {
		got[c.From["name"].(string)] = len(c.FromRanges)
	}
	// total calls area twice, main once, and the file's top level once;
	// the import line is not a call.
	if got["total"] != 2 || got["main"] != 1 || got["main.mojo"] != 1 || len(got) != 3 {
		t.Errorf("incoming calls of area: %v", got)
	}

	h.callOK("textDocument/prepareCallHierarchy", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(2, 5)}, &items)
	var out []struct {
		To         map[string]any `json:"to"`
		FromRanges []any          `json:"fromRanges"`
	}
	h.callOK("callHierarchy/outgoingCalls", map[string]any{"item": items[0]}, &out)
	got = map[string]int{}
	for _, c := range out {
		got[c.To["name"].(string)] = len(c.FromRanges)
	}
	if got["area"] != 2 || got["Circle.scaled"] != 1 || got["Circle.__init__"] != 1 || len(got) != 3 {
		t.Errorf("outgoing calls of total: %v", got)
	}

	// Constructing a struct is a call of its __init__.
	h.callOK("textDocument/prepareCallHierarchy", map[string]any{"textDocument": map[string]any{"uri": shapesURI}, "position": pos(3, 9)}, &items)
	h.callOK("callHierarchy/incomingCalls", map[string]any{"item": items[0]}, &in)
	got = map[string]int{}
	for _, c := range in {
		got[c.From["name"].(string)] = len(c.FromRanges)
	}
	if got["Circle.scaled"] != 1 || got["main"] != 1 || got["main.mojo"] != 1 || got["total"] != 1 {
		t.Errorf("incoming calls of __init__: %v", got)
	}
	// And the item reached that way can be followed further.
	h.callOK("callHierarchy/outgoingCalls", map[string]any{"item": items[0]}, &out)
}

func TestFallbackHoverAndOutlineWithoutBackend(t *testing.T) {
	dir := indexedWorkspace(t)
	os.WriteFile(filepath.Join(dir, "pkg/geo/shapes.mojo"), []byte("struct Circle:\n    var r: Float64\n\ndef area(c: Circle) -> Float64:\n    \"\"\"The area.\n\n    Uses r squared.\n    \"\"\"\n    return c.r * c.r\n"), 0o644)
	h := newIndexedHarness(t, dir, []string{"MOJOLS_FAKE_EXIT_ON_START=1"}, false)
	h.expect("degraded", 10*time.Second, func(m *jsonrpc.Message) bool { return m.Method == "window/showMessage" })
	mainURI := pathToURI(filepath.Join(dir, "app/main.mojo"))
	src, _ := os.ReadFile(filepath.Join(dir, "app/main.mojo"))
	h.openURI(mainURI, string(src))
	var hov struct {
		Contents struct{ Kind, Value string } `json:"contents"`
	}
	h.callOK("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 11)}, &hov)
	want := "```mojo\ndef area(c: Circle) -> Float64\n```\n\n---\n\nThe area.\n\nUses r squared."
	if hov.Contents.Kind != "markdown" || hov.Contents.Value != want {
		t.Errorf("hover:\n%q\nwant\n%q", hov.Contents.Value, want)
	}
	h.callOK("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "position": pos(4, 22)}, &hov)
	if !strings.Contains(hov.Contents.Value, "var r: Float64") || !strings.Contains(hov.Contents.Value, "In `struct Circle`") {
		t.Errorf("field hover: %q", hov.Contents.Value)
	}
	var syms []struct {
		Name     string `json:"name"`
		Children []struct {
			Name string `json:"name"`
		} `json:"children"`
	}
	h.callOK("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": pathToURI(filepath.Join(dir, "pkg/geo/shapes.mojo"))}}, &syms)
	if len(syms) != 2 || syms[0].Name != "Circle" || len(syms[0].Children) != 1 || syms[1].Name != "area" {
		t.Errorf("outline: %+v", syms)
	}
	var actions []any
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": mainURI}, "range": map[string]any{"start": pos(0, 0), "end": pos(0, 0)}, "context": map[string]any{"diagnostics": []any{}}}, &actions)
}

func TestOrganizeImportsAction(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/tidy.mojo"))
	h.openURI(uri, "from geo import Circle, area\nfrom geo import area\nimport unused\n\ndef main():\n    print(area(Circle(1.0)))\n")
	type action struct {
		Title string `json:"title"`
		Kind  string `json:"kind"`
		Edit  struct {
			Changes map[string][]struct {
				NewText string `json:"newText"`
			} `json:"changes"`
		} `json:"edit"`
	}
	// On save: only organize imports, answered by mojols alone.
	var actions []action
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": uri}, "range": map[string]any{"start": pos(0, 0), "end": pos(0, 0)},
		"context": map[string]any{"diagnostics": []any{}, "only": []string{"source.organizeImports"}}}, &actions)
	if len(actions) != 1 || actions[0].Kind != "source.organizeImports" || actions[0].Edit.Changes[uri][0].NewText != "from geo import Circle, area" {
		t.Fatalf("organize on save: %+v", actions)
	}
	if h.fakeState().Definitions != 0 {
		t.Fatal("unexpected backend traffic")
	}
	// The light bulb: the backend's quick fixes plus organize imports.
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": uri}, "range": map[string]any{"start": pos(5, 4), "end": pos(5, 4)},
		"context": map[string]any{"diagnostics": []any{}}}, &actions)
	if len(actions) != 2 || actions[0].Title != "fake quick fix" || actions[1].Title != "Organize imports" {
		t.Fatalf("merged code actions: %+v", actions)
	}
	// Nothing to organize: no action offered.
	h.replaceText(uri, 2, "from geo import Circle, area\n\ndef main():\n    print(area(Circle(1.0)))\n")
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": uri}, "range": map[string]any{"start": pos(0, 0), "end": pos(0, 0)},
		"context": map[string]any{"diagnostics": []any{}, "only": []string{"source.organizeImports"}}}, &actions)
	if len(actions) != 0 {
		t.Errorf("already organized: %+v", actions)
	}
}

func TestOrganizeKeepsPackageImports(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	// A module of the geo package: its imports may be public API.
	uri := pathToURI(filepath.Join(dir, "pkg/geo/extra.mojo"))
	h.openURI(uri, "from .shapes import area\nfrom .shapes import Circle\n")
	var actions []struct {
		Edit struct {
			Changes map[string][]struct {
				NewText string `json:"newText"`
			} `json:"changes"`
		} `json:"edit"`
	}
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": uri}, "range": map[string]any{"start": pos(0, 0), "end": pos(0, 0)},
		"context": map[string]any{"diagnostics": []any{}, "only": []string{"source.organizeImports"}}}, &actions)
	if len(actions) != 1 || actions[0].Edit.Changes[uri][0].NewText != "from .shapes import Circle, area" {
		t.Errorf("package module: merged, nothing removed: %+v", actions)
	}
}
