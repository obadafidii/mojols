package discover

import (
	"errors"
	"os"
	"path/filepath"
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

func TestNestedProjectEnvironment(t *testing.T) {
	ws := t.TempDir()
	mk := func(rel string, exec bool) {
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if exec {
			mode = 0o755
		}
		if err := os.WriteFile(p, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	// modular/modular: the Mojo project lives in Mojo/, not at the root.
	mk("Mojo/pixi.toml", false)
	mk("Mojo/.pixi/envs/default/bin/"+Binary, true)
	mk("max/examples/deep/pixi.toml", false) // deeper: loses
	mk("max/examples/deep/.pixi/envs/default/bin/"+Binary, true)
	mk("docs/.pixi/envs/default/bin/"+Binary, true) // no project file: ignored
	env := OSEnv()
	env.Getenv = func(k string) string { return map[string]string{"HOME": t.TempDir()}[k] }
	env.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	res, err := Find("", []string{ws}, env)
	if err != nil || res.Path != filepath.Join(ws, "Mojo/.pixi/envs/default/bin", Binary) || res.Source != "workspace Mojo/.pixi/envs/default/bin" {
		t.Fatalf("nested project: %+v, %v", res, err)
	}
	// An environment at the root still wins.
	mk(".pixi/envs/default/bin/"+Binary, true)
	if res, _ := Find("", []string{ws}, env); res.Source != "workspace .pixi/envs/default/bin" {
		t.Fatalf("root environment: %+v", res)
	}
}

func TestActivatedEnvironment(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin", Binary)
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, nil, 0o755)
	env := OSEnv()
	env.Getenv = func(k string) string { return map[string]string{"CONDA_PREFIX": prefix}[k] }
	env.LookPath = func(string) (string, error) { return "/usr/bin/" + Binary, nil }
	if res, _ := Find("", []string{t.TempDir()}, env); res.Path != bin || res.Source != "$CONDA_PREFIX" {
		t.Fatalf("conda: %+v", res)
	}
}
