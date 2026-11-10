package syntax

import (
	"sort"
	"strings"
)

// hardKeywords never name anything. Soft keywords (out, mut, read, owned,
// deinit, raises, where, capturing, ...) are keywords only in a few
// positions and are ordinary names elsewhere (`file.read()`).
var hardKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true,
	"break": true, "continue": true, "def": true, "fn": true, "del": true, "elif": true,
	"else": true, "except": true, "finally": true, "for": true, "from": true, "if": true,
	"import": true, "in": true, "is": true, "not": true, "or": true, "pass": true,
	"raise": true, "return": true, "struct": true, "trait": true, "try": true, "var": true,
	"while": true, "with": true, "yield": true, "comptime": true, "alias": true, "ref": true,
	"lambda": true, "global": true, "nonlocal": true, "async": true, "await": true,
}

// conventions can precede an argument name: `out self`, `mut x`, `ref[o] y`.
var conventions = map[string]bool{
	"out": true, "mut": true, "read": true, "owned": true, "deinit": true, "inout": true,
	"borrowed": true, "var": true, "ref": true, "comptime": true, "fwd": true, "imm": true,
}

// sigWords are keywords in the part of a signature after the argument list
// (`raises`, `where`, capture lists like `{mut self}`).
var sigWords = map[string]bool{
	"raises": true, "where": true, "capturing": true, "escaping": true, "mut": true,
	"imm": true, "read": true, "var": true, "ref": true, "owned": true, "unified": true,
}

// blockWords open an indented block with a ':' at bracket depth 0.
var blockWords = map[string]bool{
	"def": true, "fn": true, "struct": true, "trait": true, "class": true, "if": true,
	"elif": true, "else": true, "for": true, "while": true, "with": true, "try": true,
	"except": true, "finally": true, "match": true, "case": true,
}

type line struct {
	toks     []Token // significant tokens of one logical line
	children []*line
}

type parser struct {
	src        []byte
	f          *File
	decorators []string
}

// Parse reads src. It never fails: whatever cannot be understood is
// skipped one logical line at a time, keeping every identifier in it as a
// plain occurrence.
func Parse(src []byte) (f *File) {
	lx := Lex(src)
	f = &File{Src: src}
	f.Module = &Scope{Kind: SModule, Span: Span{0, len(src)}}
	p := &parser{src: src, f: f}
	defer func() {
		// A bug here must cost one file's precision, never the server.
		if r := recover(); r != nil {
			f.Panic = r
			f.Refs = dropDangling(f.Refs)
			p.finish()
		}
	}()
	root := buildLines(lx.Tokens)
	p.block(root.children, f.Module, nil)
	p.finish()
	return f
}

// dropDangling clears Base and Callee links that point past the end, which
// a parse interrupted by a panic can leave behind.
func dropDangling(refs []Ref) []Ref {
	for i := range refs {
		if refs[i].Base >= len(refs) {
			refs[i].Base = -1
		}
		if refs[i].Callee >= len(refs) {
			refs[i].Callee = -1
		}
		if refs[i].Call >= len(refs) {
			refs[i].Call = -1
		}
	}
	return refs
}

func buildLines(toks []Token) *line {
	root := &line{}
	stack := []*line{root}
	var last *line
	start := -1 // index of the first token of the current logical line
	flush := func(end int) {
		if start < 0 {
			return
		}
		// A logical line is a contiguous run of tokens: slice, don't copy.
		l := &line{toks: toks[start:end:end]}
		parent := stack[len(stack)-1]
		parent.children = append(parent.children, l)
		last = l
		start = -1
	}
	for i, t := range toks {
		switch t.Kind {
		case Newline, EOF:
			flush(i)
		case Indent:
			flush(i)
			if last == nil || last == stack[len(stack)-1] {
				// Indented with nothing to belong to (e.g. the first line):
				// hang it off an empty line so its statements are still read.
				last = &line{}
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, last)
			}
			stack = append(stack, last)
		case Dedent:
			flush(i)
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	flush(len(toks))
	return root
}

// --- helpers over token slices ---

func (p *parser) text(t Token) string { return string(p.src[t.Start:t.End]) }

func (p *parser) isOp(toks []Token, i int, op string) bool {
	return i >= 0 && i < len(toks) && toks[i].Kind == Op && p.text(toks[i]) == op
}

func (p *parser) isWord(toks []Token, i int, w string) bool {
	return i >= 0 && i < len(toks) && toks[i].Kind == Ident && p.text(toks[i]) == w
}

func isOpen(s string) bool  { return s == "(" || s == "[" || s == "{" }
func isClose(s string) bool { return s == ")" || s == "]" || s == "}" }

// match returns the index of the bracket closing the one at i, or
// len(toks) if it is never closed.
func (p *parser) match(toks []Token, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		if toks[j].Kind != Op {
			continue
		}
		s := p.text(toks[j])
		if isOpen(s) {
			depth++
		} else if isClose(s) {
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(toks)
}

// findTop returns the first index >= from where pred holds at bracket
// depth 0, or -1.
func (p *parser) findTop(toks []Token, from int, pred func(i int) bool) int {
	depth := 0
	for i := from; i < len(toks); i++ {
		if toks[i].Kind == Op {
			s := p.text(toks[i])
			if isOpen(s) {
				if depth == 0 && pred(i) {
					return i
				}
				depth++
				continue
			}
			if isClose(s) {
				if depth > 0 {
					depth--
				}
				continue
			}
		}
		if depth == 0 && pred(i) {
			return i
		}
	}
	return -1
}

// splitTop splits toks at depth-0 occurrences of op.
func (p *parser) splitTop(toks []Token, op string) [][]Token {
	var parts [][]Token
	start := 0
	for {
		i := p.findTop(toks, start, func(i int) bool { return p.isOp(toks, i, op) })
		if i < 0 {
			parts = append(parts, toks[start:])
			return parts
		}
		parts = append(parts, toks[start:i])
		start = i + 1
	}
}

func spanOf(toks []Token) Span {
	if len(toks) == 0 {
		return Span{}
	}
	return Span{toks[0].Start, toks[len(toks)-1].End}
}

func lineEnd(l *line) int {
	for len(l.children) > 0 {
		l = l.children[len(l.children)-1]
	}
	if len(l.toks) == 0 {
		return 0
	}
	return l.toks[len(l.toks)-1].End
}

func lineSpan(l *line) Span {
	start := 0
	if len(l.toks) > 0 {
		start = l.toks[0].Start
	}
	end := lineEnd(l)
	if end < start {
		end = start
	}
	return Span{start, end}
}

// --- declarations and refs ---

func (p *parser) newScope(kind ScopeKind, sp Span, parent *Scope, owner *Decl) *Scope {
	s := &Scope{Kind: kind, Span: sp, Parent: parent, Owner: owner}
	p.f.Scopes = append(p.f.Scopes, s)
	return s
}

func (p *parser) declare(scope *Scope, kind DeclKind, name Token, parent *Decl) *Decl {
	d := &Decl{Kind: kind, Name: IdentName(p.src, name), NameSpan: Span{name.Start, name.End}, Parent: parent}
	d.Span = d.NameSpan
	d.Visible = name.Start
	d.Index = len(p.f.Decls)
	p.f.Decls = append(p.f.Decls, d)
	scope.declare(d)
	p.f.Refs = append(p.f.Refs, Ref{Name: d.Name, Span: d.NameSpan, Kind: RDecl, Scope: scope, Decl: d, Base: -1, Callee: -1, Call: -1})
	return d
}

func (p *parser) takeDecorators() []string {
	d := p.decorators
	p.decorators = nil
	return d
}

// --- statements ---

// block reads a sequence of lines in scope.
func (p *parser) block(lines []*line, scope *Scope, owner *Decl) {
	for _, l := range lines {
		for _, part := range p.splitTop(l.toks, ";") {
			if len(part) == 0 {
				continue
			}
			sub := &line{toks: part}
			if len(part) == len(l.toks) {
				sub.children = l.children
			}
			p.statement(sub, scope, owner)
		}
		if len(l.toks) == 0 && len(l.children) > 0 {
			p.block(l.children, scope, owner)
		}
	}
}

// bodyOf returns the lines of a block statement's body: an inline body
// after the colon, then the indented lines.
func (p *parser) bodyOf(l *line, colon int) []*line {
	var body []*line
	if colon >= 0 && colon+1 < len(l.toks) {
		body = append(body, &line{toks: l.toks[colon+1:]})
	}
	return append(body, l.children...)
}

// blockColon is the ':' that ends a block statement's header.
func (p *parser) blockColon(toks []Token, from int) int {
	return p.findTop(toks, from, func(i int) bool { return p.isOp(toks, i, ":") })
}

func (p *parser) statement(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	if len(t) == 0 {
		return
	}
	if p.isOp(t, 0, "@") {
		p.decorator(l, scope)
		return
	}
	if t[0].Kind != Ident || p.src[t[0].Start] == '`' {
		p.exprStatement(l, scope, owner)
		return
	}
	switch p.text(t[0]) {
	case "async":
		// `async def`, `async for`, `async with`: the statement after it.
		if len(t) > 1 {
			p.statement(&line{toks: t[1:], children: l.children}, scope, owner)
			return
		}
	case "def", "fn":
		p.funcDecl(l, scope, owner)
		return
	case "struct", "trait", "class":
		p.typeDecl(l, scope, owner)
		return
	case "comptime", "alias":
		if len(t) > 1 && t[1].Kind == Ident && !hardKeywords[p.text(t[1])] && !p.isOp(t, 2, ".") {
			p.comptimeDecl(l, scope, owner)
			return
		}
		// `comptime if`, `comptime for`, `comptime assert`: the statement after it.
		rest := &line{toks: t[1:], children: l.children}
		p.statement(rest, scope, owner)
		return
	case "var":
		p.varDecl(l, scope, owner)
		return
	case "ref":
		if len(t) > 1 {
			p.refDecl(l, scope, owner)
			return
		}
	case "import", "from":
		p.importStmt(l, scope, owner)
		return
	case "for":
		p.forStmt(l, scope, owner)
		return
	case "with":
		p.withStmt(l, scope, owner)
		return
	case "except":
		p.exceptStmt(l, scope, owner)
		return
	}
	if blockWords[p.text(t[0])] {
		colon := p.blockColon(t, 1)
		end := len(t)
		if colon >= 0 {
			end = colon
		}
		p.scan(t[1:end], scope)
		p.subBlock(l, colon, scope, owner, nil)
		return
	}
	p.exprStatement(l, scope, owner)
}

// subBlock reads the body of a block statement. Inside a def each block
// gets its own scope (Mojo's `var` is block-scoped); at module and struct
// level the body's declarations belong to the enclosing scope (`comptime
// if` choosing between definitions). bind declares names into the body
// scope before it is read.
func (p *parser) subBlock(l *line, colon int, scope *Scope, owner *Decl, bind func(*Scope)) {
	body := p.bodyOf(l, colon)
	inner := scope
	if scope.Func() != nil {
		sp := lineSpan(l)
		inner = p.newScope(SBlock, sp, scope, owner)
	}
	if bind != nil {
		bind(inner)
	}
	p.block(body, inner, owner)
}

func (p *parser) decorator(l *line, scope *Scope) {
	t := l.toks
	name := ""
	for i := 1; i < len(t); i++ {
		if p.isOp(t, i, "(") {
			break
		}
		name += p.text(t[i])
	}
	p.decorators = append(p.decorators, name)
	p.scan(t[1:], scope)
}

func (p *parser) funcDecl(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	if len(t) < 2 || t[1].Kind != Ident {
		p.scan(t[1:], scope)
		p.block(l.children, scope, owner)
		return
	}
	d := p.declare(scope, KFunc, t[1], owner)
	d.Span = lineSpan(l)
	d.Decorators = p.takeDecorators()
	fs := p.newScope(SFunc, d.Span, scope, d)
	d.Body = fs

	i := 2
	if p.isOp(t, i, "[") {
		j := p.match(t, i)
		p.params(t[i+1:min(j, len(t))], fs, KParam, d)
		i = j + 1
	}
	if p.isOp(t, i, "(") {
		j := p.match(t, i)
		p.params(t[i+1:min(j, len(t))], fs, KArg, d)
		i = j + 1
	}
	colon := p.blockColon(t, min(i, len(t)))
	end := len(t)
	if colon >= 0 {
		end = colon
	}
	d.Header = Span{t[0].Start, t[max(end-1, 0)].End}
	if i < end {
		tail := t[i:end]
		if arrow := p.findTop(tail, 0, func(k int) bool { return p.isOp(tail, k, "->") }); arrow >= 0 {
			res := tail[arrow+1:]
			if w := p.findTop(res, 0, func(k int) bool { return p.isWord(res, k, "where") || p.isOp(res, k, "{") }); w >= 0 {
				res = res[:w]
			}
			if p.isWord(res, 0, "ref") {
				res = res[1:]
				if p.isOp(res, 0, "[") {
					res = res[min(p.match(res, 0)+1, len(res)):]
				}
			}
			d.Result = spanOf(res)
		}
		// A capture list declares the captured names in the closure.
		if brace := p.findTop(tail, 0, func(k int) bool { return p.isOp(tail, k, "{") }); brace >= 0 {
			closeAt := min(p.match(tail, brace), len(tail))
			p.scanSkipping(tail[:brace], fs, sigWords)
			for _, entry := range p.splitTop(tail[brace+1:closeAt], ",") {
				if n := len(entry); n > 0 && entry[n-1].Kind == Ident && !sigWords[p.text(entry[n-1])] && !conventions[p.text(entry[n-1])] {
					c := p.declare(fs, KVar, entry[n-1], d)
					c.Capture = true
				}
			}
			if closeAt+1 < len(tail) {
				p.scanSkipping(tail[closeAt+1:], fs, sigWords)
			}
		} else {
			p.scanSkipping(tail, fs, sigWords)
		}
	}
	body := p.bodyOf(l, colon)
	d.Doc = p.docstring(body)
	p.block(body, fs, d)
}

func (p *parser) docstring(body []*line) Span {
	if len(body) > 0 && len(body[0].toks) == 1 && body[0].toks[0].Kind == String {
		return Span{body[0].toks[0].Start, body[0].toks[0].End}
	}
	return Span{}
}

// params reads a `[...]` parameter list or a `(...)` argument list.
func (p *parser) params(toks []Token, scope *Scope, kind DeclKind, owner *Decl) {
	var declared []*Decl
	for _, entry := range p.splitTop(toks, ",") {
		if len(entry) == 1 && p.isOp(entry, 0, "//") {
			for _, d := range declared {
				d.InferOnly = true
			}
			continue
		}
		i := 0
		for i < len(entry) && entry[i].Kind == Op {
			s := p.text(entry[i])
			if s != "*" && s != "**" && s != "/" && s != "//" {
				break
			}
			i++
		}
		for i < len(entry) && entry[i].Kind == Ident && conventions[p.text(entry[i])] && i+1 < len(entry) {
			// A convention only if a name (or ref's origin list) follows.
			if p.isOp(entry, i+1, "[") {
				j := p.match(entry, i+1)
				p.scan(entry[i+2:min(j, len(entry))], scope)
				i = j + 1
				continue
			}
			if entry[i+1].Kind != Ident {
				break
			}
			i++
		}
		if i >= len(entry) {
			continue
		}
		if entry[i].Kind != Ident || hardKeywords[p.text(entry[i])] && p.src[entry[i].Start] != '`' {
			p.scan(entry[i:], scope)
			continue
		}
		next := i + 1
		if next < len(entry) && !p.isOp(entry, next, ":") && !p.isOp(entry, next, "=") {
			// Not `name`, `name: T` or `name = v`: an expression.
			p.scan(entry[i:], scope)
			continue
		}
		d := p.declare(scope, kind, entry[i], owner)
		rest := entry[next:]
		eq := p.findTop(rest, 0, func(k int) bool { return p.isOp(rest, k, "=") })
		if p.isOp(rest, 0, ":") {
			typeEnd := len(rest)
			if eq >= 0 {
				typeEnd = eq
			}
			d.Type = spanOf(rest[1:typeEnd])
			p.scan(rest[1:typeEnd], scope)
		}
		if eq >= 0 {
			d.Value = spanOf(rest[eq+1:])
			p.scan(rest[eq+1:], scope)
			// `dtype: DType = .float32`: the member belongs to the type.
			if v := rest[eq+1:]; len(v) >= 2 && p.isOp(v, 0, ".") && v[1].Kind == Ident && d.Type.End > d.Type.Start {
				if k := p.lastIdentRef(v[1]); k >= 0 {
					p.setCtx(k, Context{Callee: -1, Expect: d.Type})
				}
			}
		}
		d.Span = spanOf(entry)
		declared = append(declared, d)
	}
}

func (p *parser) setCtx(ref int, c Context) {
	if p.f.Ctx == nil {
		p.f.Ctx = map[int]Context{}
	}
	p.f.Ctx[ref] = c
}

func (p *parser) typeDecl(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	if len(t) < 2 || t[1].Kind != Ident {
		p.scan(t[1:], scope)
		p.block(l.children, scope, owner)
		return
	}
	kind := KStruct
	if p.text(t[0]) == "trait" {
		kind = KTrait
	}
	d := p.declare(scope, kind, t[1], owner)
	d.Span = lineSpan(l)
	d.Decorators = p.takeDecorators()
	ss := p.newScope(SStruct, d.Span, scope, d)
	d.Body = ss
	i := 2
	if p.isOp(t, i, "[") {
		j := p.match(t, i)
		p.params(t[i+1:min(j, len(t))], ss, KParam, d)
		i = j + 1
	}
	colon := p.blockColon(t, min(i, len(t)))
	end := len(t)
	if colon >= 0 {
		end = colon
	}
	d.Header = Span{t[0].Start, t[max(end-1, 0)].End}
	if p.isOp(t, i, "(") {
		j := min(p.match(t, i), end)
		for _, entry := range p.splitTop(t[i+1:j], ",") {
			// The leading dotted name of each entry: `a.b.Trait where ...`.
			k := 0
			for k+2 < len(entry) && entry[k].Kind == Ident && p.isOp(entry, k+1, ".") && entry[k+2].Kind == Ident {
				k += 2
			}
			if k < len(entry) && entry[k].Kind == Ident {
				d.Bases = append(d.Bases, Span{entry[k].Start, entry[k].End})
			}
		}
	}
	if i < end {
		p.scanSkipping(t[i:end], ss, sigWords)
	}
	body := p.bodyOf(l, colon)
	d.Doc = p.docstring(body)
	p.block(body, ss, d)
}

func (p *parser) comptimeDecl(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	d := p.declare(scope, KComptime, t[1], owner)
	d.Span = lineSpan(l)
	d.Visible = spanOf(t).End
	d.Decorators = p.takeDecorators()
	inner := scope
	i := 2
	if p.isOp(t, i, "[") {
		// Parameterized: its parameters are visible only in its own type
		// and value.
		inner = p.newScope(SComp, d.Span, scope, d)
		d.Body = inner
		j := p.match(t, i)
		p.params(t[i+1:min(j, len(t))], inner, KParam, d)
		i = j + 1
	}
	rest := t[min(i, len(t)):]
	eq := p.findTop(rest, 0, func(k int) bool { return p.isOp(rest, k, "=") })
	head := rest
	if eq >= 0 {
		head = rest[:eq]
		d.Value = spanOf(rest[eq+1:])
	}
	if p.isOp(head, 0, ":") {
		where := p.findTop(head, 1, func(k int) bool { return p.isWord(head, k, "where") })
		typeEnd := len(head)
		if where >= 0 {
			typeEnd = where
		}
		d.Type = spanOf(head[1:typeEnd])
	}
	p.scanSkipping(rest, inner, map[string]bool{"where": true})
	p.block(l.children, scope, owner)
}

func (p *parser) varDecl(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	kind := KVar
	if scope.Kind == SStruct {
		kind = KField
	}
	rest := t[1:]
	colon := p.findTop(rest, 0, func(k int) bool { return p.isOp(rest, k, ":") })
	eq := p.findTop(rest, 0, func(k int) bool { return p.isOp(rest, k, "=") })
	namesEnd := len(rest)
	if colon >= 0 && (eq < 0 || colon < eq) {
		namesEnd = colon
	} else if eq >= 0 {
		namesEnd = eq
	}
	var typ, val Span
	if colon >= 0 && colon == namesEnd {
		typeEnd := len(rest)
		if eq >= 0 {
			typeEnd = eq
		}
		typ = spanOf(rest[colon+1 : typeEnd])
		p.scan(rest[colon+1:typeEnd], scope)
	}
	if eq >= 0 {
		val = spanOf(rest[eq+1:])
		p.scan(rest[eq+1:], scope)
		if v := rest[eq+1:]; len(v) >= 2 && p.isOp(v, 0, ".") && v[1].Kind == Ident && typ.End > typ.Start {
			if k := p.lastIdentRef(v[1]); k >= 0 {
				p.setCtx(k, Context{Callee: -1, Expect: typ})
			}
		}
	}
	decorators := p.takeDecorators()
	for _, tok := range rest[:namesEnd] {
		if tok.Kind != Ident || hardKeywords[p.text(tok)] {
			continue
		}
		d := p.declare(scope, kind, tok, owner)
		d.Type, d.Value, d.Decorators = typ, val, decorators
		d.Span = lineSpan(l)
		d.Visible = spanOf(t).End
	}
	p.block(l.children, scope, owner)
}

func (p *parser) refDecl(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	i := 1
	if p.isOp(t, i, "[") {
		j := p.match(t, i)
		p.scan(t[i+1:min(j, len(t))], scope)
		i = j + 1
	}
	if i >= len(t) || t[i].Kind != Ident {
		p.scan(t[1:], scope)
		return
	}
	rest := t[i+1:]
	eq := p.findTop(rest, 0, func(k int) bool { return p.isOp(rest, k, "=") })
	if eq >= 0 {
		p.scan(rest[eq+1:], scope)
	}
	d := p.declare(scope, KVar, t[i], owner)
	d.Span = lineSpan(l)
	d.Visible = spanOf(t).End
	if eq >= 0 {
		d.Value = spanOf(rest[eq+1:])
		if p.isOp(rest, 0, ":") {
			d.Type = spanOf(rest[1:eq])
			p.scan(rest[1:eq], scope)
		}
	}
}

func (p *parser) importStmt(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	im := &Import{Span: spanOf(t), From: p.text(t[0]) == "from"}
	p.f.Imports = append(p.f.Imports, im)
	// The module path: dots and names up to `import` (from) or `as`/end.
	i := 1
	var mod strings.Builder
	modStart := -1
	for i < len(t) {
		s := p.text(t[i])
		if t[i].Kind == Ident && (s == "import" || s == "as") {
			break
		}
		if t[i].Kind == Ident {
			p.f.Refs = append(p.f.Refs, Ref{Name: IdentName(p.src, t[i]), Span: Span{t[i].Start, t[i].End}, Kind: RModule, Scope: scope, Base: -1, Callee: -1, Call: -1})
		} else if !(t[i].Kind == Op && (s == "." || s == "...")) {
			break
		}
		if modStart < 0 {
			modStart = t[i].Start
		}
		mod.WriteString(s)
		im.ModuleSpan = Span{modStart, t[i].End}
		i++
	}
	im.Module = mod.String()
	if !im.From {
		if len(t) < 2 {
			return
		}
		bound := t[1]
		if p.isWord(t, i, "as") && i+1 < len(t) && t[i+1].Kind == Ident {
			im.Alias = IdentName(p.src, t[i+1])
			im.AliasSpan = Span{t[i+1].Start, t[i+1].End}
			bound = t[i+1]
		}
		if bound.Kind == Ident {
			d := p.declare(scope, KImport, bound, owner)
			d.Import = im
			d.Span = im.Span
			if im.Alias == "" {
				// `import std.collections` binds `std`; its ref already exists.
				p.f.Refs = p.f.Refs[:len(p.f.Refs)-1]
			}
		}
		return
	}
	if !p.isWord(t, i, "import") {
		return
	}
	names := t[i+1:]
	if p.isOp(names, 0, "(") {
		names = names[1:]
		if n := len(names); n > 0 && p.isOp(names, n-1, ")") {
			names = names[:n-1]
		}
	}
	for _, entry := range p.splitTop(names, ",") {
		if len(entry) == 0 {
			continue
		}
		if p.isOp(entry, 0, "*") {
			im.Star = true
			continue
		}
		if entry[0].Kind != Ident {
			continue
		}
		in := ImportName{Name: IdentName(p.src, entry[0]), NameSpan: Span{entry[0].Start, entry[0].End}}
		bound := entry[0]
		if p.isWord(entry, 1, "as") && len(entry) > 2 && entry[2].Kind == Ident {
			in.Alias = IdentName(p.src, entry[2])
			in.AliasSpan = Span{entry[2].Start, entry[2].End}
			bound = entry[2]
			// The imported name itself is a use of the target.
			p.f.Refs = append(p.f.Refs, Ref{Name: in.Name, Span: in.NameSpan, Kind: RImported, Scope: scope, Base: -1, Callee: -1, Call: -1})
		}
		d := p.declare(scope, KImport, bound, owner)
		d.Import = im
		d.Span = spanOf(entry)
		in.Decl = d
		im.Names = append(im.Names, in)
	}
}

// forStmt: `for a, b in xs:`; the targets belong to the loop body.
func (p *parser) forStmt(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	in := p.findTop(t, 1, func(k int) bool { return p.isWord(t, k, "in") })
	colon := p.blockColon(t, 1)
	if in < 0 || (colon >= 0 && colon < in) {
		end := len(t)
		if colon >= 0 {
			end = colon
		}
		p.scan(t[1:end], scope)
		p.subBlock(l, colon, scope, owner, nil)
		return
	}
	iterEnd := len(t)
	if colon >= 0 {
		iterEnd = colon
	}
	p.scan(t[in+1:iterEnd], scope)
	targets := t[1:in]
	p.subBlock(l, colon, scope, owner, func(body *Scope) {
		for _, tok := range targets {
			if tok.Kind == Ident && !hardKeywords[p.text(tok)] && !conventions[p.text(tok)] {
				d := p.declare(body, KVar, tok, owner)
				d.Span = spanOf(targets)
				d.Value = spanOf(t[in+1 : iterEnd])
				d.Visible = t[max(iterEnd-1, 0)].End
				d.Loop = true
			}
		}
	})
}

// withStmt: `with a as x, b as y:`.
func (p *parser) withStmt(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	colon := p.blockColon(t, 1)
	end := len(t)
	if colon >= 0 {
		end = colon
	}
	header := t[1:end]
	var names []Token
	for _, item := range p.splitTop(header, ",") {
		as := p.findTop(item, 0, func(k int) bool { return p.isWord(item, k, "as") })
		if as < 0 {
			p.scan(item, scope)
			continue
		}
		p.scan(item[:as], scope)
		for _, tok := range item[as+1:] {
			if tok.Kind == Ident && !hardKeywords[p.text(tok)] {
				names = append(names, tok)
			}
		}
	}
	p.subBlock(l, colon, scope, owner, func(body *Scope) {
		for _, tok := range names {
			p.declare(body, KVar, tok, owner).Span = spanOf(header)
		}
	})
}

// exceptStmt: `except E as e:`.
func (p *parser) exceptStmt(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	colon := p.blockColon(t, 1)
	end := len(t)
	if colon >= 0 {
		end = colon
	}
	header := t[1:end]
	as := p.findTop(header, 0, func(k int) bool { return p.isWord(header, k, "as") })
	var name *Token
	if as >= 0 {
		p.scan(header[:as], scope)
		if as+1 < len(header) && header[as+1].Kind == Ident {
			name = &header[as+1]
		}
	} else if len(header) == 1 && header[0].Kind == Ident && !hardKeywords[p.text(header[0])] {
		// Mojo's `except e:` names the error value.
		name = &header[0]
	} else {
		p.scan(header, scope)
	}
	p.subBlock(l, colon, scope, owner, func(body *Scope) {
		if name != nil {
			p.declare(body, KVar, *name, owner).Span = spanOf(header)
		}
	})
}

// exprStatement handles everything else, including assignments: in a def,
// assigning to a name not yet declared in that def declares it.
func (p *parser) exprStatement(l *line, scope *Scope, owner *Decl) {
	t := l.toks
	p.decorators = nil
	eqs := []int{}
	start := 0
	for {
		k := p.findTop(t, start, func(k int) bool { return p.isOp(t, k, "=") })
		if k < 0 {
			break
		}
		eqs = append(eqs, k)
		start = k + 1
	}
	if len(eqs) == 0 {
		// `x: Int` alone is a declaration without a value.
		if len(t) >= 3 && t[0].Kind == Ident && p.isOp(t, 1, ":") && scope.Kind != SStruct {
			p.scan(t[2:], scope)
			p.assignTarget(t[:1], Span{t[2].Start, t[len(t)-1].End}, Span{}, scope, owner)
			p.block(l.children, scope, owner)
			return
		}
		p.scan(t, scope)
		p.block(l.children, scope, owner)
		return
	}
	last := eqs[len(eqs)-1]
	value := t[last+1:]
	p.scan(value, scope)
	prev := 0
	for _, k := range eqs {
		target := t[prev:k]
		var typ Span
		colon := p.findTop(target, 0, func(i int) bool { return p.isOp(target, i, ":") })
		if colon > 0 {
			p.scan(target[colon+1:], scope)
			typ = spanOf(target[colon+1:])
			target = target[:colon]
		}
		p.assignTarget(target, typ, spanOf(value), scope, owner)
		prev = k + 1
	}
	p.block(l.children, scope, owner)
}

// assignTarget handles the left of an `=`: plain names (optionally a
// tuple of them) are declared if new in this def; anything else (`a.b`,
// `a[i]`) is ordinary uses.
func (p *parser) assignTarget(target []Token, typ, value Span, scope *Scope, owner *Decl) {
	simple := len(target) > 0
	for i, tok := range target {
		switch tok.Kind {
		case Ident:
			if hardKeywords[p.text(tok)] {
				simple = false
			}
			if i+1 < len(target) && p.isOp(target, i+1, ".") || i > 0 && p.isOp(target, i-1, ".") {
				simple = false
			}
		case Op:
			s := p.text(tok)
			if s != "," && s != "(" && s != ")" {
				simple = false
			}
		default:
			simple = false
		}
	}
	if !simple {
		p.scan(target, scope)
		return
	}
	declScope := scope.Func()
	if declScope == nil && scope.Kind == SModule {
		declScope = scope
	}
	for _, tok := range target {
		if tok.Kind != Ident {
			continue
		}
		name := IdentName(p.src, tok)
		if name == "_" {
			continue // the discard target names nothing
		}
		if declScope == nil || p.declaredInFunc(name, scope, tok.Start) {
			p.scan([]Token{tok}, scope)
			continue
		}
		d := p.declare(declScope, KVar, tok, owner)
		d.Implicit = true
		d.Type, d.Value = typ, value
		d.Span = Span{tok.Start, max(value.End, tok.End)}
		d.Visible = d.Span.End
		// The reference's scope is where it appears.
		p.f.Refs[len(p.f.Refs)-1].Scope = scope
	}
}

// declaredInFunc reports whether name is already declared before pos in
// scope or the scopes around it, including enclosing defs (a nested def
// assigns to the variables it captures) and the module.
func (p *parser) declaredInFunc(name string, scope *Scope, pos int) bool {
	for s := scope; s != nil; s = s.Parent {
		for _, d := range s.Names[name] {
			if d.NameSpan.Start < pos || s.Kind == SModule || s.Kind == SStruct {
				return true
			}
		}
		if s.Kind == SModule || s.Kind == SStruct {
			return false
		}
	}
	return false
}

// --- expressions ---

func (p *parser) scan(toks []Token, scope *Scope) { p.scanSkipping(toks, scope, nil) }

type openBracket struct {
	op     string
	callee int    // ref index of the identifier just before it, or -1
	pos    int    // index of the current argument (commas seen)
	kw     string // keyword of the current argument, if `name=`
	start  int    // offset just after the opening bracket
	inner  Span   // set when closed: what was inside
}

// scanSkipping records every identifier in toks. Words in skip are treated
// as keywords. Comprehensions get their own scope.
func (p *parser) scanSkipping(toks []Token, scope *Scope, skip map[string]bool) {
	var stack []openBracket
	lastRef := -1 // ref index of the previous token if it was a recorded identifier
	lastClosed := openBracket{callee: -1}
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch tok.Kind {
		case Op:
			s := p.text(tok)
			if isOpen(s) {
				j := p.match(toks, i)
				inner := toks[i+1 : min(j, len(toks))]
				if p.findTop(inner, 0, func(k int) bool { return p.isWord(inner, k, "for") }) >= 0 {
					p.comprehension(inner, scope, skip)
					i = j
					lastRef = -1
					continue
				}
				callee := lastRef
				if callee < 0 && i > 0 && toks[i-1].Kind == Op && isClose(p.text(toks[i-1])) {
					// A postfix chain: `f[T](x)` calls what `f[T]` names.
					callee = lastClosed.callee
				}
				stack = append(stack, openBracket{op: s, callee: callee, start: tok.End})
				lastRef = -1
				continue
			}
			if isClose(s) && len(stack) > 0 {
				lastClosed = stack[len(stack)-1]
				lastClosed.inner = Span{lastClosed.start, tok.Start}
				stack = stack[:len(stack)-1]
			}
			if s == "," && len(stack) > 0 {
				stack[len(stack)-1].pos++
				stack[len(stack)-1].kw = ""
			}
			lastRef = -1
		case String:
			for _, sp := range tok.Interp {
				p.scanSource(sp, scope)
			}
			lastRef = -1
		case Ident:
			name := IdentName(p.src, tok)
			backtick := p.src[tok.Start] == '`'
			if !backtick && (name == "def" || name == "fn") && (p.isOp(toks, i+1, "[") || p.isOp(toks, i+1, "(")) {
				// A function type, `def[T: AnyType](x: T) -> T`: its
				// parameters and arguments are visible in the rest of it.
				end := p.findTop(toks, i+1, func(k int) bool { return p.isOp(toks, k, ",") })
				if end < 0 {
					end = len(toks)
				}
				p.funcType(toks[i+1:end], scope, skip)
				i = end - 1
				lastRef = -1
				continue
			}
			if !backtick && name == "lambda" {
				end := p.findTop(toks, i+1, func(k int) bool { return p.isOp(toks, k, ",") })
				if end < 0 {
					end = len(toks)
				}
				p.lambda(toks[i+1:end], scope, skip)
				i = end - 1
				lastRef = -1
				continue
			}
			if !backtick && (hardKeywords[name] || skip[name]) {
				lastRef = -1
				continue
			}
			r := Ref{Name: name, Span: Span{tok.Start, tok.End}, Kind: RName, Scope: scope, Base: -1, Callee: -1, Call: -1}
			switch {
			case i > 0 && p.isOp(toks, i-1, "."):
				r.Kind = RMember
				switch {
				case i > 1 && toks[i-2].Kind == Ident && lastDotBase(p, toks, i):
					r.Base = p.lastIdentRef(toks[i-2])
				case i > 2 && p.isOp(toks, i-2, "^") && toks[i-3].Kind == Ident:
					r.Base = p.lastIdentRef(toks[i-3]) // `x^.y`
				case i > 1 && toks[i-2].Kind == Op && (p.text(toks[i-2]) == ")" || p.text(toks[i-2]) == "]"):
					if lastClosed.callee >= 0 {
						r.Call, r.Post = lastClosed.callee, lastClosed.op[0]
					} else if lastClosed.op == "(" {
						// `(a + b).c`: a member of the parenthesized value.
						p.f.Refs = append(p.f.Refs, r)
						p.setCtx(len(p.f.Refs)-1, Context{Callee: -1, Inner: lastClosed.inner})
						lastRef = len(p.f.Refs) - 1
						continue
					}
				case i > 1 && (toks[i-2].Kind == String || toks[i-2].Kind == Number || toks[i-2].Kind == Ident && (p.text(toks[i-2]) == "True" || p.text(toks[i-2]) == "False")):
					// A member of a literal: `"abc".byte_length()`.
					lit := byte('b')
					switch toks[i-2].Kind {
					case String:
						lit = 's'
						if c := p.src[toks[i-2].Start]; c == 't' || c == 'T' || c == 'f' || c == 'F' {
							lit = 't'
						}
					case Number:
						lit = 'i'
						if strings.ContainsAny(p.text(toks[i-2]), ".eE") && !strings.HasPrefix(p.text(toks[i-2]), "0x") {
							lit = 'f'
						}
					}
					p.f.Refs = append(p.f.Refs, r)
					p.setCtx(len(p.f.Refs)-1, Context{Callee: -1, Lit: lit})
					lastRef = len(p.f.Refs) - 1
					continue
				case i > 2 && toks[i-2].Kind == Op && (p.text(toks[i-2]) == "==" || p.text(toks[i-2]) == "!=") && toks[i-3].Kind == Ident:
					// `dtype == .float32`: the type of the other side.
					peer := p.lastIdentRef(toks[i-3])
					p.f.Refs = append(p.f.Refs, r)
					if peer >= 0 {
						p.setCtx(len(p.f.Refs)-1, Context{Callee: -1, Peer: peer, HasPeer: true})
					}
					lastRef = len(p.f.Refs) - 1
					continue
				case i == 1 && p.isWord(toks, 0, "return") || i > 1 && p.isWord(toks, i-2, "return"):
					p.f.Refs = append(p.f.Refs, r)
					p.setCtx(len(p.f.Refs)-1, Context{Callee: -1, Return: true})
					lastRef = len(p.f.Refs) - 1
					continue
				case len(stack) > 0 && (i == 1 || toks[i-2].Kind == Op && (p.text(toks[i-2]) == "(" || p.text(toks[i-2]) == "[" || p.text(toks[i-2]) == "," || p.text(toks[i-2]) == "=")):
					// A contextual member: its type comes from the
					// parameter or argument it is passed as.
					top := stack[len(stack)-1]
					p.f.Refs = append(p.f.Refs, r)
					p.setCtx(len(p.f.Refs)-1, Context{Callee: top.callee, Op: top.op[0], Pos: top.pos, Name: top.kw})
					lastRef = len(p.f.Refs) - 1
					continue
				}
			case p.isOp(toks, i+1, "=") && len(stack) > 0 && (stack[len(stack)-1].op == "(" || stack[len(stack)-1].op == "[") &&
				(i == 0 || p.isOp(toks, i-1, ",") || p.isOp(toks, i-1, "(") || p.isOp(toks, i-1, "[")):
				r.Kind = RKwarg
				r.Callee = stack[len(stack)-1].callee
				stack[len(stack)-1].kw = name
			case p.isOp(toks, i+1, ":=") && scope.Func() != nil && !p.declaredInFunc(name, scope, tok.Start):
				d := p.declare(scope.Func(), KVar, tok, scope.Owner)
				d.Implicit = true
				p.f.Refs[len(p.f.Refs)-1].Scope = scope
				lastRef = len(p.f.Refs) - 1
				continue
			}
			p.f.Refs = append(p.f.Refs, r)
			lastRef = len(p.f.Refs) - 1
		default:
			lastRef = -1
		}
	}
}

// lambda reads `lambda (x: T) {captures} -> R: body` (or `lambda x: body`)
// after the keyword: arguments and captures are visible in the body.
func (p *parser) lambda(toks []Token, scope *Scope, skip map[string]bool) {
	ls := p.newScope(SComp, spanOf(toks), scope, scope.Owner)
	colon := p.findTop(toks, 0, func(k int) bool { return p.isOp(toks, k, ":") })
	if colon < 0 {
		p.scanSkipping(toks, ls, skip)
		return
	}
	head := toks[:colon]
	i := 0
	if p.isOp(head, 0, "[") {
		j := min(p.match(head, 0), len(head))
		p.params(head[1:j], ls, KParam, scope.Owner)
		i = j + 1
	}
	if p.isOp(head, i, "(") {
		j := min(p.match(head, i), len(head))
		p.params(head[i+1:j], ls, KArg, scope.Owner)
		i = j + 1
	} else if i == 0 {
		for i < len(head) && !p.isOp(head, i, "{") && !p.isOp(head, i, "->") {
			if head[i].Kind == Ident && !hardKeywords[p.text(head[i])] {
				p.declare(ls, KArg, head[i], scope.Owner)
			}
			i++
		}
	}
	if p.isOp(head, i, "{") {
		j := min(p.match(head, i), len(head))
		for _, entry := range p.splitTop(head[i+1:j], ",") {
			if n := len(entry); n > 0 && entry[n-1].Kind == Ident && !sigWords[p.text(entry[n-1])] && !conventions[p.text(entry[n-1])] {
				p.declare(ls, KVar, entry[n-1], scope.Owner).Capture = true
			}
		}
		i = j + 1
	}
	if i < len(head) {
		p.scanSkipping(head[i:], ls, sigWords)
	}
	p.scanSkipping(toks[colon+1:], ls, skip)
}

// funcType reads the part of a function type after `def`.
func (p *parser) funcType(toks []Token, scope *Scope, skip map[string]bool) {
	ts := p.newScope(SComp, spanOf(toks), scope, scope.Owner)
	i := 0
	if p.isOp(toks, i, "[") {
		// `def[T: AnyType]` names a parameter; `def[Int]` only types one.
		j := p.match(toks, i)
		for _, entry := range p.splitTop(toks[i+1:min(j, len(toks))], ",") {
			if len(entry) >= 2 && entry[0].Kind == Ident && (p.isOp(entry, 1, ":") || p.isOp(entry, 1, "=")) {
				p.params(entry, ts, KParam, scope.Owner)
			} else {
				p.scanSkipping(entry, ts, skip)
			}
		}
		i = j + 1
	}
	if p.isOp(toks, i, "(") {
		j := p.match(toks, i)
		inner := toks[i+1 : min(j, len(toks))]
		// Arguments may be named (`x: Int`) or bare types (`Int`).
		for _, entry := range p.splitTop(inner, ",") {
			k := 0
			for k < len(entry) && (p.isOp(entry, k, "*") || p.isOp(entry, k, "**")) {
				k++
			}
			if k < len(entry) && entry[k].Kind == Ident && conventions[p.text(entry[k])] {
				k++
			}
			if k+1 < len(entry) && entry[k].Kind == Ident && p.isOp(entry, k+1, ":") {
				p.params(entry, ts, KArg, scope.Owner)
			} else {
				p.scanSkipping(entry, ts, skip)
			}
		}
		i = j + 1
	}
	if i < len(toks) {
		p.scanSkipping(toks[i:], ts, sigWords)
	}
}

// lastDotBase reports whether the identifier two tokens before i (`a` in
// `a.b`) is itself a plain name or member, not the end of a call or
// subscript.
func lastDotBase(p *parser, toks []Token, i int) bool {
	return toks[i-2].Kind == Ident && !hardKeywords[p.text(toks[i-2])]
}

// lastIdentRef finds the ref recorded for tok, searching from the end.
func (p *parser) lastIdentRef(tok Token) int {
	for k := len(p.f.Refs) - 1; k >= 0 && k >= len(p.f.Refs)-8; k-- {
		if p.f.Refs[k].Span.Start == tok.Start {
			return k
		}
	}
	return -1
}

// scanSource lexes a span of the source (a t-string interpolation) and
// records its identifiers.
func (p *parser) scanSource(sp Span, scope *Scope) {
	sub := Lex(p.src[sp.Start:sp.End])
	var toks []Token
	for _, t := range sub.Tokens {
		if t.Kind == Newline || t.Kind == Indent || t.Kind == Dedent || t.Kind == EOF {
			continue
		}
		t.Start += sp.Start
		t.End += sp.Start
		for k := range t.Interp {
			t.Interp[k].Start += sp.Start
			t.Interp[k].End += sp.Start
		}
		toks = append(toks, t)
	}
	p.scan(toks, scope)
}

// comprehension: `[expr for a, b in xs if cond for c in ys]`. Its targets
// are visible throughout the brackets, before and after `for`.
func (p *parser) comprehension(inner []Token, scope *Scope, skip map[string]bool) {
	cs := p.newScope(SComp, spanOf(inner), scope, scope.Owner)
	i := 0
	for i < len(inner) {
		f := p.findTop(inner, i, func(k int) bool { return p.isWord(inner, k, "for") })
		if f < 0 {
			p.scanSkipping(inner[i:], cs, skip)
			return
		}
		p.scanSkipping(inner[i:f], cs, skip)
		in := p.findTop(inner, f+1, func(k int) bool { return p.isWord(inner, k, "in") })
		if in < 0 {
			p.scanSkipping(inner[f+1:], cs, skip)
			return
		}
		for _, tok := range inner[f+1 : in] {
			if tok.Kind == Ident && !hardKeywords[p.text(tok)] && !conventions[p.text(tok)] {
				p.declare(cs, KVar, tok, scope.Owner)
			}
		}
		i = in + 1
	}
}

// finish sorts occurrences into source order (keeping Base and Callee
// pointing at the same occurrences) and closes up scopes.
func (p *parser) finish() {
	refs := p.f.Refs
	order := make([]int, len(refs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return refs[order[a]].Span.Start < refs[order[b]].Span.Start })
	newIndex := make([]int, len(refs))
	for n, old := range order {
		newIndex[old] = n
	}
	sorted := make([]Ref, len(refs))
	for n, old := range order {
		r := refs[old]
		if r.Base >= 0 {
			r.Base = newIndex[r.Base]
		}
		if r.Callee >= 0 {
			r.Callee = newIndex[r.Callee]
		}
		if r.Call >= 0 {
			r.Call = newIndex[r.Call]
		}
		sorted[n] = r
	}
	p.f.Refs = sorted
	if p.f.Ctx != nil {
		ctx := make(map[int]Context, len(p.f.Ctx))
		for old, c := range p.f.Ctx {
			if old >= len(newIndex) {
				continue
			}
			if c.Callee >= 0 && c.Callee < len(newIndex) {
				c.Callee = newIndex[c.Callee]
			} else {
				c.Callee = -1
			}
			if c.HasPeer {
				if c.Peer >= 0 && c.Peer < len(newIndex) {
					c.Peer = newIndex[c.Peer]
				} else {
					c.HasPeer = false
				}
			}
			ctx[newIndex[old]] = c
		}
		p.f.Ctx = ctx
	}
}

// IsKeyword reports whether name is reserved (it cannot name anything
// unless written in backticks).
func IsKeyword(name string) bool { return hardKeywords[name] }

// IsIdent reports whether name is a plain identifier (not a keyword).
func IsIdent(name string) bool {
	if name == "" || !isIdentStart(name[0]) || hardKeywords[name] {
		return false
	}
	for i := 1; i < len(name); i++ {
		if c := name[i]; !isIdentStart(c) && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
