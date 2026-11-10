package organize

import (
	"sort"
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// RemoveNames returns the edit removing the names at the given indices of
// im.Names (or the whole statement, for a plain import or when every name
// goes). ok is false when the statement cannot be rewritten safely: it
// holds a comment, or it is the only statement of an indented block.
func RemoveNames(f *syntax.File, im *syntax.Import, drop map[int]bool) (Edit, bool) {
	src := f.Src
	if im.Span.Start < 0 || im.Span.End > len(src) || hasComment(f, im.Span.Start, im.Span.End) {
		return Edit{}, false
	}
	var keep []name
	if im.From {
		for i, n := range im.Names {
			if !drop[i] {
				keep = append(keep, name{n.Name, n.Alias})
			}
		}
	}
	top := atTopLevel(src, im.Span.Start)
	if len(keep) == 0 && !im.Star {
		if !top {
			return Edit{}, false
		}
		// The whole line, with its line break.
		end := im.Span.End
		for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
			end++
		}
		if end < len(src) && src[end] == '\r' {
			end++
		}
		if end < len(src) && src[end] == '\n' {
			end++
		} else if end < len(src) {
			return Edit{}, false // something else on the line
		}
		return Edit{Start: im.Span.Start, End: end}, true
	}
	if !im.From {
		return Edit{}, false
	}
	s := &stmt{from: true, module: im.Module, names: keep, star: im.Star}
	text := render(s)
	if !top {
		text = renderLine(s)
	}
	return Edit{Start: im.Span.Start, End: im.Span.End, Text: eol(src, text)}, true
}

// AddImport returns the edit that makes `name` importable in f as
// `from module import name`: added to an existing top-level import of the
// module, or as a new statement placed with its group (std, other
// absolute, relative) in the leading import block, or after the module
// docstring when the file has no imports.
func AddImport(f *syntax.File, module, nm string) (Edit, bool) {
	src := f.Src
	if module == "" || nm == "" {
		return Edit{}, false
	}
	var block []*syntax.Import
	for _, im := range f.Imports {
		if im.Span.Start < 0 || !atTopLevel(src, im.Span.Start) {
			continue
		}
		if im.From && !im.Star && im.Module == module && !hasComment(f, im.Span.Start, im.Span.End) {
			names := make([]name, 0, len(im.Names)+1)
			for _, n := range im.Names {
				if n.Name == nm && n.Alias == "" {
					return Edit{}, false // already imported
				}
				names = append(names, name{n.Name, n.Alias})
			}
			sorted := sort.SliceIsSorted(names, func(a, b int) bool { return nameLess(names[a].name, names[b].name) })
			names = append(names, name{name: nm})
			if sorted {
				sort.SliceStable(names, func(a, b int) bool { return nameLess(names[a].name, names[b].name) })
			}
			text := render(&stmt{from: true, module: module, names: names})
			return Edit{Start: im.Span.Start, End: im.Span.End, Text: eol(src, text)}, true
		}
		// The leading block: imports separated only by blank lines.
		if len(block) == 0 || onlyBlank(src[block[len(block)-1].Span.End:im.Span.Start]) {
			if len(block) > 0 || leading(f, im.Span.Start) {
				block = append(block, im)
			}
		}
	}
	line := "from " + module + " import " + nm
	g := group(module)
	if len(block) == 0 {
		at := afterHeader(src)
		text := line + "\n"
		if at < len(src) && src[at] != '\n' && src[at] != '\r' {
			text += "\n"
		}
		if at > 0 && !blankBefore(src, at) {
			text = "\n" + text
		}
		return Edit{Start: at, End: at, Text: eol(src, text)}, true
	}
	// Among the statements of its group, keep module order if there is one.
	for _, im := range block {
		if group(im.Module) == g && im.Module > module {
			at := im.Span.Start
			return Edit{Start: at, End: at, Text: eol(src, line+"\n")}, true
		}
	}
	last := -1
	for i, im := range block {
		if group(im.Module) <= g {
			last = i
		}
	}
	if last < 0 {
		at := block[0].Span.Start
		return Edit{Start: at, End: at, Text: eol(src, line+"\n\n")}, true
	}
	at := block[last].Span.End
	text := "\n" + line
	if group(block[last].Module) != g {
		text = "\n" + text
	}
	return Edit{Start: at, End: at, Text: eol(src, text)}, true
}

// leading reports whether nothing but comments, blank lines and the
// module docstring comes before off.
func leading(f *syntax.File, off int) bool {
	for _, r := range f.Refs {
		return r.Span.Start >= off
	}
	return true
}

// afterHeader is the offset just after the leading comment lines and the
// module docstring: where a first import goes.
func afterHeader(src []byte) int {
	lx := syntax.Lex(src)
	for _, t := range lx.Tokens {
		switch t.Kind {
		case syntax.Newline, syntax.Indent, syntax.Dedent:
			continue
		case syntax.String:
			if lineStart(src, t.Start) == t.Start {
				return nextLine(src, t.End)
			}
		}
		break
	}
	// No docstring: after the leading comment block (a license header).
	end := 0
	for _, c := range lx.Comments {
		if lineStart(src, c.Start) != c.Start || !onlyBlank(src[end:c.Start]) {
			break
		}
		end = nextLine(src, c.End)
	}
	return end
}

func nextLine(src []byte, off int) int {
	for off < len(src) && src[off] != '\n' {
		off++
	}
	if off < len(src) {
		off++
	}
	return off
}

// blankBefore reports whether the line before off is empty.
func blankBefore(src []byte, off int) bool {
	if off < 2 {
		return off == 0
	}
	i := off - 2 // src[off-1] is the line break ending the previous line
	if src[i] == '\r' && i > 0 {
		i--
	}
	return src[i] == '\n'
}

// renderLine renders s on one line, whatever its length.
func renderLine(s *stmt) string {
	var parts []string
	if s.star {
		parts = append(parts, "*")
	}
	for _, n := range s.names {
		parts = append(parts, n.String())
	}
	return "from " + s.module + " import " + strings.Join(parts, ", ")
}

func eol(src []byte, text string) string {
	if strings.Contains(string(src), "\r\n") {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}
