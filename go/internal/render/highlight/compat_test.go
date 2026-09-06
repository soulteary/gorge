package highlight

import (
	"regexp"
	"strings"
	"testing"
)

func TestPygmentsCSSClassCompatibility(t *testing.T) {
	h := New()

	source := `def hello():
    print("Hello, World!")
    x = 42
    # comment
    return x
`
	result, err := h.Highlight(source, "python")
	if err != nil {
		t.Fatal(err)
	}

	re := regexp.MustCompile(`class="([^"]+)"`)
	matches := re.FindAllStringSubmatch(result.HTML, -1)
	if len(matches) == 0 {
		t.Fatal("no CSS classes found in output")
	}

	classes := make(map[string]bool)
	for _, m := range matches {
		classes[m[1]] = true
	}

	t.Logf("Found CSS classes: %v", classes)

	pygmentsKeyClasses := []string{"k", "n", "nf", "nb", "s2", "mi", "c1"}
	foundAny := false
	for _, cls := range pygmentsKeyClasses {
		if classes[cls] {
			foundAny = true
			t.Logf("  Pygments class %q: FOUND", cls)
		}
	}

	if !foundAny {
		t.Error("no Pygments-compatible CSS classes found; " +
			"Chroma may not be producing compatible output")
	}
}

func TestNoWrappingElements(t *testing.T) {
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

func TestOutputStructure(t *testing.T) {
	h := New()
	result, err := h.Highlight("print('hello')\n", "python")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(result.HTML, "<span") {
		t.Error("output should contain <span> elements")
	}
	if !strings.Contains(result.HTML, "hello") {
		t.Error("output should preserve source content")
	}
}

func TestCRLFHandling(t *testing.T) {
	h := New()
	source := "x = 1\r\ny = 2\r\n"
	result, err := h.Highlight(source, "python")
	if err != nil {
		t.Fatal(err)
	}
	if result.HTML == "" {
		t.Error("should handle CRLF input")
	}
}

func TestLoneCRHandling(t *testing.T) {
	h := New()
	source := "x = 1\ry = 2\r"
	result, err := h.Highlight(source, "python")
	if err != nil {
		t.Fatal(err)
	}
	if result.HTML == "" {
		t.Error("should handle lone CR input (unlike Pygments)")
	}
}
