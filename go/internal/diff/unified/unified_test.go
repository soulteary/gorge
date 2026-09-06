package unified

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The expectations in TestOutputMatchesSystemDiff were not written by hand.
// Each one was captured from the real binary:
//
//	printf '<old>' > a; printf '<new>' > b
//	diff -U65535 -L 'a 9999-99-99' -L 'b 9999-99-99' a b
//
// If one of them starts failing, re-run that command before touching the
// expectation. The output of this package is parsed by ArcanistDiffParser,
// which accepts a wrong hunk header without complaint and then misplaces
// every line after it, so a "harmless looking" edit here has no local symptom
// at all. See compat/phorge/README.md.
func TestOutputMatchesSystemDiff(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     string
	}{{
		// A single line on each side omits the count entirely. This is the
		// divergence that is easiest to introduce and hardest to notice.
		name: "one line each way omits the count",
		old:  "hello\n",
		new:  "gopher\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1 +1 @@\n-hello\n+gopher\n",
	}, {
		name: "two lines keeps the count",
		old:  "hello\nworld\n",
		new:  "hello\ngopher\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n hello\n-world\n+gopher\n",
	}, {
		name: "the two sides are counted independently",
		old:  "a\n",
		new:  "a\nb\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1 +1,2 @@\n a\n+b\n",
	}, {
		// An empty side starts at 0, not 1: there is no line 1 to point at.
		name: "empty old side is 0,0",
		old:  "",
		new:  "x\ny\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -0,0 +1,2 @@\n+x\n+y\n",
	}, {
		name: "empty new side is 0,0",
		old:  "x\ny\n",
		new:  "",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +0,0 @@\n-x\n-y\n",
	}, {
		// Both last lines are unterminated and differ, so the marker is
		// emitted twice, once per side.
		name: "both sides unterminated",
		old:  "a\nb",
		new:  "a\nc",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n a\n" +
			"-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n",
	}, {
		// The texts share the characters "b" on the last line, yet it is not
		// context: unterminated b and terminated b are different lines. This
		// is the case that forces hasNewline into the equality test.
		name: "unterminated line is not equal to the terminated one",
		old:  "a\nb",
		new:  "a\nb\nc\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,3 @@\n a\n" +
			"-b\n\\ No newline at end of file\n+b\n+c\n",
	}, {
		// Only the new side lacks its newline, so the marker lands after the
		// "+" record rather than the "-" one.
		name: "marker follows the side that lacks the newline",
		old:  "a\nb\n",
		new:  "a\nb",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n a\n" +
			"-b\n+b\n\\ No newline at end of file\n",
	}, {
		// Here the unterminated last line *is* shared, so it stays context
		// and the marker appears once.
		name: "shared unterminated tail emits the marker once",
		old:  "x\nsame",
		new:  "y\nsame",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n" +
			"-x\n+y\n same\n\\ No newline at end of file\n",
	}, {
		name: "insertion in the middle",
		old:  "a\nb\nc\n",
		new:  "a\nx\nb\nc\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,3 +1,4 @@\n a\n+x\n b\n c\n",
	}, {
		name: "deletion in the middle",
		old:  "a\nb\nc\n",
		new:  "a\nc\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,3 +1,2 @@\n a\n-b\n c\n",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Generate(&contracts.DiffRequest{
				Old: tc.old, New: tc.new, OldName: "a", NewName: "b",
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got.Equal {
				t.Error("expected Equal to be false")
			}
			if got.Diff != tc.want {
				t.Errorf("diff mismatch\n got: %q\nwant: %q", got.Diff, tc.want)
			}
		})
	}
}

// TestIdenticalDiffFollowsPHPNotSystemDiff pins the one branch that is
// deliberately not aligned with diff, because diff produces nothing at all
// when the files match and PHP synthesises its own string instead. Its quirks
// are the contract; see buildIdenticalDiff's comment for why each is here.
func TestIdenticalDiffFollowsPHPNotSystemDiff(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{{
		// explode("\n", "a\nb\n") is three elements, the last one empty, so
		// the count is 3 and the body ends with a line holding one space.
		name: "trailing newline yields an extra blank context line",
		text: "a\nb\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,3 +1,3 @@\n a\n b\n \n",
	}, {
		// Empty input still splits to one element, so this is a one-line
		// hunk rather than a header-only diff.
		name: "empty input is a one line hunk",
		text: "",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,1 +1,1 @@\n \n",
	}, {
		// The ",1" that GNU would omit is kept, because PHP hardcodes it.
		name: "single line keeps the redundant count",
		text: "only\n",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n only\n \n",
	}, {
		name: "unterminated input has no extra element",
		text: "a\nb",
		want: "--- a 9999-99-99\n+++ b 9999-99-99\n@@ -1,2 +1,2 @@\n a\n b\n",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Generate(&contracts.DiffRequest{
				Old: tc.text, New: tc.text, OldName: "a", NewName: "b",
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !got.Equal {
				t.Error("expected Equal to be true")
			}
			if got.Diff != tc.want {
				t.Errorf("diff mismatch\n got: %q\nwant: %q", got.Diff, tc.want)
			}
		})
	}
}

func TestDefaultNamesArePhorgesPlaceholder(t *testing.T) {
	got, err := Generate(&contracts.DiffRequest{Old: "x\n", New: "y\n"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "--- /dev/universe 9999-99-99\n+++ /dev/universe 9999-99-99\n"
	if !strings.HasPrefix(got.Diff, want) {
		t.Errorf("expected the /dev/universe placeholder on both labels, got %q", got.Diff)
	}
}

func TestNormalizeComparesWithoutSpacesAndTabs(t *testing.T) {
	cases := []struct {
		name      string
		old, new  string
		wantEqual bool
	}{
		{"spaces only", "hello world\n", "helloworld\n", true},
		{"tabs only", "hello\tworld\n", "helloworld\n", true},
		{"indentation", "    a\n", "a\n", true},
		// Newlines are not stripped, so a real content change still shows.
		{"real change survives", "hello world\n", "helloxworld\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Generate(&contracts.DiffRequest{
				Old: tc.old, New: tc.new, Normalize: true,
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got.Equal != tc.wantEqual {
				t.Errorf("Equal = %v, want %v (diff %q)", got.Equal, tc.wantEqual, got.Diff)
			}
		})
	}
}

// TestTooManyCellsIsRejectedButSkewIsNot covers both halves of the guard: it
// has to stop a table that would not fit in memory, and it must not punish a
// comparison that is merely lopsided.
func TestTooManyCellsIsRejectedButSkewIsNot(t *testing.T) {
	lines := func(prefix string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(prefix)
			b.WriteString(strconv.Itoa(i))
			b.WriteByte('\n')
		}
		return b.String()
	}

	t.Run("square input over the cell budget is refused", func(t *testing.T) {
		// 2001 x 2001 is just over maxCells.
		_, err := Generate(&contracts.DiffRequest{
			Old: lines("o", 2001), New: lines("n", 2001),
		})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("expected ErrTooLarge, got %v", err)
		}
	})

	t.Run("skewed input under the budget is allowed", func(t *testing.T) {
		// 10 x 100000 is 1M cells: far more lines than the square case, but
		// a much smaller table. A line-count ceiling would reject this.
		_, err := Generate(&contracts.DiffRequest{
			Old: lines("o", 10), New: lines("n", 100000),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestSplitLinesRecordsTermination(t *testing.T) {
	cases := []struct {
		input string
		want  []line
	}{
		{"", nil},
		{"a\n", []line{{"a", true}}},
		{"a\nb\n", []line{{"a", true}, {"b", true}}},
		{"a\nb", []line{{"a", true}, {"b", false}}},
		{"a", []line{{"a", false}}},
		// A trailing blank line is a terminated empty line, not an absence.
		{"a\n\n", []line{{"a", true}, {"", true}}},
	}

	for _, tc := range cases {
		got := splitLines(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitLines(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLines(%q)[%d] = %v, want %v", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestLinesEqualComparesTermination(t *testing.T) {
	cases := []struct {
		name string
		a, b []line
		want bool
	}{
		{"both empty", nil, nil, true},
		{"same text and termination", []line{{"a", true}}, []line{{"a", true}}, true},
		{"different text", []line{{"a", true}}, []line{{"b", true}}, false},
		{"different length", []line{{"a", true}, {"b", true}}, []line{{"a", true}}, false},
		// The reason this type exists rather than a plain string.
		{"same text, different termination", []line{{"a", false}}, []line{{"a", true}}, false},
	}

	for _, tc := range cases {
		if got := linesEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: linesEqual = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormatRangeHasThreeShapes(t *testing.T) {
	cases := []struct {
		count int
		want  string
	}{
		{0, "0,0"},
		{1, "1"},
		{2, "1,2"},
		{65535, "1,65535"},
	}

	for _, tc := range cases {
		if got := formatRange(tc.count); got != tc.want {
			t.Errorf("formatRange(%d) = %q, want %q", tc.count, got, tc.want)
		}
	}
}

func TestNormalizeText(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"hello world", "helloworld"},
		{"hello\tworld", "helloworld"},
		{"  a  b  ", "ab"},
		{"nochange", "nochange"},
		{"", ""},
		// Newlines survive: stripping them would merge lines.
		{"a\nb", "a\nb"},
	}

	for _, tc := range cases {
		if got := normalizeText(tc.input); got != tc.want {
			t.Errorf("normalizeText(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestLCSEdgeShapes(t *testing.T) {
	l := func(texts ...string) []line {
		out := make([]line, len(texts))
		for i, text := range texts {
			out[i] = line{text: text, hasNewline: true}
		}
		return out
	}

	count := func(ops []editOp, want editOp) int {
		n := 0
		for _, op := range ops {
			if op == want {
				n++
			}
		}
		return n
	}

	t.Run("empty old is all inserts", func(t *testing.T) {
		ops := lcs(nil, l("a", "b"))
		if len(ops) != 2 || count(ops, opInsert) != 2 {
			t.Fatalf("expected two inserts, got %q", ops)
		}
	})

	t.Run("empty new is all deletes", func(t *testing.T) {
		ops := lcs(l("a", "b"), nil)
		if len(ops) != 2 || count(ops, opDelete) != 2 {
			t.Fatalf("expected two deletes, got %q", ops)
		}
	})

	t.Run("both empty is no ops", func(t *testing.T) {
		if ops := lcs(nil, nil); len(ops) != 0 {
			t.Fatalf("expected no ops, got %q", ops)
		}
	})

	t.Run("shared lines are kept", func(t *testing.T) {
		ops := lcs(l("a", "b", "c", "d"), l("a", "x", "c", "y"))
		if got := count(ops, opEqual); got != 2 {
			t.Fatalf("expected a and c to be shared, got %d equal ops in %q", got, ops)
		}
	})

	t.Run("nothing shared means no equal ops", func(t *testing.T) {
		ops := lcs(l("x", "y"), l("m", "n"))
		if got := count(ops, opEqual); got != 0 {
			t.Fatalf("expected no equal ops, got %d in %q", got, ops)
		}
	})

	t.Run("a replaced line comes out delete before insert", func(t *testing.T) {
		// The order is the contract: the hunk parser expects -old then +new.
		ops := lcs(l("keep", "old"), l("keep", "new"))
		if string(ops) != "=-+" {
			t.Fatalf("expected \"=-+\", got %q", ops)
		}
	})
}
