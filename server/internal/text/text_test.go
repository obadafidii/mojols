package text

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestOffset(t *testing.T) {
	const doc = "a😀b\r\ncd\re\nfn main():"
	cases := []struct {
		line, char uint32
		enc        Encoding
		want       int
	}{
		{0, 0, UTF16, 0},
		{0, 1, UTF16, 1},
		{0, 2, UTF16, 1}, // inside the surrogate pair: snaps to its start
		{0, 3, UTF16, 5}, // after the emoji (4 bytes)
		{0, 4, UTF16, 6}, // end of line 0, before \r\n
		{0, 99, UTF16, 6},
		{0, 2, UTF32, 5},
		{0, 5, UTF8, 5},
		{0, 3, UTF8, 1},  // inside the emoji's bytes
		{1, 0, UTF16, 8}, // after \r\n
		{1, 2, UTF16, 10},
		{2, 0, UTF16, 11}, // after lone \r
		{3, 2, UTF16, 15},
		{9, 0, UTF16, len(doc)},
	}
	for _, c := range cases {
		got := Offset(doc, Position{c.line, c.char}, c.enc)
		if got != c.want {
			t.Errorf("Offset(%d:%d, %v) = %d, want %d", c.line, c.char, c.enc, got, c.want)
		}
	}
}

func TestApply(t *testing.T) {
	r := func(l1, c1, l2, c2 uint32) *Range { return &Range{Position{l1, c1}, Position{l2, c2}} }
	got, err := Apply("def f():\n    pass\n", []ContentChange{
		{Range: r(1, 4, 1, 8), Text: "return 1"},
		{Range: r(0, 4, 0, 5), Text: "main"},
		{Range: r(2, 0, 2, 0), Text: "# 😀\n"},
		{Range: r(2, 4, 2, 4), Text: "!"}, // after the emoji: 2 UTF-16 units
	}, UTF16)
	if err != nil {
		t.Fatal(err)
	}
	if want := "def main():\n    return 1\n# 😀!\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := Apply("abc", []ContentChange{{Range: r(0, 2, 0, 1)}}, UTF16); err == nil {
		t.Fatal("want error for inverted range")
	}
	got, _ = Apply("old", []ContentChange{{Text: "new"}}, UTF16)
	if got != "new" {
		t.Fatalf("full replace: %q", got)
	}
}

func TestStore(t *testing.T) {
	s := NewStore()
	s.Open(Document{URI: "file:///b.mojo", Version: 1, Text: "x"})
	s.Open(Document{URI: "file:///a.mojo", Version: 1, Text: "y"})
	if err := s.Change("file:///b.mojo", 2, []ContentChange{{Range: &Range{Position{0, 1}, Position{0, 1}}, Text: "z"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Change("file:///missing.mojo", 1, nil); err == nil {
		t.Fatal("want error for unknown document")
	}
	snap := s.Snapshot()
	if len(snap) != 2 || snap[0].URI != "file:///a.mojo" || snap[1].Text != "xz" || snap[1].Version != 2 {
		t.Fatalf("snapshot: %+v", snap)
	}
	s.Close("file:///a.mojo")
	if s.Len() != 1 {
		t.Fatal("close did not remove")
	}
}

// refOffset is an independent, slow implementation used to check Offset.
func refOffset(content string, p Position, enc Encoding) int {
	var lines []string // each with its terminator
	start := 0
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\n':
			lines = append(lines, content[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(content) && content[i+1] == '\n' {
				continue
			}
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	lines = append(lines, content[start:])
	if int(p.Line) >= len(lines) {
		return len(content)
	}
	base := 0
	for _, l := range lines[:p.Line] {
		base += len(l)
	}
	line := strings.TrimRight(lines[p.Line], "\r\n")
	off := 0
	var col uint32
	for off < len(line) {
		r, size := utf8.DecodeRuneInString(line[off:])
		var u uint32
		switch enc {
		case UTF8:
			u = uint32(size)
		case UTF32:
			u = 1
		default:
			u = uint32(len(utf16.Encode([]rune{r})))
		}
		if col+u > p.Character {
			break
		}
		col += u
		off += size
	}
	return base + off
}

func FuzzOffset(f *testing.F) {
	f.Add("a😀b\r\ncd\re\n", uint32(0), uint32(3), uint8(0))
	f.Add("\r\r\n\n", uint32(2), uint32(0), uint8(1))
	f.Add("é\xffx", uint32(0), uint32(2), uint8(2))
	f.Fuzz(func(t *testing.T, content string, line, char uint32, e uint8) {
		enc := Encoding(e % 3)
		p := Position{line % 8, char % 16}
		got, want := Offset(content, p, enc), refOffset(content, p, enc)
		if got != want {
			t.Fatalf("Offset(%q, %v, %v) = %d, reference %d", content, p, enc, got, want)
		}
	})
}
