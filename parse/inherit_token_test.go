package parse

import (
	"bytes"
	"strings"
	"testing"
)

// lexAll drains the lexer for input and returns every emitted token,
// including the terminal EOF (or an error token if lexing fails).
func lexAll(t *testing.T, input string) []token {
	t.Helper()
	l := newLexer(bytes.NewReader([]byte(input)))
	go l.tokenize()
	var toks []token
	for {
		tok := l.nextToken()
		toks = append(toks, tok)
		if tok.tokenType == tokenEOF || tok.tokenType == tokenError {
			return toks
		}
	}
}

// tokenSummary is the minimal structural assertion surface: type and value.
// Whitespace tokens keep their text so interleaving with Text nodes is
// visible. Positions are asserted separately (see TestTokenPositions).
func tokenSummary(toks []token) string {
	parts := make([]string, 0, len(toks))
	for _, tok := range toks {
		parts = append(parts, tok.tokenType.String()+":"+tok.value)
	}
	return strings.Join(parts, "|")
}

// TestInheritTokenStream fixes the exact token order of the fragment the
// whole task hinges on: an extends tag, a same-name block override and an
// include with `with`/`only`. A regression that re-tokenizes any of these
// constructs silently changes the node tree downstream; asserting the raw
// stream (instead of only the rendered output) makes such a breakage point
// directly at the lexer.
func TestInheritTokenStream(t *testing.T) {
	input := "{% extends 'mid.html.twig' %}\n" +
		"{% block a %}{{ x|raw }}{% endblock %}\n" +
		"{% include 'p.html.twig' with vars only %}"
	toks := lexAll(t, input)

	// Locate every "keyword" token by value and assert the subsequence that
	// drives parser decisions. This deliberately does not snapshot the full
	// stream: whitespace/text detail is covered by lexTests already.
	var kw []string
	for _, tok := range toks {
		if tok.tokenType == tokenName {
			switch tok.value {
			case "extends", "block", "endblock", "include", "with", "only", "raw", "vars", "x":
				kw = append(kw, tok.value)
			}
		}
	}
	want := []string{
		"extends",
		"mid.html.twig",
		"block", "a", "x", "raw", "endblock",
		"include", "p.html.twig", "with", "vars", "only",
	}
	// String literals are tokenized as tokenStringOpen/text/close triples,
	// so the quoted template names are not tokenName values; collect their
	// text separately.
	var strs []string
	for _, tok := range toks {
		if tok.tokenType == tokenText && (strings.Contains(tok.value, ".twig")) {
			strs = append(strs, tok.value)
		}
	}
	if len(strs) != 2 || strs[0] != "mid.html.twig" || strs[1] != "p.html.twig" {
		t.Fatalf("expected ordered string tokens [mid.html.twig p.html.twig], got %v\nfull stream:\n%s", strs, tokenSummary(toks))
	}
	got := []string{"extends"}
	got = append(got, strs[0])
	got = append(got, "block", "a", "x", "raw", "endblock")
	got = append(got, "include")
	got = append(got, strs[1])
	got = append(got, "with", "vars", "only")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keyword/string subsequence mismatch:\n got=%v\nwant=%v", got, want)
	}

	// Structural delimiters must appear in matched pairs; an opener or
	// closer missing or reordered would let the parser accept nonsense.
	count := func(ty tokenType) int {
		n := 0
		for _, tok := range toks {
			if tok.tokenType == ty {
				n++
			}
		}
		return n
	}
	if count(tokenTagOpen) != count(tokenTagClose) {
		t.Errorf("unbalanced tag delimiters: %d open vs %d close", count(tokenTagOpen), count(tokenTagClose))
	}
	if count(tokenPrintOpen) != 1 || count(tokenPrintClose) != 1 {
		t.Errorf("expected exactly one print pair, got %d/%d", count(tokenPrintOpen), count(tokenPrintClose))
	}
	if toks[len(toks)-1].tokenType != tokenEOF {
		t.Errorf("stream must terminate with EOF, got %s", toks[len(toks)-1].tokenType)
	}
}

// TestTokenPositions pins the precise position information later surfaced
// in parse errors. Pos.Line is 1-based and Pos.Offset is the 0-based
// column within the current line (the lexer resets it after each
// newline — see lexer.emit), so the print opener on line 2 sits at
// column 0, while the name token inside it sits two columns later
// (opener "{{" occupies columns 0 and 1).
func TestTokenPositions(t *testing.T) {
	input := "{% extends 'a.twig' %}\n{{ x }}"
	toks := lexAll(t, input)
	var printOpen, nameTok token
	found := false
	for _, tok := range toks {
		switch {
		case tok.tokenType == tokenPrintOpen:
			printOpen, found = tok, true
		case found && tok.tokenType == tokenName:
			nameTok = tok
		}
	}
	if !found {
		t.Fatalf("no print open token in stream:\n%s", tokenSummary(toks))
	}
	if printOpen.Line != 2 || printOpen.Offset != 0 {
		t.Errorf("print open pos = %d:%d, want 2:0 (line:column)", printOpen.Line, printOpen.Offset)
	}
	if nameTok.Line != 2 || nameTok.Offset != 3 {
		t.Errorf("name x pos = %d:%d, want 2:3 (after the print opener and a space)", nameTok.Line, nameTok.Offset)
	}
}

// TestInheritParseErrorKeepsNameAndPos checks the error enrichment path used
// for every template reached through extends/include: a syntax error in a
// named tree must report the template Name() plus the exact line/column,
// never an anonymous error.
func TestInheritParseErrorKeepsNameAndPos(t *testing.T) {
	// A number where the block name belongs yields an UnexpectedTokenError
	// (a baseError), and enrichError stamps the tree name onto it. The token
	// sits on line 3 at column 9, right after the 9 columns of "{% block ".
	input := "ok\n\n{% block 42 %}\n"
	tree := NewNamedTree("mid.html.twig", bytes.NewReader([]byte(input)))
	err := tree.Parse()
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	pe, ok := err.(ParsingError)
	if !ok {
		t.Fatalf("expected ParsingError, got %T: %v", err, err)
	}
	if pe.Name() != "mid.html.twig" {
		t.Errorf("error name = %q, want mid.html.twig", pe.Name())
	}
	if pe.Pos().Line != 3 || pe.Pos().Offset != 9 {
		t.Errorf("error pos = %d:%d, want 3:9", pe.Pos().Line, pe.Pos().Offset)
	}
	if !strings.Contains(err.Error(), "mid.html.twig") {
		t.Errorf("error message %q does not carry the template name", err.Error())
	}
}
