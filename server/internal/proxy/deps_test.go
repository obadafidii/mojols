package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
)

// project lays out the two-package project measured on a real toolchain:
// app/main.mojo imports mathlib, whose __init__ re-exports from ops.mojo.
type project struct{ root, src, main, other, init, ops string }

func newProject(t *testing.T) project {
	t.Helper()
	root := t.TempDir()
	p := project{
		root:  root,
		src:   filepath.Join(root, "src"),
		main:  filepath.Join(root, "app", "main.mojo"),
		other: filepath.Join(root, "app", "other.mojo"),
		init:  filepath.Join(root, "src", "mathlib", "__init__.mojo"),
		ops:   filepath.Join(root, "src", "mathlib", "ops.mojo"),
	}
	for path, text := range map[string]string{
		p.main:  "from mathlib import double\n\ndef main():\n    print(double(21))\n",
		p.other: "def unrelated():\n    pass\n",
		p.init:  "from .ops import double\n",
		p.ops:   "def double(x: Int) -> Int:\n    return x * 2\n",
	} {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func uri(path string) string { return "file://" + filepath.ToSlash(path) }

func withIncludes(dirs []string, more func(*Options)) func(*Options) {
	return func(o *Options) {
		inner := o.Resolve
		o.Resolve = func(roots []string) (Command, error) {
			c, err := inner(roots)
			c.IncludeDirs = dirs
			return c, err
		}
		o.RefreshDebounce = 20 * time.Millisecond
		if more != nil {
			more(o)
		}
	}
}

func (h *harness) openFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	h.openURI(uri(path), string(b))
}

func (h *harness) waitOpens(what string, want map[string]int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got map[string]int
	for time.Now().Before(deadline) {
		got = h.fakeState().Opens
		match := true
		for u, n := range want {
			if got[u] != n {
				match = false
			}
		}
		if match {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("%s: opens %v, want %v", what, got, want)
}

func TestSaveRefreshesTransitiveDependents(t *testing.T) {
	p := newProject(t)
	h := newHarness(t, nil, withIncludes([]string{p.src}, nil))
	h.initialize()
	h.openFile(p.main)
	h.openFile(p.other)
	h.openFile(p.ops)
	h.waitOpens("initial opens", map[string]int{uri(p.main): 1, uri(p.other): 1, uri(p.ops): 1})

	// main -> mathlib/__init__ -> ops: saving ops must reach main, not other.
	h.save(uri(p.ops))
	h.waitOpens("after saving ops.mojo", map[string]int{uri(p.main): 2, uri(p.other): 1, uri(p.ops): 1})
	if st := h.status(); st.Refreshes != 1 {
		t.Fatalf("refreshes: %+v", st)
	}
	// The refreshed document still answers with its current text.
	if got := hoverText(t, h.hoverURI(uri(p.main))); got == "" {
		t.Fatal("refreshed document lost its text")
	}
}

func TestDiskChangeRefreshesDependents(t *testing.T) {
	p := newProject(t)
	h := newHarness(t, nil, withIncludes([]string{p.src}, nil))
	h.initialize()
	h.openFile(p.main)
	h.waitOpens("initial open", map[string]int{uri(p.main): 1})

	// A git checkout rewrites __init__.mojo while it is not open.
	h.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": uri(p.init), "type": 2}}})
	h.waitOpens("after the disk change", map[string]int{uri(p.main): 2})
}

func TestNewImportIsFollowed(t *testing.T) {
	p := newProject(t)
	h := newHarness(t, nil, withIncludes([]string{p.src}, nil))
	h.initialize()
	h.openFile(p.other)
	h.waitOpens("initial open", map[string]int{uri(p.other): 1})

	// other.mojo starts importing mathlib (unsaved edit is enough: the graph
	// reads open documents from the editor's text, not from disk).
	h.replaceText(uri(p.other), 2, "from mathlib.ops import double\n")
	h.save(uri(p.ops))
	h.waitOpens("after saving ops.mojo", map[string]int{uri(p.other): 2})
}

func TestRefreshCanBeDisabled(t *testing.T) {
	p := newProject(t)
	h := newHarness(t, nil, withIncludes([]string{p.src}, func(o *Options) { o.NoRefresh = true }))
	h.initialize()
	h.openFile(p.main)
	h.save(uri(p.ops))
	time.Sleep(200 * time.Millisecond)
	if opens := h.fakeState().Opens[uri(p.main)]; opens != 1 {
		t.Fatalf("refreshed with NoRefresh set: %d opens", opens)
	}
}

func TestRegistersFileWatchers(t *testing.T) {
	h := newHarness(t, nil, nil)
	m := h.call("initialize", map[string]any{"rootUri": "file:///ws", "capabilities": map[string]any{
		"workspace": map[string]any{"didChangeWatchedFiles": map[string]any{"dynamicRegistration": true}},
	}})
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	h.notify("initialized", map[string]any{})
	req := h.expect("client/registerCapability", 5*time.Second, func(m *jsonrpc.Message) bool {
		return m.Method == "client/registerCapability"
	})
	resp, _ := jsonrpc.NewResponse(*req.ID, nil)
	h.send(resp) // must be absorbed by mojols, not forwarded to the backend
	if fs := h.fakeState(); len(fs.ClientResponses) != 0 {
		t.Fatalf("registration reply leaked to the backend: %s", fs.ClientResponses)
	}
}
