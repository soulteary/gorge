package highlight

import (
	"strings"
	"testing"
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
	}

	for _, tc := range cases {
		resolved := h.resolveLexer(tc.input)
		if resolved != tc.expected {
			t.Errorf("resolveLexer(%q) = %q, want %q", tc.input, resolved, tc.expected)
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
