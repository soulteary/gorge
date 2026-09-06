// Package highlight renders source code to Pygments-compatible HTML using
// Chroma. See TECHNICAL_REPORT.md in this directory for the design rationale
// and compat/phorge/README.md for the constraints that must not be changed.
package highlight

import (
	"bytes"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/soulteary/gorge/go/internal/contracts"
)

var (
	formatter = html.New(
		html.WithClasses(true),
		html.PreventSurroundingPre(true),
	)
	// Use pygments style for maximum compatibility with existing Phorge CSS
	defaultStyle = styles.Get("pygments")
)

// Highlighter performs syntax highlighting using Chroma, producing
// Pygments-compatible CSS class names in <span> elements.
type Highlighter struct {
	lexerMap map[string]string
}

func New() *Highlighter {
	return &Highlighter{
		lexerMap: buildLexerMap(),
	}
}

// Highlight takes source code and an optional language hint, returns
// HTML with <span class="..."> using Pygments-compatible CSS classes.
//
// It returns the wire type from internal/contracts directly rather than a
// separate domain struct: a second struct would need its own json tags and
// the two would drift.
func (h *Highlighter) Highlight(source, language string) (*contracts.HighlightResult, error) {
	resolved := h.resolveLexer(language)

	lexer := lexers.Get(resolved)
	if lexer == nil {
		lexer = lexers.Analyse(source)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}

	lexer = chroma.Coalesce(lexer)

	iterator, err := lexer.Tokenise(nil, source)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := formatter.Format(&buf, defaultStyle, iterator); err != nil {
		return nil, err
	}

	output := buf.String()

	detectedLang := strings.ToLower(lexer.Config().Name)
	if language != "" {
		detectedLang = language
	}

	return &contracts.HighlightResult{
		HTML:     output,
		Language: detectedLang,
	}, nil
}

// Languages returns all supported lexer names.
func (h *Highlighter) Languages() []string {
	names := lexers.Names(false)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, strings.ToLower(name))
	}
	return result
}

func (h *Highlighter) resolveLexer(language string) string {
	if language == "" {
		return ""
	}
	lang := strings.ToLower(language)
	if mapped, ok := h.lexerMap[lang]; ok {
		return mapped
	}
	return lang
}
