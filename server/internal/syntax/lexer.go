// Package syntax is mojols' own, error-tolerant reading of Mojo source: a
// lexer, an outline of declarations and scopes, and every identifier
// occurrence. It is written in Go with no dependencies (no cgo, no
// tree-sitter) so mojols stays a single static binary, and it never fails:
// broken code yields a best-effort result, never an error or a panic.
//
// It is not a compiler front end. It recovers what an editor needs to find
// and rename symbols across files quickly; the official server stays the
// authority on types and semantics.
package syntax

import (
	"unicode"
	"unicode/utf8"
)

// Kind classifies a token.
type Kind uint8

const (
	EOF     Kind = iota
	Newline      // end of a logical line
	Indent       // the next line is indented deeper
	Dedent       // one indentation level closed
	Ident        // identifier, keyword, or `backtick` identifier
	Number
	String // any string literal; see Token.Interp
	Op     // operator or punctuation
	Illegal
)

func (k Kind) String() string {
	switch k {
	case EOF:
		return "EOF"
	case Newline:
		return "Newline"
	case Indent:
		return "Indent"
	case Dedent:
		return "Dedent"
	case Ident:
		return "Ident"
	case Number:
		return "Number"
	case String:
		return "String"
	case Op:
		return "Op"
	}
	return "Illegal"
}

// Span is a half-open byte range [Start, End) in the source.
type Span struct{ Start, End int }

// Token is one lexical token. Positions are byte offsets into the source.
type Token struct {
	Kind       Kind
	Start, End int
	// Interp lists the expressions interpolated in a t- or f-string
	// (`t"x is {self.x}"` has one: `self.x`). They hold real references.
	Interp []Span
}

// Lexer output: the tokens of one file, plus the comment spans.
type Lexed struct {
	Src      []byte
	Tokens   []Token
	Comments []Span
}

// Text returns the source text of t.
func (l *Lexed) Text(t Token) string { return string(l.Src[t.Start:t.End]) }

// IdentName returns an identifier's name, without backticks.
func IdentName(src []byte, t Token) string {
	s := src[t.Start:t.End]
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return string(s[1 : len(s)-1])
	}
	return string(s)
}

type lexer struct {
	src      []byte
	pos      int
	toks     []Token
	comments []Span

	indents  []int // indentation stack, always starting with 0
	brackets []bracket
	lineHas  bool // the current logical line has produced tokens
}

type bracket struct {
	open   byte
	indent int // indentation of the line the bracket opened on
}

// Lex tokenizes src. It never fails; malformed input produces Illegal
// tokens and a best-effort structure.
func Lex(src []byte) *Lexed {
	// About one token per 4.5 bytes of source on the stdlib.
	l := &lexer{src: src, indents: []int{0}, toks: make([]Token, 0, len(src)/4+16)}
	l.run()
	return &Lexed{Src: src, Tokens: l.toks, Comments: l.comments}
}

func (l *lexer) emit(k Kind, start, end int) {
	l.toks = append(l.toks, Token{Kind: k, Start: start, End: end})
	if k != Newline && k != Indent && k != Dedent {
		l.lineHas = true
	}
}

func (l *lexer) curIndent() int { return l.indents[len(l.indents)-1] }

func (l *lexer) run() {
	l.lineStart()
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\n' || c == '\r':
			l.pos++
			if c == '\r' && l.pos < len(l.src) && l.src[l.pos] == '\n' {
				l.pos++
			}
			if len(l.brackets) == 0 {
				l.endLine(l.pos)
				l.lineStart()
			} else {
				l.bracketLineStart()
			}
		case c == ' ' || c == '\t' || c == '\f':
			l.pos++
		case c == '#':
			start := l.pos
			for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
				l.pos++
			}
			l.comments = append(l.comments, Span{start, l.pos})
		case c == '\\' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == '\n' || l.src[l.pos+1] == '\r'):
			// Explicit line continuation.
			l.pos++
			if l.src[l.pos] == '\r' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
				l.pos++
			}
			l.pos++
		default:
			l.token()
		}
	}
	l.endLine(len(l.src))
	for len(l.indents) > 1 {
		l.indents = l.indents[:len(l.indents)-1]
		l.emit(Dedent, len(l.src), len(l.src))
	}
	l.emit(EOF, len(l.src), len(l.src))
}

// endLine closes the current logical line, if it had any tokens.
func (l *lexer) endLine(at int) {
	if l.lineHas {
		l.emit(Newline, at, at)
		l.lineHas = false
	}
}

// lineStart measures the indentation of the line at l.pos and emits Indent
// or Dedent tokens. Blank and comment-only lines do not count.
func (l *lexer) lineStart() {
	for {
		width, p := l.measure(l.pos)
		if p >= len(l.src) {
			l.pos = p
			return
		}
		c := l.src[p]
		if c == '\n' || c == '\r' || c == '#' {
			// Blank or comment-only: skip it without touching indentation.
			l.pos = p
			if c == '#' {
				start := p
				for l.pos < len(l.src) && l.src[l.pos] != '\n' && l.src[l.pos] != '\r' {
					l.pos++
				}
				l.comments = append(l.comments, Span{start, l.pos})
			}
			if l.pos < len(l.src) {
				if l.src[l.pos] == '\r' {
					l.pos++
				}
				if l.pos < len(l.src) && l.src[l.pos] == '\n' {
					l.pos++
				}
			}
			continue
		}
		l.pos = p
		l.setIndent(width, p)
		return
	}
}

func (l *lexer) setIndent(width, at int) {
	switch {
	case width > l.curIndent():
		l.indents = append(l.indents, width)
		l.emit(Indent, at, at)
	case width < l.curIndent():
		for len(l.indents) > 1 && width < l.curIndent() {
			l.indents = l.indents[:len(l.indents)-1]
			l.emit(Dedent, at, at)
		}
		if width > l.curIndent() {
			// Inconsistent dedent: treat the odd level as a new block.
			l.indents = append(l.indents, width)
			l.emit(Indent, at, at)
		}
	}
}

// measure returns the indentation width of the line starting at p and the
// offset of its first non-blank byte. Tabs advance to the next multiple of 8.
func (l *lexer) measure(p int) (int, int) {
	w := 0
	for p < len(l.src) {
		switch l.src[p] {
		case ' ':
			w++
		case '\t':
			w = (w/8 + 1) * 8
		case '\f':
			w = 0
		default:
			return w, p
		}
		p++
	}
	return w, p
}

// recoveryWords start a statement and can never begin a continuation line
// inside brackets. Seeing one, at or left of the indentation where the
// outermost open bracket started, means that bracket was never closed.
var recoveryWords = map[string]bool{
	"def": true, "fn": true, "struct": true, "trait": true, "comptime": true,
	"alias": true, "var": true, "return": true, "raise": true, "from": true,
	"import": true, "pass": true, "while": true, "try": true, "except": true,
	"finally": true, "elif": true, "class": true, "with": true, "break": true,
	"continue": true,
}

// bracketLineStart handles a new physical line inside open brackets: it is
// normally a continuation, unless it plainly starts a new statement.
func (l *lexer) bracketLineStart() {
	width, p := l.measure(l.pos)
	if p >= len(l.src) {
		return
	}
	if width > l.brackets[0].indent {
		return
	}
	word := l.wordAt(p)
	isDecorator := l.src[p] == '@' && p+1 < len(l.src) && isIdentStart(l.src[p+1])
	if !recoveryWords[word] && !isDecorator {
		return
	}
	// Unclosed bracket: abandon it and start a fresh logical line here.
	l.brackets = l.brackets[:0]
	l.endLine(l.pos)
	l.pos = p
	l.setIndent(width, p)
}

func (l *lexer) wordAt(p int) string {
	e := p
	for e < len(l.src) && (isIdentStart(l.src[e]) || isDigit(l.src[e])) {
		e++
	}
	if e+1 < len(l.src) && l.src[e] == '"' { // a string prefix, not a word
		return ""
	}
	return string(l.src[p:e])
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= utf8.RuneSelf
}

var ops3 = []string{"**=", "//=", ">>=", "<<=", "..."}
var ops2 = []string{"**", "//", ">>", "<<", "<=", ">=", "==", "!=", "->", ":=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "@="}

func (l *lexer) token() {
	start := l.pos
	c := l.src[l.pos]

	// String, possibly with a prefix (r, b, f, t, u, and pairs like rb).
	if q, n := l.stringStart(); q != 0 {
		l.str(start, n, q)
		return
	}
	switch {
	case c == '`':
		e := l.pos + 1
		for e < len(l.src) && l.src[e] != '`' && l.src[e] != '\n' {
			e++
		}
		if e < len(l.src) && l.src[e] == '`' {
			l.pos = e + 1
			l.emit(Ident, start, l.pos)
			return
		}
		l.pos++
		l.emit(Illegal, start, l.pos)
		return
	case isDigit(c) || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		l.number()
		return
	case isIdentStart(c):
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRune(l.src[l.pos:])
			if !unicode.IsLetter(r) {
				l.pos += size
				l.emit(Illegal, start, l.pos)
				return
			}
		}
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if c < utf8.RuneSelf {
				if !isIdentStart(c) && !isDigit(c) {
					break
				}
				l.pos++
				continue
			}
			r, size := utf8.DecodeRune(l.src[l.pos:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				break
			}
			l.pos += size
		}
		l.emit(Ident, start, l.pos)
		return
	}

	for _, set := range [][]string{ops3, ops2} {
		for _, op := range set {
			if l.hasPrefix(op) {
				l.pos += len(op)
				l.emit(Op, start, l.pos)
				return
			}
		}
	}
	switch c {
	case '(', '[', '{':
		indent := l.curIndent()
		if len(l.brackets) > 0 {
			indent = l.brackets[0].indent
		}
		l.brackets = append(l.brackets, bracket{open: c, indent: indent})
	case ')', ']', '}':
		l.closeBracket(c)
	}
	if c < utf8.RuneSelf && isOpByte(c) {
		l.pos++
		l.emit(Op, start, l.pos)
		return
	}
	_, size := utf8.DecodeRune(l.src[l.pos:])
	l.pos += size
	l.emit(Illegal, start, l.pos)
}

func isOpByte(c byte) bool {
	switch c {
	case '(', ')', '[', ']', '{', '}', ',', ':', ';', '.', '=', '+', '-', '*', '/', '%', '&', '|', '^', '~', '<', '>', '@', '!', '?', '$':
		return true
	}
	return false
}

func (l *lexer) closeBracket(c byte) {
	want := map[byte]byte{')': '(', ']': '[', '}': '{'}[c]
	for i := len(l.brackets) - 1; i >= 0; i-- {
		if l.brackets[i].open == want {
			l.brackets = l.brackets[:i]
			return
		}
	}
	// Stray closer: ignore it for nesting purposes.
}

func (l *lexer) hasPrefix(s string) bool {
	return len(l.src)-l.pos >= len(s) && string(l.src[l.pos:l.pos+len(s)]) == s
}

// stringStart reports whether a string literal starts at l.pos: the quote
// byte and the length of its prefix.
func (l *lexer) stringStart() (byte, int) { return stringAt(l.src, l.pos) }

func isStringPrefix(c byte) bool {
	switch c {
	case 'r', 'R', 'b', 'B', 'f', 'F', 't', 'T', 'u', 'U':
		return true
	}
	return false
}

func (l *lexer) str(start, prefixLen int, q byte) {
	end, interp := scanString(l.src, start, prefixLen, q)
	l.pos = end
	l.emit(String, start, end)
	l.toks[len(l.toks)-1].Interp = interp
}

// stringAt reports whether a string literal (with an optional prefix of up
// to two letters) starts at p: its quote and prefix length.
func stringAt(src []byte, p int) (byte, int) {
	if p > 0 && (isIdentStart(src[p-1]) || isDigit(src[p-1])) {
		return 0, 0 // the middle of an identifier, like `count"`
	}
	n := 0
	for n < 2 && p+n < len(src) && isStringPrefix(src[p+n]) {
		n++
	}
	if p+n < len(src) && (src[p+n] == '"' || src[p+n] == '\'') {
		return src[p+n], n
	}
	return 0, 0
}

// scanString scans the string literal at start (prefixLen prefix bytes, then
// quote q) and returns its end offset and, for t- and f-strings, the spans of
// the interpolated expressions. A string nested in an expression is part
// of that expression (`t"a {rt"b {x}"}"` yields one span, `rt"b {x}"`). An unterminated single-quoted string
// ends at the end of its line; an unterminated triple-quoted one at EOF.
func scanString(src []byte, start, prefixLen int, q byte) (int, []Span) {
	raw, interp := false, false
	for _, c := range src[start : start+prefixLen] {
		switch c {
		case 'r', 'R':
			raw = true
		case 'f', 'F', 't', 'T':
			interp = true
		}
	}
	p := start + prefixLen
	triple := p+2 < len(src) && src[p+1] == q && src[p+2] == q
	if triple {
		p += 3
	} else {
		p++
	}
	var spans []Span
	for p < len(src) {
		c := src[p]
		switch {
		case c == '\\' && p+1 < len(src) && (!raw || src[p+1] == q || src[p+1] == '\\'):
			p += 2
			continue
		case !triple && (c == '\n' || c == '\r'):
			return p, spans
		case c == q:
			if !triple {
				return p + 1, spans
			}
			if p+2 < len(src) && src[p+1] == q && src[p+2] == q {
				return p + 3, spans
			}
		case interp && c == '{':
			if p+1 < len(src) && src[p+1] == '{' {
				p += 2
				continue
			}
			var stop bool
			p, spans, stop = scanInterp(src, p+1, triple, spans)
			if stop {
				return p, spans
			}
			continue
		}
		p++
	}
	return len(src), spans
}

// scanInterp scans one `{expr[!conv][:spec]}` starting just after the '{'
// and returns the offset after its closing '}'. stop is true when the
// enclosing single-line string ended first (an unterminated literal).
func scanInterp(src []byte, p int, triple bool, spans []Span) (int, []Span, bool) {
	exprStart, exprEnd := p, -1
	depth := 0
	add := func(end int) {
		if end > exprStart {
			spans = append(spans, Span{exprStart, end})
		}
	}
	for p < len(src) {
		c := src[p]
		if !triple && (c == '\n' || c == '\r') {
			if exprEnd < 0 {
				exprEnd = p
			}
			add(exprEnd)
			return p, spans, true
		}
		if exprEnd < 0 {
			if q, n := stringAt(src, p); q != 0 {
				// A nested literal: skip it whole. Its own interpolations
				// are found when the parser lexes this expression.
				p, _ = scanString(src, p, n, q)
				continue
			}
		}
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case '}':
			if depth == 0 {
				if exprEnd < 0 {
					exprEnd = p
				}
				add(exprEnd)
				return p + 1, spans, false
			}
			depth--
		case ':':
			if depth == 0 && exprEnd < 0 {
				exprEnd = p
			}
		case '!':
			if depth == 0 && exprEnd < 0 && (p+1 >= len(src) || src[p+1] != '=') {
				exprEnd = p
			}
		}
		p++
	}
	if exprEnd < 0 {
		exprEnd = p
	}
	add(exprEnd)
	return p, spans, true
}

func (l *lexer) number() {
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isDigit(c) || c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			// Stop before a method call on a literal like `1.__add__`
			// only if it cannot be part of the number; keep it simple: a
			// '.' followed by a letter other than e/E/j ends the number.
			if c == '.' && l.pos+1 < len(l.src) && isIdentStart(l.src[l.pos+1]) &&
				l.src[l.pos+1] != 'e' && l.src[l.pos+1] != 'E' && l.src[l.pos+1] != 'j' {
				break
			}
			if (c == 'e' || c == 'E') && l.pos+1 < len(l.src) && (l.src[l.pos+1] == '+' || l.src[l.pos+1] == '-') &&
				!(l.pos-start >= 1 && (l.src[start+1] == 'x' || l.src[start+1] == 'X')) {
				l.pos += 2
				continue
			}
			l.pos++
			continue
		}
		break
	}
	l.emit(Number, start, l.pos)
}
