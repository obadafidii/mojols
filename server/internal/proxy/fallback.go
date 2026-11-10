package proxy

import (
	"encoding/json"

	"github.com/obadafidii/mojols/server/internal/index"
	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/syntax"
)

// fallbackResult answers a request mojo-lsp-server cannot serve right now
// (the file is quarantined, or mojols is in degraded mode) from mojols'
// own index where it can (hover, document symbols), and otherwise with a
// valid empty result rather than an error, so the editor does not log a
// failure for every keystroke. ok is false for requests it does not know.
func (s *Server) fallbackResult(m *jsonrpc.Message) (any, bool) {
	switch m.Method {
	case "textDocument/hover":
		return s.indexHover(m), true
	case "textDocument/documentSymbol":
		return s.indexOutline(m), true
	case "textDocument/codeAction":
		actions := []any{}
		for _, a := range s.actions(m.Params) {
			actions = append(actions, a)
		}
		return actions, true
	case "textDocument/inlayHint", "textDocument/documentHighlight",
		"textDocument/foldingRange", "textDocument/codeLens", "textDocument/documentLink",
		"textDocument/selectionRange":
		return []any{}, true
	case "textDocument/completion":
		return map[string]any{"isIncomplete": true, "items": []any{}}, true
	case "textDocument/semanticTokens/full", "textDocument/semanticTokens/range":
		return map[string]any{"data": []any{}}, true
	case "textDocument/signatureHelp", "textDocument/semanticTokens/full/delta", "textDocument/formatting",
		"textDocument/rangeFormatting":
		return nil, true
	}
	return nil, false
}

// fallbackReply is fallbackResult as a response to m, or nil.
func (s *Server) fallbackReply(m *jsonrpc.Message, id jsonrpc.ID) *jsonrpc.Message {
	if s.ix == nil {
		return nil
	}
	res, ok := s.fallbackResult(m)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	return &jsonrpc.Message{ID: &id, Result: raw}
}

func (s *Server) indexFile(m *jsonrpc.Message) (*index.File, int) {
	var p indexParams
	if json.Unmarshal(m.Params, &p) != nil {
		return nil, 0
	}
	path, ok := uriToPath(p.TextDocument.URI)
	if !ok {
		return nil, 0
	}
	f := s.ix.File(path)
	if f == nil {
		return nil, 0
	}
	return f, f.Lines.Offset(p.Position, s.docs.Encoding())
}

func (s *Server) indexHover(m *jsonrpc.Message) any {
	f, offset := s.indexFile(m)
	if f == nil {
		return nil
	}
	md, sp, ok := s.ix.HoverAt(f, offset)
	if !ok {
		return nil
	}
	return map[string]any{
		"contents": map[string]any{"kind": "markdown", "value": md},
		"range":    f.Lines.Range(sp.Start, sp.End, s.docs.Encoding()),
	}
}

func (s *Server) indexOutline(m *jsonrpc.Message) any {
	f, _ := s.indexFile(m)
	if f == nil {
		return []any{}
	}
	enc := s.docs.Encoding()
	var conv func([]index.Outline) []map[string]any
	conv = func(os []index.Outline) []map[string]any {
		out := []map[string]any{}
		for _, o := range os {
			d := o.Decl
			kind := symFunction
			switch d.Kind {
			case syntax.KStruct:
				kind = symStruct
			case syntax.KTrait:
				kind = symInterface
			case syntax.KComptime:
				kind = symConstant
			case syntax.KField:
				kind = symField
			case syntax.KFunc:
				if d.Parent != nil {
					kind = symMethod
				}
			}
			end := max(d.Span.End, d.NameSpan.End)
			item := map[string]any{
				"name":           d.Name,
				"kind":           kind,
				"range":          f.Lines.Range(min(d.Span.Start, d.NameSpan.Start), end, enc),
				"selectionRange": f.Lines.Range(d.NameSpan.Start, d.NameSpan.End, enc),
			}
			if len(o.Children) > 0 {
				item["children"] = conv(o.Children)
			}
			out = append(out, item)
		}
		return out
	}
	return conv(index.OutlineOf(f))
}
