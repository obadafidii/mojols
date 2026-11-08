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
//	MOJOLS_FAKE_CRASH_ON_TEXT=<marker>  segfault-like exit(139) when an opened or changed document contains <marker>
//
// Custom methods for tests: fake/state (request) returns what the fake has
// seen; fake/ask (request) makes the fake send workspace/configuration to the
// client; fake/log (notification) makes it send window/logMessage.
package fakelsp

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
	"github.com/tdadadavid/mojols/server/internal/text"
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
	Opens           map[string]int      `json:"opens"` // didOpen count per URI
}

type fake struct {
	opts  Options
	w     *jsonrpc.Writer
	docs  *text.Store
	mu    sync.Mutex
	state State
	hung  map[jsonrpc.ID]bool
	asked int
}

// Run serves LSP on in/out until exit, returning the process exit code.
func Run(in io.Reader, out io.Writer, opts Options) int {
	if opts.ExitOnStart {
		return 3
	}
	f := &fake{
		opts:  opts,
		w:     jsonrpc.NewWriter(out),
		docs:  text.NewStore(),
		state: State{PID: os.Getpid(), Docs: map[string]DocState{}, Args: os.Args[1:], Opens: map[string]int{}},
		hung:  map[jsonrpc.ID]bool{},
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
}

func (f *fake) handle(m *jsonrpc.Message) {
	if m.IsRequest() && m.Method == f.opts.HangOn {
		f.mu.Lock()
		f.hung[*m.ID] = true
		f.mu.Unlock()
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
		f.publishDiagnostics(uri)
	case "textDocument/didChange":
		_ = f.docs.Change(uri, p.TextDocument.Version, p.ContentChanges)
		f.crashOnText(uri)
		f.publishDiagnostics(uri)
	case "textDocument/didClose":
		f.docs.Close(uri)
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

// publishDiagnostics reports no problems, like the real server after a clean parse.
func (f *fake) publishDiagnostics(uri string) {
	n, _ := jsonrpc.NewNotification("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": []any{}})
	f.w.Write(n)
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
