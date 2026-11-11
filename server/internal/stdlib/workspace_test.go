package stdlib

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInWorkspace(t *testing.T) {
	ws := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(ws, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	// A test mock of std (modular's test/test-packages) is not the stdlib.
	mk("Mojo/test/test-packages/std/__init__.mojo")
	if got := InWorkspace([]string{ws}); got != "" {
		t.Fatalf("mock taken for the stdlib: %s", got)
	}
	for _, f := range []string{"__init__.mojo", "builtin/__init__.mojo", "collections/__init__.mojo", "prelude/__init__.mojo"} {
		mk("Mojo/stdlib/std/" + f)
	}
	want := filepath.Join(ws, "Mojo/stdlib")
	if got := InWorkspace([]string{ws}); got != want {
		t.Fatalf("checkout below: %q, want %q", got, want)
	}
	mk("Mojo/stdlib/test/x.mojo")
	if got := InWorkspace([]string{filepath.Join(ws, "Mojo/stdlib/test")}); got != want {
		t.Fatalf("checkout above: %q, want %q", got, want)
	}
	if got := InWorkspace([]string{t.TempDir()}); got != "" {
		t.Fatalf("none: %q", got)
	}
}
