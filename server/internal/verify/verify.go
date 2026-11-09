// Package verify asks a dedicated mojo-lsp-server where names lead, for
// the occurrences mojols' own index cannot resolve statically (typically a
// member of an expression whose type it cannot infer). Answers are cached
// by file content, so each file version is checked once.
//
// The verifier process is separate from the backends that serve the
// editor: it is started when there is work, reads files from disk or the
// editor's unsaved text, and is stopped when idle. Its crashes and hangs
// cost only unverified answers.
package verify

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/tdadadavid/mojols/server/internal/jsonrpc"
	"github.com/tdadadavid/mojols/server/internal/text"
)

// Command is how to start mojo-lsp-server.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Options configure a Verifier.
type Options struct {
	// Command returns the server to start, or an error if there is none.
	Command func() (Command, error)
	Logger  *slog.Logger
	// IdleStop stops the process after this long without work (60 s).
	IdleStop time.Duration
	// FileTimeout bounds parsing one file and answering its queries (90 s).
	FileTimeout time.Duration
	// MaxCached bounds the number of file versions remembered (5,000).
	MaxCached int
}

// Loc is where the server says a name leads.
type Loc struct {
	Path  string
	Range text.Range
}

// Result is the server's answer at one offset; Loc is nil when it found
// nothing there.
type Result struct {
	Loc *Loc
}

type key struct {
	path string
	sum  [32]byte
}

type entry struct {
	answers map[int]Result
	failed  bool // the server crashed or hung on this file version
	used    time.Time
}

type job struct {
	k       key
	src     []byte
	offsets []int
	done    chan struct{}
}

// Verifier checks occurrences with a mojo-lsp-server of its own.
type Verifier struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	cache   map[key]*entry
	queue   []*job
	queued  map[key]*job
	working bool
	stats   Stats

	proc *process // owned by the worker goroutine
}

// Stats describe the verifier for mojols/status.
type Stats struct {
	Running   bool `json:"running"`
	Files     int  `json:"filesChecked"`
	Queries   int  `json:"queries"`
	Failures  int  `json:"failedFiles"`
	Restarts  int  `json:"restarts"`
	Queued    int  `json:"queued"`
	CacheSize int  `json:"cached"`
}

// New returns a verifier; its process starts on first use.
func New(opts Options) *Verifier {
	if opts.IdleStop == 0 {
		opts.IdleStop = time.Minute
	}
	if opts.FileTimeout == 0 {
		opts.FileTimeout = 90 * time.Second
	}
	if opts.MaxCached == 0 {
		opts.MaxCached = 5000
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Verifier{opts: opts, log: opts.Logger.With("component", "verifier"), cache: map[key]*entry{}, queued: map[key]*job{}}
}

// Stats returns current counters.
func (v *Verifier) Stats() Stats {
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.stats
	s.Queued = len(v.queue)
	s.CacheSize = len(v.cache)
	return s
}

// Lookup returns the cached answers for offsets in the given version of
// path. complete is false if some offsets are not checked yet; failed is
// true if the server could not check this version at all.
func (v *Verifier) Lookup(path string, src []byte, offsets []int) (answers map[int]Result, complete, failed bool) {
	k := key{path, sha256.Sum256(src)}
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.cache[k]
	answers = map[int]Result{}
	if e == nil {
		return answers, len(offsets) == 0, false
	}
	e.used = time.Now()
	complete = true
	for _, o := range offsets {
		if r, ok := e.answers[o]; ok {
			answers[o] = r
		} else if !e.failed {
			complete = false
		}
	}
	return answers, complete, e.failed
}

// Check makes sure offsets in this version of path are checked, waiting
// until they are or ctx is done. Work not finished by then continues in
// the background.
func (v *Verifier) Check(ctx context.Context, path string, src []byte, offsets []int) error {
	if len(offsets) == 0 {
		return nil
	}
	k := key{path, sha256.Sum256(src)}
	v.mu.Lock()
	if e := v.cache[k]; e != nil {
		missing := false
		for _, o := range offsets {
			if _, ok := e.answers[o]; !ok {
				missing = true
			}
		}
		if !missing || e.failed {
			v.mu.Unlock()
			return nil
		}
	}
	j := v.queued[k]
	if j == nil {
		j = &job{k: k, src: src, done: make(chan struct{})}
		v.queued[k] = j
		v.queue = append(v.queue, j)
	}
	j.offsets = mergeOffsets(j.offsets, offsets)
	if !v.working {
		v.working = true
		go v.work()
	}
	v.mu.Unlock()
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mergeOffsets(a, b []int) []int {
	seen := map[int]bool{}
	for _, o := range a {
		seen[o] = true
	}
	for _, o := range b {
		if !seen[o] {
			a = append(a, o)
			seen[o] = true
		}
	}
	return a
}

// work processes the queue, then waits for more until IdleStop, then
// stops the process.
func (v *Verifier) work() {
	for {
		v.mu.Lock()
		if len(v.queue) == 0 {
			v.working = false
			v.mu.Unlock()
			v.idleStop()
			return
		}
		j := v.queue[0]
		v.queue = v.queue[1:]
		delete(v.queued, j.k)
		v.mu.Unlock()

		answers, err := v.checkFile(j)
		v.mu.Lock()
		e := v.cache[j.k]
		if e == nil {
			e = &entry{answers: map[int]Result{}}
			v.cache[j.k] = e
		}
		e.used = time.Now()
		for o, r := range answers {
			e.answers[o] = r
		}
		if err != nil {
			e.failed = true
			v.stats.Failures++
			v.log.Warn("verification failed", "path", j.k.path, "err", err)
		} else {
			v.stats.Files++
		}
		v.stats.Queries += len(answers)
		v.evictLocked()
		v.mu.Unlock()
		close(j.done)
	}
}

func (v *Verifier) evictLocked() {
	for len(v.cache) > v.opts.MaxCached {
		var oldest key
		var t time.Time
		first := true
		for k, e := range v.cache {
			if first || e.used.Before(t) {
				oldest, t, first = k, e.used, false
			}
		}
		delete(v.cache, oldest)
	}
}

func (v *Verifier) idleStop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := v.proc
	if p == nil {
		return
	}
	time.AfterFunc(v.opts.IdleStop, func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.working || v.proc != p {
			return
		}
		v.proc = nil
		v.stats.Running = false
		go p.stop()
		v.log.Info("verifier stopped (idle)")
	})
}

// Close stops the verifier's process.
func (v *Verifier) Close() {
	v.mu.Lock()
	p := v.proc
	v.proc = nil
	v.stats.Running = false
	v.mu.Unlock()
	if p != nil {
		p.stop()
	}
}

// checkFile opens one file version in the verifier's server and asks for
// the definition at each offset.
func (v *Verifier) checkFile(j *job) (map[int]Result, error) {
	p, err := v.ensureProc()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), v.opts.FileTimeout)
	defer cancel()
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(j.k.path)}).String()
	answers := map[int]Result{}
	lines := text.NewLines(string(j.src))
	err = p.withFile(ctx, uri, string(j.src), func() error {
		for _, o := range j.offsets {
			pos := lines.Position(o, text.UTF16)
			res, err := p.call(ctx, "textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": uri}, "position": pos})
			if err != nil {
				return err
			}
			answers[o] = Result{Loc: firstLocation(res)}
		}
		return nil
	})
	if err != nil {
		// A crash or hang: restart for the next file, keep what we got.
		v.mu.Lock()
		if v.proc == p {
			v.proc = nil
			v.stats.Running = false
			v.stats.Restarts++
		}
		v.mu.Unlock()
		go p.stop()
		return answers, err
	}
	return answers, nil
}

func firstLocation(res json.RawMessage) *Loc {
	var many []struct {
		URI       string     `json:"uri"`
		Range     text.Range `json:"range"`
		TargetURI string     `json:"targetUri"`
		TargetSel text.Range `json:"targetSelectionRange"`
	}
	if json.Unmarshal(res, &many) != nil || len(many) == 0 {
		var one struct {
			URI   string     `json:"uri"`
			Range text.Range `json:"range"`
		}
		if json.Unmarshal(res, &one) != nil || one.URI == "" {
			return nil
		}
		many = append(many[:0], struct {
			URI       string     `json:"uri"`
			Range     text.Range `json:"range"`
			TargetURI string     `json:"targetUri"`
			TargetSel text.Range `json:"targetSelectionRange"`
		}{URI: one.URI, Range: one.Range})
	}
	m := many[0]
	uri, rg := m.URI, m.Range
	if m.TargetURI != "" {
		uri, rg = m.TargetURI, m.TargetSel
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return nil
	}
	return &Loc{Path: filepath.FromSlash(u.Path), Range: rg}
}

func (v *Verifier) ensureProc() (*process, error) {
	v.mu.Lock()
	p := v.proc
	v.mu.Unlock()
	if p != nil && !p.dead() {
		return p, nil
	}
	if v.opts.Command == nil {
		return nil, errors.New("no mojo-lsp-server configured")
	}
	c, err := v.opts.Command()
	if err != nil {
		return nil, err
	}
	p, err = startProcess(c, v.log)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := p.call(ctx, "initialize", map[string]any{"processId": os.Getpid(), "rootUri": nil, "capabilities": map[string]any{}}); err != nil {
		p.stop()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	p.notify("initialized", map[string]any{})
	v.mu.Lock()
	v.proc = p
	v.stats.Running = true
	v.mu.Unlock()
	v.log.Info("verifier started", "path", c.Path)
	return p, nil
}

// process is a minimal LSP client for one mojo-lsp-server.
type process struct {
	cmd  *exec.Cmd
	w    *jsonrpc.Writer
	wmu  sync.Mutex
	log  *slog.Logger
	mu   sync.Mutex
	next int64
	resp map[int64]chan *jsonrpc.Message
	diag map[string]chan struct{}
	exit chan struct{}
}

func startProcess(c Command, log *slog.Logger) (*process, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Dir = c.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, w: jsonrpc.NewWriter(stdin), log: log, resp: map[int64]chan *jsonrpc.Message{}, diag: map[string]chan struct{}{}, exit: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			log.Debug("verifier stderr", "line", sc.Text())
		}
	}()
	go p.read(jsonrpc.NewReader(stdout))
	return p, nil
}

func (p *process) read(r *jsonrpc.Reader) {
	defer func() {
		p.cmd.Wait()
		close(p.exit)
	}()
	for {
		m, _, err := r.Read()
		if err != nil {
			var de *jsonrpc.DecodeError
			if errors.As(err, &de) {
				continue // garbage: skip it, the request times out
			}
			return
		}
		switch {
		case m.IsResponse() && m.ID != nil && !m.ID.IsString():
			p.mu.Lock()
			ch := p.resp[m.ID.Number()]
			delete(p.resp, m.ID.Number())
			p.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.IsRequest():
			reply, _ := jsonrpc.NewResponse(*m.ID, nil)
			p.send(reply)
		case m.Method == "textDocument/publishDiagnostics":
			var d struct {
				URI string `json:"uri"`
			}
			json.Unmarshal(m.Params, &d)
			p.mu.Lock()
			if ch := p.diag[d.URI]; ch != nil {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			p.mu.Unlock()
		}
	}
}

func (p *process) dead() bool {
	select {
	case <-p.exit:
		return true
	default:
		return false
	}
}

func (p *process) send(m *jsonrpc.Message) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := p.w.Write(m)
	return err
}

func (p *process) notify(method string, params any) {
	if m, err := jsonrpc.NewNotification(method, params); err == nil {
		p.send(m)
	}
}

func (p *process) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.next++
	id := p.next
	ch := make(chan *jsonrpc.Message, 1)
	p.resp[id] = ch
	p.mu.Unlock()
	m, err := jsonrpc.NewRequest(jsonrpc.NumberID(id), method, params)
	if err != nil {
		return nil, err
	}
	if err := p.send(m); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-p.exit:
		return nil, errors.New("mojo-lsp-server exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// withFile opens uri with text, waits for it to be parsed, runs f, and
// closes it.
func (p *process) withFile(ctx context.Context, uri, src string, f func() error) error {
	ch := make(chan struct{}, 1)
	p.mu.Lock()
	p.diag[uri] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.diag, uri)
		p.mu.Unlock()
		p.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
	}()
	p.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "mojo", "version": 1, "text": src}})
	select {
	case <-ch:
	case <-p.exit:
		return errors.New("mojo-lsp-server exited while parsing")
	case <-ctx.Done():
		return ctx.Err()
	}
	return f()
}

func (p *process) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.call(ctx, "shutdown", nil)
	p.notify("exit", nil)
	select {
	case <-p.exit:
	case <-time.After(2 * time.Second):
		p.cmd.Process.Kill()
	}
}
