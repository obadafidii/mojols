// Package organize rewrites a Mojo file's leading import block: unused
// names removed, imports of the same module merged, names and statements
// sorted and grouped (std, then other absolute imports, then relative
// ones), long statements wrapped the way `mojo format` wraps them.
//
// It is conservative, because editors run it on save: a block with
// comments in it, code between imports, or imports it cannot read is left
// alone; a name is "unused" only if it appears nowhere else in the file;
// package __init__ files keep every import (they re-export).
package organize

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// LineLength is where statements are wrapped, as `mojo format` does.
const LineLength = 80

// Edit replaces Src[Start:End] with Text.
type Edit struct {
	Start, End int
	Text       string
}

type name struct {
	name, alias string
}

func (n name) String() string {
	if n.alias != "" {
		return n.name + " as " + n.alias
	}
	return n.name
}

// bound is the name a binding introduces into the file.
func (n name) bound() string {
	if n.alias != "" {
		return n.alias
	}
	return n.name
}

type stmt struct {
	from   bool
	module string
	names  []name // from-imports
	alias  string // plain `import a.b as c`
	star   bool
}

// Organize returns the edit organizing f's leading imports, or ok false
// if there is nothing to do or the block is not safe to rewrite.
//
// A name unused in the file is removed only if importedElsewhere (given
// the name the import binds) says no other module imports it from this
// one: in Mojo every imported name can be re-exported. With a nil
// importedElsewhere nothing is removed.
func Organize(f *syntax.File, path string, importedElsewhere func(bound string) bool) (Edit, bool) {
	src := f.Src
	var block []*syntax.Import
	for _, im := range f.Imports {
		if im.Span.Start < 0 || !atTopLevel(src, im.Span.Start) {
			break
		}
		if len(block) > 0 && !onlyBlank(src[block[len(block)-1].Span.End:im.Span.Start]) {
			break // code or a comment in between: stop the block here
		}
		block = append(block, im)
	}
	if len(block) == 0 {
		return Edit{}, false
	}
	// Only a leading block: nothing but the module docstring and comments
	// may come before it.
	for _, r := range f.Refs {
		if r.Span.Start < block[0].Span.Start {
			return Edit{}, false
		}
		break
	}
	start, end := lineStart(src, block[0].Span.Start), block[len(block)-1].Span.End
	if hasComment(f, start, end) {
		return Edit{}, false
	}
	keepAll := filepath.Base(path) == "__init__.mojo" || importedElsewhere == nil
	localUse := usedNames(f, start, end)
	used := map[string]bool{}
	isUsed := func(bound string) bool {
		if v, ok := used[bound]; ok {
			return v
		}
		v := localUse[bound] || importedElsewhere(bound)
		used[bound] = v
		return v
	}

	var stmts []*stmt
	fromIdx := map[string]*stmt{}
	for _, im := range block {
		if im.Module == "" {
			return Edit{}, false // unreadable
		}
		if !im.From {
			bound := im.Alias
			if bound == "" {
				bound = strings.Split(im.Module, ".")[0]
			}
			if !keepAll && !isUsed(bound) {
				continue
			}
			dup := false
			for _, s := range stmts {
				if !s.from && s.module == im.Module && s.alias == im.Alias {
					dup = true
				}
			}
			if !dup {
				stmts = append(stmts, &stmt{module: im.Module, alias: im.Alias})
			}
			continue
		}
		s := fromIdx[im.Module]
		if s == nil {
			s = &stmt{from: true, module: im.Module}
			fromIdx[im.Module] = s
			stmts = append(stmts, s)
		}
		if im.Star {
			s.star = true
		}
		for _, n := range im.Names {
			nm := name{n.Name, n.Alias}
			if !keepAll && !isUsed(nm.bound()) {
				continue
			}
			if !containsName(s.names, nm) {
				s.names = append(s.names, nm)
			}
		}
	}
	var kept []*stmt
	for _, s := range stmts {
		if s.from && !s.star && len(s.names) == 0 {
			continue // every name unused
		}
		sort.SliceStable(s.names, func(a, b int) bool { return nameLess(s.names[a].name, s.names[b].name) })
		kept = append(kept, s)
	}
	sort.SliceStable(kept, func(a, b int) bool {
		ga, gb := group(kept[a].module), group(kept[b].module)
		if ga != gb {
			return ga < gb
		}
		if kept[a].module != kept[b].module {
			return kept[a].module < kept[b].module
		}
		return !kept[a].from && kept[b].from
	})

	var out strings.Builder
	for i, s := range kept {
		if i > 0 && group(kept[i-1].module) != group(s.module) {
			out.WriteString("\n")
		}
		out.WriteString(render(s))
		if i < len(kept)-1 {
			out.WriteString("\n")
		}
	}
	text := out.String()
	if strings.Contains(string(src[start:end]), "\r\n") {
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	if text == string(src[start:end]) {
		return Edit{}, false
	}
	if text == "" {
		// Everything was unused: remove the block and the line break after it.
		if end < len(src) && src[end] == '\r' {
			end++
		}
		if end < len(src) && src[end] == '\n' {
			end++
		}
	}
	return Edit{Start: start, End: end, Text: text}, true
}

func containsName(ns []name, n name) bool {
	for _, x := range ns {
		if x == n {
			return true
		}
	}
	return false
}

// nameLess orders imported names as isort does: CONSTANTS, then Classes,
// then functions, each alphabetically.
func nameLess(a, b string) bool {
	ca, cb := nameClass(a), nameClass(b)
	if ca != cb {
		return ca < cb
	}
	return a < b
}

func nameClass(n string) int {
	t := strings.TrimLeft(n, "_")
	switch {
	case t == "":
		return 2
	case strings.ToUpper(t) == t && strings.ToLower(t) != t:
		return 0
	case t[0] >= 'A' && t[0] <= 'Z':
		return 1
	}
	return 2
}

// group: 0 for std, 1 other absolute imports, 2 relative imports.
func group(module string) int {
	switch {
	case strings.HasPrefix(module, "."):
		return 2
	case module == "std" || strings.HasPrefix(module, "std."):
		return 0
	}
	return 1
}

func render(s *stmt) string {
	if !s.from {
		if s.alias != "" {
			return "import " + s.module + " as " + s.alias
		}
		return "import " + s.module
	}
	var parts []string
	if s.star {
		parts = append(parts, "*")
	}
	for _, n := range s.names {
		parts = append(parts, n.String())
	}
	line := "from " + s.module + " import " + strings.Join(parts, ", ")
	if len(line) <= LineLength || s.star {
		return line
	}
	var b strings.Builder
	b.WriteString("from " + s.module + " import (\n")
	for _, p := range parts {
		b.WriteString("    " + p + ",\n")
	}
	b.WriteString(")")
	return b.String()
}

// usedNames is the set of names used anywhere in f outside [start,end):
// plain identifier uses and the base of member accesses. Name-based on
// purpose: if a name appears at all, its import stays.
func usedNames(f *syntax.File, start, end int) map[string]bool {
	used := map[string]bool{}
	for _, r := range f.Refs {
		if r.Span.Start >= start && r.Span.Start < end {
			continue
		}
		switch r.Kind {
		case syntax.RName, syntax.RKwarg, syntax.RMember, syntax.RImported:
			used[r.Name] = true
		}
	}
	// Anything spelled in a docstring or string is not a use, but a name
	// mentioned in a comment-free decorator or annotation already shows up
	// as a reference above.
	return used
}

func atTopLevel(src []byte, off int) bool {
	ls := lineStart(src, off)
	return ls == off
}

func lineStart(src []byte, off int) int {
	for off > 0 && src[off-1] != '\n' {
		off--
	}
	return off
}

func onlyBlank(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	return true
}

func hasComment(f *syntax.File, start, end int) bool {
	return len(syntax.Lex(f.Src[start:end]).Comments) > 0
}
