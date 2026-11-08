package roots

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func touch(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func rel(t *testing.T, base string, dirs []string) []string {
	out := []string{}
	for _, d := range dirs {
		r, err := filepath.Rel(base, d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, filepath.ToSlash(r))
	}
	return out
}

func TestInfer(t *testing.T) {
	ws := t.TempDir()
	touch(t, ws,
		"src/mathlib/__init__.mojo", // the pkgproj layout measured on a real toolchain
		"src/mathlib/ops.mojo",
		"src/mathlib/sub/__init__.mojo", // subpackage: no extra root
		"app/main.mojo",
		"toppkg/__init__.mojo",                              // package at the workspace top: root is ws itself
		"libs/vendor.mojopkg",                               // prebuilt package
		"third_party/modular/Mojo/stdlib/std/__init__.mojo", // stdlib copy: must not shadow
		".pixi/envs/default/lib/x/__init__.mojo",            // environments are ignored
		"node_modules/y/__init__.mojo",
		"bazel-out/z/__init__.mojo",
	)
	res := Infer([]string{ws}, Options{})
	if got, want := rel(t, ws, res.Roots), []string{".", "libs", "src"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roots %v, want %v", got, want)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "std") ||
		!strings.HasSuffix(filepath.ToSlash(res.Skipped[0].Dir), "Mojo/stdlib") {
		t.Fatalf("stdlib copy not skipped: %+v", res.Skipped)
	}
	if res.Truncated {
		t.Fatal("small tree reported as truncated")
	}
}

func TestWorkspaceIsAPackage(t *testing.T) {
	base := t.TempDir()
	touch(t, base, "src/mathlib/__init__.mojo", "src/mathlib/inner/__init__.mojo")
	res := Infer([]string{filepath.Join(base, "src/mathlib/inner")}, Options{})
	if got := rel(t, base, res.Roots); !reflect.DeepEqual(got, []string{"src"}) {
		t.Fatalf("opening a package directory should root above its outermost package: %v", got)
	}
}

func TestCaps(t *testing.T) {
	ws := t.TempDir()
	touch(t, ws, "a/b/c/d/pkg/__init__.mojo")
	if res := Infer([]string{ws}, Options{MaxDepth: 3}); len(res.Roots) != 0 {
		t.Fatalf("package below MaxDepth found: %v", res.Roots)
	}
	for i := 0; i < 40; i++ {
		touch(t, ws, "many/"+string(rune('a'+i%26))+strings.Repeat("x", i/26)+"/p/__init__.mojo")
	}
	res := Infer([]string{ws}, Options{MaxRoots: 5})
	if len(res.Roots) != 5 || !res.Truncated {
		t.Fatalf("MaxRoots not enforced: %d roots, truncated=%v", len(res.Roots), res.Truncated)
	}
	if res := Infer([]string{ws}, Options{MaxEntries: 10}); !res.Truncated {
		t.Fatal("MaxEntries not enforced")
	}
}

func TestMissingWorkspace(t *testing.T) {
	res := Infer([]string{filepath.Join(t.TempDir(), "gone")}, Options{})
	if len(res.Roots) != 0 {
		t.Fatalf("roots from a missing dir: %v", res.Roots)
	}
}
