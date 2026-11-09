package syntax

import (
	"fmt"
	"strings"
)

// Outline renders the declarations as an indented tree, one per line:
// `kind name` plus `: type` when annotated. Locals inside defs are left
// out unless withLocals is set. For tests and debugging.
func (f *File) Outline(withLocals bool) string {
	var b strings.Builder
	depth := func(d *Decl) int {
		n := 0
		for p := d.Parent; p != nil; p = p.Parent {
			n++
		}
		return n
	}
	for _, d := range f.Decls {
		local := d.Scope.Func() != nil && d.Kind != KParam && d.Kind != KArg && d.Kind != KFunc
		if local && !withLocals {
			continue
		}
		fmt.Fprintf(&b, "%s%s %s", strings.Repeat("  ", depth(d)), d.Kind, d.Name)
		if t := f.Text(d.Type); t != "" {
			fmt.Fprintf(&b, ": %s", t)
		}
		if d.Implicit {
			b.WriteString(" (implicit)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RefsString renders occurrences compactly: `name` for a plain use,
// `.name` for a member (`a.b` when its base is known), `name=` for a
// keyword argument (`f:name=` with its callee), `+name` for a
// declaration, `@name` for a module path component.
func (f *File) RefsString() string {
	var parts []string
	for _, r := range f.Refs {
		switch r.Kind {
		case RName:
			parts = append(parts, r.Name)
		case RMember:
			if r.Base >= 0 {
				parts = append(parts, f.Refs[r.Base].Name+"."+r.Name)
			} else {
				parts = append(parts, "."+r.Name)
			}
		case RKwarg:
			if r.Callee >= 0 {
				parts = append(parts, f.Refs[r.Callee].Name+":"+r.Name+"=")
			} else {
				parts = append(parts, r.Name+"=")
			}
		case RDecl:
			parts = append(parts, "+"+r.Name)
		case RModule:
			parts = append(parts, "@"+r.Name)
		case RImported:
			parts = append(parts, "<"+r.Name)
		}
	}
	return strings.Join(parts, " ")
}
