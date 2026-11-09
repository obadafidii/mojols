package text

import (
	"testing"
	"unicode/utf8"
)

func TestLinesRoundTrip(t *testing.T) {
	docs := []string{"", "a", "a\nb", "a\r\nb\rc\n", "é😀x\nyé\n\n", "\n\n\r\n"}
	for _, doc := range docs {
		l := NewLines(doc)
		for _, enc := range []Encoding{UTF8, UTF16, UTF32} {
			for off := 0; off <= len(doc); off++ {
				if off < len(doc) && !utf8.RuneStart(doc[off]) {
					continue
				}
				p := l.Position(off, enc)
				if got := l.Offset(p, enc); got != off {
					// A position on a line terminator byte maps to the end
					// of the line, which is the same place.
					if !(off < len(doc) && (doc[off] == '\n' || doc[off] == '\r')) {
						t.Errorf("%q %v: offset %d -> %v -> %d", doc, enc, off, p, got)
					}
				}
				if got := Offset(doc, p, enc); got != l.Offset(p, enc) {
					t.Errorf("%q %v: Lines.Offset %d != Offset %d at %v", doc, enc, l.Offset(p, enc), got, p)
				}
			}
		}
	}
}

func TestLinesPosition(t *testing.T) {
	l := NewLines("ab\n😀c")
	if p := l.Position(8, UTF16); p != (Position{1, 3}) {
		t.Errorf("utf16: %v", p)
	}
	if p := l.Position(8, UTF8); p != (Position{1, 5}) {
		t.Errorf("utf8: %v", p)
	}
	if p := l.Position(5, UTF16); p != (Position{1, 0}) {
		t.Errorf("inside a character: %v", p)
	}
}

func FuzzLines(f *testing.F) {
	f.Add("a\r\nb😀\rc", 3, uint32(1), uint32(2))
	f.Fuzz(func(t *testing.T, doc string, off int, line, char uint32) {
		l := NewLines(doc)
		for _, enc := range []Encoding{UTF8, UTF16, UTF32} {
			_ = l.Position(off, enc)
			p := Position{Line: line % 64, Character: char % 256}
			if a, b := l.Offset(p, enc), Offset(doc, p, enc); a != b {
				t.Fatalf("%q %v %v: Lines.Offset %d, Offset %d", doc, enc, p, a, b)
			}
		}
	})
}
