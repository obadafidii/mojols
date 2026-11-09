package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCrashBundleCapturesTheReproduction(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "crashed")
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/hover", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker},
		func(o *Options) { o.CrashDir = filepath.Join(dir, "crashes") })
	h.initialize()
	h.openURI(aURI, "def f():\n    pass\n")
	h.openURI(bURI, "other")
	h.hoverURI(aURI) // crashes the backend while in flight

	var files []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(dir, "crashes", "crash-*.json"))
		time.Sleep(10 * time.Millisecond)
	}
	if len(files) != 1 {
		t.Fatalf("want one crash bundle, got %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var b CrashBundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	if len(b.InFlight) != 1 || b.InFlight[0].Method != "textDocument/hover" || !strings.Contains(string(b.InFlight[0].Params), aURI) {
		t.Fatalf("in-flight requests: %+v", b.InFlight)
	}
	if len(b.Documents) != 2 || !b.Documents[0].Target || b.Documents[0].URI != aURI || b.Documents[0].Text != "def f():\n    pass\n" {
		t.Fatalf("documents (target first, exact text): %+v", b.Documents)
	}
	if !strings.Contains(b.CrashOutput, "Crash handling textDocument/hover") {
		t.Fatalf("crash output: %q", b.CrashOutput)
	}
	if b.Backend.Path == "" || len(b.InitParams) == 0 {
		t.Fatalf("bundle cannot be replayed without the backend command and initialize params: %+v", b.Backend)
	}
}
