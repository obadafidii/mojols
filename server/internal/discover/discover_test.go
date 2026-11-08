package discover

import (
	"errors"
	"testing"
)

func fakeEnv(execs map[string]bool, vars map[string]string, path map[string]string) Env {
	return Env{
		Getenv: func(k string) string { return vars[k] },
		LookPath: func(name string) (string, error) {
			if p, ok := path[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		IsExec: func(p string) bool { return execs[p] },
	}
}

func TestOrder(t *testing.T) {
	execs := map[string]bool{
		"/ws/.pixi/envs/default/bin/mojo-lsp-server": true,
		"/usr/bin/mojo-lsp-server":                   true,
		"/home/k/.pixi/bin/mojo-lsp-server":          true,
		"/explicit/mojo-lsp-server":                  true,
	}
	vars := map[string]string{"HOME": "/home/k"}
	path := map[string]string{"mojo-lsp-server": "/usr/bin/mojo-lsp-server"}

	res, err := Find("", []string{"/ws"}, fakeEnv(execs, vars, path))
	if err != nil || res.Source != "workspace .pixi/envs/default/bin" {
		t.Fatalf("workspace env should win: %+v %v", res, err)
	}
	res, err = Find("", []string{"/other"}, fakeEnv(execs, vars, path))
	if err != nil || res.Path != "/usr/bin/mojo-lsp-server" {
		t.Fatalf("PATH next: %+v %v", res, err)
	}
	res, err = Find("", nil, fakeEnv(execs, vars, nil))
	if err != nil || res.Path != "/home/k/.pixi/bin/mojo-lsp-server" {
		t.Fatalf("standard location next: %+v %v", res, err)
	}
	res, err = Find("/explicit/mojo-lsp-server", []string{"/ws"}, fakeEnv(execs, vars, path))
	if err != nil || res.Source != "--backend flag" {
		t.Fatalf("flag wins: %+v %v", res, err)
	}
	vars[EnvBackend] = "/nope"
	if _, err = Find("", []string{"/ws"}, fakeEnv(execs, vars, path)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a bad $%s must fail loudly, not fall through: %v", EnvBackend, err)
	}
}

func TestURIToPath(t *testing.T) {
	p, ok := URIToPath("file:///Users/king/my%20proj")
	if !ok || p != "/Users/king/my proj" {
		t.Fatalf("got %q %v", p, ok)
	}
	if _, ok := URIToPath("untitled:Untitled-1"); ok {
		t.Fatal("non-file URI accepted")
	}
}
