package roots

import (
	"fmt"
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
		name := fmt.Sprintf("d%02d", i)
		touch(t, ws, "many/"+name+"/pkg_"+name+"/__init__.mojo")
	}
	res := Infer([]string{ws}, Options{MaxRoots: 5})
	if len(res.Roots) != 5 || !res.Truncated || res.Limit != "roots" {
		t.Fatalf("MaxRoots not enforced: %d roots, truncated=%v, limit=%q", len(res.Roots), res.Truncated, res.Limit)
	}
	if res := Infer([]string{ws}, Options{MaxEntries: 10}); !res.Truncated || res.Limit != "entries" {
		t.Fatalf("MaxEntries not enforced: %+v", res)
	}
}

func TestMissingWorkspace(t *testing.T) {
	res := Infer([]string{filepath.Join(t.TempDir(), "gone")}, Options{})
	if len(res.Roots) != 0 {
		t.Fatalf("roots from a missing dir: %v", res.Roots)
	}
}

// Shapes seen in modular/modular: fixture trees full of packages with
// deliberately clashing names must not become include dirs.
func TestFixturesAndDuplicates(t *testing.T) {
	ws := t.TempDir()
	touch(t, ws,
		"src/mathlib/__init__.mojo",
		"test/helpers/__init__.mojo",               // a real test helper package: kept
		"test/import/inputs/mathlib/__init__.mojo", // fixture: never scanned
		"test/run/Inputs/clash/__init__.mojo",      // case-insensitive
		"graph/testdata/g/__init__.mojo",           // fixture
		"tools/node/test/data/pkg/__init__.mojo",   // data under a test dir: fixture
		"examples/a/mathlib/__init__.mojo",         // only re-provides mathlib: dropped
		"examples/b/mathlib/__init__.mojo",         // same
		"examples/c/mathlib/__init__.mojo",         // mathlib again ...
		"examples/c/extra/__init__.mojo",           // ... plus something new: kept
	)
	res := Infer([]string{ws}, Options{})
	if got, want := rel(t, ws, res.Roots), []string{"src", "test", "examples/c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("roots %v, want %v (skipped %+v)", got, want, res.Skipped)
	}
	if res.Fixtures != 4 {
		t.Fatalf("fixture dirs skipped: %d, want 4", res.Fixtures)
	}
	dropped := 0
	for _, sk := range res.Skipped {
		if strings.Contains(sk.Reason, "already provided by") {
			dropped++
		}
	}
	if dropped != 2 || res.Truncated {
		t.Fatalf("want examples/a and examples/b dropped as redundant: %+v", res.Skipped)
	}
}
