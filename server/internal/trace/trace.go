// Package trace records every LSP message mojols sees, as JSON lines, so a
// real editing session can be replayed in tests or attached to a bug report.
package trace

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Direction of a traced message.
type Direction string

const (
	ClientToProxy  Direction = "client->mojols"
	ProxyToClient  Direction = "mojols->client"
	ProxyToBackend Direction = "mojols->backend"
	BackendToProxy Direction = "backend->mojols"
)

// Tracer writes one JSON object per message. A nil *Tracer is a no-op.
type Tracer struct {
	mu sync.Mutex
	w  *bufio.Writer
	c  io.Closer
}

// New returns a tracer writing to w; it closes w on Close if w is an io.Closer.
func New(w io.Writer) *Tracer {
	t := &Tracer{w: bufio.NewWriter(w)}
	if c, ok := w.(io.Closer); ok {
		t.c = c
	}
	return t
}

type record struct {
	Time string          `json:"t"`
	Dir  Direction       `json:"dir"`
	Gen  int             `json:"gen,omitempty"`
	Msg  json.RawMessage `json:"msg,omitempty"`
	Raw  string          `json:"raw,omitempty"` // bodies that are not valid JSON
}

// Log records one message body. gen is the backend generation, 0 if none.
func (t *Tracer) Log(dir Direction, gen int, body []byte) {
	if t == nil {
		return
	}
	r := record{Time: time.Now().UTC().Format(time.RFC3339Nano), Dir: dir, Gen: gen}
	if json.Valid(body) {
		r.Msg = body
	} else {
		r.Raw = string(body)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w.Write(line)
	t.w.WriteByte('\n')
	t.w.Flush()
}

// Close flushes and closes the underlying writer.
func (t *Tracer) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.w.Flush()
	if t.c != nil {
		return t.c.Close()
	}
	return nil
}
