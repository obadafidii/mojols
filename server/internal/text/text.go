// Package text maps LSP positions to byte offsets and applies document edits.
//
// LSP positions count characters in a negotiated encoding (UTF-16 by
// default). Getting this wrong corrupts every document that contains an
// emoji or a non-BMP character, so all conversions live here and are tested.
package text

import (
	"fmt"
	"unicode/utf8"
)

// Encoding is a position encoding negotiated in initialize.
type Encoding int

const (
	UTF16 Encoding = iota // LSP default
	UTF8
	UTF32
)

// ParseEncoding maps an LSP PositionEncodingKind to an Encoding.
// Unknown or empty values mean UTF-16, the protocol default.
func ParseEncoding(kind string) Encoding {
	switch kind {
	case "utf-8":
		return UTF8
	case "utf-32":
		return UTF32
	default:
		return UTF16
	}
}

func (e Encoding) String() string {
	switch e {
	case UTF8:
		return "utf-8"
	case UTF32:
		return "utf-32"
	default:
		return "utf-16"
	}
}

// Position is an LSP position: zero-based line and character.
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range is an LSP range; End is exclusive.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// ContentChange is a TextDocumentContentChangeEvent. A nil Range replaces
// the whole document.
type ContentChange struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

func units(r rune, size int, enc Encoding) uint32 {
	switch enc {
	case UTF8:
		return uint32(size)
	case UTF32:
		return 1
	default:
		if r >= 0x10000 {
			return 2
		}
		return 1
	}
}

// Offset converts p to a byte offset in content.
//
// Like editors, it is lenient: a line past the end maps to len(content), a
// character past the end of its line maps to the end of that line, and a
// position inside a multi-unit character snaps to the start of it.
// Line terminators are \n, \r\n and \r, per the LSP specification.
func Offset(content string, p Position, enc Encoding) int {
	i := 0
	for line := uint32(0); line < p.Line; line++ {
		next := lineEnd(content, i)
		if next >= len(content) {
			return len(content)
		}
		if content[next] == '\r' && next+1 < len(content) && content[next+1] == '\n' {
			next++
		}
		i = next + 1
	}
	end := lineEnd(content, i)
	var col uint32
	for i < end {
		r, size := utf8.DecodeRuneInString(content[i:end])
		u := units(r, size, enc)
		if col+u > p.Character {
			return i
		}
		col += u
		i += size
	}
	return i
}

// lineEnd returns the index of the first line terminator at or after i, or
// len(content).
func lineEnd(content string, i int) int {
	for j := i; j < len(content); j++ {
		if content[j] == '\n' || content[j] == '\r' {
			return j
		}
	}
	return len(content)
}

// Apply applies changes in order and returns the new content.
func Apply(content string, changes []ContentChange, enc Encoding) (string, error) {
	for i, c := range changes {
		if c.Range == nil {
			content = c.Text
			continue
		}
		start := Offset(content, c.Range.Start, enc)
		end := Offset(content, c.Range.End, enc)
		if start > end {
			return "", fmt.Errorf("change %d: range start %v is after end %v", i, c.Range.Start, c.Range.End)
		}
		content = content[:start] + c.Text + content[end:]
	}
	return content, nil
}
