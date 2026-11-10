package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/obadafidii/mojols/server/internal/index"
	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/organize"
	"github.com/obadafidii/mojols/server/internal/syntax"
	"github.com/obadafidii/mojols/server/internal/text"
)

// mojols' own inspections and quick fixes.
//
// The official server already reports unknown names (as errors) and unused
// variables (as warnings), so mojols does not repeat those: it adds the
// fixes for them. Unused imports the server does not report; mojols does.

const (
	quickFixKind     = "quickfix"
	codeUnusedImport = "unused-import"
	maxImportChoices = 5
)

type lspDiagnostic struct {
	Range   text.Range      `json:"range"`
	Message string          `json:"message"`
	Source  string          `json:"source,omitempty"`
	Code    json.RawMessage `json:"code,omitempty"`
}

type actionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Range   text.Range `json:"range"`
	Context struct {
		Only        []string          `json:"only"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	} `json:"context"`
}

// ---- diagnostics -----------------------------------------------------------

// ownDiagnostics are mojols' diagnostics for the document at uri.
func (s *Server) ownDiagnostics(uri string) []any {
	if s.ix == nil || s.opts.NoInspect {
		return nil
	}
	path, ok := uriToPath(uri)
	if !ok {
		return nil
	}
	if _, open := s.docs.Get(uri); !open {
		return nil
	}
	f := s.ix.File(path)
	if f == nil {
		return nil
	}
	var out []any
	enc := s.docs.Encoding()
	for _, u := range s.ix.UnusedImports(f, inPackage(path)) {
		out = append(out, map[string]any{
			"range":    f.Lines.Range(u.Span.Start, u.Span.End, enc),
			"severity": 4, // hint: editors fade the code rather than underline it
			"source":   "mojols",
			"code":     codeUnusedImport,
			"message":  "'" + u.Bound + "' is imported but not used",
			"tags":     []int{1}, // unnecessary
		})
	}
	return out
}

// withOwnDiagnostics adds mojols' diagnostics to a publishDiagnostics
// notification.
func (s *Server) withOwnDiagnostics(m *jsonrpc.Message) *jsonrpc.Message {
	var p map[string]json.RawMessage
	if json.Unmarshal(m.Params, &p) != nil {
		return m
	}
	var uri string
	var diags []json.RawMessage
	if json.Unmarshal(p["uri"], &uri) != nil || json.Unmarshal(p["diagnostics"], &diags) != nil {
		return m
	}
	own := s.ownDiagnostics(uri)
	if len(own) == 0 {
		return m
	}
	for _, d := range own {
		raw, _ := json.Marshal(d)
		diags = append(diags, raw)
	}
	p["diagnostics"], _ = json.Marshal(diags)
	out := *m
	out.Params, _ = json.Marshal(p)
	return &out
}

// diagQueue sends diagnostics to the editor in order, off the goroutine
// that reads the backend: computing mojols' own can touch the whole
// workspace.
type diagQueue struct {
	mu      sync.Mutex
	items   []*jsonrpc.Message
	running bool
}

func (s *Server) publishDiagnostics(m *jsonrpc.Message) {
	q := &s.diags
	q.mu.Lock()
	q.items = append(q.items, m)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()
	go func() {
		for {
			q.mu.Lock()
			if len(q.items) == 0 {
				q.running = false
				q.mu.Unlock()
				return
			}
			next := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			s.write(s.safeOwnDiagnostics(next))
		}
	}()
}

func (s *Server) safeOwnDiagnostics(m *jsonrpc.Message) (out *jsonrpc.Message) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("inspections failed", "panic", r)
			out = m
		}
	}()
	return s.withOwnDiagnostics(m)
}

// ---- code actions ----------------------------------------------------------

// actions lists mojols' code actions for a request: quick fixes, then
// organize imports.
func (s *Server) actions(params json.RawMessage) (out []map[string]any) {
	if s.ix == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("code actions failed", "panic", r)
			out = nil
		}
	}()
	var p actionParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	if wants(p.Context.Only, quickFixKind) && !s.opts.NoInspect {
		if path, ok := uriToPath(p.TextDocument.URI); ok {
			if f := s.ix.File(path); f != nil {
				out = append(out, s.quickFixes(f, path, &p)...)
			}
		}
	}
	if a, ok := s.organizeAction(params); ok {
		out = append(out, a)
	}
	return out
}

// withActions appends mojols' code actions to a backend's code action
// result.
func (s *Server) withActions(params json.RawMessage, m *jsonrpc.Message) *jsonrpc.Message {
	if m.Error != nil {
		return m
	}
	own := s.actions(params)
	if len(own) == 0 {
		return m
	}
	var actions []json.RawMessage
	if len(m.Result) > 0 && string(m.Result) != "null" && json.Unmarshal(m.Result, &actions) != nil {
		return m // not a list: leave it alone
	}
	for _, a := range own {
		raw, _ := json.Marshal(a)
		actions = append(actions, raw)
	}
	out := *m
	out.Result, _ = json.Marshal(actions)
	return &out
}

var (
	unusedAssignRe = regexp.MustCompile(`^assignment to '([^']+)' was never used`)
	unusedRefRe    = regexp.MustCompile(`^ref '([^']+)' was never used`)
)

func (s *Server) quickFixes(f *index.File, path string, p *actionParams) []map[string]any {
	enc := s.docs.Encoding()
	uri := p.TextDocument.URI
	start, end := f.Lines.Offset(p.Range.Start, enc), f.Lines.Offset(p.Range.End, enc)
	if end < start {
		start, end = end, start
	}
	touches := func(sp syntax.Span) bool { return sp.Start <= end && start <= sp.End }
	fix := func(title string, diag json.RawMessage, preferred bool, edits ...organize.Edit) map[string]any {
		tes := make([]textEdit, len(edits))
		for i, e := range edits {
			tes[i] = textEdit{Range: f.Lines.Range(e.Start, e.End, enc), NewText: e.Text}
		}
		a := map[string]any{
			"title": title,
			"kind":  quickFixKind,
			"edit":  map[string]any{"changes": map[string][]textEdit{uri: tes}},
		}
		if diag != nil {
			a["diagnostics"] = []json.RawMessage{diag}
		}
		if preferred {
			a["isPreferred"] = true
		}
		return a
	}
	var out []map[string]any

	// Spans to look for unknown names in: the request's range, and every
	// "unknown declaration" diagnostic the editor sent with it.
	type area struct {
		sp   syntax.Span
		diag json.RawMessage
	}
	areas := []area{{syntax.Span{Start: start, End: end}, nil}}
	for _, raw := range p.Context.Diagnostics {
		var d lspDiagnostic
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		ds, de := f.Lines.Offset(d.Range.Start, enc), f.Lines.Offset(d.Range.End, enc)
		switch {
		case strings.Contains(d.Message, "unknown declaration"):
			areas = append(areas, area{syntax.Span{Start: ds, End: de}, raw})
		case d.Source == "mojols":
			continue
		default:
			if m := unusedAssignRe.FindStringSubmatch(d.Message); m != nil {
				if e, ok := discardAssignment(f.Syn, m[1], ds); ok {
					out = append(out, fix("Assign to '_' instead of '"+m[1]+"'", raw, true, e))
				}
			} else if m := unusedRefRe.FindStringSubmatch(d.Message); m != nil {
				if e, ok := discardLoopVar(f.Syn, m[1], ds); ok {
					out = append(out, fix("Rename '"+m[1]+"' to '_'", raw, true, e))
				}
			}
		}
	}

	// Import an unknown name.
	seen := map[string]bool{}
	for _, a := range areas {
		for i, r := range f.Syn.Refs {
			if r.Kind != syntax.RName || seen[r.Name] || !(r.Span.Start <= a.sp.End && a.sp.Start <= r.Span.End) {
				continue
			}
			if !s.ix.Unresolved(f, i) {
				continue
			}
			seen[r.Name] = true
			cands := s.ix.ImportCandidates(f, r.Name, maxImportChoices)
			for _, c := range cands {
				e, ok := organize.AddImport(f.Syn, c.Module, c.Name)
				if !ok {
					continue
				}
				out = append(out, fix("Import '"+c.Name+"' from "+c.Module, a.diag, len(cands) == 1, e))
			}
		}
	}

	// Remove unused imports.
	unused := s.ix.UnusedImports(f, inPackage(path))
	if len(unused) > 0 {
		byStmt := map[*syntax.Import]map[int]bool{}
		var order []*syntax.Import
		for _, u := range unused {
			if byStmt[u.Import] == nil {
				byStmt[u.Import] = map[int]bool{}
				order = append(order, u.Import)
			}
			byStmt[u.Import][u.Name] = true
		}
		for _, u := range unused {
			line := syntax.Span{Start: lineStartOf(f.Syn.Src, u.Span.Start), End: lineEndOf(f.Syn.Src, u.Span.End)}
			if !touches(line) {
				continue
			}
			if e, ok := organize.RemoveNames(f.Syn, u.Import, map[int]bool{u.Name: true}); ok {
				out = append(out, fix("Remove unused import '"+u.Bound+"'", nil, len(unused) == 1, e))
			}
		}
		if len(unused) > 1 {
			var edits []organize.Edit
			for _, im := range order {
				if e, ok := organize.RemoveNames(f.Syn, im, byStmt[im]); ok {
					edits = append(edits, e)
				}
			}
			if len(edits) > 0 {
				out = append(out, fix("Remove all unused imports", nil, false, edits...))
			}
		}
	}
	return out
}

// discardAssignment turns `var name = value` (whose value starts at
// valueStart, where the server puts its warning) into `_ = value`.
func discardAssignment(f *syntax.File, name string, valueStart int) (organize.Edit, bool) {
	for _, d := range f.Decls {
		if d.Kind != syntax.KVar || d.Name != name || d.Loop || d.Value.Start != valueStart || d.Value.End <= d.Value.Start {
			continue
		}
		from := d.Span.Start
		if d.Implicit {
			from = d.NameSpan.Start
		}
		if from < 0 || from > d.NameSpan.Start || d.NameSpan.End > valueStart {
			return organize.Edit{}, false
		}
		// Only `var name [: T] = value`: one name, nothing else declared.
		head := string(f.Src[from:valueStart])
		if strings.ContainsAny(head, ",()\n") {
			return organize.Edit{}, false
		}
		return organize.Edit{Start: from, End: valueStart, Text: "_ = "}, true
	}
	return organize.Edit{}, false
}

// discardLoopVar renames the loop variable declared at off to `_`.
func discardLoopVar(f *syntax.File, name string, off int) (organize.Edit, bool) {
	for _, d := range f.Decls {
		if d.Kind == syntax.KVar && d.Loop && d.Name == name && d.NameSpan.Start == off {
			return organize.Edit{Start: d.NameSpan.Start, End: d.NameSpan.End, Text: "_"}, true
		}
	}
	return organize.Edit{}, false
}

func lineStartOf(src []byte, off int) int {
	for off > 0 && src[off-1] != '\n' {
		off--
	}
	return off
}

func lineEndOf(src []byte, off int) int {
	for off < len(src) && src[off] != '\n' {
		off++
	}
	return off
}
