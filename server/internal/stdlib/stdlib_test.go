package stdlib

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, c := range []struct{ in, num, commit string }{
		{"Mojo 1.1.0 (8189361e)\n", "1.1.0", "8189361e"},
		{"Mojo 1.3.0.dev2026110805 (abc123)", "1.3.0.dev2026110805", "abc123"},
		{"mojo 0.25.7.0", "", ""},
		{"Mojo 1.2.0", "1.2.0", ""},
	} {
		v, ok := ParseVersion(c.in)
		if c.num == "" {
			if ok {
				t.Errorf("%q: unexpectedly parsed %+v", c.in, v)
			}
			continue
		}
		if !ok || v.Number != c.num || v.Commit != c.commit {
			t.Errorf("%q: got %+v %v", c.in, v, ok)
		}
	}
}

// fakeGitHub serves files (path under Mojo/stdlib/std -> content) for one
// tag, imitating the commits, trees and raw endpoints.
type fakeGitHub struct {
	tag      string
	files    map[string]string
	extra    []treeEntry // extra entries in the listing (for hostile tests)
	failOnce atomic.Int32
	requests atomic.Int32
}

const commitSHA = "0123456789abcdef0123456789abcdef01234567"

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repos/modular/modular/commits/", func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		ref := strings.TrimPrefix(r.URL.Path, "/api/repos/modular/modular/commits/")
		if ref != g.tag {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sha": commitSHA})
	})
	mux.HandleFunc("/api/repos/modular/modular/git/trees/", func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		sha := strings.TrimPrefix(r.URL.Path, "/api/repos/modular/modular/git/trees/")
		var entries []treeEntry
		switch sha {
		case commitSHA:
			entries = []treeEntry{{Path: "Mojo", Type: "tree", SHA: "t-mojo"}, {Path: "max", Type: "tree", SHA: "t-max"}}
		case "t-mojo":
			entries = []treeEntry{{Path: "stdlib", Type: "tree", SHA: "t-stdlib"}}
		case "t-stdlib":
			entries = []treeEntry{{Path: "std", Type: "tree", SHA: "t-std"}, {Path: "test", Type: "tree", SHA: "t-test"}}
		case "t-std":
			for p, c := range g.files {
				entries = append(entries, treeEntry{Path: p, Type: "blob", Size: int64(len(c))})
			}
			entries = append(entries, g.extra...)
			entries = append(entries, treeEntry{Path: "BUILD.bazel", Type: "blob", Size: 10})
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(tree{Tree: entries})
	})
	mux.HandleFunc("/raw/modular/modular/", func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		rest := strings.TrimPrefix(r.URL.Path, "/raw/modular/modular/"+commitSHA+"/Mojo/stdlib/std/")
		if g.failOnce.Add(-1) >= 0 {
			http.Error(w, "flaky", http.StatusBadGateway)
			return
		}
		c, ok := g.files[rest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, c)
	})
	return mux
}

func setup(t *testing.T, g *fakeGitHub) (Options, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	return Options{CacheDir: t.TempDir(), API: srv.URL + "/api", Raw: srv.URL + "/raw", Client: srv.Client()}, srv
}

func TestEnsureDownloadsAndCaches(t *testing.T) {
	g := &fakeGitHub{tag: "mojo/v1.1.0", files: map[string]string{
		"__init__.mojo":             "",
		"collections/__init__.mojo": "from .list import List\n",
		"collections/list.mojo":     "struct List[T: AnyType]:\n    pass\n",
	}}
	g.failOnce.Store(2) // two transient failures are retried
	opts, _ := setup(t, g)
	root, err := Ensure(context.Background(), Version{Number: "1.1.0", Commit: "8189361e"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(string(root), "std", "collections", "list.mojo"))
	if err != nil || !strings.HasPrefix(string(b), "struct List") {
		t.Fatalf("list.mojo: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(string(root), "std", "BUILD.bazel")); err == nil {
		t.Error("non-Mojo files must not be fetched")
	}
	n := g.requests.Load()
	if _, err := Ensure(context.Background(), Version{Number: "1.1.0"}, opts); err != nil {
		t.Fatal(err)
	}
	if g.requests.Load() != n {
		t.Error("a cached version was fetched again")
	}
}

func TestEnsureRefusesHostileListings(t *testing.T) {
	for _, bad := range []treeEntry{
		{Path: "../escape.mojo", Type: "blob", Size: 1},
		{Path: "/abs.mojo", Type: "blob", Size: 1},
		{Path: "huge.mojo", Type: "blob", Size: maxFileBytes + 1},
	} {
		g := &fakeGitHub{tag: "mojo/v1.1.0", files: map[string]string{"__init__.mojo": ""}, extra: []treeEntry{bad}}
		opts, _ := setup(t, g)
		if _, err := Ensure(context.Background(), Version{Number: "1.1.0"}, opts); err == nil {
			t.Errorf("%q: accepted", bad.Path)
		}
		entries, _ := os.ReadDir(opts.CacheDir)
		for _, e := range entries {
			t.Errorf("%q: left %s behind", bad.Path, e.Name())
		}
	}
}

func TestEnsureUnknownVersion(t *testing.T) {
	g := &fakeGitHub{tag: "mojo/v1.1.0", files: map[string]string{"__init__.mojo": ""}}
	opts, _ := setup(t, g)
	if _, err := Ensure(context.Background(), Version{Number: "9.9.9"}, opts); err == nil {
		t.Error("an unknown version must fail")
	}
}

// TestEnsureRealStdlib serves the real stdlib from $MOJOLS_STDLIB (a
// directory holding std/), when set.
func TestEnsureRealStdlib(t *testing.T) {
	src := os.Getenv("MOJOLS_STDLIB")
	if src == "" {
		t.Skip("set MOJOLS_STDLIB to a directory containing std/")
	}
	files := map[string]string{}
	filepath.WalkDir(filepath.Join(src, "std"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mojo") {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(filepath.Join(src, "std"), p)
			files[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	g := &fakeGitHub{tag: "mojo/v1.1.0", files: files}
	opts, _ := setup(t, g)
	root, err := Ensure(context.Background(), Version{Number: "1.1.0"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	filepath.WalkDir(string(root), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".mojo") {
			n++
		}
		return nil
	})
	if n != len(files) {
		t.Errorf("downloaded %d of %d files", n, len(files))
	}
	t.Logf("%d files, %d API/raw requests", n, g.requests.Load())
}
