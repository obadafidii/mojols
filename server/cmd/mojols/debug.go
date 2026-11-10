package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/obadafidii/mojols/server/internal/syntax"
	"github.com/obadafidii/mojols/server/internal/text"
)

// debugParse prints mojols' own reading of each file as one JSON object per
// line: declarations and identifier occurrences with UTF-16 positions. It is
// for validating the parser against mojo-lsp-server and for bug reports.
func debugParse(args []string) int {
	enc := json.NewEncoder(os.Stdout)
	status := 0
	for _, path := range args {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mojols:", err)
			status = 1
			continue
		}
		f := syntax.Parse(src)
		lines := text.NewLines(string(src))
		type decl struct {
			Kind     string     `json:"kind"`
			Name     string     `json:"name"`
			Range    text.Range `json:"range"`
			Parent   string     `json:"parent,omitempty"`
			Scope    string     `json:"scope"`
			Implicit bool       `json:"implicit,omitempty"`
		}
		type ref struct {
			Kind  string     `json:"kind"`
			Name  string     `json:"name"`
			Range text.Range `json:"range"`
			Base  int        `json:"base"`
		}
		out := struct {
			File  string `json:"file"`
			Panic string `json:"panic,omitempty"`
			Decls []decl `json:"decls"`
			Refs  []ref  `json:"refs"`
		}{File: path}
		if f.Panic != nil {
			out.Panic = fmt.Sprint(f.Panic)
		}
		scopeNames := map[syntax.ScopeKind]string{syntax.SModule: "module", syntax.SStruct: "struct", syntax.SFunc: "def", syntax.SBlock: "block", syntax.SComp: "expr"}
		for _, d := range f.Decls {
			x := decl{Kind: d.Kind.String(), Name: d.Name, Range: lines.Range(d.NameSpan.Start, d.NameSpan.End, text.UTF16),
				Scope: scopeNames[d.Scope.Kind], Implicit: d.Implicit}
			if d.Parent != nil {
				x.Parent = d.Parent.Name
			}
			out.Decls = append(out.Decls, x)
		}
		for _, r := range f.Refs {
			out.Refs = append(out.Refs, ref{Kind: r.Kind.String(), Name: r.Name, Range: lines.Range(r.Span.Start, r.Span.End, text.UTF16), Base: r.Base})
		}
		enc.Encode(out)
	}
	return status
}
