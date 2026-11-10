package proxy

import (
	"fmt"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"path/filepath"
	"testing"
	"time"
)

// editThenHover types into a document and hovers immediately after each
// keystroke, the pattern that crashed the official server in the soak.
func editThenHover(t *testing.T, h *harness, n int) {
	t.Helper()
	text := "x"
	for i := 0; i < n; i++ {
		text += fmt.Sprint(i % 10)
		h.replaceText(aURI, i+2, text)
		if got := hoverText(t, h.hoverURI(aURI)); got != text {
			t.Fatalf("hover %d answered for stale text %q, want %q", i, got, text)
		}
	}
}

func TestSettledRequestsNeverOverlapAParse(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_PARSE_MS=60"}, func(o *Options) { o.SettleRequests = true })
	h.initialize()
	h.openURI(aURI, "x")
	editThenHover(t, h, 15)
	if fs := h.fakeState(); fs.Overlaps != 0 {
		t.Fatalf("%d requests reached the backend while their document was being parsed", fs.Overlaps)
	}
	if st := h.status(); st.HeldRequests == 0 {
		t.Fatalf("nothing was held: %+v", st)
	}
}

// The control: without settling, the same session does overlap parses.
func TestWithoutSettlingRequestsOverlap(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_PARSE_MS=60"}, nil)
	h.initialize()
	h.openURI(aURI, "x")
	text := "x"
	for i := 0; i < 15; i++ {
		text += "y"
		h.replaceText(aURI, i+2, text)
		h.hoverURI(aURI)
	}
	if fs := h.fakeState(); fs.Overlaps == 0 {
		t.Fatal("control saw no overlap: the fake's parse window is not being exercised")
	}
}

func TestSettleTimeoutReleasesRequests(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_PARSE_MS=3000"}, func(o *Options) {
		o.SettleRequests = true
		o.SettleTimeout = 150 * time.Millisecond
	})
	h.initialize()
	h.openURI(aURI, "x")
	h.replaceText(aURI, 2, "xy")
	start := time.Now()
	if got := hoverText(t, h.hoverURI(aURI)); got != "xy" {
		t.Fatalf("hover: %q", got)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("held for %v; the settle timeout is 150ms", took)
	}
}

func TestHeldRequestSurvivesACrash(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	h := newHarness(t, []string{"MOJOLS_FAKE_PARSE_MS=1500", "MOJOLS_FAKE_CRASH_ON=fake/boom", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker},
		func(o *Options) {
			o.SettleRequests = true
			o.SettleTimeout = 5 * time.Second
		})
	h.initialize()
	h.openURI(aURI, "x")
	h.replaceText(aURI, 2, "xy")
	id := h.request("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": aURI}, "position": map[string]any{"line": 0, "character": 0}})
	time.Sleep(50 * time.Millisecond) // the hover is now held
	h.notify("fake/boom", nil)

	// The document had not finished parsing, so after the crash it is a
	// suspect, reopened alone. The held hover must get an answer, never
	// "unknown document": the text, or ContentModified while it is re-checked.
	m := h.response(id)
	if m.Error != nil && m.Error.Code != jsonrpc.CodeContentModified {
		t.Fatalf("held hover after the crash: %v", m.Error)
	}
	h.waitStatus("re-check finished", settled)
	if got := hoverText(t, h.hoverURI(aURI)); got != "xy" {
		t.Fatalf("hover after the re-check: %q", got)
	}
}

// Requests queued during a restart can target more documents than the
// replay reopens (the residency cap): each must reopen its document, not
// reach the backend unopened.
func TestQueuedRequestReopensEvictedDocument(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=fake/boom", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker},
		func(o *Options) {
			o.MaxResidentDocs = 1
			o.BackoffMin = 400 * time.Millisecond // a wide restart window to queue into
		})
	h.initialize()
	h.openURI(aURI, "a")
	h.openURI(bURI, "b") // a is evicted; the replay will reopen only b
	h.fakeState()
	h.notify("fake/boom", nil)
	h.waitStatus("crash noticed", func(st Status) bool { return !st.BackendReady })
	// Two requests queue for two documents; the replay reopens only one.
	pos := map[string]any{"line": 0, "character": 0}
	ida := h.request("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": aURI}, "position": pos})
	idb := h.request("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": bURI}, "position": pos})
	if got := hoverText(t, h.response(ida)); got != "a" {
		t.Fatalf("hover on a: %q", got)
	}
	if got := hoverText(t, h.response(idb)); got != "b" {
		t.Fatalf("hover on b: %q", got)
	}
}
