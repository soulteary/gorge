package highlight

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2/lexers"
)

func TestHighlightPython(t *testing.T) {
	h := New()
	result, err := h.Highlight("print('hello')", "python")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.HTML, "<span") {
		t.Error("expected HTML with span elements")
	}
	if !strings.Contains(result.HTML, "hello") {
		t.Error("expected source content in output")
	}
	if result.Language != "python" {
		t.Errorf("expected language python, got %s", result.Language)
	}
}

func TestHighlightGo(t *testing.T) {
	h := New()
	source := `package main

import "fmt"

func main() {
	fmt.Println("Hello, World!")
}
`
	result, err := h.Highlight(source, "go")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.HTML, "<span") {
		t.Error("expected HTML with span elements")
	}
}

func TestHighlightWithAlias(t *testing.T) {
	h := New()

	result, err := h.Highlight("int x = 42;", "cc")
	if err != nil {
		t.Fatal(err)
	}
	if result.Language != "cc" {
		t.Errorf("expected cc, got %s", result.Language)
	}
}

func TestHighlightUnknownLanguage(t *testing.T) {
	h := New()
	result, err := h.Highlight("some text", "unknownlang12345")
	if err != nil {
		t.Fatal(err)
	}
	if result.HTML == "" {
		t.Error("expected non-empty output even for unknown language")
	}
}

func TestHighlightAutoDetect(t *testing.T) {
	h := New()
	source := `#!/bin/bash
echo "hello"
for i in 1 2 3; do
    echo $i
done
`
	result, err := h.Highlight(source, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.HTML == "" {
		t.Error("expected non-empty output for auto-detect")
	}
}

func TestLanguages(t *testing.T) {
	h := New()
	langs := h.Languages()
	if len(langs) < 10 {
		t.Errorf("expected at least 10 languages, got %d", len(langs))
	}

	found := false
	for _, l := range langs {
		if l == "python" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected python in supported languages")
	}
}

func TestLexerMapResolution(t *testing.T) {
	h := New()

	cases := []struct {
		input    string
		expected string
	}{
		{"py", "python"},
		{"cc", "cpp"},
		{"sh", "bash"},
		{"rs", "rust"},
		{"ts", "typescript"},
		{"yml", "yaml"},
		{"go", "go"},

		// The PHP table is case-sensitive and the language arrives as a raw
		// filename extension, so these four must not collapse into two.
		{"R", "splus"},
		{"r", "rebol"},
		{"S", "splus"},
		{"s", "gas"},

		// Mixed-case keys with no differing lowercase twin still have to
		// resolve, whether they hit the verbatim entry or the folded one.
		{"Makefile", "make"},
		{"GNUmakefile", "make"},
		{"SConstruct", "python"},
		{"ASM", "nasm"},

		// An unmapped name keeps falling through lowercased.
		{"RUST", "rust"},
	}

	for _, tc := range cases {
		resolved := h.resolveLexer(tc.input)
		if resolved != tc.expected {
			t.Errorf("resolveLexer(%q) = %q, want %q", tc.input, resolved, tc.expected)
		}
	}
}

// TestCaseSensitiveAliasesReachDistinctLexers guards the half of the fix that
// resolveLexer alone cannot: a mapped name is only useful if Chroma actually
// has a lexer under it. "splus" is an alias of Chroma's R lexer, so "R" and
// "S" land on R, while "s" lands on GAS.
func TestCaseSensitiveAliasesReachDistinctLexers(t *testing.T) {
	h := New()

	cases := []struct {
		language string
		want     string
	}{
		{"R", "R"},
		{"S", "R"},
		{"s", "GAS"},
	}

	for _, tc := range cases {
		lexer := lexers.Get(h.resolveLexer(tc.language))
		if lexer == nil {
			t.Errorf("language %q resolves to a lexer Chroma does not have", tc.language)
			continue
		}
		if got := lexer.Config().Name; got != tc.want {
			t.Errorf("language %q selected the %q lexer, want %q", tc.language, got, tc.want)
		}
	}
}

func TestHighlightEmptySource(t *testing.T) {
	h := New()
	result, err := h.Highlight("", "python")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Error("expected non-nil result for empty source")
	}
}

func TestHighlightNoWrapping(t *testing.T) {
	h := New()
	result, err := h.Highlight("x = 1", "python")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(result.HTML, "<pre>") {
		t.Error("output should not contain <pre> wrapper")
	}
	if strings.Contains(result.HTML, `<div class="highlight">`) {
		t.Error("output should not contain <div> wrapper")
	}
}
