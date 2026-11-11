package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/obadafidii/mojols/server/internal/index"
	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/organize"
	"github.com/obadafidii/mojols/server/internal/syntax"
	"github.com/obadafidii/mojols/server/internal/text"
)

// mojols' part of completion.
//
// Inside an import statement mojols answers by itself, from its index:
// modules after `from` or `import`, a module's names after `import`. The
// official server needs a second or more for these and knows only the
// packages it was given; mojols answers in milliseconds and also sees the
// workspace's own packages and relative imports.
//
// Everywhere else the official server completes, and mojols adds names the
// file has not imported yet: picking one inserts the name and adds its
// import.

const (
	completionLockWait = 30 * time.Millisecond
	maxUnimported      = 25
	// Suggestions mojols adds sort after the server's for equal matches.
	unimportedSortPrefix = "~"
)

// importCtx is the import statement being written at the cursor.
type importCtx struct {
	names   bool   // completing names after `from module import`
	module  string // the module (names) or the parent package (modules)
	partial string // what is typed of the word being completed
	keyword bool   // after `from module `: offer `import`
	exclude map[string]bool
}

// importContext finds the import statement the cursor at off of src is in.
func importContext(src []byte, off int) (importCtx, bool) {
	if off > len(src) {
		return importCtx{}, false
	}
	ls := off
	for ls > 0 && src[ls-1] != '\n' {
		ls--
	}
	stmt := string(src[ls:off])
	trimmed := strings.TrimLeft(stmt, " \t")
	if !strings.HasPrefix(trimmed, "from ") && !strings.HasPrefix(trimmed, "import ") {
		// Maybe inside a parenthesized list: `from m import (` on an earlier
		// line, not closed yet.
		start := ls
		found := false
		for i := 0; i < 200 && start > 0; i++ {
			prev := start - 1
			for prev > 0 && src[prev-1] != '\n' {
				prev--
			}
			line := strings.TrimSpace(string(src[prev : start-1]))
			if strings.Contains(line, ")") {
				break
			}
			if strings.HasPrefix(line, "from ") && strings.HasSuffix(strings.SplitN(line, "#", 2)[0], "(") || (strings.HasPrefix(line, "from ") && strings.Contains(line, " import (")) {
				start, found = prev, true
				break
			}
			start = prev
		}
		if !found {
			return importCtx{}, false
		}
		stmt = string(src[start:off])
		trimmed = strings.TrimLeft(stmt, " \t")
	}
	if strings.ContainsAny(stmt, "#\"'") {
		return importCtx{}, false // a comment or a string: not code
	}
	flat := strings.Join(strings.Fields(trimmed), " ")
	if strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t") || strings.HasSuffix(trimmed, "\n") || strings.HasSuffix(trimmed, "(") || strings.HasSuffix(trimmed, ",") {
		flat += " " // the cursor starts a new word
	}
	if rest, ok := strings.CutPrefix(flat, "import "); ok {
		items := strings.Split(rest, ",")
		item := strings.TrimSpace(items[len(items)-1])
		if strings.Contains(item, " ") {
			return importCtx{}, false // `import a as b`
		}
		parent, partial := splitModule(item)
		return importCtx{module: parent, partial: partial}, validModulePrefix(parent)
	}
	rest, ok := strings.CutPrefix(flat, "from ")
	if !ok {
		return importCtx{}, false
	}
	module, after, hasSpace := strings.Cut(rest, " ")
	if !hasSpace {
		parent, partial := splitModule(module)
		return importCtx{module: parent, partial: partial}, validModulePrefix(parent)
	}
	if !validModulePrefix(module) || strings.Trim(module, ".") == "" && !strings.HasPrefix(module, ".") {
		return importCtx{}, false
	}
	names, ok := strings.CutPrefix(after, "import ")
	if !ok {
		if after == "" || strings.HasPrefix("import", after) {
			return importCtx{module: module, partial: after, keyword: true}, true
		}
		return importCtx{}, false
	}
	names = strings.TrimPrefix(strings.TrimSpace(names), "(")
	items := strings.Split(names, ",")
	partial := strings.TrimSpace(items[len(items)-1])
	if strings.Contains(partial, " ") || partial == "*" {
		return importCtx{}, false // `x as y`, or a star
	}
	ex := map[string]bool{}
	for _, it := range items[:len(items)-1] {
		if f := strings.Fields(it); len(f) > 0 {
			ex[f[0]] = true
		}
	}
	return importCtx{names: true, module: module, partial: partial, exclude: ex}, true
}

// splitModule splits a dotted module being typed into its parent and the
// partial last part: ".a.b" -> (".a", "b"), "..x" -> ("..", "x").
func splitModule(m string) (parent, partial string) {
	dots := len(m) - len(strings.TrimLeft(m, "."))
	rest := m[dots:]
	if i := strings.LastIndexByte(rest, '.'); i >= 0 {
		return m[:dots+i], rest[i+1:]
	}
	return m[:dots], rest
}

func validModulePrefix(m string) bool {
	for _, part := range strings.Split(strings.TrimLeft(m, "."), ".") {
		if part != "" && !syntax.IsIdent(part) {
			return false
		}
	}
	return true
}

type completionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position text.Position `json:"position"`
}

// completionItem is the LSP item mojols builds.
type completionItem struct {
	Label               string          `json:"label"`
	Kind                int             `json:"kind,omitempty"`
	Detail              string          `json:"detail,omitempty"`
	LabelDetails        map[string]any  `json:"labelDetails,omitempty"`
	Documentation       map[string]any  `json:"documentation,omitempty"`
	SortText            string          `json:"sortText,omitempty"`
	FilterText          string          `json:"filterText,omitempty"`
	TextEdit            *textEdit       `json:"textEdit,omitempty"`
	AdditionalTextEdits []textEdit      `json:"additionalTextEdits,omitempty"`
	Data                json.RawMessage `json:"data,omitempty"`
}

var mojolsItemData = json.RawMessage(`{"mojols":true}`)

func lspKind(k syntax.DeclKind) int {
	switch k {
	case index.KModule:
		return 9
	case syntax.KStruct:
		return 22
	case syntax.KTrait:
		return 8
	case syntax.KFunc:
		return 3
	case syntax.KComptime:
		return 21
	}
	return 6
}

func entryItem(e index.Entry, edit *textEdit) completionItem {
	it := completionItem{Label: e.Name, Kind: lspKind(e.Kind), Detail: e.Detail, TextEdit: edit, Data: mojolsItemData}
	if e.Doc != "" {
		it.Documentation = map[string]any{"kind": "markdown", "value": e.Doc}
	}
	return it
}

// importCompletion answers a completion request inside an import
// statement. ok is false when the cursor is not in one, or mojols cannot
// see the module (then the official server answers).
func (s *Server) importCompletion(params json.RawMessage) (result any, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("import completion failed", "panic", r)
			result, ok = nil, false
		}
	}()
	var p completionParams
	if json.Unmarshal(params, &p) != nil {
		return nil, false
	}
	d, open := s.docs.Get(p.TextDocument.URI)
	path, okPath := uriToPath(p.TextDocument.URI)
	if !open || !okPath {
		return nil, false
	}
	src := []byte(d.Text)
	lines := text.NewLines(d.Text)
	enc := s.docs.Encoding()
	off := lines.Offset(p.Position, enc)
	ctx, ok := importContext(src, off)
	if !ok {
		return nil, false
	}
	edit := &textEdit{Range: lines.Range(off-len(ctx.partial), off, enc)}
	var items []completionItem
	switch {
	case ctx.keyword:
		e := *edit
		e.NewText = "import "
		items = append(items, completionItem{Label: "import", Kind: 14, TextEdit: &e, Data: mojolsItemData})
	case ctx.names:
		es, ok := s.ix.ModuleNames(path, ctx.module, strings.HasPrefix(ctx.partial, "_"), completionLockWait)
		if !ok {
			return nil, false
		}
		for i, e := range es {
			if ctx.exclude[e.Name] {
				continue
			}
			te := *edit
			te.NewText = e.Name
			it := entryItem(e, &te)
			it.SortText = sortKey(i)
			items = append(items, it)
		}
	default:
		es, ok := s.ix.ImportModules(path, ctx.module, completionLockWait)
		if !ok {
			return nil, false
		}
		private := strings.HasPrefix(ctx.partial, "_")
		for _, e := range es {
			if strings.HasPrefix(e.Name, "_") && !private {
				continue
			}
			te := *edit
			te.NewText = e.Name
			items = append(items, entryItem(e, &te))
		}
	}
	return map[string]any{"isIncomplete": false, "items": items}, true
}

func sortKey(i int) string {
	const digits = "0123456789"
	b := []byte("0000")
	for k := 3; k >= 0; k-- {
		b[k] = digits[i%10]
		i /= 10
	}
	return string(b)
}

// ---- names not imported yet ------------------------------------------------

type ownCompletion struct {
	items []completionItem
	// incomplete asks the editor to request again as the word grows: the
	// word was too short to look up, or there were more names than shown.
	incomplete bool
	at         time.Time
}

type completionStore struct {
	mu    sync.Mutex
	byReq map[string]ownCompletion
}

// noteCompletion prepares, for a completion request the official server
// will answer, the unimported names mojols adds to its answer. It runs as
// the request arrives, against the text it was made on.
func (s *Server) noteCompletion(m *jsonrpc.Message) {
	if s.ix == nil || s.opts.NoInspect || m.ID == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("completion suggestions failed", "panic", r)
		}
	}()
	items, incomplete := s.unimportedItems(m.Params)
	if len(items) == 0 && !incomplete {
		return
	}
	st := &s.completions
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.byReq == nil {
		st.byReq = map[string]ownCompletion{}
	}
	for k, v := range st.byReq {
		if time.Since(v.at) > time.Minute {
			delete(st.byReq, k) // answered some other way (cancelled)
		}
	}
	st.byReq[m.ID.String()] = ownCompletion{items: items, incomplete: incomplete, at: time.Now()}
}

func (s *Server) unimportedItems(params json.RawMessage) (out []completionItem, incomplete bool) {
	var p completionParams
	if json.Unmarshal(params, &p) != nil {
		return nil, false
	}
	d, open := s.docs.Get(p.TextDocument.URI)
	path, ok := uriToPath(p.TextDocument.URI)
	if !open || !ok {
		return nil, false
	}
	src := []byte(d.Text)
	lines := text.NewLines(d.Text)
	enc := s.docs.Encoding()
	off := lines.Offset(p.Position, enc)
	start := off
	for start > 0 && isWordByte(src[start-1]) {
		start--
	}
	prefix := string(src[start:off])
	if prefix == "" || (prefix[0] >= '0' && prefix[0] <= '9') || strings.HasPrefix(prefix, "_") {
		return nil, false
	}
	if start > 0 && src[start-1] == '.' {
		return nil, false // a member: the server's business
	}
	ls := start
	for ls > 0 && src[ls-1] != '\n' {
		ls--
	}
	line := string(src[ls:start])
	if strings.ContainsAny(line, "#\"'") {
		return nil, false // maybe in a comment or string; don't guess
	}
	if t := strings.TrimSpace(line); strings.HasPrefix(t, "def ") || strings.HasPrefix(t, "struct ") || strings.HasPrefix(t, "trait ") || t == "var" || strings.HasSuffix(t, " var") || strings.HasPrefix(t, "comptime ") && !strings.Contains(t, "=") {
		if !strings.Contains(t, "(") && !strings.Contains(t, ":") {
			return nil, false // naming something new
		}
	}
	if len(prefix) < 2 {
		return nil, true // too short to look up yet: ask again
	}
	f := s.ix.File(path)
	if f == nil {
		return nil, false
	}
	es, truncated := s.ix.Unimported(f, prefix, maxUnimported, completionLockWait)
	if len(es) == 0 {
		return nil, truncated
	}
	syn := syntax.Parse(src) // the text as of this request
	enc2 := enc
	for _, e := range es {
		ed, ok := organize.AddImport(syn, e.Module, e.Name)
		if !ok {
			continue
		}
		te := &textEdit{Range: lines.Range(start, off, enc2), NewText: e.Name}
		it := entryItem(e, te)
		it.LabelDetails = map[string]any{"description": e.Module}
		it.Detail = "from " + e.Module + " import " + e.Name
		it.SortText = unimportedSortPrefix + e.Name
		it.AdditionalTextEdits = []textEdit{{Range: lines.Range(ed.Start, ed.End, enc2), NewText: ed.Text}}
		out = append(out, it)
	}
	return out, truncated
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// finishCompletion adds mojols' prepared items to the official server's
// answer to request id.
func (s *Server) finishCompletion(id jsonrpc.ID, result json.RawMessage) json.RawMessage {
	st := &s.completions
	st.mu.Lock()
	own, ok := st.byReq[id.String()]
	delete(st.byReq, id.String())
	st.mu.Unlock()
	if !ok || (len(own.items) == 0 && !own.incomplete) {
		return result
	}
	list := struct {
		IsIncomplete bool              `json:"isIncomplete"`
		Items        []json.RawMessage `json:"items"`
	}{}
	trimmed := strings.TrimSpace(string(result))
	switch {
	case trimmed == "" || trimmed == "null":
	case strings.HasPrefix(trimmed, "["):
		if json.Unmarshal(result, &list.Items) != nil {
			return result
		}
	default:
		if json.Unmarshal(result, &list) != nil {
			return result
		}
	}
	have := map[string]bool{}
	for _, raw := range list.Items {
		var it struct {
			Label string `json:"label"`
		}
		if json.Unmarshal(raw, &it) == nil {
			have[it.Label] = true
		}
	}
	for _, it := range own.items {
		if have[it.Label] {
			continue
		}
		raw, _ := json.Marshal(it)
		list.Items = append(list.Items, raw)
	}
	list.IsIncomplete = list.IsIncomplete || own.incomplete
	out, err := json.Marshal(list)
	if err != nil {
		return result
	}
	return out
}

// isMojolsItem reports whether a completionItem/resolve is for an item
// mojols made (already complete: answered with itself).
func isMojolsItem(params json.RawMessage) bool {
	var it struct {
		Data json.RawMessage `json:"data"`
	}
	return json.Unmarshal(params, &it) == nil && string(it.Data) == string(mojolsItemData)
}
