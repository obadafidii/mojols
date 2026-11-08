package proxy

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func residentDocs(h *harness) []string {
	var out []string
	for u := range h.fakeState().Docs {
		out = append(out, filepath.Base(u))
	}
	sort.Strings(out)
	return out
}

func wantResident(t *testing.T, h *harness, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = residentDocs(h)
		if len(got) == len(want) {
			match := true
			for i := range want {
				if got[i] != want[i] {
					match = false
				}
			}
			if match {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend holds %v, want %v", got, want)
}

const (
	aURI = "file:///ws/a.mojo"
	bURI = "file:///ws/b.mojo"
	cURI = "file:///ws/c.mojo"
)

func TestResidencyCapEvictsLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.MaxResidentDocs = 2 })
	h.initialize()
	h.openURI(aURI, "WARN a") // opened in a burst: evicted before or after its diagnostics arrive
	h.openURI(bURI, "b")
	h.openURI(cURI, "c")
	wantResident(t, h, "b.mojo", "c.mojo")

	// The editor still has a.mojo open, so it must still show a.mojo's
	// warning: the backend's "cleared on close" must not reach it.
	if last := lastDiagnosticsFor(h, aURI); !strings.Contains(last, "fake warning") {
		t.Fatalf("evicted a.mojo should still show its warning; last diagnostics the editor got: %s", last)
	}

	// Touching a.mojo brings it back with its text and evicts the LRU (b).
	if got := hoverText(t, h.hoverURI(aURI)); got != "WARN a" {
		t.Fatalf("hover on evicted document: %q", got)
	}
	wantResident(t, h, "a.mojo", "c.mojo")
	if st := h.status(); st.Evictions != 2 || st.ResidentDocs != 2 || st.OpenDocuments != 3 {
		t.Fatalf("status: %+v", st)
	}
}

// lastDiagnosticsFor returns the params of the last diagnostics message for
// uri the editor received (a fake/state round trip flushes everything).
func lastDiagnosticsFor(h *harness, uri string) string {
	h.fakeState()
	for drained := false; !drained; {
		select {
		case m := <-h.inbox:
			h.backlog = append(h.backlog, m)
		default:
			drained = true
		}
	}
	last := "(none)"
	for _, m := range h.backlog {
		if m.Method == "textDocument/publishDiagnostics" && strings.Contains(string(m.Params), `"uri":"`+uri+`"`) {
			last = string(m.Params)
		}
	}
	return last
}

func TestEditsToEvictedDocumentSurvive(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) { o.MaxResidentDocs = 1 })
	h.initialize()
	h.openURI(aURI, "first")
	h.openURI(bURI, "b")
	wantResident(t, h, "b.mojo")

	h.replaceText(aURI, 2, "second") // edited while not resident in the backend
	wantResident(t, h, "a.mojo")
	if got := hoverText(t, h.hoverURI(aURI)); got != "second" {
		t.Fatalf("backend has %q, want the edit made while evicted", got)
	}
	if got := hoverText(t, h.hoverURI(bURI)); got != "b" {
		t.Fatalf("b after re-residency: %q", got)
	}
}

func TestReplayAfterCrashReopensMostRecent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=fake/boom", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker},
		func(o *Options) { o.MaxResidentDocs = 2 })
	h.initialize()
	h.openURI(aURI, "a")
	h.openURI(bURI, "b")
	h.openURI(cURI, "c")
	hoverText(t, h.hoverURI(aURI)) // a is now the most recent, then c
	wantResident(t, h, "a.mojo", "c.mojo")
	h.notify("fake/boom", nil)
	h.waitStatus("restart", func(st Status) bool { return st.BackendReady && st.BackendGen >= 2 })
	wantResident(t, h, "a.mojo", "c.mojo")
}

func TestRecycleOverMemoryLimitFailsNothing(t *testing.T) {
	var sampled atomic.Int32
	h := newHarness(t, nil, func(o *Options) {
		o.MemoryLimitMB = 100
		o.MemoryCheck = 20 * time.Millisecond
		o.RecycleCooldown = time.Hour
		o.SampleRSS = func(int) (int64, error) {
			if sampled.Add(1) == 5 {
				return 10 << 30, nil // one sample over the limit
			}
			return 50 << 20, nil
		}
	})
	h.initialize()
	h.openURI(aURI, "a")
	firstPID := h.status().BackendPID

	// Keep the editor busy across the recycle: every request must succeed.
	for i := 0; i < 40; i++ {
		if got := hoverText(t, h.hoverURI(aURI)); got != "a" {
			t.Fatalf("hover %d during recycle: %q", i, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := h.waitStatus("recycle", func(st Status) bool { return st.Recycles == 1 && st.BackendReady })
	if st.Restarts != 0 || st.Degraded || st.BackendPID == firstPID {
		t.Fatalf("a recycle must be a fresh process, not a failure: %+v", st)
	}
	wantResident(t, h, "a.mojo")
}

func TestMemorySamplerReadsProcess(t *testing.T) {
	rss, err := SampleRSS(os.Getpid())
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	if rss < 1<<20 {
		t.Fatalf("implausible RSS for this test process: %d bytes", rss)
	}
}
