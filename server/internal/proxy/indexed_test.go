package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
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
