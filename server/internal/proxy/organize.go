package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/organize"
)

const organizeKind = "source.organizeImports"

type codeActionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Context struct {
		Only []string `json:"only"`
	} `json:"context"`
}

// wants reports whether a code action request filtered by only accepts
// kind (a kind accepts its sub-kinds: "source" accepts
// "source.organizeImports").
func wants(only []string, kind string) bool {
	if len(only) == 0 {
		return true
	}
	for _, o := range only {
		if kind == o || strings.HasPrefix(kind, o+".") {
			return true
		}
	}
	return false
}

// onlyOrganize reports whether a request asks for nothing but organizing
// imports (as editors do on save); mojols answers those itself.
func onlyOrganize(params json.RawMessage) bool {
	var p codeActionParams
	if json.Unmarshal(params, &p) != nil || len(p.Context.Only) == 0 {
		return false
	}
	for _, o := range p.Context.Only {
		if o != organizeKind {
			return false
		}
	}
	return true
}

// organizeAction is the "Organize imports" code action for the document
// in params, if it wants one and there is something to do.
func (s *Server) organizeAction(params json.RawMessage) (map[string]any, bool) {
	if s.ix == nil {
		return nil, false
	}
	var p codeActionParams
	if json.Unmarshal(params, &p) != nil || !wants(p.Context.Only, organizeKind) {
		return nil, false
	}
	path, ok := uriToPath(p.TextDocument.URI)
	if !ok {
		return nil, false
	}
	f := s.ix.File(path)
	if f == nil {
		return nil, false
	}
	// Unused imports are removed only outside packages: in a package
	// module any import may be public API for code outside the workspace.
	var importedElsewhere func(string) bool
	if !inPackage(path) {
		importedElsewhere = func(bound string) bool { return s.ix.ImportedElsewhere(f, bound) }
	}
	e, ok := organize.Organize(f.Syn, path, importedElsewhere)
	if !ok {
		return nil, false
	}
	edit := textEdit{Range: f.Lines.Range(e.Start, e.End, s.docs.Encoding()), NewText: e.Text}
	return map[string]any{
		"title": "Organize imports",
		"kind":  organizeKind,
		"edit":  map[string]any{"changes": map[string][]textEdit{p.TextDocument.URI: {edit}}},
	}, true
}

// withOrganize appends the organize action to a backend's code action
// result.
func (s *Server) withOrganize(params json.RawMessage, m *jsonrpc.Message) *jsonrpc.Message {
	if m.Error != nil {
		return m
	}
	a, ok := s.organizeAction(params)
	if !ok {
		return m
	}
	var actions []json.RawMessage
	if len(m.Result) > 0 && string(m.Result) != "null" && json.Unmarshal(m.Result, &actions) != nil {
		return m // not a list: leave it alone
	}
	raw, _ := json.Marshal(a)
	actions = append(actions, raw)
	out := *m
	out.Result, _ = json.Marshal(actions)
	return &out
}

// inPackage reports whether path is a module of a package (its directory
// has an __init__.mojo).
func inPackage(path string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(path), "__init__.mojo"))
	return err == nil
}
