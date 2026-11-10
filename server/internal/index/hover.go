package index

import (
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// HoverAt describes the declaration named at offset in f, from source: its
// signature (or `var name: Type`) and docstring, as Markdown. ok is false
// when nothing there resolves.
func (ix *Index) HoverAt(f *File, offset int) (string, syntax.Span, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	i := f.Syn.RefAt(offset)
	if i < 0 {
		return "", syntax.Span{}, false
	}
	ix.budget = stepBudget
	t, ok := Final(ix.resolveRef(f, i, 0))
	if !ok {
		return "", syntax.Span{}, false
	}
	if t.IsModule() {
		return "```mojo\nmodule " + moduleName(t.File.Path) + "\n```", f.Syn.Refs[i].Span, true
	}
	return Describe(t.File, t.Decl), f.Syn.Refs[i].Span, true
}

// Describe renders a declaration as Markdown: a Mojo code block with its
// signature, the struct or trait it belongs to, and its docstring.
func Describe(f *File, d *syntax.Decl) string {
	var sig string
	switch d.Kind {
	case syntax.KStruct, syntax.KTrait, syntax.KFunc:
		sig = dedent(f.Syn.Text(d.Header))
	case syntax.KImport:
		sig = strings.TrimSpace(f.Syn.Text(d.Import.Span))
	default:
		kw := map[syntax.DeclKind]string{syntax.KComptime: "comptime ", syntax.KField: "var ", syntax.KVar: "var "}[d.Kind]
		sig = kw + d.Name
		if t := oneLine(f.Syn.Text(d.Type)); t != "" {
			sig += ": " + t
		}
		if v := oneLine(f.Syn.Text(d.Value)); v != "" && (d.Kind == syntax.KComptime || d.Kind == syntax.KParam || d.Kind == syntax.KArg) {
			if len(v) > 120 {
				v = v[:117] + "..."
			}
			sig += " = " + v
		}
	}
	var b strings.Builder
	b.WriteString("```mojo\n" + sig + "\n```")
	if d.Parent != nil && (d.Parent.Kind == syntax.KStruct || d.Parent.Kind == syntax.KTrait) && d.Scope == d.Parent.Body {
		b.WriteString("\n\nIn `" + d.Parent.Kind.String() + " " + d.Parent.Name + "`")
	}
	if doc := docText(f.Syn.Text(d.Doc)); doc != "" {
		b.WriteString("\n\n---\n\n" + doc)
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func moduleName(path string) string {
	path = strings.TrimSuffix(strings.TrimSuffix(path, ".mojo"), "/__init__")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// dedent removes the indentation common to a header's continuation lines.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 1 {
		return s
	}
	min := -1
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if min < 0 || n < min {
			min = n
		}
	}
	base := 0
	if min > 4 {
		base = min - 4
	}
	for k := 1; k < len(lines); k++ {
		if len(lines[k]) >= base {
			lines[k] = lines[k][base:]
		}
	}
	return strings.Join(lines, "\n")
}

// docText turns a docstring literal into Markdown text.
func docText(lit string) string {
	s := strings.TrimSpace(lit)
	for _, q := range []string{`"""`, `'''`, `"`, `'`} {
		if strings.HasPrefix(s, q) && strings.HasSuffix(s, q) && len(s) >= 2*len(q) {
			s = s[len(q) : len(s)-len(q)]
			break
		}
	}
	lines := strings.Split(s, "\n")
	min := -1
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if min < 0 || n < min {
			min = n
		}
	}
	for k := 1; k < len(lines); k++ {
		if min > 0 && len(lines[k]) >= min {
			lines[k] = lines[k][min:]
		}
	}
	lines[0] = strings.TrimSpace(lines[0])
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Outline is a document symbol: a declaration and its members.
type Outline struct {
	Decl     *syntax.Decl
	Children []Outline
}

// OutlineOf lists f's module-level structs, traits, defs and comptime
// declarations, with the members of structs and traits.
func OutlineOf(f *File) []Outline {
	var walk func(s *syntax.Scope) []Outline
	walk = func(s *syntax.Scope) []Outline {
		var out []Outline
		for _, d := range f.Syn.Decls {
			if d.Scope != s {
				continue
			}
			switch d.Kind {
			case syntax.KStruct, syntax.KTrait:
				out = append(out, Outline{Decl: d, Children: walk(d.Body)})
			case syntax.KFunc, syntax.KComptime, syntax.KField:
				out = append(out, Outline{Decl: d})
			}
		}
		return out
	}
	return walk(f.Syn.Module)
}
