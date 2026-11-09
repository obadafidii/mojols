package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tdadadavid/mojols/server/internal/index"
	"github.com/tdadadavid/mojols/server/internal/syntax"
	"github.com/tdadadavid/mojols/server/internal/text"
)

// debugResolve prints, for every identifier occurrence in each file, the
// chain of declarations mojols resolves it to (one JSON object per file).
// Each location is [path, line, char] (UTF-16) of the declared name; a
// module is [path, -1, -1]. `overloads` lists the other declarations with
// the final one's name in the same scope.
func debugResolve(args []string) int {
	fs := flag.NewFlagSet("debug-resolve", flag.ContinueOnError)
	var roots multiFlag
	fs.Var(&roots, "I", "package root (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var abs []string
	for _, r := range roots {
		if a, err := filepath.Abs(r); err == nil {
			abs = append(abs, a)
		}
	}
	ix := index.New(abs)
	enc := json.NewEncoder(os.Stdout)
	loc := func(t index.Target) []any {
		if t.IsModule() {
			return []any{t.File.Path, -1, -1}
		}
		p := t.File.Lines.Position(t.Decl.NameSpan.Start, text.UTF16)
		return []any{t.File.Path, p.Line, p.Character}
	}
	for _, path := range fs.Args() {
		a, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		f := ix.File(a)
		if f == nil {
			fmt.Fprintln(os.Stderr, "mojols: cannot read", path)
			continue
		}
		type row struct {
			Line      uint32  `json:"line"`
			Char      uint32  `json:"char"`
			Name      string  `json:"name"`
			Kind      string  `json:"kind"`
			Chain     [][]any `json:"chain"`
			Overloads [][]any `json:"overloads,omitempty"`
			// Owners are the lines of the defs that arguments and
			// captures on the chain belong to.
			Owners []int `json:"owners,omitempty"`
			// Final describes the declaration resolved to: its kind,
			// and "trait-member" for a member declared in a trait.
			Final string `json:"final,omitempty"`
		}
		var rows []row
		for i, r := range f.Syn.Refs {
			if r.Kind == syntax.RModule {
				continue
			}
			p := f.Lines.Position(r.Span.Start, text.UTF16)
			x := row{Line: p.Line, Char: p.Character, Name: r.Name, Kind: r.Kind.String()}
			chain := ix.Resolve(f, i)
			for _, t := range chain {
				x.Chain = append(x.Chain, loc(t))
			}
			if last, ok := index.Final(chain); ok {
				switch {
				case last.IsModule():
					x.Final = "module"
				case last.Decl.Parent != nil && last.Decl.Parent.Kind == syntax.KTrait && last.Decl.Scope == last.Decl.Parent.Body:
					x.Final = "trait-member"
				case last.Decl.Kind == syntax.KVar && last.Decl.Scope != nil && last.Decl.Scope.Kind == syntax.SBlock:
					x.Final = "block-var"
				default:
					x.Final = last.Decl.Kind.String()
				}
			}
			for _, t := range chain {
				if !t.IsModule() && (t.Decl.Kind == syntax.KArg || t.Decl.Capture) && t.Decl.Parent != nil {
					x.Owners = append(x.Owners, int(t.File.Lines.Position(t.Decl.Parent.Span.Start, text.UTF16).Line))
				}
			}
			// Closures crossed between the use and an outer variable it
			// captures implicitly (`{mut}`): the server reports those at
			// the closure's def line.
			if last, ok := index.Final(chain); ok && !last.IsModule() && last.File == f && last.Decl.Scope != nil {
				declFunc := last.Decl.Scope.Func()
				for sc := r.Scope; sc != nil && sc != declFunc; sc = sc.Parent {
					if sc.Kind == syntax.SFunc && sc.Owner != nil {
						x.Owners = append(x.Owners, int(f.Lines.Position(sc.Owner.Span.Start, text.UTF16).Line))
					}
				}
			}
			if last, ok := index.Final(chain); ok && !last.IsModule() && last.Decl.Scope != nil {
				for _, o := range last.Decl.Scope.Names[last.Decl.Name] {
					if o != last.Decl {
						x.Overloads = append(x.Overloads, loc(index.Target{File: last.File, Decl: o}))
					}
				}
			}
			rows = append(rows, x)
		}
		enc.Encode(map[string]any{"file": a, "refs": rows})
	}
	return 0
}
