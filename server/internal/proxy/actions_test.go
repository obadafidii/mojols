package proxy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
)

type testAction struct {
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	IsPreferred bool   `json:"isPreferred"`
	Edit        struct {
		Changes map[string][]struct {
			Range struct {
				Start, End struct{ Line, Character int }
			} `json:"range"`
			NewText string `json:"newText"`
		} `json:"changes"`
	} `json:"edit"`
}

func (h *harness) actionsAt(uri string, line, char int, diags ...any) []testAction {
	h.t.Helper()
	if diags == nil {
		diags = []any{}
	}
	var out []testAction
	h.callOK("textDocument/codeAction", map[string]any{"textDocument": map[string]any{"uri": uri},
		"range": map[string]any{"start": pos(line, char), "end": pos(line, char)}, "context": map[string]any{"diagnostics": diags}}, &out)
	return out
}

func findAction(as []testAction, title string) *testAction {
	for i := range as {
		if as[i].Title == title {
			return &as[i]
		}
	}
	return nil
}

// ownDiags waits for a publishDiagnostics for uri carrying want mojols
// diagnostics, and returns their messages.
func (h *harness) ownDiags(uri string, want int) []string {
	h.t.Helper()
	var msgs []string
	h.expect("diagnostics with mojols' own", 5*time.Second, func(m *jsonrpc.Message) bool {
		if m.Method != "textDocument/publishDiagnostics" {
			return false
		}
		var p struct {
			URI         string `json:"uri"`
			Diagnostics []struct {
				Source, Message, Code string
				Severity              int
				Tags                  []int
			} `json:"diagnostics"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.URI != uri {
			return false
		}
		msgs = nil
		for _, d := range p.Diagnostics {
			if d.Source == "mojols" && d.Code == codeUnusedImport && d.Severity == 4 && len(d.Tags) == 1 {
				msgs = append(msgs, d.Message)
			}
		}
		return len(msgs) == want
	})
	return msgs
}

func TestUnusedImportIsReportedAndRemoved(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/tidy.mojo"))
	h.openURI(uri, "from geo import Circle, area\nimport geo\n\ndef main():\n    print(area)\n")
	msgs := h.ownDiags(uri, 2)
	if !strings.Contains(msgs[0], "'Circle'") || !strings.Contains(msgs[1], "'geo'") {
		t.Fatalf("diagnostics: %v", msgs)
	}
	as := h.actionsAt(uri, 0, 3)
	a := findAction(as, "Remove unused import 'Circle'")
	if a == nil || a.Kind != "quickfix" || a.Edit.Changes[uri][0].NewText != "from geo import area" {
		t.Fatalf("remove one name: %+v", as)
	}
	if findAction(as, "Remove unused import 'geo'") != nil {
		t.Errorf("fix offered away from its line: %+v", as)
	}
	all := findAction(as, "Remove all unused imports")
	if all == nil || len(all.Edit.Changes[uri]) != 2 {
		t.Fatalf("remove all: %+v", as)
	}
	// The plain import goes with its line.
	whole := findAction(h.actionsAt(uri, 1, 0), "Remove unused import 'geo'")
	if whole == nil || whole.Edit.Changes[uri][0].NewText != "" || whole.Edit.Changes[uri][0].Range.End.Line != 2 {
		t.Fatalf("remove a whole statement: %+v", whole)
	}
	// Using the name clears its diagnostic.
	h.replaceText(uri, 2, "from geo import Circle, area\n\ndef main():\n    print(area(Circle(1.0)))\n")
	h.ownDiags(uri, 0)
}

func TestPackageModuleImportsAreNotReported(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "pkg/geo/extra.mojo"))
	h.openURI(uri, "from .shapes import area\n")
	h.ownDiags(uri, 0)
	if as := h.actionsAt(uri, 0, 3); len(as) != 1 || as[0].Title != "fake quick fix" {
		t.Errorf("package module: %+v", as)
	}
}

func TestImportQuickFix(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/new.mojo"))
	h.openURI(uri, "\"\"\"Doc.\"\"\"\n\ndef main():\n    var c = Circle(2.0)\n    print(c.r)\n")
	as := h.actionsAt(uri, 3, 14)
	a := findAction(as, "Import 'Circle' from geo")
	if a == nil || !a.IsPreferred {
		t.Fatalf("import fix: %+v", as)
	}
	e := a.Edit.Changes[uri][0]
	if e.NewText != "\nfrom geo import Circle\n" || e.Range.Start.Line != 1 || e.Range.End.Line != 1 {
		t.Fatalf("new import after the docstring: %+v", e)
	}
	// Into an existing import of the module, through the server's diagnostic.
	h.replaceText(uri, 2, "from geo import area\n\ndef main():\n    var c = Circle(2.0)\n    print(area(c))\n")
	diag := map[string]any{"range": map[string]any{"start": pos(3, 12), "end": pos(3, 18)}, "severity": 1, "message": "use of unknown declaration 'Circle'"}
	a = findAction(h.actionsAt(uri, 0, 0, diag), "Import 'Circle' from geo")
	if a == nil || a.Edit.Changes[uri][0].NewText != "from geo import Circle, area" {
		t.Fatalf("merged into the existing import: %+v", a)
	}
	// A name that resolves gets no import fix.
	for _, a := range h.actionsAt(uri, 4, 11) {
		if strings.HasPrefix(a.Title, "Import ") {
			t.Errorf("import offered for a known name: %+v", a)
		}
	}
}

func TestUnusedVariableQuickFixes(t *testing.T) {
	dir := indexedWorkspace(t)
	h := newIndexedHarness(t, dir, nil, false)
	uri := pathToURI(filepath.Join(dir, "app/vars.mojo"))
	h.openURI(uri, "def main():\n    var x: Int = compute()\n    for i in range(3):\n        pass\n")
	assign := map[string]any{"range": map[string]any{"start": pos(1, 17), "end": pos(1, 26)}, "severity": 2, "message": "assignment to 'x' was never used; assign to '_' instead?"}
	a := findAction(h.actionsAt(uri, 1, 17, assign), "Assign to '_' instead of 'x'")
	if a == nil || a.Edit.Changes[uri][0].NewText != "_ = " || a.Edit.Changes[uri][0].Range.Start.Character != 4 || a.Edit.Changes[uri][0].Range.End.Character != 17 {
		t.Fatalf("discard assignment: %+v", a)
	}
	loop := map[string]any{"range": map[string]any{"start": pos(2, 8), "end": pos(2, 9)}, "severity": 2, "message": "ref 'i' was never used, remove it?"}
	a = findAction(h.actionsAt(uri, 2, 8, loop), "Rename 'i' to '_'")
	if a == nil || a.Edit.Changes[uri][0].NewText != "_" || a.Edit.Changes[uri][0].Range.End.Character != 9 {
		t.Fatalf("discard loop variable: %+v", a)
	}
}
