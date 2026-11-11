package proxy

// Completion isolation.
//
// Crash bundles from soaks on ordinary code showed every crash of the
// official server came from textDocument/completion, and half of them
// reproduce every time from one file and one request: completion in the
// middle of an existing word (`assert_equal(ele|m[0], 1)`). The server
// re-parses the document at the cursor for each completion, and that
// throwaway parse crashes.
//
// So completion and signature help (which re-parses the same way) run in a
// second, sacrificial mojo-lsp-server that holds only the few documents
// being completed in. When it crashes, the request in flight gets an empty
// "incomplete" list, so the editor simply asks again; the main backend,
// with diagnostics, hover, definition and every open document, never
// notices. A completion that crashed once (same text, same position) is
// answered empty without crashing again. If the sidecar keeps crashing,
// completion pauses rather than looping.

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/text"
)

// isolatedMethods go to the completion backend.
var isolatedMethods = map[string]bool{
	"textDocument/completion":    true,
	"textDocument/signatureHelp": true,
	"completionItem/resolve":     true,
}

const (
	sidecarResident    = 3
	sidecarCrashWindow = time.Minute
	sidecarCrashLimit  = 4
	sidecarPause       = 2 * time.Minute
	sidecarGenBase     = 1000 // its generations are logged as 1001, 1002, ...
)

type sideDoc struct {
	version int32
	hash    string
	used    time.Time
}

type sidePending struct {
	clientID jsonrpc.ID
	method   string
	key      string // crash memo key
	timer    *time.Timer
	msg      *jsonrpc.Message // the request, for a retry
	uri      string
	retried  bool
}

type sidecar struct {
	s *Server

	mu       sync.Mutex
	be       *backend
	runs     int
	ready    chan struct{} // closed once the current backend is initialized
	starting bool
	initID   int64
	pending  map[int64]*sidePending
	docs     map[string]*sideDoc
	nextID   int64
	crashes  []time.Time
	paused   time.Time // no completion until then
	memo     map[string]bool

	crashCount, memoHits, retries int
}

func newSidecar(s *Server) *sidecar {
	return &sidecar{s: s, pending: map[int64]*sidePending{}, docs: map[string]*sideDoc{}, memo: map[string]bool{}}
}

// emptyResult is what an isolated request gets when its backend could not
// answer: nothing to show, and for completion a hint to ask again.
func emptyResult(method string) json.RawMessage {
	if method == "textDocument/completion" {
		return json.RawMessage(`{"isIncomplete":true,"items":[]}`)
	}
	return json.RawMessage(`null`)
}

func (c *sidecar) reply(id jsonrpc.ID, method string, result json.RawMessage) {
	if method == "textDocument/completion" && c.s.ix != nil {
		result = c.s.finishCompletion(id, result)
	}
	c.s.write(&jsonrpc.Message{ID: &id, Result: result})
}

// handle serves one isolated request. It may block while the sidecar
// starts, so it runs on its own goroutine.
func (c *sidecar) handle(m *jsonrpc.Message) {
	id, method := *m.ID, m.Method
	uri := docURIOf(m.Params)
	doc, open := c.s.docs.Get(uri)
	if method != "completionItem/resolve" && !open {
		c.reply(id, method, emptyResult(method))
		return
	}
	key := ""
	if open {
		var p struct {
			Position struct{ Line, Character int } `json:"position"`
		}
		_ = json.Unmarshal(m.Params, &p)
		key = fmt.Sprintf("%s|%s|%s|%d:%d", method, uri, contentHash(doc.Text), p.Position.Line, p.Position.Character)
	}

	c.mu.Lock()
	if key != "" && c.memo[key] {
		c.memoHits++
		c.mu.Unlock()
		c.reply(id, method, emptyResult(method))
		return
	}
	if time.Now().Before(c.paused) {
		c.mu.Unlock()
		c.reply(id, method, emptyResult(method))
		return
	}
	ready := c.startLocked()
	c.mu.Unlock()

	select {
	case <-ready:
	case <-time.After(c.s.opts.InitTimeout):
		c.reply(id, method, emptyResult(method))
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.be == nil {
		c.reply(id, method, emptyResult(method)) // died while starting
		return
	}
	if open {
		c.syncLocked(c.s.docs.Get(uri))
	}
	c.nextID++
	pid := c.nextID
	timeout := c.s.opts.DefaultTimeout
	if d, ok := c.s.opts.Timeouts[method]; ok {
		timeout = d
	}
	p := &sidePending{clientID: id, method: method, key: key, msg: m, uri: uri}
	p.timer = time.AfterFunc(timeout, func() {
		c.mu.Lock()
		_, live := c.pending[pid]
		delete(c.pending, pid)
		c.mu.Unlock()
		if live {
			c.reply(id, method, emptyResult(method))
		}
	})
	c.pending[pid] = p
	out := *m
	nid := jsonrpc.NumberID(pid)
	out.ID = &nid
	c.sendLocked(&out)
}

// syncLocked makes the sidecar hold d's current text.
func (c *sidecar) syncLocked(d text.Document, ok bool) {
	if !ok {
		return
	}
	h := contentHash(d.Text)
	cur := c.docs[d.URI]
	switch {
	case cur == nil:
		c.sendLocked(notification("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": d.URI, "languageId": d.LanguageID, "version": d.Version, "text": d.Text},
		}))
		c.docs[d.URI] = &sideDoc{version: d.Version, hash: h}
	case cur.hash != h:
		version := d.Version
		if version <= cur.version {
			version = cur.version + 1
		}
		c.sendLocked(notification("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": d.URI, "version": version},
			"contentChanges": []any{map[string]any{"text": d.Text}},
		}))
		cur.version, cur.hash = version, h
	}
	c.docs[d.URI].used = time.Now()
	for len(c.docs) > sidecarResident {
		victim, oldest := "", time.Time{}
		for u, sd := range c.docs {
			if u != d.URI && (victim == "" || sd.used.Before(oldest)) {
				victim, oldest = u, sd.used
			}
		}
		c.sendLocked(notification("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": victim}}))
		delete(c.docs, victim)
	}
}

func notification(method string, params any) *jsonrpc.Message {
	n, _ := jsonrpc.NewNotification(method, params)
	return n
}

func (c *sidecar) sendLocked(m *jsonrpc.Message) {
	if c.be == nil {
		return
	}
	body, err := m.MarshalJSON()
	if err != nil {
		return
	}
	c.s.opts.Tracer.Log("mojols->completion", c.be.gen, body)
	c.be.send(body)
}

// startLocked starts the sidecar if needed and returns a channel closed
// once it is ready (or failed to start).
func (c *sidecar) startLocked() <-chan struct{} {
	if c.be != nil || c.starting {
		return c.ready
	}
	c.starting = true
	c.ready = make(chan struct{})
	ready := c.ready
	c.runs++
	gen := sidecarGenBase + c.runs
	go func() {
		defer func() {
			c.mu.Lock()
			c.starting = false
			c.mu.Unlock()
		}()
		s := c.s
		s.mu.Lock()
		roots, params := s.roots, s.initParams
		s.mu.Unlock()
		cmd, err := s.opts.Resolve(roots)
		if err != nil {
			s.log.Warn("completion backend cannot start", "err", err)
			close(ready)
			return
		}
		be, err := startBackend(cmd, gen, s.log.With("role", "completion"), s.opts.Tracer, c.onMessage, s.countMalformed, c.onExit)
		if err != nil {
			s.log.Warn("completion backend cannot start", "err", err)
			close(ready)
			return
		}
		c.mu.Lock()
		c.be = be
		c.nextID++
		c.initID = c.nextID
		id := jsonrpc.NumberID(c.initID)
		c.sendLocked(&jsonrpc.Message{ID: &id, Method: "initialize", Params: params})
		c.mu.Unlock()
	}()
	return ready
}

func (c *sidecar) onMessage(b *backend, m *jsonrpc.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b != c.be {
		return
	}
	switch {
	case m.IsResponse():
		if m.ID == nil || m.ID.IsString() {
			return
		}
		if m.ID.Number() == c.initID && c.initID != 0 {
			c.initID = 0
			c.sendLocked(notification("initialized", json.RawMessage(`{}`)))
			close(c.ready)
			c.s.log.Info("completion backend ready", "generation", b.gen)
			return
		}
		p, ok := c.pending[m.ID.Number()]
		if !ok {
			return
		}
		delete(c.pending, m.ID.Number())
		if m.Error != nil && m.Error.Code == jsonrpc.CodeContentModified && !p.retried {
			// The backend dropped it for an edit that arrived just before:
			// sync the current text and ask once more, rather than making
			// the editor retry.
			p.retried = true
			c.retries++
			if p.uri != "" {
				c.syncLocked(c.s.docs.Get(p.uri))
			}
			c.nextID++
			pid := c.nextID
			c.pending[pid] = p
			out := *p.msg
			nid := jsonrpc.NumberID(pid)
			out.ID = &nid
			c.sendLocked(&out)
			return
		}
		p.timer.Stop()
		out := *m
		out.ID = &p.clientID
		if p.method == "textDocument/completion" && c.s.ix != nil && m.Error == nil {
			go func() {
				out.Result = c.s.finishCompletion(p.clientID, out.Result)
				c.s.write(&out)
			}()
			return
		}
		go c.s.write(&out)
	case m.IsRequest():
		// workspace/configuration and the like: answer locally.
		resp, _ := jsonrpc.NewResponse(*m.ID, nil)
		c.sendLocked(resp)
	default:
		// Diagnostics, progress and logs come from the main backend only.
	}
}

func (c *sidecar) onExit(b *backend, err error) {
	c.mu.Lock()
	if b != c.be {
		c.mu.Unlock()
		return
	}
	c.be = nil
	c.docs = map[string]*sideDoc{}
	if c.initID != 0 {
		c.initID = 0
		close(c.ready)
	}
	stopping := b.stopping.Load()
	var inFlight []string
	var replies []*jsonrpc.Message
	for id, p := range c.pending {
		p.timer.Stop()
		delete(c.pending, id)
		if !stopping && p.key != "" {
			c.memo[p.key] = true
		}
		inFlight = append(inFlight, p.key)
		cid := p.clientID
		replies = append(replies, &jsonrpc.Message{ID: &cid, Result: emptyResult(p.method)})
	}
	if !stopping {
		c.crashCount++
		now := time.Now()
		recent := c.crashes[:0]
		for _, t := range c.crashes {
			if now.Sub(t) < sidecarCrashWindow {
				recent = append(recent, t)
			}
		}
		c.crashes = append(recent, now)
		if len(c.crashes) >= sidecarCrashLimit {
			c.paused = now.Add(sidecarPause)
			c.crashes = nil
		}
	}
	paused := time.Now().Before(c.paused)
	c.mu.Unlock()

	if !stopping {
		sort.Strings(inFlight)
		c.s.log.Warn("completion backend crashed; main backend unaffected", "reason", err, "inFlight", inFlight, "pausedCompletion", paused)
	}
	for _, r := range replies {
		c.s.write(r)
	}
}

// forget closes uri in the sidecar once the editor has closed it. Edits are
// deliberately not mirrored: three 1-hour soaks measured completion p50 at
// 420 ms when the sidecar syncs only on request, 495 ms mirroring every
// keystroke and 490 ms mirroring after a 300 ms pause (p95 883, 946 and
// 1,148 ms). Any extra parsing there competes with the main backend and
// with completion's own re-parse at the cursor.
func (c *sidecar) forget(uri string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.be == nil || c.docs[uri] == nil {
		return
	}
	if _, open := c.s.docs.Get(uri); open {
		return
	}
	c.sendLocked(notification("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}}))
	delete(c.docs, uri)
}

func (c *sidecar) stop() {
	c.mu.Lock()
	be := c.be
	c.mu.Unlock()
	if be != nil {
		be.stop(c.s.opts.ShutdownGrace)
	}
}

// SidecarStatus is the completion backend's part of mojols/status.
type SidecarStatus struct {
	Running  bool `json:"running"`
	Crashes  int  `json:"crashes"`
	MemoHits int  `json:"memoHits"`
	Paused   bool `json:"paused"`
	Docs     int  `json:"documents"`
	Retries  int  `json:"retries"`
}

func (c *sidecar) status() SidecarStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SidecarStatus{Running: c.be != nil, Crashes: c.crashCount, MemoHits: c.memoHits, Paused: time.Now().Before(c.paused),
		Docs: len(c.docs), Retries: c.retries}
}
