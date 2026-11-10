package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obadafidii/mojols/server/internal/jsonrpc"
	"github.com/obadafidii/mojols/server/internal/trace"
)

// Command is how to start one backend process.
type Command struct {
	Path string   `json:"path"`
	Args []string `json:"args"`
	// IncludeDirs are the -I dirs in Args, for resolving imports when
	// deciding which documents a changed file affects.
	IncludeDirs []string `json:"includeDirs"`
	Env         []string `json:"env,omitempty"` // added to the inherited environment
	Dir         string   `json:"dir"`
}

// backend is one generation of mojo-lsp-server.
//
// Writes go through an unbounded outbox drained by a dedicated goroutine, so
// a backend that stops reading stdin can never block mojols itself: the
// request timeouts notice the hang and the supervisor kills the process.
type backend struct {
	gen     int
	cmd     *exec.Cmd
	started time.Time

	mu     sync.Mutex
	outbox [][]byte
	closed bool
	signal chan struct{}

	exited  chan struct{} // closed after the process has been reaped
	exitErr error

	stopping atomic.Bool // mojols asked it to exit: not a failure

	tailMu sync.Mutex
	tail   []string // last stderr lines, for crash reports
}

const stderrTail = 120

func (b *backend) keepStderr(line string) {
	b.tailMu.Lock()
	defer b.tailMu.Unlock()
	if len(b.tail) == stderrTail {
		copy(b.tail, b.tail[1:])
		b.tail = b.tail[:stderrTail-1]
	}
	b.tail = append(b.tail, line)
}

// crashReport returns the backend's final output, starting at its crash
// banner (LLVM prints "PLEASE submit a bug report" then "Stack dump:") when
// there is one, otherwise the last 40 lines.
func (b *backend) crashReport() string {
	b.tailMu.Lock()
	defer b.tailMu.Unlock()
	start := len(b.tail) - 40
	for i, l := range b.tail {
		if strings.Contains(l, "PLEASE submit a bug report") || strings.Contains(l, "Please submit a bug report") ||
			strings.HasPrefix(l, "Stack dump:") || strings.Contains(l, "Assertion") || strings.Contains(l, "LLVM ERROR") {
			start = i
			break
		}
	}
	if start < 0 {
		start = 0
	}
	return strings.Join(b.tail[start:], "\n")
}

func startBackend(c Command, gen int, log *slog.Logger, tr *trace.Tracer,
	onMessage func(*backend, *jsonrpc.Message), onMalformed func(string), onExit func(*backend, error)) (*backend, error) {

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
		return nil, fmt.Errorf("start %s: %w", c.Path, err)
	}
	b := &backend{
		gen:     gen,
		cmd:     cmd,
		started: time.Now(),
		signal:  make(chan struct{}, 1),
		exited:  make(chan struct{}),
	}
	blog := log.With("backend", gen, "pid", cmd.Process.Pid)
	blog.Info("backend started", "path", c.Path, "args", c.Args)

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			blog.Debug("backend stderr", "line", sc.Text())
			b.keepStderr(sc.Text())
		}
	}()
	go b.writeLoop(stdin, blog)
	go func() {
		readErr := b.readLoop(stdout, blog, tr, onMessage, onMalformed)
		b.kill()
		<-stderrDone
		waitErr := cmd.Wait()
		b.mu.Lock()
		b.closed = true
		b.outbox = nil
		b.mu.Unlock()
		reason := readErr
		if waitErr != nil {
			reason = fmt.Errorf("%v (%w)", readErr, waitErr)
		}
		b.exitErr = reason
		close(b.exited)
		if b.stopping.Load() {
			blog.Info("backend stopped", "uptime", time.Since(b.started).Round(time.Millisecond))
		} else {
			blog.Warn("backend exited", "reason", reason, "uptime", time.Since(b.started).Round(time.Millisecond))
			if report := b.crashReport(); report != "" {
				// Logged at any level: an upstream crash is only fixable with
				// its stack dump, and it only exists in this output.
				blog.Warn("backend crash output", "output", report)
			}
		}
		onExit(b, reason)
	}()
	return b, nil
}

// send queues one message body for the backend. It never blocks.
func (b *backend) send(body []byte) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.outbox = append(b.outbox, body)
	b.mu.Unlock()
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

func (b *backend) writeLoop(stdin io.WriteCloser, log *slog.Logger) {
	defer stdin.Close()
	w := jsonrpc.NewWriter(stdin)
	for {
		select {
		case <-b.signal:
		case <-b.exited:
			return
		}
		b.mu.Lock()
		batch := b.outbox
		b.outbox = nil
		b.mu.Unlock()
		for _, body := range batch {
			if err := w.WriteRaw(body); err != nil {
				log.Debug("backend stdin closed", "err", err)
				return
			}
		}
	}
}

func (b *backend) readLoop(stdout io.Reader, log *slog.Logger, tr *trace.Tracer, onMessage func(*backend, *jsonrpc.Message), onMalformed func(string)) error {
	r := jsonrpc.NewReader(stdout)
	for {
		m, body, err := r.Read()
		var de *jsonrpc.DecodeError
		switch {
		case errors.As(err, &de):
			tr.Log(trace.BackendToProxy, b.gen, de.Body)
			onMalformed("malformed message from backend: " + de.Reason)
			continue
		case errors.Is(err, io.EOF):
			return errors.New("backend closed stdout")
		case err != nil:
			return fmt.Errorf("reading backend: %w", err)
		}
		tr.Log(trace.BackendToProxy, b.gen, body)
		onMessage(b, m)
	}
}

func (b *backend) kill() {
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}

// stop asks the backend to exit and kills it after grace.
func (b *backend) stop(grace time.Duration) {
	b.stopping.Store(true)
	if exit, err := (&jsonrpc.Message{Method: "exit"}).MarshalJSON(); err == nil {
		b.send(exit)
	}
	select {
	case <-b.exited:
	case <-time.After(grace):
		b.kill()
		<-b.exited
	}
}
