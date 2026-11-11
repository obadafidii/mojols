package stdlib

import (
	"os"
	"path/filepath"
	"strings"
)

// InWorkspace finds a checkout of the stdlib's sources inside the workspace
// (a clone of modular/modular has one at Mojo/stdlib): a directory holding
// a real std package, not a test mock of one. It looks at most three levels
// down, shallowest first, then a few levels up (a folder opened inside a
// checkout), and returns "" if there is none.
func InWorkspace(roots []string) string {
	// A folder inside a checkout (Mojo/stdlib/test opened on its own).
	for _, root := range roots {
		for d, i := filepath.Dir(root), 0; i < 4 && d != filepath.Dir(d); d, i = filepath.Dir(d), i+1 {
			if IsCheckout(d) {
				return d
			}
		}
	}
	for _, root := range roots {
		level := []string{root}
		for depth := 0; depth <= 3 && len(level) > 0; depth++ {
			var next []string
			for _, dir := range level {
				if IsCheckout(dir) {
					return dir
				}
				if depth == 3 {
					continue
				}
				ents, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, e := range ents {
					n := e.Name()
					if e.IsDir() && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "bazel-") && n != "node_modules" && !testDir(n) {
						next = append(next, filepath.Join(dir, n))
					}
				}
				if len(next) > 4000 {
					break
				}
			}
			level = next
		}
	}
	return ""
}

// IsCheckout reports whether dir/std is the stdlib: a package with the
// stdlib's core subpackages.
func IsCheckout(dir string) bool {
	for _, p := range []string{"std/__init__.mojo", "std/builtin", "std/collections", "std/prelude"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			return false
		}
	}
	return true
}

func testDir(n string) bool {
	switch strings.ToLower(n) {
	case "test", "tests", "unittests", "testdata", "inputs", "fixtures":
		return true
	}
	return false
}
