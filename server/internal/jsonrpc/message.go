// Package jsonrpc implements JSON-RPC 2.0 messages and the LSP base protocol
// (Content-Length framed) used to talk to editors and to mojo-lsp-server.
//
// It is deliberately strict about what it decodes: a message that does not
// match the JSON-RPC 2.0 shape is rejected with a *DecodeError instead of
// being forwarded, so garbage from one side never reaches the other.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Error codes from JSON-RPC 2.0 and the LSP specification.
const (
	CodeParseError           = -32700
	CodeInvalidRequest       = -32600
	CodeMethodNotFound       = -32601
	CodeInvalidParams        = -32602
	CodeInternalError        = -32603
	CodeServerNotInitialized = -32002
	CodeUnknownErrorCode     = -32001
	CodeRequestFailed        = -32803
	CodeServerCancelled      = -32802
	CodeContentModified      = -32801
	CodeRequestCancelled     = -32800
)

// ID is a JSON-RPC request id, which is either an integer or a string.
// It is comparable, so it can be used as a map key.
type ID struct {
	num   int64
	str   string
	isStr bool
}

// NumberID returns an integer id.
func NumberID(n int64) ID { return ID{num: n} }

// StringID returns a string id.
func StringID(s string) ID { return ID{str: s, isStr: true} }

// IsString reports whether the id is a string.
func (id ID) IsString() bool { return id.isStr }

// Number returns the integer value of a numeric id.
func (id ID) Number() int64 { return id.num }

// String renders the id for logs: 7 or "abc".
func (id ID) String() string {
	if id.isStr {
		return strconv.Quote(id.str)
	}
	return strconv.FormatInt(id.num, 10)
}

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.isStr {
		return json.Marshal(id.str)
	}
	return strconv.AppendInt(nil, id.num, 10), nil
}

// UnmarshalJSON implements json.Unmarshaler. It accepts integers and strings.
func (id *ID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return errors.New("empty id")
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("invalid string id: %w", err)
		}
		*id = StringID(s)
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("id must be an integer or a string, got %s", truncate(b, 32))
	}
	*id = NumberID(n)
	return nil
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// NewError builds an error object.
func NewError(code int64, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Message is any JSON-RPC 2.0 message: request, notification or response.
//
//   - request:      ID != nil, Method != ""
//   - notification: ID == nil, Method != ""
//   - response:     Method == "" (ID may be nil for an error about an unreadable request)
type Message struct {
	ID     *ID
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *Error
}

// IsRequest reports whether m is a request.
func (m *Message) IsRequest() bool { return m.Method != "" && m.ID != nil }

// IsNotification reports whether m is a notification.
func (m *Message) IsNotification() bool { return m.Method != "" && m.ID == nil }

// IsResponse reports whether m is a response.
func (m *Message) IsResponse() bool { return m.Method == "" }

// Kind returns "request", "notification" or "response".
func (m *Message) Kind() string {
	switch {
	case m.IsRequest():
		return "request"
	case m.IsNotification():
		return "notification"
	default:
		return "response"
	}
}

// NewRequest builds a request. params may be nil.
func NewRequest(id ID, method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{ID: &id, Method: method, Params: raw}, nil
}

// NewNotification builds a notification. params may be nil.
func NewNotification(method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{Method: method, Params: raw}, nil
}

// NewResponse builds a successful response. A nil result encodes as null.
func NewResponse(id ID, result any) (*Message, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &Message{ID: &id, Result: raw}, nil
}

// NewErrorResponse builds an error response. id may be nil.
func NewErrorResponse(id *ID, e *Error) *Message {
	return &Message{ID: id, Error: e}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

// MarshalJSON implements json.Marshaler. It always emits valid JSON-RPC 2.0:
// responses carry exactly one of result or error, and an id (null if unknown).
func (m *Message) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0"`)
	writeID := func() error {
		b.WriteString(`,"id":`)
		if m.ID == nil {
			b.WriteString("null")
			return nil
		}
		raw, err := m.ID.MarshalJSON()
		if err != nil {
			return err
		}
		b.Write(raw)
		return nil
	}
	if m.Method != "" {
		if m.ID != nil {
			if err := writeID(); err != nil {
				return nil, err
			}
		}
		method, err := json.Marshal(m.Method)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"method":`)
		b.Write(method)
		if len(m.Params) > 0 {
			b.WriteString(`,"params":`)
			b.Write(m.Params)
		}
	} else {
		if err := writeID(); err != nil {
			return nil, err
		}
		if m.Error != nil {
			e, err := json.Marshal(m.Error)
			if err != nil {
				return nil, err
			}
			b.WriteString(`,"error":`)
			b.Write(e)
		} else {
			b.WriteString(`,"result":`)
			if len(m.Result) == 0 {
				b.WriteString("null")
			} else {
				b.Write(m.Result)
			}
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// DecodeError reports a message body that was framed correctly but is not a
// valid JSON-RPC 2.0 message. The stream is still usable after it.
type DecodeError struct {
	Reason string
	Body   []byte
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("invalid JSON-RPC message: %s (body: %s)", e.Reason, truncate(e.Body, 120))
}

type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// Decode parses one message body.
func Decode(body []byte) (*Message, error) {
	var in inbound
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, &DecodeError{Reason: err.Error(), Body: body}
	}
	if in.JSONRPC != "2.0" {
		return nil, &DecodeError{Reason: fmt.Sprintf("jsonrpc must be \"2.0\", got %q", in.JSONRPC), Body: body}
	}
	m := &Message{}
	if len(in.ID) > 0 && string(in.ID) != "null" {
		var id ID
		if err := id.UnmarshalJSON(in.ID); err != nil {
			return nil, &DecodeError{Reason: err.Error(), Body: body}
		}
		m.ID = &id
	}
	if in.Method != nil {
		if *in.Method == "" {
			return nil, &DecodeError{Reason: "empty method", Body: body}
		}
		if len(in.Result) > 0 || len(in.Error) > 0 {
			return nil, &DecodeError{Reason: "message has both method and result/error", Body: body}
		}
		m.Method = *in.Method
		if len(in.Params) > 0 && string(in.Params) != "null" {
			if p := bytes.TrimSpace(in.Params); len(p) == 0 || (p[0] != '{' && p[0] != '[') {
				return nil, &DecodeError{Reason: "params must be an object or array", Body: body}
			}
			m.Params = in.Params
		}
		return m, nil
	}
	// Response.
	hasResult := len(in.Result) > 0
	hasError := len(in.Error) > 0 && string(in.Error) != "null"
	switch {
	case hasResult && hasError:
		return nil, &DecodeError{Reason: "response has both result and error", Body: body}
	case !hasResult && !hasError:
		return nil, &DecodeError{Reason: "response has neither result nor error", Body: body}
	case hasError:
		var e Error
		if err := json.Unmarshal(in.Error, &e); err != nil {
			return nil, &DecodeError{Reason: "invalid error object: " + err.Error(), Body: body}
		}
		m.Error = &e
	default:
		if m.ID == nil {
			return nil, &DecodeError{Reason: "successful response without id", Body: body}
		}
		m.Result = in.Result
	}
	return m, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
