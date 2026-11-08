// Package proxy is the core of mojols: an LSP server that supervises
// mojo-lsp-server and stands between it and the editor.
//
// The rule it enforces: the backend is allowed to fail; mojols is not.
//
//   - Every request the editor sends gets exactly one answer, even if the
//     backend crashes, hangs, or replies with garbage.
//   - A crashed backend is restarted with exponential backoff and brought back
//     to the editor's exact state: initialize, initialized, then every open
//     document replayed from mojols' own copy.
//   - Repeated crashes put mojols in degraded mode instead of a restart storm.
//   - Request ids are remapped in both directions, so a restarted backend can
//     never confuse a late reply with a new request.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/tdadadavid/mojols/server/internal/discover"
	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
	"github.com/tdadadavid/mojols/server/internal/text"
	"github.com/tdadadavid/mojols/server/internal/trace"
)

// Options configures a Server. Zero values take the defaults below.
type Options struct {
	// Resolve returns the backend command for the given workspace roots.
	// It runs before every (re)start, so a toolchain installed while the
	// editor is open is picked up on the next restart.
	Resolve func(roots []string) (Command, error)

	Logger  *slog.Logger
	Tracer  *trace.Tracer
	Version string

	Timeouts        map[string]time.Duration // per-method request deadline
	DefaultTimeout  time.Duration            // 15s
	HangThreshold   int                      // consecutive timeouts before the backend is killed: 3
	InitTimeout     time.Duration            // 60s
	BackoffMin      time.Duration            // 250ms
	BackoffMax      time.Duration            // 30s
	CrashLoopLimit  int                      // 5 failures ...
	CrashLoopWindow time.Duration            // ... within 2m enters degraded mode
	DegradedRetry   time.Duration            // 2m
	ShutdownGrace   time.Duration            // 2s
	ProbeTimeout    time.Duration            // how long a crash suspect may take to parse alone: 30s
	NoRefresh       bool                     // do not re-open documents whose imports changed on disk
	RefreshDebounce time.Duration            // wait for saves to settle before refreshing: 150ms
}

// DefaultTimeouts are deliberately generous: the official server parses a
// file on first use, and a cold parse must not look like a hang.
var DefaultTimeouts = map[string]time.Duration{
	"textDocument/hover":         5 * time.Second,
	"textDocument/signatureHelp": 5 * time.Second,
	"textDocument/completion":    10 * time.Second,
	"textDocument/definition":    10 * time.Second,
	"textDocument/references":    20 * time.Second,
	"textDocument/rename":        20 * time.Second,
}

func (o *Options) setDefaults() {
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Timeouts == nil {
		o.Timeouts = DefaultTimeouts
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.DefaultTimeout, 15*time.Second)
	def(&o.InitTimeout, 60*time.Second)
	def(&o.BackoffMin, 250*time.Millisecond)
	def(&o.BackoffMax, 30*time.Second)
	def(&o.CrashLoopWindow, 2*time.Minute)
	def(&o.DegradedRetry, 2*time.Minute)
	def(&o.ShutdownGrace, 2*time.Second)
	def(&o.ProbeTimeout, 30*time.Second)
	def(&o.RefreshDebounce, 150*time.Millisecond)
	if o.HangThreshold == 0 {
		o.HangThreshold = 3
	}
	if o.CrashLoopLimit == 0 {
		o.CrashLoopLimit = 5
	}
	if o.Version == "" {
		o.Version = "dev"
	}
}

type phase int

const (
	phaseUninitialized phase = iota
	phaseInitializing
	phaseRunning
)

// pending is one request mojols is waiting on the backend to answer.
type pending struct {
	id       int64
	clientID jsonrpc.ID
	method   string
	gen      int // 0 while queued for a backend that is not ready
	msg      *jsonrpc.Message
	timer    *time.Timer
	internal chan *jsonrpc.Message // non-nil for requests mojols itself sent
}

// serverReq is a request from the backend that mojols forwarded to the editor.
type serverReq struct {
	gen       int
	backendID jsonrpc.ID
}

// Status is the result of the mojols/status request.
type Status struct {
	Version          string   `json:"version"`
	Phase            string   `json:"phase"`
	BackendGen       int      `json:"backendGeneration"`
	BackendPID       int      `json:"backendPid,omitempty"`
	BackendReady     bool     `json:"backendReady"`
	Degraded         bool     `json:"degraded"`
	OpenDocuments    int      `json:"openDocuments"`
	Pending          int      `json:"pendingRequests"`
	Queued           int      `json:"queuedMessages"`
	Restarts         int      `json:"restarts"`
	Timeouts         int      `json:"timeouts"`
	DroppedMalformed int      `json:"droppedMalformed"`
	PositionEncoding string   `json:"positionEncoding"`
	LastBackendError string   `json:"lastBackendError,omitempty"`
	Quarantined      []string `json:"quarantined,omitempty"`
	Probing          string   `json:"probing,omitempty"`
	Suspects         []string `json:"suspects,omitempty"`
	Refreshes        int      `json:"dependentRefreshes"`
}

// Server is one editor session.
type Server struct {
	opts Options
	log  *slog.Logger
	in   *jsonrpc.Reader
	out  *jsonrpc.Writer
	docs *text.Store

	mu                sync.Mutex
	phase             phase
	initParams        json.RawMessage
	initID            *jsonrpc.ID // the editor's initialize request, until answered
	initResult        json.RawMessage
	clientInitialized bool
	roots             []string

	be         *backend
	gen        int
	failedGen  int
	ready      bool
	queue      []*pending         // requests waiting for a ready backend, in order
	queuedNote []*jsonrpc.Message // notifications waiting for a ready backend

	nextID     int64
	pending    map[int64]*pending
	byClient   map[jsonrpc.ID]int64
	serverReqs map[int64]serverReq

	failures      []time.Time
	backoffStep   int
	timeoutStreak int
	degraded      bool
	restartTimer  *time.Timer
	shutdown      bool
	exited        bool

	restarts, timeouts, malformed int
	lastErr                       string

	// Crash attribution and quarantine (quarantine.go).
	health     map[string]*docHealth
	quarantine map[string]*quarantined
	probeQueue []string
	probing    string
	probeTimer *time.Timer

	// Cross-file invalidation (deps.go).
	deps         *depGraph
	changed      map[string]bool
	refreshTimer *time.Timer
	refreshes    int
	watching     bool
}

// New returns a server reading the editor's messages from in and writing to out.
func New(in io.Reader, out io.Writer, opts Options) *Server {
	opts.setDefaults()
	s := &Server{
		opts:       opts,
		log:        opts.Logger,
		in:         jsonrpc.NewReader(in),
		out:        jsonrpc.NewWriter(out),
		docs:       text.NewStore(),
		pending:    map[int64]*pending{},
		byClient:   map[jsonrpc.ID]int64{},
		serverReqs: map[int64]serverReq{},
		health:     map[string]*docHealth{},
		quarantine: map[string]*quarantined{},
		changed:    map[string]bool{},
	}
	s.deps = newDepGraph(func(path string) (string, bool) {
		for _, d := range s.docs.Snapshot() {
			if p, ok := uriToPath(d.URI); ok && p == path {
				return d.Text, true
			}
		}
		return "", false
	})
	return s
}

// Run serves the session until the editor sends exit or closes stdin. It
// returns the process exit code required by the LSP specification.
func (s *Server) Run(ctx context.Context) int {
	msgs := make(chan *jsonrpc.Message)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			m, body, err := s.in.Read()
			var de *jsonrpc.DecodeError
			if errors.As(err, &de) {
				s.opts.Tracer.Log(trace.ClientToProxy, 0, de.Body)
				s.log.Warn("dropped malformed message from editor", "err", de.Reason)
				continue
			}
			if err != nil {
				readErr <- err
				return
			}
			s.opts.Tracer.Log(trace.ClientToProxy, 0, body)
			select {
			case msgs <- m:
			case <-stop:
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			s.teardown()
			return 1
		case err := <-readErr:
			if !errors.Is(err, io.EOF) {
				s.log.Error("reading from editor failed", "err", err)
			} else {
				s.log.Info("editor closed the connection without exit")
			}
			s.teardown()
			return 1
		case m := <-msgs:
			if m.Method == "exit" {
				s.mu.Lock()
				code := 1
				if s.shutdown {
					code = 0
				}
				s.mu.Unlock()
				s.teardown()
				return code
			}
			s.handleClient(m)
		}
	}
}

// ---- editor -> mojols ------------------------------------------------------

func (s *Server) handleClient(m *jsonrpc.Message) {
	if m.IsResponse() {
		s.handleClientResponse(m)
		return
	}
	s.mu.Lock()
	ph := s.phase
	s.mu.Unlock()

	if m.Method == "initialize" {
		s.handleInitialize(m)
		return
	}
	if ph == phaseUninitialized {
		if m.IsRequest() {
			s.replyError(*m.ID, jsonrpc.CodeServerNotInitialized, "mojols: initialize has not been called")
		}
		return
	}
	switch m.Method {
	case "initialized":
		s.mu.Lock()
		s.clientInitialized = true
		if s.ready {
			s.sendBackendLocked(m)
		}
		reg := s.registerWatchersLocked()
		s.mu.Unlock()
		if reg != nil {
			s.write(reg)
		}
		return
	case "shutdown":
		s.mu.Lock()
		s.shutdown = true
		s.mu.Unlock()
		go s.handleShutdown(*m.ID)
		return
	case "mojols/status":
		s.reply(*m.ID, s.Status())
		return
	case "$/cancelRequest":
		s.handleCancel(m)
		return
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didClose", "textDocument/didSave":
		s.handleDocSync(m)
		return
	}
	if m.Method == "workspace/didChangeWatchedFiles" {
		var p struct {
			Changes []struct {
				URI string `json:"uri"`
			} `json:"changes"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			s.mu.Lock()
			for _, c := range p.Changes {
				s.noteChangedLocked(c.URI)
			}
			s.mu.Unlock()
		}
		// Also forwarded below, in case the backend ever watches files itself.
	}
	if m.IsNotification() {
		s.mu.Lock()
		switch {
		case s.ready:
			s.sendBackendLocked(m)
		case !s.degraded:
			s.queuedNote = append(s.queuedNote, m)
		}
		s.mu.Unlock()
		return
	}
	s.handleRequest(m)
}

func (s *Server) handleInitialize(m *jsonrpc.Message) {
	s.mu.Lock()
	if s.phase != phaseUninitialized {
		s.mu.Unlock()
		s.replyError(*m.ID, jsonrpc.CodeInvalidRequest, "mojols: initialize was already called")
		return
	}
	s.phase = phaseInitializing
	s.initParams = m.Params
	id := *m.ID
	s.initID = &id
	s.roots = workspaceRoots(m.Params)
	s.mu.Unlock()
	s.log.Info("initialize", "roots", s.roots)
	go s.launch()
}

func (s *Server) handleRequest(m *jsonrpc.Message) {
	timeout := s.opts.DefaultTimeout
	if d, ok := s.opts.Timeouts[m.Method]; ok {
		timeout = d
	}
	s.mu.Lock()
	if s.degraded {
		s.mu.Unlock()
		s.replyError(*m.ID, jsonrpc.CodeRequestFailed, "mojols: mojo-lsp-server is unavailable (degraded mode after repeated crashes); see the mojols log")
		return
	}
	if q, probation := s.blockedLocked(docURIOf(m.Params)); q || probation {
		s.mu.Unlock()
		name := fileName(docURIOf(m.Params))
		if q {
			s.replyError(*m.ID, jsonrpc.CodeRequestFailed, "mojols: "+name+" is quarantined because it crashes mojo-lsp-server; save a change to it to retry")
		} else {
			s.replyError(*m.ID, jsonrpc.CodeContentModified, "mojols: "+name+" is being re-checked after a backend crash; retry shortly")
		}
		return
	}
	if old, dup := s.byClient[*m.ID]; dup {
		s.log.Warn("editor reused an in-flight request id", "id", m.ID.String(), "previous", old)
	}
	s.nextID++
	p := &pending{id: s.nextID, clientID: *m.ID, method: m.Method, msg: m}
	s.pending[p.id] = p
	s.byClient[p.clientID] = p.id
	p.timer = time.AfterFunc(timeout, func() { s.onTimeout(p.id, timeout) })
	if s.ready {
		s.dispatchLocked(p)
	} else {
		s.queue = append(s.queue, p)
	}
	s.mu.Unlock()
}

// dispatchLocked sends a client request to the current backend under a
// mojols-assigned id.
func (s *Server) dispatchLocked(p *pending) {
	p.gen = s.be.gen
	out := *p.msg
	id := jsonrpc.NumberID(p.id)
	out.ID = &id
	s.sendBackendLocked(&out)
}

func (s *Server) handleCancel(m *jsonrpc.Message) {
	var c struct {
		ID jsonrpc.ID `json:"id"`
	}
	if err := json.Unmarshal(m.Params, &c); err != nil {
		return
	}
	s.mu.Lock()
	pid, ok := s.byClient[c.ID]
	if !ok {
		s.mu.Unlock()
		return
	}
	p := s.pending[pid]
	if p.gen == 0 {
		// Still queued: answer it here, the backend never saw it.
		s.removeLocked(p)
		s.removeQueuedLocked(p)
		s.mu.Unlock()
		s.replyError(p.clientID, jsonrpc.CodeRequestCancelled, "cancelled")
		return
	}
	if s.ready && s.be.gen == p.gen {
		n, _ := jsonrpc.NewNotification("$/cancelRequest", map[string]any{"id": p.id})
		s.sendBackendLocked(n)
	}
	s.mu.Unlock()
}

type docSyncParams struct {
	TextDocument struct {
		URI        string `json:"uri"`
		LanguageID string `json:"languageId"`
		Version    int32  `json:"version"`
		Text       string `json:"text"`
	} `json:"textDocument"`
	ContentChanges []text.ContentChange `json:"contentChanges"`
}

func (s *Server) handleDocSync(m *jsonrpc.Message) {
	var p docSyncParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.log.Error("invalid document sync params", "method", m.Method, "err", err)
		return
	}
	td := p.TextDocument
	switch m.Method {
	case "textDocument/didOpen":
		s.docs.Open(text.Document{URI: td.URI, LanguageID: td.LanguageID, Version: td.Version, Text: td.Text})
	case "textDocument/didChange":
		if err := s.docs.Change(td.URI, td.Version, p.ContentChanges); err != nil {
			// Our copy no longer matches the editor's. The backend still gets
			// the change; only a replay after a restart would be wrong.
			s.log.Error("document store out of sync with editor", "err", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Method == "textDocument/didClose" {
		// Closed after the quarantine rules ran, so they still see the text.
		defer s.docs.Close(td.URI)
	}
	if m.Method == "textDocument/didSave" {
		s.noteChangedLocked(td.URI) // the file on disk changed: its importers are stale
	}
	if s.handleBlockedSyncLocked(m.Method, td.URI) {
		return
	}
	if !s.ready {
		// Dropped on purpose (except didSave, which carries no state): the
		// replay after (re)start sends every open document's current text,
		// which already includes this change.
		if m.Method == "textDocument/didSave" && !s.degraded {
			s.queuedNote = append(s.queuedNote, m)
		}
		return
	}
	s.sendBackendLocked(m)
	if m.Method == "textDocument/didOpen" || m.Method == "textDocument/didChange" {
		s.health[td.URI] = &docHealth{gen: s.be.gen}
	}
}

func (s *Server) handleClientResponse(m *jsonrpc.Message) {
	if m.ID == nil || m.ID.IsString() {
		return
	}
	s.mu.Lock()
	r, ok := s.serverReqs[m.ID.Number()]
	delete(s.serverReqs, m.ID.Number())
	if ok && s.be != nil && s.be.gen == r.gen {
		out := *m
		id := r.backendID
		out.ID = &id
		s.sendBackendLocked(&out)
	}
	s.mu.Unlock()
}

func (s *Server) handleShutdown(id jsonrpc.ID) {
	s.mu.Lock()
	if s.restartTimer != nil {
		s.restartTimer.Stop()
	}
	var ch chan *jsonrpc.Message
	if s.ready {
		ch = s.sendInternalLocked("shutdown", nil)
	}
	s.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(s.opts.ShutdownGrace):
			s.log.Warn("backend did not answer shutdown in time")
		}
	}
	s.reply(id, nil)
}

// ---- backend lifecycle -----------------------------------------------------

func (s *Server) launch() {
	s.mu.Lock()
	if s.exited || s.shutdown || s.be != nil {
		s.mu.Unlock()
		return
	}
	s.gen++
	gen := s.gen
	roots := s.roots
	s.mu.Unlock()

	var cmd Command
	var err error
	if s.opts.Resolve == nil {
		err = errors.New("no backend configured")
	} else {
		cmd, err = s.opts.Resolve(roots)
	}
	if err != nil {
		s.backendFailed(gen, err)
		return
	}
	s.deps.setRoots(cmd.IncludeDirs)
	be, err := startBackend(cmd, gen, s.log, s.opts.Tracer, s.onBackendMessage, s.countMalformed, func(b *backend, err error) {
		s.backendFailed(b.gen, err)
	})
	if err != nil {
		s.backendFailed(gen, err)
		return
	}
	s.mu.Lock()
	if s.exited || gen != s.gen || s.failedGen == gen {
		s.mu.Unlock()
		be.kill()
		return
	}
	s.be = be
	ch := s.sendInternalLocked("initialize", s.initParams)
	s.mu.Unlock()

	var resp *jsonrpc.Message
	select {
	case r, ok := <-ch:
		if !ok {
			return // backend died; backendFailed already ran
		}
		resp = r
	case <-time.After(s.opts.InitTimeout):
		s.backendFailed(gen, fmt.Errorf("backend did not answer initialize within %v", s.opts.InitTimeout))
		return
	}
	if resp.Error != nil {
		s.backendFailed(gen, fmt.Errorf("backend rejected initialize: %s", resp.Error.Message))
		return
	}
	s.onBackendInitialized(gen, resp.Result)
}

func (s *Server) onBackendInitialized(gen int, result json.RawMessage) {
	s.mu.Lock()
	if gen != s.gen || s.be == nil || s.be.gen != gen {
		s.mu.Unlock()
		return
	}
	var answerID *jsonrpc.ID
	var answer json.RawMessage
	if s.initResult == nil {
		s.initResult = rewriteInitResult(result, s.opts.Version)
		s.docs.SetEncoding(text.ParseEncoding(positionEncoding(result)))
		answerID, answer = s.initID, s.initResult
		s.initID = nil
		s.phase = phaseRunning
	} else if enc := text.ParseEncoding(positionEncoding(result)); enc != s.docs.Encoding() {
		s.log.Error("restarted backend chose a different position encoding", "was", s.docs.Encoding(), "now", enc)
	}
	// Bring the backend to the editor's exact state, then open the gates.
	// All of it happens under the lock, so no editor message can slip in
	// between the snapshot and ready=true.
	if s.clientInitialized {
		n, _ := jsonrpc.NewNotification("initialized", json.RawMessage(`{}`))
		s.sendBackendLocked(n)
	}
	replayed := 0
	for _, d := range s.docs.Snapshot() {
		if q, probation := s.blockedLocked(d.URI); q || probation {
			continue // quarantined, or reopened alone after the others
		}
		s.sendDidOpenLocked(d)
		replayed++
	}
	s.ready = true
	wasDegraded := s.degraded
	s.degraded = false
	s.backoffStep = 0
	s.timeoutStreak = 0
	for _, n := range s.queuedNote {
		s.sendBackendLocked(n)
	}
	s.queuedNote = nil
	for _, p := range s.queue {
		s.dispatchLocked(p)
	}
	s.queue = nil
	s.startNextProbeLocked()
	s.mu.Unlock()

	s.log.Info("backend ready", "generation", gen, "replayedDocuments", replayed)
	if answerID != nil {
		s.write(&jsonrpc.Message{ID: answerID, Result: answer})
	}
	if wasDegraded {
		s.showMessage(3, "mojols: mojo-lsp-server is back; full language features restored.")
	}
}

// backendFailed is the single funnel for every way a backend generation can
// die: crash, failed start, failed initialize, or a hang we killed.
func (s *Server) backendFailed(gen int, reason error) {
	s.mu.Lock()
	if gen != s.gen || s.failedGen == gen || s.exited {
		s.mu.Unlock()
		return
	}
	s.failedGen = gen
	be := s.be
	s.be = nil
	s.ready = false
	s.lastErr = reason.Error()
	s.restarts++

	var replies []*jsonrpc.Message
	for id, p := range s.pending {
		if p.gen != gen {
			continue
		}
		s.removeLocked(p)
		delete(s.pending, id)
		if p.internal != nil {
			close(p.internal)
			continue
		}
		replies = append(replies, errorMessage(p.clientID, jsonrpc.CodeContentModified,
			"mojols: mojo-lsp-server stopped while handling this request; it is restarting, retry shortly"))
	}
	for id, r := range s.serverReqs {
		if r.gen == gen {
			delete(s.serverReqs, id)
		}
	}
	if s.shutdown {
		s.mu.Unlock()
		s.sendAll(replies)
		return
	}

	convicted := s.attributeCrashLocked(gen)
	if convicted != "" {
		// The cause is known and contained: this crash says nothing about
		// whether the backend is healthy for every other file.
		s.failures = nil
		s.backoffStep = 0
	}
	now := time.Now()
	recent := s.failures[:0]
	for _, t := range s.failures {
		if now.Sub(t) < s.opts.CrashLoopWindow {
			recent = append(recent, t)
		}
	}
	s.failures = append(recent, now)

	enteringDegraded := false
	var delay time.Duration
	switch {
	case s.degraded:
		delay = s.opts.DegradedRetry
	case len(s.failures) >= s.opts.CrashLoopLimit:
		enteringDegraded = true
		s.degraded = true
		s.failures = nil
		delay = s.opts.DegradedRetry
		for _, p := range s.queue {
			s.removeLocked(p)
			replies = append(replies, errorMessage(p.clientID, jsonrpc.CodeRequestFailed,
				"mojols: mojo-lsp-server is unavailable (degraded mode after repeated crashes)"))
		}
		s.queue = nil
		s.queuedNote = nil
		if s.initID != nil {
			// Never leave the editor waiting on initialize: answer with the
			// minimal capabilities mojols can honour on its own.
			s.initResult = minimalInitResult(s.opts.Version)
			replies = append(replies, &jsonrpc.Message{ID: s.initID, Result: s.initResult})
			s.initID = nil
			s.phase = phaseRunning
		}
	default:
		delay = s.opts.BackoffMin << s.backoffStep
		if delay > s.opts.BackoffMax || delay <= 0 {
			delay = s.opts.BackoffMax
		}
		s.backoffStep++
	}
	s.restartTimer = time.AfterFunc(delay, s.launch)
	s.mu.Unlock()

	if be != nil {
		be.kill()
	}
	s.log.Warn("backend failed", "generation", gen, "reason", reason, "nextAttemptIn", delay, "degraded", s.isDegraded())
	s.sendAll(replies)
	if convicted != "" {
		s.log.Warn("document quarantined", "uri", convicted)
		s.write(quarantineDiagnostics(convicted, true))
		s.showMessage(2, fmt.Sprintf("mojols: %s crashes mojo-lsp-server, so it is quarantined. Other files keep full features; save a change to it to retry.", fileName(convicted)))
	}
	if enteringDegraded {
		s.showMessage(1, fmt.Sprintf("mojols: mojo-lsp-server keeps failing (%v). Running in degraded mode; retrying every %v.", reason, s.opts.DegradedRetry))
	}
}

func (s *Server) onTimeout(pid int64, after time.Duration) {
	s.mu.Lock()
	p, ok := s.pending[pid]
	if !ok {
		s.mu.Unlock()
		return
	}
	s.removeLocked(p)
	var msg string
	killGen := 0
	if p.gen == 0 {
		s.removeQueuedLocked(p)
		msg = fmt.Sprintf("mojols: mojo-lsp-server was not ready within %v", after)
	} else {
		s.timeouts++
		s.timeoutStreak++
		if s.be != nil && s.be.gen == p.gen {
			n, _ := jsonrpc.NewNotification("$/cancelRequest", map[string]any{"id": p.id})
			s.sendBackendLocked(n)
			if s.timeoutStreak >= s.opts.HangThreshold {
				killGen = p.gen
			}
		}
		msg = fmt.Sprintf("mojols: mojo-lsp-server did not answer %s within %v", p.method, after)
	}
	s.mu.Unlock()
	s.replyError(p.clientID, jsonrpc.CodeRequestFailed, msg)
	if killGen != 0 {
		s.backendFailed(killGen, fmt.Errorf("backend unresponsive: %d consecutive requests timed out", s.opts.HangThreshold))
	}
}

// ---- backend -> mojols -----------------------------------------------------

func (s *Server) onBackendMessage(b *backend, m *jsonrpc.Message) {
	switch {
	case m.IsResponse():
		if m.ID == nil || m.ID.IsString() {
			s.countMalformed("backend response with unknown id")
			return
		}
		s.mu.Lock()
		p, ok := s.pending[m.ID.Number()]
		if !ok || p.gen != b.gen {
			s.mu.Unlock()
			return // answered already (timeout, cancel) or from a dead generation
		}
		s.removeLocked(p)
		s.timeoutStreak = 0
		s.mu.Unlock()
		if p.internal != nil {
			p.internal <- m
			return
		}
		out := *m
		out.ID = &p.clientID
		s.write(&out)

	case m.IsRequest():
		s.mu.Lock()
		if s.be != b || s.exited {
			s.mu.Unlock()
			return
		}
		s.nextID++
		pid := s.nextID
		s.serverReqs[pid] = serverReq{gen: b.gen, backendID: *m.ID}
		s.mu.Unlock()
		out := *m
		id := jsonrpc.NumberID(pid)
		out.ID = &id
		s.write(&out)

	default:
		s.mu.Lock()
		current := s.be == b
		if current && m.Method == "textDocument/publishDiagnostics" {
			var p struct {
				URI string `json:"uri"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				s.onDiagnosedLocked(b.gen, p.URI)
			}
		}
		s.mu.Unlock()
		if current {
			s.write(m)
		}
	}
}

// ---- helpers ---------------------------------------------------------------

// removeLocked forgets a pending request and stops its timer.
func (s *Server) removeLocked(p *pending) {
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(s.pending, p.id)
	if s.byClient[p.clientID] == p.id {
		delete(s.byClient, p.clientID)
	}
}

func (s *Server) removeQueuedLocked(p *pending) {
	for i, q := range s.queue {
		if q == p {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *Server) sendInternalLocked(method string, params json.RawMessage) chan *jsonrpc.Message {
	s.nextID++
	ch := make(chan *jsonrpc.Message, 1)
	p := &pending{id: s.nextID, method: method, gen: s.be.gen, internal: ch}
	s.pending[p.id] = p
	id := jsonrpc.NumberID(p.id)
	s.sendBackendLocked(&jsonrpc.Message{ID: &id, Method: method, Params: params})
	return ch
}

func (s *Server) sendBackendLocked(m *jsonrpc.Message) {
	if s.be == nil {
		return
	}
	body, err := m.MarshalJSON()
	if err != nil {
		s.log.Error("encode for backend", "err", err)
		return
	}
	s.opts.Tracer.Log(trace.ProxyToBackend, s.be.gen, body)
	s.be.send(body)
}

func (s *Server) write(m *jsonrpc.Message) {
	body, err := s.out.Write(m)
	if err != nil {
		s.log.Error("write to editor failed", "err", err)
		return
	}
	s.opts.Tracer.Log(trace.ProxyToClient, 0, body)
}

func (s *Server) sendAll(ms []*jsonrpc.Message) {
	for _, m := range ms {
		s.write(m)
	}
}

func (s *Server) reply(id jsonrpc.ID, result any) {
	m, err := jsonrpc.NewResponse(id, result)
	if err != nil {
		s.replyError(id, jsonrpc.CodeInternalError, "mojols: "+err.Error())
		return
	}
	s.write(m)
}

func (s *Server) replyError(id jsonrpc.ID, code int64, msg string) {
	s.write(errorMessage(id, code, msg))
}

func errorMessage(id jsonrpc.ID, code int64, msg string) *jsonrpc.Message {
	return jsonrpc.NewErrorResponse(&id, &jsonrpc.Error{Code: code, Message: msg})
}

func (s *Server) showMessage(typ int, msg string) {
	n, _ := jsonrpc.NewNotification("window/showMessage", map[string]any{"type": typ, "message": msg})
	s.write(n)
}

func (s *Server) countMalformed(what string) {
	s.mu.Lock()
	s.malformed++
	s.mu.Unlock()
	s.log.Warn("dropped message", "why", what)
}

func (s *Server) isDegraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded
}

// Status reports the supervisor's state, for mojols/status and tests.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Version:          s.opts.Version,
		Phase:            [...]string{"uninitialized", "initializing", "running"}[s.phase],
		BackendGen:       s.gen,
		BackendReady:     s.ready,
		Degraded:         s.degraded,
		OpenDocuments:    s.docs.Len(),
		Pending:          len(s.pending),
		Queued:           len(s.queue) + len(s.queuedNote),
		Restarts:         s.restarts,
		Timeouts:         s.timeouts,
		DroppedMalformed: s.malformed,
		PositionEncoding: s.docs.Encoding().String(),
		LastBackendError: s.lastErr,
		Refreshes:        s.refreshes,
	}
	if s.be != nil && s.be.cmd.Process != nil {
		st.BackendPID = s.be.cmd.Process.Pid
	}
	for uri := range s.quarantine {
		st.Quarantined = append(st.Quarantined, uri)
	}
	sort.Strings(st.Quarantined)
	st.Probing = s.probing
	st.Suspects = append([]string(nil), s.probeQueue...)
	return st
}

func (s *Server) teardown() {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	if s.restartTimer != nil {
		s.restartTimer.Stop()
	}
	for _, p := range s.pending {
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	be := s.be
	s.be = nil
	s.mu.Unlock()
	if be != nil {
		be.stop(s.opts.ShutdownGrace)
	}
	s.opts.Tracer.Close()
}

// ---- initialize payloads ---------------------------------------------------

func workspaceRoots(params json.RawMessage) []string {
	var p struct {
		RootURI          string `json:"rootUri"`
		RootPath         string `json:"rootPath"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	var roots []string
	seen := map[string]bool{}
	add := func(path string) {
		if path != "" && !seen[path] {
			seen[path] = true
			roots = append(roots, path)
		}
	}
	for _, f := range p.WorkspaceFolders {
		if path, ok := discover.URIToPath(f.URI); ok {
			add(path)
		}
	}
	if path, ok := discover.URIToPath(p.RootURI); ok {
		add(path)
	}
	add(p.RootPath)
	return roots
}

func positionEncoding(result json.RawMessage) string {
	var r struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	_ = json.Unmarshal(result, &r)
	return r.Capabilities.PositionEncoding
}

func rewriteInitResult(result json.RawMessage, version string) json.RawMessage {
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil || r == nil {
		r = map[string]json.RawMessage{}
	}
	backendName := "mojo-lsp-server"
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(r["serverInfo"], &info) == nil && info.Name != "" {
		backendName = info.Name
		if info.Version != "" {
			backendName += " " + info.Version
		}
	}
	r["serverInfo"], _ = json.Marshal(map[string]string{
		"name":    "mojols",
		"version": version + " (backend: " + backendName + ")",
	})
	out, _ := json.Marshal(r)
	return out
}

func minimalInitResult(version string) json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{
			"textDocumentSync": map[string]any{"openClose": true, "change": 2, "save": true},
		},
		"serverInfo": map[string]string{"name": "mojols", "version": version + " (degraded: no backend)"},
	})
	return out
}
