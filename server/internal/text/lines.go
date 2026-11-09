package text

import (
	"sort"
	"unicode/utf8"
)

// Lines indexes the line starts of a document so offsets and positions
// convert in O(log lines). It is immutable; build a new one per version.
type Lines struct {
	content string
	starts  []int // byte offset of each line's first byte
}

// NewLines indexes content. Line terminators are \n, \r\n and \r.
func NewLines(content string) *Lines {
	starts := make([]int, 1, len(content)/40+1)
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case '\n':
			starts = append(starts, i+1)
		case '\r':
			if i+1 < len(content) && content[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		}
	}
	return &Lines{content: content, starts: starts}
}

// Count returns the number of lines.
func (l *Lines) Count() int { return len(l.starts) }

// Position converts a byte offset to a position. An offset inside a
// multi-byte character maps to that character's start; past the end maps
// to the end.
func (l *Lines) Position(offset int, enc Encoding) Position {
	if offset > len(l.content) {
		offset = len(l.content)
	}
	if offset < 0 {
		offset = 0
	}
	line := sort.Search(len(l.starts), func(i int) bool { return l.starts[i] > offset }) - 1
	start := l.starts[line]
	var col uint32
	for i := start; i < offset; {
		r, size := utf8.DecodeRuneInString(l.content[i:])
		if i+size > offset {
			break
		}
		col += units(r, size, enc)
		i += size
	}
	return Position{Line: uint32(line), Character: col}
}

// Offset converts a position to a byte offset, as the package-level
// Offset does, without rescanning earlier lines.
func (l *Lines) Offset(p Position, enc Encoding) int {
	if int(p.Line) >= len(l.starts) {
		return len(l.content)
	}
	start := l.starts[p.Line]
	end := len(l.content)
	if int(p.Line)+1 < len(l.starts) {
		end = l.starts[p.Line+1]
	}
	// Exclude the terminator.
	for end > start && (l.content[end-1] == '\n' || l.content[end-1] == '\r') {
		end--
	}
	i := start
	var col uint32
	for i < end {
		r, size := utf8.DecodeRuneInString(l.content[i:end])
		u := units(r, size, enc)
		if col+u > p.Character {
			return i
		}
		col += u
		i += size
	}
	return i
}

// Range converts a byte range to an LSP range.
func (l *Lines) Range(start, end int, enc Encoding) Range {
	return Range{Start: l.Position(start, enc), End: l.Position(end, enc)}
}
