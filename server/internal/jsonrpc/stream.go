package jsonrpc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

const (
	// DefaultMaxBody caps a single message body. Semantic tokens for a huge
	// file are the largest real messages; 256 MiB is far above that.
	DefaultMaxBody = 256 << 20
	maxHeaderLine  = 4096
	maxHeaderLines = 32
)

// ErrFraming reports a broken base-protocol stream (bad or missing headers).
// Unlike *DecodeError, the stream cannot be resynchronised after it.
var ErrFraming = errors.New("jsonrpc: framing error")

// Reader reads Content-Length framed messages.
type Reader struct {
	br      *bufio.Reader
	MaxBody int
}

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10), MaxBody: DefaultMaxBody}
}

// ReadRaw returns the next message body. It returns io.EOF only when the
// stream ends cleanly between messages.
func (r *Reader) ReadRaw() ([]byte, error) {
	length := -1
	lines := 0
	for {
		line, err := r.br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxHeaderLine {
				return nil, fmt.Errorf("%w: header line too long", ErrFraming)
			}
			if errors.Is(err, io.EOF) {
				if lines == 0 && len(line) == 0 {
					return nil, io.EOF
				}
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if len(line) > maxHeaderLine {
			return nil, fmt.Errorf("%w: header line too long", ErrFraming)
		}
		lines++
		if lines > maxHeaderLines {
			return nil, fmt.Errorf("%w: too many header lines", ErrFraming)
		}
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			if lines == 1 {
				// Tolerate stray blank lines between messages.
				lines = 0
				continue
			}
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, fmt.Errorf("%w: malformed header %q", ErrFraming, trimmed)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad Content-Length %q", ErrFraming, value)
			}
			length = n
		}
		// Content-Type and unknown headers are ignored.
	}
	if length < 0 {
		return nil, fmt.Errorf("%w: missing Content-Length", ErrFraming)
	}
	if length > r.MaxBody {
		return nil, fmt.Errorf("%w: body of %d bytes exceeds limit %d", ErrFraming, length, r.MaxBody)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r.br, body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}

// Read returns the next decoded message and its raw body. A *DecodeError
// means the body was skipped and reading may continue.
func (r *Reader) Read() (*Message, []byte, error) {
	body, err := r.ReadRaw()
	if err != nil {
		return nil, nil, err
	}
	m, err := Decode(body)
	return m, body, err
}

// Writer writes Content-Length framed messages. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteRaw writes one framed body in a single Write call.
func (w *Writer) WriteRaw(body []byte) error {
	var buf bytes.Buffer
	buf.Grow(len(body) + 32)
	buf.WriteString("Content-Length: ")
	buf.WriteString(strconv.Itoa(len(body)))
	buf.WriteString("\r\n\r\n")
	buf.Write(body)
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(buf.Bytes())
	return err
}

// Write encodes and writes m, returning the body that was written.
func (w *Writer) Write(m *Message) ([]byte, error) {
	body, err := m.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return body, w.WriteRaw(body)
}
