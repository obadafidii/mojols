package index

import (
	"sort"

	"github.com/tdadadavid/mojols/server/internal/syntax"
)

// IsCall reports whether the occurrence at sp in f is called: followed by
// `(`, optionally after compile-time arguments (`f[T](x)`).
func IsCall(f *File, sp syntax.Span) bool {
	for _, g := range bracketGroups(f.Syn.Src, sp.End, len(f.Syn.Src)) {
		if g.op == '(' {
			return true
		}
	}
	return false
}

// EnclosingFunc returns the innermost def containing offset in f, or nil
// at module or struct level.
func EnclosingFunc(f *File, offset int) *syntax.Decl {
	var best *syntax.Decl
	for _, d := range f.Syn.Decls {
		if d.Kind != syntax.KFunc || d.Span.Start > offset || offset >= d.Span.End {
			continue
		}
		if best == nil || d.Span.End-d.Span.Start < best.Span.End-best.Span.Start {
			best = d
		}
	}
	return best
}

// FuncAt returns the def for a call hierarchy at offset in f: the one
// named there (declared or used).
func (ix *Index) FuncAt(f *File, offset int) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	i := f.Syn.RefAt(offset)
	if i < 0 {
		return Target{}, false
	}
	ix.budget = stepBudget
	t, ok := Final(ix.resolveRef(f, i, 0))
	if !ok || t.IsModule() || t.Decl.Kind != syntax.KFunc {
		return Target{}, false
	}
	return t, true
}

// FuncDeclAt finds the def named name nearest offset in path (a call
// hierarchy item from an earlier request).
func (ix *Index) FuncDeclAt(path, name string, offset int) (Target, bool) {
	ix.lock()
	defer ix.mu.Unlock()
	f := ix.file(path)
	if f == nil {
		return Target{}, false
	}
	var best *syntax.Decl
	for _, d := range f.Syn.Decls {
		if d.Name == name && d.Kind == syntax.KFunc && (best == nil || abs(d.NameSpan.Start-offset) < abs(best.NameSpan.Start-offset)) {
			best = d
		}
	}
	if best == nil {
		return Target{}, false
	}
	return Target{File: f, Decl: best}, true
}

// Call is one callee of a function, with the call sites naming it.
type Call struct {
	To    Target
	Sites []syntax.Span
}

// OutgoingCalls lists what fn's body calls: functions, methods and struct
// constructions that resolve, each with its call sites, in order of first
// call. Calls inside nested defs belong to those defs and are left out.
func (ix *Index) OutgoingCalls(fn Target) []Call {
	ix.lock()
	defer ix.mu.Unlock()
	f, d := fn.File, fn.Decl
	if d.Body == nil {
		return nil
	}
	byTarget := map[*syntax.Decl]int{}
	var out []Call
	for i, r := range f.Syn.Refs {
		if r.Span.Start < d.Header.End || r.Span.Start >= d.Span.End || r.Kind == syntax.RDecl || r.Kind == syntax.RKwarg {
			continue
		}
		if inner := EnclosingFunc(f, r.Span.Start); inner != d {
			continue
		}
		if !IsCall(f, r.Span) {
			continue
		}
		ix.budget = stepBudget
		t, ok := Final(ix.resolveRef(f, i, 0))
		if !ok || t.IsModule() || (t.Decl.Kind != syntax.KFunc && t.Decl.Kind != syntax.KStruct) {
			continue
		}
		k, seen := byTarget[t.Decl]
		if !seen {
			k = len(out)
			byTarget[t.Decl] = k
			out = append(out, Call{To: t})
		}
		out[k].Sites = append(out[k].Sites, r.Span)
	}
	return out
}

// Caller groups the call sites of a function by the def they are in; Func
// is nil for calls at module level.
type Caller struct {
	File  *File
	Func  *syntax.Decl
	Sites []syntax.Span
}

// GroupCallers turns call-site occurrences into callers, sorted by file
// and position.
func GroupCallers(occs []Occurrence) []Caller {
	type key struct {
		f *File
		d *syntax.Decl
	}
	idx := map[key]int{}
	var out []Caller
	for _, o := range occs {
		k := key{o.File, EnclosingFunc(o.File, o.Span.Start)}
		n, ok := idx[k]
		if !ok {
			n = len(out)
			idx[k] = n
			out = append(out, Caller{File: o.File, Func: k.d})
		}
		out[n].Sites = append(out[n].Sites, o.Span)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].File.Path != out[b].File.Path {
			return out[a].File.Path < out[b].File.Path
		}
		return out[a].Sites[0].Start < out[b].Sites[0].Start
	})
	return out
}
