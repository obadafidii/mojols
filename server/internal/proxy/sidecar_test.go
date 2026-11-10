package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
)

func (h *harness) complete(uri string, line, char int) *jsonrpc.Message {
	h.t.Helper()
	return h.call("textDocument/completion", map[string]any{"textDocument": map[string]any{"uri": uri},
		"position": map[string]any{"line": line, "character": char}})
}

func completionLabels(t *testing.T, m *jsonrpc.Message) (labels []string, incomplete bool) {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("completion failed: %v", m.Error)
	}
	var r struct {
		IsIncomplete bool `json:"isIncomplete"`
		Items        []struct {
			Label string `json:"label"`
		} `json:"items"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatalf("completion result %s: %v", m.Result, err)
	}
	for _, it := range r.Items {
		labels = append(labels, it.Label)
	}
	return labels, r.IsIncomplete
}

func isolated(o *Options) { o.IsolateCompletion = true }

func TestCompletionRunsInTheSidecarWithCurrentText(t *testing.T) {
	h := newHarness(t, nil, isolated)
	h.initialize()
	h.openURI(aURI, "first")
	if l, _ := completionLabels(t, h.complete(aURI, 0, 1)); len(l) != 1 || l[0] != "fake:first" {
		t.Fatalf("completion: %v", l)
	}
	h.replaceText(aURI, 2, "second")
	if l, _ := completionLabels(t, h.complete(aURI, 0, 1)); len(l) != 1 || l[0] != "fake:second" {
		t.Fatalf("completion after an edit must see the new text: %v", l)
	}
	st := h.status()
	if st.Completion == nil || !st.Completion.Running || st.Completion.Docs != 1 {
		t.Fatalf("sidecar status: %+v", st.Completion)
	}
	// The sidecar's diagnostics must not reach the editor: the main backend
	// sends two for a.mojo (open, edit); the sidecar's two must be dropped.
	lastDiagnosticsFor(h, aURI) // flushes everything into the backlog
	n := 0
	for _, m := range h.backlog {
		if m.Method == "textDocument/publishDiagnostics" && strings.Contains(string(m.Params), `"uri":"`+aURI+`"`) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("editor got %d diagnostics messages for a.mojo, want 2 (the sidecar's must be dropped)", n)
	}
}

func TestCompletionCrashLeavesTheMainBackendAlone(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/completion"}, isolated)
	h.initialize()
	h.openURI(aURI, "a")
	mainPID := h.status().BackendPID

	labels, incomplete := completionLabels(t, h.complete(aURI, 0, 0))
	if len(labels) != 0 || !incomplete {
		t.Fatalf("a crashed completion should be an empty, incomplete list: %v %v", labels, incomplete)
	}
	st := h.status()
	if st.Restarts != 0 || st.BackendPID != mainPID || st.Completion.Crashes != 1 {
		t.Fatalf("main backend must not notice a completion crash: %+v completion %+v", st, st.Completion)
	}
	if got := hoverText(t, h.hoverURI(aURI)); got != "a" {
		t.Fatalf("hover after a completion crash: %q", got)
	}

	// The same completion again is answered from the memo, without a crash.
	completionLabels(t, h.complete(aURI, 0, 0))
	if c := h.status().Completion; c.Crashes != 1 || c.MemoHits != 1 {
		t.Fatalf("memo did not prevent a repeat crash: %+v", c)
	}
}

func TestRepeatedCompletionCrashesPause(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/completion"}, isolated)
	h.initialize()
	h.openURI(aURI, "a")
	for i := 0; i < sidecarCrashLimit; i++ {
		completionLabels(t, h.complete(aURI, 0, i)) // new positions: no memo
	}
	c := h.status().Completion
	if !c.Paused || c.Crashes != sidecarCrashLimit {
		t.Fatalf("want completion paused after %d crashes: %+v", sidecarCrashLimit, c)
	}
	completionLabels(t, h.complete(aURI, 0, 99))
	if c := h.status().Completion; c.Crashes != sidecarCrashLimit || c.Running {
		t.Fatalf("a paused sidecar must not restart: %+v", c)
	}
	if st := h.status(); st.Restarts != 0 {
		t.Fatalf("main backend restarted: %+v", st)
	}
}

func TestSidecarHoldsFewDocuments(t *testing.T) {
	h := newHarness(t, nil, isolated)
	h.initialize()
	for i, uri := range []string{aURI, bURI, cURI, "file:///ws/d.mojo", "file:///ws/e.mojo"} {
		h.openURI(uri, string(rune('a'+i)))
		completionLabels(t, h.complete(uri, 0, 0))
	}
	if c := h.status().Completion; c.Docs != sidecarResident {
		t.Fatalf("sidecar holds %d documents, want %d", c.Docs, sidecarResident)
	}
}

func TestClosingADocumentClosesItInTheSidecar(t *testing.T) {
	h := newHarness(t, nil, isolated)
	h.initialize()
	h.openURI(aURI, "x")
	completionLabels(t, h.complete(aURI, 0, 0)) // the sidecar now holds a.mojo
	h.replaceText(aURI, 2, "edited")            // not mirrored: synced on the next request
	if l, _ := completionLabels(t, h.complete(aURI, 0, 0)); len(l) != 1 || l[0] != "fake:edited" {
		t.Fatalf("completion after an edit: %v", l)
	}
	h.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": aURI}})
	h.waitStatus("closed in the sidecar too", func(st Status) bool { return st.Completion.Docs == 0 })
}

func TestContentModifiedIsRetriedOnce(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_MODIFIED_ONCE_ON=textDocument/completion"}, isolated)
	h.initialize()
	h.openURI(aURI, "x")
	if l, _ := completionLabels(t, h.complete(aURI, 0, 0)); len(l) != 1 {
		t.Fatalf("the retry should have produced results: %v", l)
	}
	if c := h.status().Completion; c.Retries != 1 {
		t.Fatalf("retries: %+v", c)
	}
}
