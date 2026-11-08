// Package imports finds and resolves the modules a Mojo file imports.
//
// It is a fast, error-tolerant line scanner, not a parser: it only needs
// module names, it must work on files that do not compile, and it runs on
// every save. A real syntax layer (tree-sitter) replaces it in M2.
package imports

import (
	"os"
	"path/filepath"
	"strings"
)

// Import is one import statement.
type Import struct {
	Module string   // "mathlib.ops", ".ops", "..pkg.mod"
	Names  []string // for `from M import a, b`: a, b ("*" kept as is)
	Line   int      // zero-based
}

// Scan returns the imports in src, in order. It skips comments and
// string literals (including docstrings) and joins continuation lines.
func Scan(src string) []Import {
	var out []Import
	for _, st := range statements(src) {
		out = append(out, parse(st.text, st.line)...)
	}
	return out
}

type statement struct {
	text string
	line int
}

// statements splits src into logical lines with comments and string
// contents removed. Parenthesised and backslash continuations are joined.
func statements(src string) []statement {
	var out []statement
	var cur strings.Builder
	start, line, depth := 0, 0, 0
	inStr := "" // active string delimiter: ", ', """ or '''
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, statement{text: t, line: start})
		}
		cur.Reset()
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr != "" {
			if c == '\\' {
				i++
				continue
			}
			if c == '\n' {
				line++
				if len(inStr) == 1 { // unterminated single-line string
					inStr = ""
					flush()
					start = line
				}
				continue
			}
			if strings.HasPrefix(src[i:], inStr) {
				i += len(inStr) - 1
				inStr = ""
				cur.WriteString(`""`)
			}
			continue
		}
		switch {
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i--
		case c == '"' || c == '\'':
			if strings.HasPrefix(src[i:], `"""`) || strings.HasPrefix(src[i:], `'''`) {
				inStr = src[i : i+3]
				i += 2
			} else {
				inStr = string(c)
			}
		case c == '\\' && i+1 < len(src) && src[i+1] == '\n':
			i++
			line++
			cur.WriteByte(' ')
		case c == '\n':
			line++
			if depth > 0 {
				cur.WriteByte(' ')
				continue
			}
			flush()
			start = line
		case c == '(' || c == '[' || c == '{':
			depth++
			cur.WriteByte(c)
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
			cur.WriteByte(c)
		case c == ';' && depth == 0:
			flush()
			start = line
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

func parse(st string, line int) []Import {
	fields := strings.Fields(st)
	if len(fields) < 2 {
		return nil
	}
	switch fields[0] {
	case "import":
		var out []Import
		for _, part := range strings.Split(strings.TrimSpace(st[len("import"):]), ",") {
			mod := firstWord(part)
			if validModule(mod) && !strings.HasPrefix(mod, ".") {
				out = append(out, Import{Module: mod, Line: line})
			}
		}
		return out
	case "from":
		rest := strings.TrimSpace(st[len("from"):])
		mod, names, ok := strings.Cut(rest, " import ")
		if !ok {
			// `from .ops import(...)` without a space before the paren
			if mod, names, ok = strings.Cut(rest, " import("); !ok {
				return nil
			}
			names = "(" + names
		}
		mod = strings.TrimSpace(mod)
		if !validModule(mod) {
			return nil
		}
		names = strings.Trim(strings.TrimSpace(names), "()")
		var list []string
		for _, n := range strings.Split(names, ",") {
			if w := firstWord(n); w != "" {
				list = append(list, w)
			}
		}
		return []Import{{Module: mod, Names: list, Line: line}}
	}
	return nil
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func validModule(m string) bool {
	trimmed := strings.TrimLeft(m, ".")
	if trimmed == "" {
		return m != "" // `from . import x`
	}
	for _, part := range strings.Split(trimmed, ".") {
		if part == "" {
			return false
		}
		for i, r := range part {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

// Resolver maps imports to files on disk.
type Resolver struct {
	// Roots are the include dirs (-I) searched for absolute imports, after
	// the importing file's own directory.
	Roots []string
	// Exists reports whether a regular file exists. Defaults to os.Stat.
	Exists func(path string) bool
}

func (r Resolver) exists(p string) bool {
	if r.Exists != nil {
		return r.Exists(p)
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Resolve returns every file whose contents can change what `from` sees
// through imp: the target module, each enclosing package's __init__.mojo,
// and submodules named in a from-import. Unresolvable imports (the
// toolchain's stdlib, typos) return nothing.
func (r Resolver) Resolve(from string, imp Import) []string {
	mod := imp.Module
	level := len(mod) - len(strings.TrimLeft(mod, "."))
	parts := splitModule(strings.TrimLeft(mod, "."))

	var base string
	if level > 0 {
		base = filepath.Dir(from)
		for i := 1; i < level; i++ {
			base = filepath.Dir(base)
		}
	} else {
		for _, dir := range append([]string{filepath.Dir(from)}, r.Roots...) {
			if r.providesTop(dir, parts[0]) {
				base = dir
				break
			}
		}
		if base == "" {
			return nil
		}
	}

	var deps []string
	add := func(p string) {
		if r.exists(p) {
			deps = append(deps, p)
		}
	}
	dir := base
	if level > 0 {
		add(filepath.Join(dir, "__init__.mojo"))
	}
	for i, part := range parts {
		last := i == len(parts)-1
		pkg := filepath.Join(dir, part)
		if r.exists(filepath.Join(pkg, "__init__.mojo")) {
			add(filepath.Join(pkg, "__init__.mojo"))
			dir = pkg
			continue
		}
		if last {
			add(filepath.Join(dir, part+".mojo"))
			add(filepath.Join(dir, part+".mojopkg"))
		}
		dir = ""
		break
	}
	// `from pkg import ops` may name a submodule rather than a symbol.
	if dir != "" {
		for _, n := range imp.Names {
			if n == "*" {
				continue
			}
			add(filepath.Join(dir, n+".mojo"))
			add(filepath.Join(dir, n, "__init__.mojo"))
		}
	}
	return deps
}

func (r Resolver) providesTop(dir, name string) bool {
	return r.exists(filepath.Join(dir, name, "__init__.mojo")) ||
		r.exists(filepath.Join(dir, name+".mojo")) ||
		r.exists(filepath.Join(dir, name+".mojopkg"))
}

func splitModule(m string) []string {
	if m == "" {
		return nil
	}
	return strings.Split(m, ".")
}
