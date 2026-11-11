package index

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/obadafidii/mojols/server/internal/syntax"
)

// Completion answers that need no compiler: what can follow `from` or
// `import`, the names a module offers, and names from elsewhere that an
// import would bring in.

// Entry is one completion candidate.
type Entry struct {
	Name   string
	Kind   syntax.DeclKind // KModule for modules and packages
	Module string          // for imports: the module it comes from
	// Detail is a one-line signature; Doc the declaration's Markdown
	// description (empty for modules).
	Detail string
	Doc    string
	// Decl locates the declaration (nil for modules).
	File *File
	Decl *syntax.Decl
}

// KModule marks a module or package entry. It is outside the parser's
// kinds.
const KModule syntax.DeclKind = 200

// TryLock takes the index lock if it can within d. Completion uses it so a
// long references search never stalls typing.
func (ix *Index) tryLock(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !ix.mu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Microsecond)
	}
	ix.mu.Unlock()
	ix.lock()
	return true
}

// ImportModules lists the modules and packages that can follow `parent.` in
// an import from the file at fromPath: with parent "" the top-level ones,
// with a relative parent (".", "..", ".pkg") those relative to the file.
// ok is false when parent is not a package mojols can see (a compiled
// package, say): ask the compiler instead.
func (ix *Index) ImportModules(fromPath, parent string, wait time.Duration) (out []Entry, ok bool) {
	if !ix.tryLock(wait) {
		return nil, false
	}
	defer ix.mu.Unlock()
	var dirs []string
	if parent == "" {
		dirs = append(dirs, filepath.Dir(fromPath))
		if top := ix.outermostPackage(filepath.Dir(fromPath)); top != "" {
			dirs = append(dirs, filepath.Dir(top))
		}
		dirs = append(dirs, ix.roots...)
		dirs = append(dirs, ix.libs...)
	} else {
		pkg := ix.resolveModule(fromPath, parent)
		if strings.Trim(parent, ".") == "" {
			// "." or "..": the directory itself, package or not.
			base := filepath.Dir(fromPath)
			for i := 1; i < len(parent); i++ {
				base = filepath.Dir(base)
			}
			dirs = []string{base}
		} else if pkg != nil && filepath.Base(pkg.Path) == "__init__.mojo" {
			dirs = []string{filepath.Dir(pkg.Path)}
		} else {
			return nil, false
		}
	}
	seen := map[string]bool{}
	self := strings.TrimSuffix(filepath.Base(fromPath), ".mojo")
	for _, dir := range dirs {
		for _, e := range ix.listDir(dir) {
			if seen[e.Name] || (dir == filepath.Dir(fromPath) && e.Name == self) {
				continue
			}
			seen[e.Name] = true
			out = append(out, e)
		}
	}
	return out, true
}

// listDir lists the modules (name.mojo) and packages (name/__init__.mojo)
// in dir.
func (ix *Index) listDir(dir string) []Entry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		switch {
		case e.IsDir():
			if syntax.IsIdent(n) && ix.exists(filepath.Join(dir, n, "__init__.mojo")) {
				out = append(out, Entry{Name: n, Kind: KModule, Detail: "package"})
			}
		case strings.HasSuffix(n, ".mojo"):
			stem := strings.TrimSuffix(n, ".mojo")
			if stem != "__init__" && syntax.IsIdent(stem) {
				out = append(out, Entry{Name: stem, Kind: KModule, Detail: "module"})
			}
		case strings.HasSuffix(n, ".mojopkg"):
			stem := strings.TrimSuffix(n, ".mojopkg")
			if syntax.IsIdent(stem) {
				out = append(out, Entry{Name: stem, Kind: KModule, Detail: "compiled package"})
			}
		}
	}
	return out
}

// ModuleNames lists what `from module import ...` can name: the module's
// declarations, the names a package re-exports, and a package's
// submodules. Private names (a leading underscore) come only when private
// is true. ok is false when mojols cannot see the module.
func (ix *Index) ModuleNames(fromPath, module string, private bool, wait time.Duration) (out []Entry, ok bool) {
	if !ix.tryLock(wait) {
		return nil, false
	}
	defer ix.mu.Unlock()
	m := ix.resolveModule(fromPath, module)
	if m == nil {
		return nil, false
	}
	isPkg := filepath.Base(m.Path) == "__init__.mojo"
	seen := map[string]bool{}
	for name, ds := range m.Syn.Module.Names {
		if !private && strings.HasPrefix(name, "_") {
			continue
		}
		d := pick(ds)
		if d == nil {
			continue
		}
		e, ok := ix.entryFor(m, name, d, isPkg)
		if !ok {
			continue
		}
		seen[name] = true
		out = append(out, e)
	}
	if isPkg {
		for _, e := range ix.listDir(filepath.Dir(m.Path)) {
			if !seen[e.Name] && (private || !strings.HasPrefix(e.Name, "_")) {
				out = append(out, e)
			}
		}
	}
	sortEntries(out)
	return out, true
}

// entryFor describes the module-level name `name` of m, bound by d. A name m
// itself imports is offered only from a package (a re-export), and is
// described by what it resolves to.
func (ix *Index) entryFor(m *File, name string, d *syntax.Decl, isPkg bool) (Entry, bool) {
	switch d.Kind {
	case syntax.KStruct, syntax.KTrait, syntax.KFunc, syntax.KComptime, syntax.KVar:
		return describeEntry(name, m, d), true
	case syntax.KImport:
		if !isPkg {
			return Entry{}, false
		}
		ix.budget = stepBudget
		t, ok := Final(ix.lookupModule(m, name, 0))
		if !ok {
			return Entry{Name: name, Kind: KModule}, true // compiled or unseen: still offer it
		}
		if t.IsModule() {
			return Entry{Name: name, Kind: KModule, Detail: "module"}, true
		}
		if t.Decl.Kind == syntax.KImport {
			return Entry{}, false
		}
		return describeEntry(name, t.File, t.Decl), true
	}
	return Entry{}, false
}

func describeEntry(name string, f *File, d *syntax.Decl) Entry {
	return Entry{Name: name, Kind: d.Kind, Detail: signatureLine(f, d), Doc: Describe(f, d), File: f, Decl: d}
}

// signatureLine is a declaration's signature on one line, shortened.
func signatureLine(f *File, d *syntax.Decl) string {
	var s string
	switch d.Kind {
	case syntax.KStruct, syntax.KTrait, syntax.KFunc:
		s = oneLine(f.Syn.Text(d.Header))
		s = strings.TrimSuffix(s, ":")
	default:
		s = d.Name
		if t := oneLine(f.Syn.Text(d.Type)); t != "" {
			s += ": " + t
		}
	}
	if len(s) > 100 {
		s = s[:97] + "..."
	}
	return s
}

func sortEntries(es []Entry) {
	sort.Slice(es, func(a, b int) bool {
		if ca, cb := nameClass(es[a].Name), nameClass(es[b].Name); ca != cb {
			return ca < cb
		}
		return es[a].Name < es[b].Name
	})
}

// nameClass orders names as imports list them: CONSTANTS, Types, functions,
// then private names.
func nameClass(n string) int {
	if strings.HasPrefix(n, "_") {
		return 3
	}
	switch {
	case strings.ToUpper(n) == n && strings.ToLower(n) != n:
		return 0
	case n[0] >= 'A' && n[0] <= 'Z':
		return 1
	}
	return 2
}

// Unimported lists names starting with prefix that an import would bring
// into the file at f (whose current text may be newer than f), with the
// module each is best imported from. Names f already defines or imports are
// left out; so are private names. At most max entries, best first;
// truncated reports that there were more.
func (ix *Index) Unimported(f *File, prefix string, max int, wait time.Duration) (out []Entry, truncated bool) {
	if len(prefix) < 2 || strings.HasPrefix(prefix, "_") {
		return nil, false
	}
	if !ix.tryLock(wait) {
		return nil, false
	}
	ix.nameTable()
	names := map[string]bool{}
	for i := sort.SearchStrings(ix.sorted, prefix); i < len(ix.sorted) && strings.HasPrefix(ix.sorted[i], prefix); i++ {
		name := ix.sorted[i]
		if f.Syn.Module.Names[name] != nil || strings.HasPrefix(name, "_") {
			continue
		}
		for _, g := range ix.names[name] {
			if g != f {
				names[name] = true
				break
			}
		}
	}
	// Names every file already has.
	if prelude := ix.resolveModule(f.Path, "std.prelude"); prelude != nil {
		for n := range names {
			ix.budget = stepBudget
			if ix.lookupModule(prelude, n, 0) != nil {
				delete(names, n)
			}
		}
	}
	defer ix.mu.Unlock()
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	// Exact case and shorter names first: what the reader is most likely
	// typing.
	sort.Slice(sorted, func(a, b int) bool {
		if len(sorted[a]) != len(sorted[b]) {
			return len(sorted[a]) < len(sorted[b])
		}
		return sorted[a] < sorted[b]
	})
	var hidden []Entry
	for _, n := range sorted {
		if len(out) >= max {
			truncated = true
			break
		}
		cands := ix.importCandidates(f, n, 1)
		if len(cands) == 0 {
			continue
		}
		c := cands[0]
		e := describeEntry(n, c.Target.File, c.Target.Decl)
		e.Module = c.Module
		if privatePath(c.Module) {
			hidden = append(hidden, e) // only from a private module: last
			continue
		}
		out = append(out, e)
	}
	for _, e := range hidden {
		if len(out) >= max {
			truncated = true
			break
		}
		out = append(out, e)
	}
	return out, truncated
}

// nameTable brings ix.names up to date with the index. Only files parsed
// again since the last call are looked at: typing in one file costs that
// file, not the workspace.
func (ix *Index) nameTable() {
	if ix.namesOK && ix.namesGen == ix.gen {
		return
	}
	if ix.names == nil {
		ix.names, ix.namesFrom = map[string][]*File{}, map[string]*File{}
	}
	changed := false
	remove := func(g *File) {
		for name := range g.Syn.Module.Names {
			fs := ix.names[name]
			for k, h := range fs {
				if h == g {
					fs = append(fs[:k:k], fs[k+1:]...)
					break
				}
			}
			ix.names[name] = fs // kept when empty: no re-sort while typing
		}
	}
	add := func(g *File) {
		old := ix.namesFrom[g.Path]
		if old == g {
			return
		}
		if old != nil {
			remove(old)
		}
		for name := range g.Syn.Module.Names {
			if _, ok := ix.names[name]; !ok {
				changed = true
			}
			ix.names[name] = append(ix.names[name], g)
		}
		ix.namesFrom[g.Path] = g
	}
	if !ix.namesOK || !ix.scanned {
		// A full pass: the set of files may have changed.
		live := map[string]bool{}
		for _, g := range ix.searchFiles() {
			live[g.Path] = true
			add(g)
		}
		for path, old := range ix.namesFrom {
			if !live[path] {
				remove(old)
				delete(ix.namesFrom, path)
			}
		}
	} else {
		// Only what the editor changed.
		for path := range ix.namesDirty {
			_, known := ix.namesFrom[path]
			_, open := ix.overlay[path]
			if g := ix.file(path); g != nil && (known || open) {
				add(g)
			} else if old := ix.namesFrom[path]; old != nil && g == nil {
				remove(old)
				delete(ix.namesFrom, path)
			}
		}
	}
	ix.namesDirty = nil
	if changed || !ix.namesOK {
		sorted := make([]string, 0, len(ix.names))
		for n, fs := range ix.names {
			if len(fs) == 0 {
				delete(ix.names, n)
				continue
			}
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		ix.sorted = sorted
	}
	ix.namesGen, ix.namesOK = ix.gen, true
}

// Warm parses every workspace and library file, a few at a time, so the
// first completion or search does not pay for it. It holds the index lock
// only briefly at a time, and stops early if stop is closed.
func (ix *Index) Warm(stop <-chan struct{}) {
	ix.lock()
	if !ix.scanned {
		ix.paths = listSources(ix.workspace)
		ix.scanned = true
	}
	if ix.libPaths == nil && len(ix.libs) > 0 {
		ix.libPaths = listSources(ix.libs)
	}
	paths := append(append([]string(nil), ix.paths...), ix.libPaths...)
	ix.mu.Unlock()
	const batch = 32
	for i := 0; i < len(paths); i += batch {
		select {
		case <-stop:
			return
		default:
		}
		ix.lock()
		for _, p := range paths[i:min(i+batch, len(paths))] {
			ix.file(p)
		}
		ix.mu.Unlock()
	}
	ix.lock()
	ix.nameTable()
	ix.mu.Unlock()
}
