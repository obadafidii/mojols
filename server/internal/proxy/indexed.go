package proxy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tdadadavid/mojols/server/internal/index"
	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
	"github.com/tdadadavid/mojols/server/internal/syntax"
	"github.com/tdadadavid/mojols/server/internal/text"
)

// indexMethods are answered from mojols' own workspace index. They work
// across files and keep working while mojo-lsp-server is down. Definition
// falls back to the backend when the index cannot resolve a name.
var indexMethods = map[string]bool{
	"textDocument/definition":    true,
	"textDocument/references":    true,
	"textDocument/prepareRename": true,
	"textDocument/rename":        true,
	"workspace/symbol":           true,
}

// indexDocLocked mirrors a document change into the index. It only records
// the change; parsing happens when a query needs the file.
func (s *Server) indexDocLocked(method, uri string) {
	if s.ix == nil {
		return
	}
	path, ok := uriToPath(uri)
	if !ok {
		return
	}
	switch method {
	case "textDocument/didOpen", "textDocument/didChange":
		if d, ok := s.docs.Get(uri); ok {
			s.ix.Update(path, []byte(d.Text))
		}
	case "textDocument/didClose":
		s.ix.Close(path)
	case "textDocument/didSave":
		s.ix.Changed(path)
	}
}

type indexParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position text.Position `json:"position"`
	Context  struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
	NewName string `json:"newName"`
	Query   string `json:"query"`
}

type location struct {
	URI   string     `json:"uri"`
	Range text.Range `json:"range"`
}

func pathToURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func (s *Server) loc(f *index.File, sp syntax.Span) location {
	return location{URI: s.uriFor(f.Path), Range: f.Lines.Range(sp.Start, sp.End, s.docs.Encoding())}
}

// uriFor returns the URI the editor uses for path if the document is open
// (so edits and locations match it exactly), else a file:// URI.
func (s *Server) uriFor(path string) string {
	for _, d := range s.docs.Snapshot() {
		if p, ok := uriToPath(d.URI); ok && p == path {
			return d.URI
		}
	}
	return pathToURI(path)
}

// handleIndexed answers m from the index, or reports false to send it to
// the backend instead.
func (s *Server) handleIndexed(m *jsonrpc.Message) bool {
	start := time.Now()
	var p indexParams
	if json.Unmarshal(m.Params, &p) != nil {
		return false
	}
	defer func() {
		s.log.Debug("answered from the index", "method", m.Method, "elapsed", time.Since(start))
	}()
	if m.Method == "workspace/symbol" {
		s.reply(*m.ID, s.workspaceSymbols(p.Query))
		return true
	}
	path, ok := uriToPath(p.TextDocument.URI)
	if !ok {
		return false
	}
	f := s.ix.File(path)
	if f == nil {
		return false
	}
	offset := f.Lines.Offset(p.Position, s.docs.Encoding())
	switch m.Method {
	case "textDocument/definition":
		t, ok := s.ix.DefinitionAt(f, offset)
		if !ok {
			return false // the backend may know (it sees types mojols does not infer)
		}
		if t.IsModule() {
			s.reply(*m.ID, []location{{URI: s.uriFor(t.File.Path), Range: text.Range{}}})
		} else {
			s.reply(*m.ID, []location{s.loc(t.File, t.Decl.NameSpan)})
		}
		return true

	case "textDocument/references":
		sym, ok := s.ix.SymbolAt(f, offset)
		if !ok {
			return false // e.g. a member of an expression mojols cannot type
		}
		var out []location
		for _, o := range s.ix.References(sym) {
			if !p.Context.IncludeDeclaration && o.Decl && isOwnDecl(sym, o) {
				continue
			}
			out = append(out, s.loc(o.File, o.Span))
		}
		s.reply(*m.ID, out)
		return true

	case "textDocument/prepareRename":
		i := s.ix.OccurrenceAt(f, offset)
		if i < 0 {
			s.reply(*m.ID, nil)
			return true
		}
		if _, why := s.renamable(f, i); why != "" {
			s.replyError(*m.ID, jsonrpc.CodeRequestFailed, why)
			return true
		}
		r := f.Syn.Refs[i]
		s.reply(*m.ID, map[string]any{"range": f.Lines.Range(r.Span.Start, r.Span.End, s.docs.Encoding()), "placeholder": r.Name})
		return true

	case "textDocument/rename":
		edit, why := s.rename(f, offset, p.NewName)
		if why != "" {
			s.replyError(*m.ID, jsonrpc.CodeRequestFailed, why)
			return true
		}
		s.reply(*m.ID, edit)
		return true
	}
	return false
}

// isOwnDecl reports whether o is the declaration of sym itself (or of one
// of its overloads), as opposed to an import binding it.
func isOwnDecl(sym index.Symbol, o index.Occurrence) bool {
	if o.File != sym.File {
		return false
	}
	if o.Span == sym.Decl.NameSpan {
		return true
	}
	if sym.Decl.Scope != nil {
		for _, d := range sym.Decl.Scope.Names[sym.Decl.Name] {
			if d.NameSpan == o.Span && d.Kind != syntax.KImport {
				return true
			}
		}
	}
	return false
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// renamable checks that occurrence i of f names a symbol mojols can
// rename everywhere: declared in the workspace, not a module, not `Self`,
// not a dunder method. It returns the symbol, or why not.
func (s *Server) renamable(f *index.File, i int) (index.Symbol, string) {
	r := f.Syn.Refs[i]
	switch {
	case r.Kind == syntax.RModule:
		return index.Symbol{}, "mojols: renaming modules is not supported"
	case r.Name == "Self":
		return index.Symbol{}, "mojols: `Self` cannot be renamed; rename the struct where it is declared"
	}
	sym, ok := s.ix.SymbolAt(f, r.Span.Start)
	if !ok {
		return index.Symbol{}, fmt.Sprintf("mojols: cannot tell what `%s` refers to here, so it cannot be renamed safely", r.Name)
	}
	if strings.HasPrefix(sym.Decl.Name, "__") && strings.HasSuffix(sym.Decl.Name, "__") {
		return index.Symbol{}, fmt.Sprintf("mojols: `%s` is a special method; renaming it would change its meaning", sym.Decl.Name)
	}
	if !s.ix.InWorkspace(sym.File.Path) {
		return index.Symbol{}, fmt.Sprintf("mojols: `%s` is declared outside the workspace (%s)", sym.Decl.Name, sym.File.Path)
	}
	return sym, ""
}

type textEdit struct {
	Range   text.Range `json:"range"`
	NewText string     `json:"newText"`
}

// rename computes the workspace edit renaming the symbol at offset, or
// why it cannot be done.
func (s *Server) rename(f *index.File, offset int, newName string) (any, string) {
	i := s.ix.OccurrenceAt(f, offset)
	if i < 0 {
		return nil, "mojols: no symbol to rename here"
	}
	sym, why := s.renamable(f, i)
	if why != "" {
		return nil, why
	}
	if !identRe.MatchString(newName) {
		return nil, fmt.Sprintf("mojols: %q is not a valid Mojo identifier", newName)
	}
	if syntax.IsKeyword(newName) {
		return nil, fmt.Sprintf("mojols: %q is a keyword", newName)
	}
	if strings.HasPrefix(newName, "__") && strings.HasSuffix(newName, "__") {
		return nil, fmt.Sprintf("mojols: %q is a special method name", newName)
	}
	if newName == sym.Decl.Name {
		return map[string]any{"changes": map[string][]textEdit{}}, ""
	}
	occs := s.ix.References(sym)
	if conflict := s.renameConflict(sym, occs, newName); conflict != "" {
		return nil, conflict
	}
	byFile := map[string][]textEdit{}
	var order []string
	for _, o := range occs {
		if o.ViaAlias {
			continue // spelled with the alias; the alias keeps its name
		}
		if !s.ix.InWorkspace(o.File.Path) {
			return nil, fmt.Sprintf("mojols: `%s` is also used in %s, outside the workspace", sym.Decl.Name, o.File.Path)
		}
		uri := s.uriFor(o.File.Path)
		if _, seen := byFile[uri]; !seen {
			order = append(order, uri)
		}
		byFile[uri] = append(byFile[uri], textEdit{Range: o.File.Lines.Range(o.Span.Start, o.Span.End, s.docs.Encoding()), NewText: newName})
	}
	sort.Strings(order)
	if !s.clientSupportsDocumentChanges() {
		return map[string]any{"changes": byFile}, ""
	}
	// Versioned edits: the editor refuses them if a document changed since.
	var changes []any
	for _, uri := range order {
		var version any
		if d, ok := s.docs.Get(uri); ok {
			version = d.Version
		}
		changes = append(changes, map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": version},
			"edits":        byFile[uri],
		})
	}
	return map[string]any{"documentChanges": changes}, ""
}

// renameConflict reports a rename that would change what code means: the
// new name already declared next to the symbol, or already meaning
// something else where the symbol is used.
func (s *Server) renameConflict(sym index.Symbol, occs []index.Occurrence, newName string) string {
	if sym.Decl.Scope != nil && len(sym.Decl.Scope.Names[newName]) > 0 {
		other := sym.Decl.Scope.Names[newName][0]
		line := sym.File.Lines.Position(other.NameSpan.Start, text.UTF16).Line + 1
		return fmt.Sprintf("mojols: `%s` is already declared in the same scope (line %d)", newName, line)
	}
	for _, o := range occs {
		if o.ViaAlias {
			continue
		}
		i := s.ix.OccurrenceAt(o.File, o.Span.Start)
		if i < 0 {
			continue
		}
		r := o.File.Syn.Refs[i]
		if r.Kind != syntax.RName && r.Kind != syntax.RDecl {
			continue
		}
		if t, ok := s.ix.LookupFrom(o.File, i, newName); ok && !t.IsModule() && t.Decl != sym.Decl {
			line := t.File.Lines.Position(t.Decl.NameSpan.Start, text.UTF16).Line + 1
			rel := t.File.Path
			return fmt.Sprintf("mojols: renaming to `%s` would clash with `%s` declared at %s:%d", newName, newName, rel, line)
		}
	}
	return ""
}

func (s *Server) clientSupportsDocumentChanges() bool {
	var p struct {
		Capabilities struct {
			Workspace struct {
				WorkspaceEdit struct {
					DocumentChanges bool `json:"documentChanges"`
				} `json:"workspaceEdit"`
			} `json:"workspace"`
		} `json:"capabilities"`
	}
	s.mu.Lock()
	params := s.initParams
	s.mu.Unlock()
	_ = json.Unmarshal(params, &p)
	return p.Capabilities.Workspace.WorkspaceEdit.DocumentChanges
}

// LSP SymbolKind values.
const (
	symMethod    = 6
	symField     = 8
	symInterface = 11
	symFunction  = 12
	symConstant  = 14
	symStruct    = 23
)

func (s *Server) workspaceSymbols(query string) []map[string]any {
	out := []map[string]any{}
	for _, m := range s.ix.Symbols(query, 200) {
		kind := symFunction
		switch m.Decl.Kind {
		case syntax.KStruct:
			kind = symStruct
		case syntax.KTrait:
			kind = symInterface
		case syntax.KComptime:
			kind = symConstant
		case syntax.KField:
			kind = symField
		case syntax.KFunc:
			if m.Decl.Parent != nil {
				kind = symMethod
			}
		}
		item := map[string]any{"name": m.Decl.Name, "kind": kind, "location": s.loc(m.File, m.Decl.NameSpan)}
		if m.Decl.Parent != nil {
			item["containerName"] = m.Decl.Parent.Name
		}
		out = append(out, item)
	}
	return out
}

// indexCapabilities are what the index adds to the backend's capabilities.
func indexCapabilities(caps map[string]any) {
	caps["definitionProvider"] = true
	caps["referencesProvider"] = true
	caps["renameProvider"] = map[string]any{"prepareProvider": true}
	caps["workspaceSymbolProvider"] = true
}
