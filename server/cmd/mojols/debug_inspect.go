package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/obadafidii/mojols/server/internal/index"
	"github.com/obadafidii/mojols/server/internal/organize"
	"github.com/obadafidii/mojols/server/internal/syntax"
)

// debugInspect validates the inspections on real files. The workspace is
// the current directory; MOJOLS_ROOTS sets the package roots (default: the
// current directory) and MOJOLS_LIBS adds library roots.
//
//	mojols debug-inspect FILES            unused imports per file
//	mojols debug-inspect -imports FILES   would "add import" find each name the files import?
//	mojols debug-inspect -reimport FILES  rewrite each file: drop its first absolute from-import name, add it back through the top candidate
func debugInspect(args []string) int {
	mode := ""
	if len(args) > 0 && (args[0] == "-imports" || args[0] == "-reimport") {
		mode, args = args[0], args[1:]
	}
	wd, _ := os.Getwd()
	roots := []string{wd}
	if r := os.Getenv("MOJOLS_ROOTS"); r != "" {
		roots = filepath.SplitList(r)
	}
	ix := index.New(roots)
	ix.SetWorkspace([]string{wd})
	if libs := os.Getenv("MOJOLS_LIBS"); libs != "" {
		ix.SetLibraries(filepath.SplitList(libs))
	}
	var first, top5, miss, total int
	for _, path := range args {
		abs, _ := filepath.Abs(path)
		f := ix.File(abs)
		if f == nil {
			continue
		}
		_, err := os.Stat(filepath.Join(filepath.Dir(abs), "__init__.mojo"))
		inPkg := err == nil
		switch mode {
		case "":
			for _, u := range ix.UnusedImports(f, inPkg) {
				p := f.Lines.Position(u.Span.Start, 0)
				fmt.Printf("%s:%d:%d unused %s\n", path, p.Line+1, p.Character, u.Bound)
			}
		case "-imports":
			for _, im := range f.Syn.Imports {
				if !im.From || im.Star || im.Module == "" || im.Module[0] == '.' {
					continue
				}
				for _, n := range im.Names {
					want, ok := ix.DefinitionAt(f, n.NameSpan.Start)
					if !ok || want.IsModule() {
						continue
					}
					total++
					rank := -1
					cands := ix.ImportCandidates(f, n.Name, 5)
					for i, c := range cands {
						if c.Target.Decl == want.Decl {
							rank = i
							break
						}
					}
					switch {
					case rank == 0:
						first++
					case rank > 0:
						top5++
						fmt.Printf("RANK%d %s: %s from %s (first: %s)\n", rank+1, path, n.Name, im.Module, cands[0].Module)
					default:
						miss++
						got := ""
						if len(cands) > 0 {
							got = cands[0].Module
						}
						fmt.Printf("MISS %s: %s from %s (first: %s)\n", path, n.Name, im.Module, got)
					}
				}
			}
		case "-reimport":
			done := false
			for _, im := range f.Syn.Imports {
				if done || !im.From || im.Star || im.Module == "" || im.Module[0] == '.' {
					continue
				}
				for i, n := range im.Names {
					if n.Alias != "" {
						continue
					}
					cands := ix.ImportCandidates(f, n.Name, 1)
					if len(cands) == 0 {
						continue
					}
					e, ok := organize.RemoveNames(f.Syn, im, map[int]bool{i: true})
					if !ok {
						continue
					}
					src := string(f.Syn.Src[:e.Start]) + e.Text + string(f.Syn.Src[e.End:])
					g := syntax.Parse([]byte(src))
					e2, ok := organize.AddImport(g, cands[0].Module, n.Name)
					if !ok {
						continue
					}
					src = src[:e2.Start] + e2.Text + src[e2.End:]
					os.WriteFile(abs, []byte(src), 0o644)
					fmt.Printf("reimported %s: %s (was %s, now %s)\n", path, n.Name, im.Module, cands[0].Module)
					done = true
					break
				}
			}
		}
	}
	if mode == "-imports" {
		fmt.Printf("names %d: first choice %d (%.1f%%), in top 5 %d, missed %d\n", total, first, 100*float64(first)/float64(max(total, 1)), top5, miss)
	}
	return 0
}
