package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/obadafidii/mojols/server/internal/fakelsp"
	"github.com/obadafidii/mojols/server/internal/jsonrpc"
)

// The test binary doubles as the fake backend: mojols re-executes it with
// MOJOLS_FAKELSP=1, so every test drives real processes over real pipes.
func TestMain(m *testing.M) {
	if os.Getenv(fakelsp.EnvMarker) == "1" {
		os.Exit(fakelsp.Run(os.Stdin, os.Stdout, fakelsp.OptionsFromEnv()))
	}
	os.Exit(m.Run())
}

const docURI = "file:///ws/main.mojo"

type harness struct {
	t       *testing.T
	srv     *Server
	w       *jsonrpc.Writer
	clientW io.Closer
	inbox   chan *jsonrpc.Message
	backlog []*jsonrpc.Message
	done    chan int
	nextID  int64
}

func newHarness(t *testing.T, fakeEnv []string, tweak func(*Options)) *harness {
	t.Helper()
	srvIn, clientOut := io.Pipe()
	clientIn, srvOut := io.Pipe()
	logOut := io.Discard
	if os.Getenv("MOJOLS_TEST_LOG") == "1" {
		logOut = os.Stderr
	}
	opts := Options{
		Resolve: func([]string) (Command, error) {
			return Command{Path: os.Args[0], Args: []string{"-test.run=^$"}, Env: append([]string{fakelsp.EnvMarker + "=1", "GORACE=atexit_sleep_ms=0"}, fakeEnv...)}, nil
		},
		Logger:          slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Version:         "test",
		BackoffMin:      20 * time.Millisecond,
		BackoffMax:      200 * time.Millisecond,
		DegradedRetry:   time.Hour,
		InitTimeout:     10 * time.Second,
		ShutdownGrace:   time.Second,
		CrashLoopWindow: time.Minute,
		StdlibSources:   "off", // never download in tests
	}
	if tweak != nil {
		tweak(&opts)
	}
	h := &harness{
		t:       t,
		srv:     New(srvIn, srvOut, opts),
		w:       jsonrpc.NewWriter(clientOut),
		clientW: clientOut,
		inbox:   make(chan *jsonrpc.Message, 256),
		done:    make(chan int, 1),
	}
	go func() {
		h.done <- h.srv.Run(context.Background())
		srvOut.Close()
	}()
	go func() {
		r := jsonrpc.NewReader(clientIn)
		for {
			m, _, err := r.Read()
			if err != nil {
				close(h.inbox)
				return
			}
			h.inbox <- m
		}
	}()
	t.Cleanup(func() {
		clientOut.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop after the editor hung up")
		}
	})
	return h
}

func (h *harness) send(m *jsonrpc.Message) {
	h.t.Helper()
	if _, err := h.w.Write(m); err != nil {
		h.t.Fatalf("write to server: %v", err)
	}
}

func (h *harness) request(method string, params any) jsonrpc.ID {
	h.t.Helper()
	h.nextID++
	id := jsonrpc.NumberID(h.nextID)
	m, err := jsonrpc.NewRequest(id, method, params)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send(m)
	return id
}

func (h *harness) notify(method string, params any) {
	h.t.Helper()
	m, err := jsonrpc.NewNotification(method, params)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send(m)
}

// expect returns the first message (backlog first) matching pred.
func (h *harness) expect(what string, timeout time.Duration, pred func(*jsonrpc.Message) bool) *jsonrpc.Message {
	h.t.Helper()
	for i, m := range h.backlog {
		if pred(m) {
			h.backlog = append(h.backlog[:i], h.backlog[i+1:]...)
			return m
		}
	}
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-h.inbox:
			if !ok {
				h.t.Fatalf("server closed the connection while waiting for %s", what)
			}
			if pred(m) {
				return m
			}
			h.backlog = append(h.backlog, m)
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (h *harness) response(id jsonrpc.ID) *jsonrpc.Message {
	h.t.Helper()
	return h.expect("response to "+id.String(), 10*time.Second, func(m *jsonrpc.Message) bool {
		return m.IsResponse() && m.ID != nil && *m.ID == id
	})
}

func (h *harness) call(method string, params any) *jsonrpc.Message {
	h.t.Helper()
	return h.response(h.request(method, params))
}

func (h *harness) callOK(method string, params any, out any) {
	h.t.Helper()
	m := h.call(method, params)
	if m.Error != nil {
		h.t.Fatalf("%s failed: %v", method, m.Error)
	}
	if out != nil {
		if err := json.Unmarshal(m.Result, out); err != nil {
			h.t.Fatalf("%s result: %v", method, err)
		}
	}
}

func (h *harness) initialize() json.RawMessage {
	h.t.Helper()
	m := h.call("initialize", map[string]any{"processId": nil, "rootUri": "file:///ws", "capabilities": map[string]any{}})
	if m.Error != nil {
		h.t.Fatalf("initialize: %v", m.Error)
	}
	h.notify("initialized", map[string]any{})
	return m.Result
}

func (h *harness) open(text string) {
	h.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": docURI, "languageId": "mojo", "version": 1, "text": text}})
}

func (h *harness) change(version int, line, from, to int, text string) {
	h.notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{"uri": docURI, "version": version},
		"contentChanges": []any{map[string]any{
			"range": map[string]any{"start": map[string]any{"line": line, "character": from}, "end": map[string]any{"line": line, "character": to}},
			"text":  text,
		}},
	})
}

func (h *harness) hover() *jsonrpc.Message {
	h.t.Helper()
	return h.call("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": docURI}, "position": map[string]any{"line": 0, "character": 0}})
}

func hoverText(t *testing.T, m *jsonrpc.Message) string {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("hover failed: %v", m.Error)
	}
	var r struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r.Contents.Value
}

func (h *harness) status() Status {
	h.t.Helper()
	var st Status
	h.callOK("mojols/status", nil, &st)
	return st
}

func (h *harness) waitReady(gen int) Status {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := h.status(); st.BackendReady && st.BackendGen >= gen {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("backend generation %d never became ready: %+v", gen, h.status())
	return Status{}
}

func (h *harness) fakeState() fakelsp.State {
	h.t.Helper()
	var st fakelsp.State
	h.callOK("fake/state", nil, &st)
	return st
}

func (h *harness) exit() int {
	h.t.Helper()
	if m := h.call("shutdown", nil); m.Error != nil || string(m.Result) != "null" {
		h.t.Fatalf("shutdown: %+v", m)
	}
	h.notify("exit", nil)
	select {
	case code := <-h.done:
		h.done <- code // let Cleanup see it too
		return code
	case <-time.After(5 * time.Second):
		h.t.Fatal("server did not exit")
		return -1
	}
}

func wantError(t *testing.T, m *jsonrpc.Message, code int64) {
	t.Helper()
	if m.Error == nil || m.Error.Code != code {
		t.Fatalf("want error %d, got %+v (error %+v)", code, m, m.Error)
	}
}

// ---- tests -------------------------------------------------------------------

func TestPassThrough(t *testing.T) {
	h := newHarness(t, nil, nil)
	var init struct {
		ServerInfo struct{ Name, Version string } `json:"serverInfo"`
	}
	json.Unmarshal(h.initialize(), &init)
	if init.ServerInfo.Name != "mojols" || !strings.Contains(init.ServerInfo.Version, "fakelsp") {
		t.Fatalf("serverInfo: %+v", init.ServerInfo)
	}
	h.open("def main():\n    pass\n")
	if got := hoverText(t, h.hover()); got != "def main():\n    pass\n" {
		t.Fatalf("hover: %q", got)
	}
	h.change(2, 0, 4, 8, "😀go")
	if got := hoverText(t, h.hover()); got != "def 😀go():\n    pass\n" {
		t.Fatalf("hover after change: %q", got)
	}
	if st := h.fakeState(); !st.Initialized || st.Docs[docURI].Version != 2 {
		t.Fatalf("backend state: %+v", st)
	}
	if st := h.status(); st.Restarts != 0 || st.OpenDocuments != 1 || st.Pending != 0 {
		t.Fatalf("status: %+v", st)
	}
	if code := h.exit(); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
}

func TestNotInitialized(t *testing.T) {
	h := newHarness(t, nil, nil)
	wantError(t, h.hover(), jsonrpc.CodeServerNotInitialized)
}

func TestCrashRestartsAndReplays(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/hover", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker}, nil)
	h.initialize()
	h.open("abc")
	firstPID := h.status().BackendPID

	wantError(t, h.hover(), jsonrpc.CodeContentModified)
	h.change(2, 0, 3, 3, "def") // edited while the backend is down

	st := h.waitReady(2)
	if st.BackendPID == firstPID || st.Restarts != 1 {
		t.Fatalf("expected a fresh backend: %+v", st)
	}
	if got := hoverText(t, h.hover()); got != "abcdef" {
		t.Fatalf("replayed document is %q, want the edit made during the outage", got)
	}
	fs := h.fakeState()
	if !fs.Initialized || fs.Docs[docURI].Version != 2 {
		t.Fatalf("new backend not brought to editor state: %+v", fs)
	}
	if code := h.exit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestCrashLoopEntersDegradedMode(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=initialize"}, func(o *Options) { o.BackoffMin = time.Millisecond })
	var init struct {
		Capabilities map[string]any                 `json:"capabilities"`
		ServerInfo   struct{ Name, Version string } `json:"serverInfo"`
	}
	json.Unmarshal(h.initialize(), &init)
	if !strings.Contains(init.ServerInfo.Version, "degraded") || init.Capabilities["hoverProvider"] != nil {
		t.Fatalf("expected minimal degraded capabilities, got %+v", init)
	}
	h.expect("showMessage error", 5*time.Second, func(m *jsonrpc.Message) bool {
		return m.Method == "window/showMessage" && strings.Contains(string(m.Params), "degraded")
	})
	h.open("x")
	// Degraded: answered by mojols itself (no error for the editor to
	// log); here there is nothing to describe, so null.
	if m := h.hover(); m.Error != nil || string(m.Result) != "null" {
		t.Fatalf("degraded hover: %v %s", m.Error, m.Result)
	}
	// A method mojols cannot answer itself still says why.
	wantError(t, h.call("textDocument/references", nil), jsonrpc.CodeRequestFailed)
	if st := h.status(); !st.Degraded || st.Restarts != 5 {
		t.Fatalf("status: %+v", st)
	}
	if code := h.exit(); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestMissingBackendDegradesWithoutHanging(t *testing.T) {
	h := newHarness(t, nil, func(o *Options) {
		o.BackoffMin = time.Millisecond
		o.Resolve = func([]string) (Command, error) { return Command{}, os.ErrNotExist }
	})
	h.initialize()
	if st := h.status(); !st.Degraded {
		t.Fatalf("status: %+v", st)
	}
}

func TestHangIsDetectedAndKilled(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_HANG_ON=textDocument/hover"}, func(o *Options) {
		o.Timeouts = map[string]time.Duration{"textDocument/hover": 100 * time.Millisecond}
	})
	h.initialize()
	h.open("abc")
	pid := h.status().BackendPID
	for i := 0; i < 3; i++ {
		wantError(t, h.hover(), jsonrpc.CodeRequestFailed)
	}
	st := h.waitReady(2)
	if st.BackendPID == pid || st.Timeouts != 3 {
		t.Fatalf("hung backend not replaced: %+v", st)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("hung backend %d still alive", pid)
	}
	if fs := h.fakeState(); fs.Docs[docURI].Text != "abc" {
		t.Fatalf("replay after hang: %+v", fs)
	}
}

func TestGarbageReplyIsContained(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_GARBAGE_ON=textDocument/hover"}, func(o *Options) {
		o.Timeouts = map[string]time.Duration{"textDocument/hover": 100 * time.Millisecond}
	})
	h.initialize()
	h.open("abc")
	wantError(t, h.hover(), jsonrpc.CodeRequestFailed)
	st := h.status()
	if st.DroppedMalformed != 1 || st.Restarts != 0 {
		t.Fatalf("status: %+v", st)
	}
	h.fakeState() // backend still serving
}

func TestCancelReachesBackend(t *testing.T) {
	h := newHarness(t, []string{"MOJOLS_FAKE_HANG_ON=textDocument/hover"}, nil)
	h.initialize()
	h.open("abc")
	id := h.request("textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": docURI}, "position": map[string]any{"line": 0, "character": 0}})
	h.notify("$/cancelRequest", map[string]any{"id": id})
	wantError(t, h.response(id), jsonrpc.CodeRequestCancelled)
	if fs := h.fakeState(); len(fs.Cancelled) != 1 {
		t.Fatalf("backend saw cancels %v", fs.Cancelled)
	}
}

func TestServerRequestRoundTrip(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize()
	h.callOK("fake/ask", nil, nil)
	req := h.expect("workspace/configuration", 5*time.Second, func(m *jsonrpc.Message) bool {
		return m.Method == "workspace/configuration"
	})
	if req.ID.IsString() {
		t.Fatalf("backend's string id leaked to the editor: %v", req.ID)
	}
	resp, _ := jsonrpc.NewResponse(*req.ID, []any{map[string]any{"includeDirs": []string{"src"}}})
	h.send(resp)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fs := h.fakeState(); len(fs.ClientResponses) == 1 {
			if !strings.Contains(string(fs.ClientResponses[0]), `"id":"srv-1"`) {
				t.Fatalf("backend got the reply under the wrong id: %s", fs.ClientResponses[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backend never received the editor's reply")
}

func TestNotificationsFlowToEditor(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize()
	h.notify("fake/log", nil)
	h.expect("logMessage", 5*time.Second, func(m *jsonrpc.Message) bool { return m.Method == "window/logMessage" })
}

func TestEditorHangupKillsBackend(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize()
	pid := h.status().BackendPID
	h.clientW.Close()
	select {
	case code := <-h.done:
		h.done <- code
		if code != 1 {
			t.Fatalf("exit code %d, want 1 without shutdown", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server kept running after the editor hung up")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("backend %d orphaned", pid)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A crash's stack dump is the only way to get it fixed upstream, and it
// only exists in the backend's stderr: it must be logged even at info level.
func TestCrashOutputIsLoggedAtInfo(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	logs := &syncBuffer{}
	h := newHarness(t, []string{"MOJOLS_FAKE_CRASH_ON=textDocument/hover", "MOJOLS_FAKE_CRASH_ONCE_FILE=" + marker}, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	h.initialize()
	h.open("x")
	h.hover()
	h.waitReady(2)
	out := logs.String()
	if !strings.Contains(out, "backend crash output") || !strings.Contains(out, "Crash handling textDocument/hover in fakelsp") {
		t.Fatalf("stack dump not in the info-level log:\n%s", out)
	}
	if strings.Contains(out, "unrelated log line") {
		t.Fatalf("crash report should start at the crash banner:\n%s", out)
	}
}
