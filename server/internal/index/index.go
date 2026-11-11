// Package index is mojols' workspace-wide view of Mojo code: every file's
// syntax (from internal/syntax), the modules they form, and the resolution
// of each identifier occurrence to the declaration it names, across files.
// Cross-file references and rename are built on it.
//
// Resolution is static and deliberately conservative. Where it cannot be
// sure (a member of an expression whose type it cannot infer), it answers
// nothing rather than guessing; the proxy can then ask the official server.
package index

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/obadafidii/mojols/server/internal/syntax"
	"github.com/obadafidii/mojols/server/internal/text"
)

// Index holds parsed files and resolves names across them. It is safe for
// concurrent use.
type Index struct {
	mu       sync.Mutex
	roots    []string
	files    map[string]*File
	overlay  map[string][]byte // editor contents of open documents
	stat     map[string]bool   // file-existence cache
	gen      uint64            // bumped on every change; memos are per generation
	readFile func(string) ([]byte, error)

	budget int // steps left for the current resolution (see stepBudget)

	libs     []string // library source roots (see SetLibraries)
	libPaths []string // .mojo files under them, listed on first use

	workspace []string // directories a references search covers
	paths     []string // .mojo files found under them
	scanned   bool     // paths is current

	// names maps every module-level name to the files declaring it, for
	// completion and import fixes; rebuilt when the index changes.
	names      map[string][]*File
	sorted     []string         // the keys of names, sorted
	namesFrom  map[string]*File // the parse of each path names reflects
	namesGen   uint64
	namesOK    bool
	namesDirty map[string]bool // paths changed since names was updated

	// Editor changes arrive here under their own lock, so recording one
	// never waits for a running query; queries apply them first.
	inboxMu sync.Mutex
	inbox   map[string]change
}

type change struct {
	text   []byte // new editor contents, if set
	closed bool   // the editor closed the document
	disk   bool   // the file changed on disk
}

// File is one parsed file.
type File struct {
	Path  string
	Syn   *syntax.File
	Lines *text.Lines
	// ByName lists occurrence indices by identifier, for finding the
	// candidates of a references search without resolving everything.
	ByName map[string][]int

	gen      uint64
	refMemo  map[int][]Target
	nameMemo map[string][]Target
	typeMemo map[int]typeResult
}

// Target is what a name resolves to: a declaration, or a whole module
// (Decl nil) for an imported module or package.
type Target struct {
	File *File
	Decl *syntax.Decl
}

// IsModule reports whether t is a module rather than a declaration.
func (t Target) IsModule() bool { return t.Decl == nil }

// New returns an empty index. roots are the package roots imports are
// resolved against (the -I directories).
func New(roots []string) *Index {
	return &Index{
		roots:    append([]string(nil), roots...),
		files:    map[string]*File{},
		overlay:  map[string][]byte{},
		stat:     map[string]bool{},
		readFile: os.ReadFile,
	}
}

// SetRoots replaces the package roots.
func (ix *Index) SetRoots(roots []string) {
	ix.lock()
	defer ix.mu.Unlock()
	ix.roots = append([]string(nil), roots...)
	ix.stat = map[string]bool{}
	ix.namesOK = false
	ix.gen++
}

// Update sets the editor's contents for path (an open document), replacing
// what is on disk until Close. It never blocks on a running query, and the
// file is parsed only when a query needs it.
func (ix *Index) Update(path string, src []byte) {
	ix.note(path, func(c *change) { c.text, c.closed = src, false })
}

// Close drops the editor's contents for path; it is read from disk again.
func (ix *Index) Close(path string) {
	ix.note(path, func(c *change) { c.text, c.closed = nil, true })
}

// Changed forgets what is known about path on disk (created, changed or
// deleted outside the editor).
func (ix *Index) Changed(path string) {
	ix.note(path, func(c *change) { c.disk = true })
}

func (ix *Index) note(path string, f func(*change)) {
	ix.inboxMu.Lock()
	defer ix.inboxMu.Unlock()
	if ix.inbox == nil {
		ix.inbox = map[string]change{}
	}
	c := ix.inbox[path]
	f(&c)
	ix.inbox[path] = c
}

// lock takes the index lock and applies pending editor changes.
func (ix *Index) lock() {
	ix.mu.Lock()
	ix.inboxMu.Lock()
	inbox := ix.inbox
	ix.inbox = nil
	ix.inboxMu.Unlock()
	for path, c := range inbox {
		if ix.namesDirty == nil {
			ix.namesDirty = map[string]bool{}
		}
		ix.namesDirty[path] = true
		if c.closed {
			delete(ix.overlay, path)
			delete(ix.files, path)
		}
		if c.text != nil {
			ix.overlay[path] = c.text
			delete(ix.files, path) // parsed again when needed
			ix.stat[path] = true
		}
		if c.disk {
			if _, open := ix.overlay[path]; !open {
				delete(ix.files, path)
			}
			delete(ix.stat, path)
			ix.scanned = false
		}
		ix.gen++
	}
}

// File returns the parsed file at path, reading it if needed, or nil.
func (ix *Index) File(path string) *File {
	ix.lock()
	defer ix.mu.Unlock()
	return ix.file(path)
}

func (ix *Index) file(path string) *File {
	if f, ok := ix.files[path]; ok {
		return f
	}
	if src, open := ix.overlay[path]; open {
		f := ix.parse(path, src)
		ix.files[path] = f
		return f
	}
	src, err := ix.readFile(path)
	if err != nil {
		ix.files[path] = nil
		return nil
	}
	f := ix.parse(path, src)
	ix.files[path] = f
	return f
}

func (ix *Index) parse(path string, src []byte) *File {
	syn := syntax.Parse(src)
	f := &File{Path: path, Syn: syn, Lines: text.NewLines(string(src)), ByName: map[string][]int{}}
	for i, r := range syn.Refs {
		f.ByName[r.Name] = append(f.ByName[r.Name], i)
	}
	return f
}

func (ix *Index) exists(path string) bool {
	if v, ok := ix.stat[path]; ok {
		return v
	}
	_, inOverlay := ix.overlay[path]
	_, err := os.Stat(path)
	v := inOverlay || err == nil
	ix.stat[path] = v
	return v
}

// memo resets f's caches when the index has changed since they were filled.
func (ix *Index) memo(f *File) {
	if f.gen != ix.gen || f.refMemo == nil {
		f.gen = ix.gen
		f.refMemo = map[int][]Target{}
		f.nameMemo = map[string][]Target{}
		f.typeMemo = map[int]typeResult{}
	}
}

// --- modules ---

// resolveModule finds the file for a module path imported from file
// `from`: a .mojo file or a package's __init__.mojo. Search order matches
// internal/imports: relative to the file, the file's own directory, the
// directory above its outermost package, then the roots.
func (ix *Index) resolveModule(from, module string) *File {
	level := len(module) - len(strings.TrimLeft(module, "."))
	var parts []string
	if rest := strings.TrimLeft(module, "."); rest != "" {
		parts = strings.Split(rest, ".")
	}
	var base string
	if level > 0 {
		base = filepath.Dir(from)
		for i := 1; i < level; i++ {
			base = filepath.Dir(base)
		}
		if len(parts) == 0 {
			return ix.file(filepath.Join(base, "__init__.mojo"))
		}
	} else {
		if len(parts) == 0 {
			return nil
		}
		search := []string{filepath.Dir(from)}
		if top := ix.outermostPackage(filepath.Dir(from)); top != "" {
			search = append(search, filepath.Dir(top))
		}
		search = append(search, ix.roots...)
		for _, dir := range append(search, ix.libs...) {
			if ix.exists(filepath.Join(dir, parts[0], "__init__.mojo")) || ix.exists(filepath.Join(dir, parts[0]+".mojo")) {
				base = dir
				break
			}
		}
		if base == "" {
			return nil
		}
	}
	dir := base
	for i, part := range parts {
		last := i == len(parts)-1
		if init := filepath.Join(dir, part, "__init__.mojo"); ix.exists(init) {
			if last {
				return ix.file(init)
			}
			dir = filepath.Join(dir, part)
			continue
		}
		if last {
			if p := filepath.Join(dir, part+".mojo"); ix.exists(p) {
				return ix.file(p)
			}
		}
		return nil
	}
	return nil
}

func (ix *Index) outermostPackage(dir string) string {
	top := ""
	for d := dir; ; d = filepath.Dir(d) {
		if !ix.exists(filepath.Join(d, "__init__.mojo")) {
			break
		}
		top = d
		if filepath.Dir(d) == d {
			break
		}
	}
	return top
}

// submodule returns the module `name` inside package pkg (an __init__.mojo).
func (ix *Index) submodule(pkg *File, name string) *File {
	if filepath.Base(pkg.Path) != "__init__.mojo" {
		return nil
	}
	dir := filepath.Dir(pkg.Path)
	if p := filepath.Join(dir, name, "__init__.mojo"); ix.exists(p) {
		return ix.file(p)
	}
	if p := filepath.Join(dir, name+".mojo"); ix.exists(p) {
		return ix.file(p)
	}
	return nil
}
