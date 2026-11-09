package syntax

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// render shows tokens compactly: identifiers and ops by text, structure by
// name. A string with interpolations lists them after a '|'.
func render(src string) string {
	l := Lex([]byte(src))
	var b []string
	for _, t := range l.Tokens {
		switch t.Kind {
		case Newline:
			b = append(b, "NL")
		case Indent:
			b = append(b, "IN")
		case Dedent:
			b = append(b, "DE")
		case EOF:
			b = append(b, "EOF")
		case String:
			s := l.Text(t)
			for _, sp := range t.Interp {
				s += "|" + src[sp.Start:sp.End]
			}
			b = append(b, s)
		case Illegal:
			b = append(b, "?"+l.Text(t))
		default:
			b = append(b, l.Text(t))
		}
	}
	return strings.Join(b, " ")
}

func TestLex(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"simple", "var x = 1\n", "var x = 1 NL EOF"},
		{"block", "def f():\n    pass\nx\n", "def f ( ) : NL IN pass NL DE x NL EOF"},
		{"nested dedent", "a:\n  b:\n    c\nd", "a : NL IN b : NL IN c NL DE DE d NL EOF"},
		{"dedent at eof", "a:\n  b", "a : NL IN b NL DE EOF"},
		{"blank and comment lines keep indentation", "a:\n  b\n\n# c\n      # d\n  e\n", "a : NL IN b NL e NL DE EOF"},
		{"brackets join lines", "f(a,\n  b)\nc\n", "f ( a , b ) NL c NL EOF"},
		{"backslash continuation", "x = 1 + \\\n    2\n", "x = 1 + 2 NL EOF"},
		{"crlf", "a:\r\n  b\r\n", "a : NL IN b NL DE EOF"},
		{"ops", "a **= b // c -> d := e != f ^ g ...", "a **= b // c -> d := e != f ^ g ... NL EOF"},
		{"transfer sigil", "f(x^)", "f ( x ^ ) NL EOF"},
		{"decorator", "@always_inline(\"nodebug\")\ndef f(): pass", "@ always_inline ( \"nodebug\" ) NL def f ( ) : pass NL EOF"},
		{"strings", `a = "x" + 'y' + r"\d" + b"z"`, `a = "x" + 'y' + r"\d" + b"z" NL EOF`},
		{"escaped quote", `"a\"b" c`, `"a\"b" c NL EOF`},
		{"triple across lines", "x = \"\"\"a\n\"b\"\n\"\"\"\ny", "x = \"\"\"a\n\"b\"\n\"\"\" NL y NL EOF"},
		{"unterminated string stops at eol", "x = \"abc\ny", "x = \"abc NL y NL EOF"},
		{"unterminated triple runs to eof", "x = '''abc\ny", "x = '''abc\ny NL EOF"},
		{"t-string interpolation", `t"x is {self.x} and {f(a[1:2])!r:>3}"`, `t"x is {self.x} and {f(a[1:2])!r:>3}"|self.x|f(a[1:2]) NL EOF`},
		{"nested string reusing the quote", `t"hello \t{x}, {rt"world \t{x}"}" y`, `t"hello \t{x}, {rt"world \t{x}"}"|x|rt"world \t{x}" y NL EOF`},
		{"unterminated quote inside interpolation", "t\"{0'0000", "t\"{0'0000|0'0000 NL EOF"},
		{"escaped braces", `f"{{literal}} {v}"`, `f"{{literal}} {v}"|v NL EOF`},
		{"prefix is not a word", "from x import rb\nrb", "from x import rb NL rb NL EOF"},
		{"backtick identifier", "var `def` = 1", "var `def` = 1 NL EOF"},
		{"numbers", "0x1F 1_000 3.14 1e-3 .5 2j 1.__add__", "0x1F 1_000 3.14 1e-3 .5 2j 1 . __add__ NL EOF"},
		{"unicode identifier", "var café = 1", "var café = 1 NL EOF"},
		{"illegal char", "a ¤ b", "a ?¤ b NL EOF"},
		{"stray closer", "a)\nb", "a ) NL b NL EOF"},
		{
			"unclosed bracket recovers at a statement keyword",
			"def f(a: Int\ndef g():\n    pass\n",
			"def f ( a : Int NL def g ( ) : NL IN pass NL DE EOF",
		},
		{
			"closing paren at def indentation is not recovery",
			"def f(\n    a: Int,\n) -> Int:\n    return a\n",
			"def f ( a : Int , ) -> Int : NL IN return a NL DE EOF",
		},
		{
			"comprehension for on a continuation line",
			"x = [a\n    for a in b]\n",
			"x = [ a for a in b ] NL EOF",
		},
		{"tabs", "a:\n\tb\n\tc\n", "a : NL IN b NL c NL DE EOF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := render(c.src); got != c.want {
				t.Errorf("\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

func TestComments(t *testing.T) {
	l := Lex([]byte("a # one\n# two\n  # three\nb\n"))
	var got []string
	for _, c := range l.Comments {
		got = append(got, string(l.Src[c.Start:c.End]))
	}
	if strings.Join(got, ",") != "# one,# two,# three" {
		t.Fatalf("comments: %q", got)
	}
}

// checkInvariants holds for every input, valid or not.
func checkInvariants(t *testing.T, src []byte) {
	t.Helper()
	l := Lex(src)
	if len(l.Tokens) == 0 || l.Tokens[len(l.Tokens)-1].Kind != EOF {
		t.Fatalf("missing EOF")
	}
	prev := 0
	depth := 0
	for i, tok := range l.Tokens {
		if tok.Start < prev || tok.End < tok.Start || tok.End > len(src) {
			t.Fatalf("token %d (%v) [%d,%d) out of order or bounds (prev end %d, len %d)", i, tok.Kind, tok.Start, tok.End, prev, len(src))
		}
		if tok.Kind != Newline && tok.Kind != Indent && tok.Kind != Dedent && tok.Kind != EOF && tok.End == tok.Start {
			t.Fatalf("empty %v token at %d", tok.Kind, tok.Start)
		}
		for _, sp := range tok.Interp {
			if sp.Start < tok.Start || sp.End > tok.End || sp.End <= sp.Start {
				t.Fatalf("interpolation %v outside its string [%d,%d)", sp, tok.Start, tok.End)
			}
		}
		switch tok.Kind {
		case Indent:
			depth++
		case Dedent:
			depth--
			if depth < 0 {
				t.Fatalf("dedent below zero at token %d", i)
			}
		}
		prev = tok.End
	}
	if depth != 0 {
		t.Fatalf("indent/dedent unbalanced: %d", depth)
	}
}

func FuzzLex(f *testing.F) {
	seeds := []string{
		"def f(x: Int) -> Int:\n    return x\n",
		"struct S[T: AnyType](Copyable):\n    var x: T\n",
		"t\"{a{b}c}\" '''\n",
		"(\n[\n{\n",
		"\\\n\\",
		"a:\n\t  b\n  \tc\n",
		"`x",
		"0x1e+5.e-",
		"rb'''\\'''",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkInvariants(t, []byte(s))
	})
}

// TestCorpus lexes every .mojo file under $MOJOLS_CORPUS, if set.
func TestCorpus(t *testing.T) {
	root := os.Getenv("MOJOLS_CORPUS")
	if root == "" {
		t.Skip("set MOJOLS_CORPUS to a directory of .mojo files")
	}
	n := 0
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".mojo") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		n++
		t.Run(path, func(t *testing.T) {
			checkInvariants(t, src)
			for _, tok := range Lex(src).Tokens {
				if tok.Kind == Illegal {
					t.Errorf("illegal token %q at %d", src[tok.Start:tok.End], tok.Start)
				}
			}
		})
		return nil
	})
	t.Logf("%d files", n)
}
