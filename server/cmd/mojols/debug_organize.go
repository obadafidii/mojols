package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/obadafidii/mojols/server/internal/index"
	"github.com/obadafidii/mojols/server/internal/organize"
	"github.com/obadafidii/mojols/server/internal/syntax"
)

// debugOrganize prints each file organized (to stdout with -w=false, or
// rewritten in place with -w), for validating organize imports.
func debugOrganize(args []string) int {
	write := len(args) > 0 && args[0] == "-w"
	if write {
		args = args[1:]
	}
	// The workspace is the current directory: re-exports are checked
	// against every file under it.
	wd, _ := os.Getwd()
	ix := index.New(nil)
	ix.SetWorkspace([]string{wd})
	for _, path := range args {
		abs, _ := filepath.Abs(path)
		src, err := os.ReadFile(abs)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		f := ix.File(abs)
		var elsewhere func(string) bool
		if _, err := os.Stat(filepath.Join(filepath.Dir(abs), "__init__.mojo")); err != nil {
			elsewhere = func(bound string) bool { return ix.ImportedElsewhere(f, bound) }
		}
		e, ok := organize.Organize(syntax.Parse(src), abs, elsewhere)
		if !ok {
			continue
		}
		out := string(src[:e.Start]) + e.Text + string(src[e.End:])
		if write {
			os.WriteFile(abs, []byte(out), 0o644)
			ix.Changed(abs)
		}
		fmt.Println("changed", path)
	}
	return 0
}
