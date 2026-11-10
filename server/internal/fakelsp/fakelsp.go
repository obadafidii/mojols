// Package fakelsp is a scriptable stand-in for mojo-lsp-server.
//
// It speaks enough LSP to drive mojols end to end, and it can be told to
// crash, hang or reply with garbage on cue. Every row of the robustness table
// in the design doc has a deterministic test built on it.
//
// Behaviour is configured through environment variables so each spawned
// process can be scripted independently:
//
//	MOJOLS_FAKE_CRASH_ON=<method>       exit(2) on receiving <method>
//	MOJOLS_FAKE_CRASH_ONCE_FILE=<path>  only crash if <path> does not exist (it is created first)
//	MOJOLS_FAKE_HANG_ON=<method>        never answer <method> (until cancelled)
//	MOJOLS_FAKE_GARBAGE_ON=<method>     answer <method> with an unparseable body
//	MOJOLS_FAKE_EXIT_ON_START=1         exit(3) before reading anything
//	MOJOLS_FAKE_DEFS=<name>=<path>:<line>:<char>;...
//	                                    answer textDocument/definition on an
//	                                    identifier <name> with that location
//	MOJOLS_FAKE_CRASH_ON_TEXT=<marker>  segfault-like exit(139) when an opened or changed document contains <marker>
//	MOJOLS_FAKE_MODIFIED_ONCE_ON=<method> answer the first <method> request with ContentModified
//	MOJOLS_FAKE_PARSE_MS=<n>            publish diagnostics n ms after an open or change, like a real parse,
//	                                    and count requests that arrive for a document still being parsed
//
// Custom methods for tests: fake/state (request) returns what the fake has
// seen; fake/ask (request) makes the fake send workspace/configuration to the
// client; fake/log (notification) makes it send window/logMessage.
package fakelsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/text"
)

// EnvMarker is set by test binaries that re-exec themselves as the fake.
const EnvMarker = "MOJOLS_FAKELSP"

// Options scripts the fake's failures.
type Options struct {
	CrashOn       string
	CrashOnceFile string
	HangOn        string
	GarbageOn     string
	ExitOnStart   bool
	CrashOnText   string
	ParseDelay    time.Duration
	ModifiedOnce  string
}

// OptionsFromEnv reads Options from MOJOLS_FAKE_* variables.
func OptionsFromEnv() Options {
	return Options{
		CrashOn:       os.Getenv("MOJOLS_FAKE_CRASH_ON"),
		CrashOnceFile: os.Getenv("MOJOLS_FAKE_CRASH_ONCE_FILE"),
		HangOn:        os.Getenv("MOJOLS_FAKE_HANG_ON"),
		GarbageOn:     os.Getenv("MOJOLS_FAKE_GARBAGE_ON"),
		ExitOnStart:   os.Getenv("MOJOLS_FAKE_EXIT_ON_START") == "1",
		CrashOnText:   os.Getenv("MOJOLS_FAKE_CRASH_ON_TEXT"),
		ParseDelay:    envMillis("MOJOLS_FAKE_PARSE_MS"),
		ModifiedOnce:  os.Getenv("MOJOLS_FAKE_MODIFIED_ONCE_ON"),
	}
}

// DocState is one document as the fake sees it.
type DocState struct {
	Version int32  `json:"version"`
	Text    string `json:"text"`
}

// State is the fake/state result.
type State struct {
	PID             int                 `json:"pid"`
	Initialized     bool                `json:"initialized"`
	Docs            map[string]DocState `json:"docs"`
	ClientResponses []json.RawMessage   `json:"clientResponses"`
	Cancelled       []string            `json:"cancelled"`
	Args            []string            `json:"args"`
	Opens           map[string]int      `json:"opens"`       // didOpen count per URI
	Overlaps        int                 `json:"overlaps"`    // requests on a document still being parsed
	Definitions     int                 `json:"definitions"` // textDocument/definition requests answered
}

type fake struct {
	opts     Options
	w        *jsonrpc.Writer
	docs     *text.Store
	mu       sync.Mutex
	state    State
	hung     map[jsonrpc.ID]bool
	asked    int
	parsing  map[string]int32 // uri -> version being parsed (ParseDelay)
	modified bool             // ModifiedOnce already answered
}

// Run serves LSP on in/out until exit, returning the process exit code.
func Run(in io.Reader, out io.Writer, opts Options) int {
	if opts.ExitOnStart {
		return 3
	}
	f := &fake{
		opts:    opts,
		w:       jsonrpc.NewWriter(out),
		docs:    text.NewStore(),
		state:   State{PID: os.Getpid(), Docs: map[string]DocState{}, Args: os.Args[1:], Opens: map[string]int{}},
		hung:    map[jsonrpc.ID]bool{},
		parsing: map[string]int32{},
	}
	r := jsonrpc.NewReader(in)
	shutdown := false
	for {
		m, _, err := r.Read()
		var de *jsonrpc.DecodeError
		if errors.As(err, &de) {
			continue
		}
		if err != nil {
			return 1 // stdin closed without exit, like the real server
		}
		if m.Method != "" && m.Method == opts.CrashOn && f.shouldCrash() {
			fmt.Fprintln(os.Stderr, "I[00:00:00.000] unrelated log line")
			fmt.Fprintln(os.Stderr, "Please submit a bug report to https://github.com/modular/modular/issues")
			fmt.Fprintln(os.Stderr, "Stack dump:")
			fmt.Fprintf(os.Stderr, "0.\tCrash handling %s in fakelsp\n", m.Method)
			return 2
		}
		switch {
		case m.IsResponse():
			raw, _ := m.MarshalJSON()
			f.mu.Lock()
			f.state.ClientResponses = append(f.state.ClientResponses, raw)
			f.mu.Unlock()
		case m.Method == "exit":
			if shutdown {
				return 0
			}
			return 1
		case m.Method == "shutdown":
			shutdown = true
			f.reply(m, nil)
		default:
			f.handle(m)
		}
	}
}

func (f *fake) shouldCrash() bool {
	if f.opts.CrashOnceFile == "" {
		return true
	}
	if _, err := os.Stat(f.opts.CrashOnceFile); err == nil {
		return false
	}
	_ = os.WriteFile(f.opts.CrashOnceFile, []byte("crashed\n"), 0o644)
	return true
}

func (f *fake) reply(m *jsonrpc.Message, result any) {
	resp, err := jsonrpc.NewResponse(*m.ID, result)
	if err == nil {
		f.w.Write(resp)
	}
}

type docParams struct {
	TextDocument struct {
		URI        string `json:"uri"`
		LanguageID string `json:"languageId"`
		Version    int32  `json:"version"`
		Text       string `json:"text"`
	} `json:"textDocument"`
	ContentChanges []text.ContentChange `json:"contentChanges"`
	Position       text.Position        `json:"position"`
}

func (f *fake) handle(m *jsonrpc.Message) {
	if m.IsRequest() && m.Method == f.opts.HangOn {
		f.mu.Lock()
		f.hung[*m.ID] = true
		f.mu.Unlock()
		return
	}
	if m.IsRequest() && m.Method == f.opts.ModifiedOnce && !f.modified {
		f.modified = true
		f.w.Write(jsonrpc.NewErrorResponse(m.ID, jsonrpc.NewError(jsonrpc.CodeContentModified, "content modified")))
		return
	}
	if m.IsRequest() && m.Method == f.opts.GarbageOn {
		f.w.WriteRaw([]byte(`{"jsonrpc":"2.0","id":` + m.ID.String() + `,"result":{oops`))
		return
	}
	var p docParams
	if len(m.Params) > 0 {
		_ = json.Unmarshal(m.Params, &p)
	}
	uri := p.TextDocument.URI
	if m.IsRequest() && uri != "" {
		f.mu.Lock()
		if _, busy := f.parsing[uri]; busy {
			f.state.Overlaps++
		}
		f.mu.Unlock()
	}
	switch m.Method {
	case "initialize":
		f.reply(m, map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":   map[string]any{"openClose": true, "change": 2, "save": true},
				"hoverProvider":      true,
				"definitionProvider": true,
				"positionEncoding":   "utf-16",
			},
			"serverInfo": map[string]any{"name": "fakelsp", "version": "0"},
		})
	case "initialized":
		f.mu.Lock()
		f.state.Initialized = true
		f.mu.Unlock()
	case "textDocument/didOpen":
		f.docs.Open(text.Document{URI: uri, LanguageID: p.TextDocument.LanguageID, Version: p.TextDocument.Version, Text: p.TextDocument.Text})
		f.mu.Lock()
		f.state.Opens[uri]++
		f.mu.Unlock()
		f.crashOnText(uri)
		f.parsed(uri)
	case "textDocument/didChange":
		_ = f.docs.Change(uri, p.TextDocument.Version, p.ContentChanges)
		f.crashOnText(uri)
		f.parsed(uri)
	case "textDocument/didClose":
		f.docs.Close(uri)
		f.mu.Lock()
		delete(f.parsing, uri)
		f.mu.Unlock()
		f.publishDiagnostics(uri) // the real server clears a closed document's diagnostics
	case "textDocument/hover":
		d, ok := f.docs.Get(uri)
		if !ok {
			f.w.Write(jsonrpc.NewErrorResponse(m.ID, jsonrpc.NewError(jsonrpc.CodeInvalidParams, "unknown document %s", uri)))
			return
		}
		f.reply(m, map[string]any{"contents": map[string]any{"kind": "plaintext", "value": d.Text}})
	case "$/cancelRequest":
		var c struct {
			ID jsonrpc.ID `json:"id"`
		}
		if json.Unmarshal(m.Params, &c) != nil {
			return
		}
		f.mu.Lock()
		hung := f.hung[c.ID]
		delete(f.hung, c.ID)
		f.state.Cancelled = append(f.state.Cancelled, c.ID.String())
		f.mu.Unlock()
		if hung {
			id := c.ID
			f.w.Write(jsonrpc.NewErrorResponse(&id, jsonrpc.NewError(jsonrpc.CodeRequestCancelled, "cancelled")))
		}
	case "textDocument/codeAction":
		f.reply(m, []any{map[string]any{"title": "fake quick fix", "kind": "quickfix"}})
	case "textDocument/definition":
		f.mu.Lock()
		f.state.Definitions++
		f.mu.Unlock()
		d, _ := f.docs.Get(uri)
		word := wordAt(d.Text, p.Position, f.docs.Encoding())
		for _, def := range strings.Split(os.Getenv("MOJOLS_FAKE_DEFS"), ";") {
			name, loc, ok := strings.Cut(def, "=")
			if !ok || name != word {
				continue
			}
			parts := strings.Split(loc, ":")
			if len(parts) != 3 {
				continue
			}
			line, _ := strconv.Atoi(parts[1])
			char, _ := strconv.Atoi(parts[2])
			pos := map[string]any{"line": line, "character": char}
			f.reply(m, []any{map[string]any{"uri": "file://" + parts[0], "range": map[string]any{"start": pos, "end": pos}}})
			return
		}
		f.reply(m, []any{})
	case "textDocument/completion":
		d, _ := f.docs.Get(uri)
		f.reply(m, map[string]any{"isIncomplete": false, "items": []any{map[string]any{"label": "fake:" + d.Text}}})
	case "textDocument/signatureHelp":
		f.reply(m, map[string]any{"signatures": []any{map[string]any{"label": "f()"}}})
	case "fake/state":
		f.mu.Lock()
		st := f.state
		st.Docs = map[string]DocState{}
		for _, d := range f.docs.Snapshot() {
			st.Docs[d.URI] = DocState{Version: d.Version, Text: d.Text}
		}
		st.ClientResponses = append([]json.RawMessage(nil), f.state.ClientResponses...)
		st.Cancelled = append([]string(nil), f.state.Cancelled...)
		st.Opens = map[string]int{}
		for k, v := range f.state.Opens {
			st.Opens[k] = v
		}
		f.mu.Unlock()
		f.reply(m, st)
	case "fake/ask":
		f.mu.Lock()
		f.asked++
		id := jsonrpc.StringID("srv-" + string(rune('0'+f.asked)))
		f.mu.Unlock()
		req, _ := jsonrpc.NewRequest(id, "workspace/configuration", map[string]any{"items": []any{map[string]any{"section": "mojo"}}})
		f.w.Write(req)
		f.reply(m, nil)
	case "fake/log":
		n, _ := jsonrpc.NewNotification("window/logMessage", map[string]any{"type": 3, "message": "hello from fake"})
		f.w.Write(n)
	default:
		if m.IsRequest() {
			f.w.Write(jsonrpc.NewErrorResponse(m.ID, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "fakelsp: %s", m.Method)))
		}
	}
}

// publishDiagnostics reports one warning if the document contains WARN,
// otherwise none; a closed document gets an empty list, like the real server.
func (f *fake) publishDiagnostics(uri string) {
	diags := []any{}
	if d, ok := f.docs.Get(uri); ok && strings.Contains(d.Text, "WARN") {
		diags = append(diags, map[string]any{
			"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 4}},
			"severity": 2, "message": "fake warning",
		})
	}
	params := map[string]any{"uri": uri, "diagnostics": diags}
	if d, ok := f.docs.Get(uri); ok {
		params["version"] = d.Version
	}
	n, _ := jsonrpc.NewNotification("textDocument/publishDiagnostics", params)
	f.w.Write(n)
}

// parsed publishes diagnostics for uri now, or after ParseDelay, marking the
// document busy until then, like a real parse.
func (f *fake) parsed(uri string) {
	if f.opts.ParseDelay == 0 {
		f.publishDiagnostics(uri)
		return
	}
	d, ok := f.docs.Get(uri)
	if !ok {
		return
	}
	f.mu.Lock()
	f.parsing[uri] = d.Version
	f.mu.Unlock()
	go func(version int32) {
		time.Sleep(f.opts.ParseDelay)
		f.mu.Lock()
		current := f.parsing[uri] == version
		if current {
			delete(f.parsing, uri)
		}
		f.mu.Unlock()
		if current {
			f.publishDiagnostics(uri)
		}
	}(d.Version)
}

func envMillis(name string) time.Duration {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// crashOnText dies, like the real server's parser, on a poisoned document.
func (f *fake) crashOnText(uri string) {
	if f.opts.CrashOnText == "" {
		return
	}
	if d, ok := f.docs.Get(uri); ok && strings.Contains(d.Text, f.opts.CrashOnText) {
		os.Exit(139)
	}
}

// wordAt returns the identifier at p in content.
func wordAt(content string, p text.Position, enc text.Encoding) string {
	off := text.Offset(content, p, enc)
	isWord := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	a, b := off, off
	for a > 0 && isWord(content[a-1]) {
		a--
	}
	for b < len(content) && isWord(content[b]) {
		b++
	}
	return content[a:b]
}
