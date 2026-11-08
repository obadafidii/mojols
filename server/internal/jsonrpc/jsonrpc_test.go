package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func frame(body string) string {
	return "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body
}

func itoa(n int) string { return string(mustJSON(n)) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	req, _ := NewRequest(NumberID(7), "textDocument/hover", map[string]any{"x": 1})
	notif, _ := NewNotification("initialized", map[string]any{})
	resp, _ := NewResponse(StringID("abc"), nil)
	errResp := NewErrorResponse(nil, NewError(CodeParseError, "bad"))
	for _, m := range []*Message{req, notif, resp, errResp} {
		if _, err := w.Write(m); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&buf)
	want := []string{"request", "notification", "response", "response"}
	for i, k := range want {
		m, _, err := r.Read()
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if m.Kind() != k {
			t.Fatalf("message %d: kind %s, want %s", i, m.Kind(), k)
		}
	}
	if _, _, err := r.Read(); err != io.EOF {
		t.Fatalf("want clean EOF, got %v", err)
	}
}

func TestMarshalShapes(t *testing.T) {
	cases := []struct {
		msg  *Message
		want string
	}{
		{&Message{ID: ptr(NumberID(1))}, `{"jsonrpc":"2.0","id":1,"result":null}`},
		{NewErrorResponse(nil, NewError(-1, "x")), `{"jsonrpc":"2.0","id":null,"error":{"code":-1,"message":"x"}}`},
		{&Message{Method: "exit"}, `{"jsonrpc":"2.0","method":"exit"}`},
		{&Message{ID: ptr(StringID("a")), Method: "m", Params: json.RawMessage(`[1]`)}, `{"jsonrpc":"2.0","id":"a","method":"m","params":[1]}`},
	}
	for _, c := range cases {
		got, err := c.msg.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("got %s\nwant %s", got, c.want)
		}
	}
}

func ptr(id ID) *ID { return &id }

func TestDecodeRejects(t *testing.T) {
	bad := []string{
		`not json`,
		`{"jsonrpc":"1.0","method":"x"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"x"}`,
		`{"jsonrpc":"2.0","id":{},"method":"x"}`,
		`{"jsonrpc":"2.0","method":""}`,
		`{"jsonrpc":"2.0","method":"x","params":3}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":""}}`,
		`{"jsonrpc":"2.0","method":"x","result":1}`,
		`{"jsonrpc":"2.0","result":1}`,
	}
	for _, b := range bad {
		_, err := Decode([]byte(b))
		var de *DecodeError
		if !errors.As(err, &de) {
			t.Errorf("Decode(%s): want DecodeError, got %v", b, err)
		}
	}
}

func TestDecodeAccepts(t *testing.T) {
	m, err := Decode([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	if err != nil || string(m.Result) != "null" || !m.IsResponse() {
		t.Fatalf("null result: %v %+v", err, m)
	}
	m, err = Decode([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"p"}}`))
	if err != nil || m.ID != nil || m.Error.Code != CodeParseError {
		t.Fatalf("null id error: %v %+v", err, m)
	}
	m, err = Decode([]byte(`{"jsonrpc":"2.0","id":"x","method":"m","params":null}`))
	if err != nil || !m.IsRequest() || m.Params != nil || m.ID.String() != `"x"` {
		t.Fatalf("string id request: %v %+v", err, m)
	}
}

func TestFramingErrors(t *testing.T) {
	cases := map[string]error{
		"Content-Type: x\r\n\r\n{}":                    ErrFraming,
		"Content-Length: abc\r\n\r\n":                  ErrFraming,
		"Content-Length: -1\r\n\r\n":                   ErrFraming,
		"garbage\r\n\r\n":                              ErrFraming,
		"Content-Length: 10\r\n\r\n{}":                 io.ErrUnexpectedEOF,
		"Content-Length: 2\r\n":                        io.ErrUnexpectedEOF,
		"Content-Length: " + strings.Repeat("9", 5000): ErrFraming,
		"Content-Length: 999999999999\r\n\r\n":         ErrFraming,
	}
	for in, want := range cases {
		_, err := NewReader(strings.NewReader(in)).ReadRaw()
		if !errors.Is(err, want) {
			t.Errorf("%.40q: got %v, want %v", in, err, want)
		}
	}
}

func TestTolerantHeaders(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"x"}`
	in := "\r\ncontent-length:" + itoa(len(body)) + "\nContent-Type: application/vscode-jsonrpc; charset=utf-8\n\n" + body + frame(body)
	r := NewReader(strings.NewReader(in))
	for i := 0; i < 2; i++ {
		got, err := r.ReadRaw()
		if err != nil || string(got) != body {
			t.Fatalf("message %d: %q %v", i, got, err)
		}
	}
}

func TestDecodeErrorKeepsStreamUsable(t *testing.T) {
	good := `{"jsonrpc":"2.0","method":"ok"}`
	r := NewReader(strings.NewReader(frame("{bad") + frame(good)))
	if _, _, err := r.Read(); err == nil {
		t.Fatal("want decode error")
	}
	m, _, err := r.Read()
	if err != nil || m.Method != "ok" {
		t.Fatalf("stream not usable after decode error: %v", err)
	}
}

func FuzzReader(f *testing.F) {
	f.Add(frame(`{"jsonrpc":"2.0","id":1,"method":"m","params":{}}`))
	f.Add(frame(`{"jsonrpc":"2.0","id":1,"result":null}`))
	f.Add("Content-Length: 3\r\n\r\n{}}")
	f.Fuzz(func(t *testing.T, in string) {
		r := NewReader(strings.NewReader(in))
		r.MaxBody = 1 << 16
		for i := 0; i < 8; i++ {
			m, _, err := r.Read()
			var de *DecodeError
			if errors.As(err, &de) {
				continue
			}
			if err != nil {
				return
			}
			// Anything we accept must re-encode to something we accept again.
			out, err := m.MarshalJSON()
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if _, err := Decode(out); err != nil {
				t.Fatalf("re-encoded message rejected: %v\n%s", err, out)
			}
		}
	})
}
