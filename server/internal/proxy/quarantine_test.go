package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
)

const (
	goodURI = "file:///ws/a_good.mojo"
	badURI  = "file:///ws/b_bad.mojo"
	lateURI = "file:///ws/c_late.mojo"
)

func (h *harness) openURI(uri, text string) {
	h.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": uri, "languageId": "mojo", "version": 1, "text": text}})
}

func (h *harness) replaceText(uri string, version int, text string) {
	h.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []any{map[string]any{"text": text}},
	})
}

func (h *harness) save(uri string) {
	h.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

func (h *harness) hoverURI(uri string) *jsonrpc.Message {
	h.t.Helper()
	return h.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 0, "character": 0}})
}

func (h *harness) waitStatus(what string, pred func(Status) bool) Status {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.status(); pred(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s: %+v", what, h.status())
	return Status{}
}

func settled(st Status) bool {
	return st.BackendReady && st.Probing == "" && len(st.Suspects) == 0
}

// The real-world case: one file segfaults mojo-lsp-server every time it is
// opened. It must cost that file its features, not the whole session.
func TestPoisonedFileIsQuarantined(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON_TEXT=POISON"}, nil)
	h.initialize()
	h.openURI(goodURI, "fine")
	if got := hoverText(t, h.hoverURI(goodURI)); got != "fine" {
		t.Fatalf("hover: %q", got)
	}
	h.openURI(badURI, "POISON")

	h.expect("quarantine notice", 10*time.Second, func(m *jsonrpc.Message) bool {
		return m.Method == "window/showMessage" && strings.Contains(string(m.Params), "b_bad.mojo") &&
			strings.Contains(string(m.Params), "quarantined")
	})
	h.expect("in-file quarantine warning", 5*time.Second, func(m *jsonrpc.Message) bool {
		return m.Method == "textDocument/publishDiagnostics" && strings.Contains(string(m.Params), badURI) &&
			strings.Contains(string(m.Params), "crashes on this file")
	})
	st := h.waitStatus("settled backend", settled)
	if st.Degraded || st.Restarts != 2 || len(st.Quarantined) != 1 || st.Quarantined[0] != badURI {
		t.Fatalf("want exactly the bad file quarantined after 2 crashes: %+v", st)
	}
	if got := hoverText(t, h.hoverURI(goodURI)); got != "fine" {
		t.Fatalf("good file lost features: %q", got)
	}
	m := h.hoverURI(badURI)
	wantError(t, m, jsonrpc.CodeRequestFailed)
	if !strings.Contains(m.Error.Message, "quarantined") {
		t.Fatalf("error should say why: %q", m.Error.Message)
	}
	if fs := h.fakeState(); len(fs.Docs) != 1 {
		t.Fatalf("quarantined file reached the backend: %+v", fs.Docs)
	}

	// Editing alone changes nothing; saving different content retries it.
	h.replaceText(badURI, 2, "fixed")
	if st := h.status(); len(st.Quarantined) != 1 {
		t.Fatalf("edit without save released the file: %+v", st)
	}
	h.save(badURI)
	st = h.waitStatus("release after save", func(st Status) bool { return settled(st) && len(st.Quarantined) == 0 })
	if st.Restarts != 2 {
		t.Fatalf("release caused a crash: %+v", st)
	}
	if got := hoverText(t, h.hoverURI(badURI)); got != "fixed" {
		t.Fatalf("released file: %q", got)
	}

	// Saving the poison again convicts it again, with one crash.
	h.replaceText(badURI, 3, "POISON again")
	h.waitStatus("re-quarantine", func(st Status) bool { return settled(st) && len(st.Quarantined) == 1 })
	if code := h.exit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// When a crash leaves several documents unparsed, only the guilty one may be
// quarantined: suspects are reopened one at a time.
func TestOnlyTheGuiltySuspectIsQuarantined(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON_TEXT=POISON"}, nil)
	id := h.request("initialize", map[string]any{"rootUri": "file:///ws", "capabilities": map[string]any{}})
	// Opened before the backend is ready, so all three are replayed together.
	h.openURI(goodURI, "good")
	h.openURI(badURI, "POISON")
	h.openURI(lateURI, "late")
	if m := h.response(id); m.Error != nil {
		t.Fatalf("initialize: %v", m.Error)
	}
	h.notify("initialized", map[string]any{})

	st := h.waitStatus("settled backend", func(st Status) bool { return settled(st) && len(st.Quarantined) == 1 })
	if st.Quarantined[0] != badURI || st.Degraded {
		t.Fatalf("wrong verdict: %+v", st)
	}
	for uri, want := range map[string]string{goodURI: "good", lateURI: "late"} {
		if got := hoverText(t, h.hoverURI(uri)); got != want {
			t.Fatalf("%s: %q", uri, got)
		}
	}
}

// A crash caused by a request, not a document, must not quarantine anything.
func TestRequestCrashQuarantinesNothing(t *testing.T) {
	marker := t.TempDir() + "/crashed"
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/hover", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker}, nil)
	h.initialize()
	h.openURI(goodURI, "fine")
	h.fakeState() // the open has been processed
	wantError(t, h.hoverURI(goodURI), jsonrpc.CodeContentModified)
	st := h.waitStatus("restart", func(st Status) bool { return settled(st) && st.BackendGen >= 2 })
	if len(st.Quarantined) != 0 {
		t.Fatalf("innocent file quarantined: %+v", st)
	}
	if got := hoverText(t, h.hoverURI(goodURI)); got != "fine" {
		t.Fatalf("hover after restart: %q", got)
	}
}
