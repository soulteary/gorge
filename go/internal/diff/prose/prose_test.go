package prose

import (
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// TestSegmentsReconstructBothTexts is the important test in this file.
//
// Prose output has no byte-exact reference to compare against, so the property
// that has to hold instead is losslessness: the "=" and "-" segments must
// rebuild the old text exactly and "=" with "+" the new one. Every split in
// the engine keeps its delimiters for this reason, and a regression that drops
// or duplicates one is invisible in a rendered diff — the text simply reads
// slightly wrong.
func TestSegmentsReconstructBothTexts(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
	}{
		{"identical", "hello world", "hello world"},
		{"word swap", "the quick fox", "the slow fox"},
		{"complete replace", "alpha", "beta"},
		{"paragraph added", "para1", "para1\n\npara2"},
		{"paragraph removed", "para1\n\npara2\n\npara3", "para1\n\npara3"},
		{"paragraph changed", "A\n\nB\n\nC", "A\n\nX\n\nC"},
		{"sentence changed", "Hello world. Goodbye moon.", "Hello world. Goodbye sun."},
		{"old empty", "", "hello"},
		{"new empty", "hello", ""},
		{"both empty", "", ""},
		{"whitespace only change", "hello world", "hello  world"},
		{"unicode", "你好世界", "你好地球"},
		{"trailing newlines", "a\n\n", "b\n\n"},
		{"leading whitespace", "   indented", "   changed"},
		{"punctuation heavy", "a, b; c! d? e.", "a, x; c! y? e."},
		{"character level", "abc", "axc"},
		{"long text small change",
			"The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs.",
			"The quick brown fox jumps over the lazy cat. Pack my box with five dozen liquor jugs."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Generate(&contracts.ProseRequest{Old: tc.old, New: tc.new})

			var oldSide, newSide strings.Builder
			for _, p := range result.Parts {
				switch p.Type {
				case "=":
					oldSide.WriteString(p.Text)
					newSide.WriteString(p.Text)
				case "-":
					oldSide.WriteString(p.Text)
				case "+":
					newSide.WriteString(p.Text)
				default:
					t.Errorf("unexpected segment type %q", p.Type)
				}
				if p.Text == "" {
					t.Error("an empty segment should never be emitted")
				}
			}

			if oldSide.String() != tc.old {
				t.Errorf("old side\n got: %q\nwant: %q", oldSide.String(), tc.old)
			}
			if newSide.String() != tc.new {
				t.Errorf("new side\n got: %q\nwant: %q", newSide.String(), tc.new)
			}
		})
	}
}

func TestGenerateShape(t *testing.T) {
	t.Run("identical input is a single unchanged segment", func(t *testing.T) {
		result := Generate(&contracts.ProseRequest{Old: "hello world", New: "hello world"})
		if len(result.Parts) != 1 || result.Parts[0].Type != "=" {
			t.Fatalf("expected one \"=\" segment, got %+v", result.Parts)
		}
	})

	t.Run("empty input produces no segments", func(t *testing.T) {
		result := Generate(&contracts.ProseRequest{Old: "", New: ""})
		if len(result.Parts) != 0 {
			t.Fatalf("expected no segments, got %+v", result.Parts)
		}
	})

	t.Run("unchanged paragraphs stay unchanged", func(t *testing.T) {
		// The point of the paragraph level: para1 and para3 must not be
		// dragged into the change just because para2 moved.
		result := Generate(&contracts.ProseRequest{
			Old: "para1\n\npara2\n\npara3",
			New: "para1\n\nchanged\n\npara3",
		})
		var kinds string
		for _, p := range result.Parts {
			kinds += p.Type
		}
		for _, want := range []string{"=", "-", "+"} {
			if !strings.Contains(kinds, want) {
				t.Fatalf("expected a %q segment, got %+v", want, result.Parts)
			}
		}
	})

	t.Run("only new text is an insertion", func(t *testing.T) {
		result := Generate(&contracts.ProseRequest{Old: "", New: "hello"})
		if len(result.Parts) != 1 || result.Parts[0].Type != "+" {
			t.Fatalf("expected a single insertion, got %+v", result.Parts)
		}
	})

	t.Run("only old text is a deletion", func(t *testing.T) {
		result := Generate(&contracts.ProseRequest{Old: "hello", New: ""})
		if len(result.Parts) != 1 || result.Parts[0].Type != "-" {
			t.Fatalf("expected a single deletion, got %+v", result.Parts)
		}
	})
}

func TestSplitCorpusIsLossless(t *testing.T) {
	inputs := []string{
		"para1\n\npara2",
		"Hello world. Goodbye world!",
		"hello world foo",
		"  leading and trailing  ",
		"a,b;c!d?e.",
		"你好世界",
		"one\ntwo\n\nthree",
	}

	for _, level := range []int{levelParagraph, levelSentence, levelWord, levelChar} {
		for _, input := range inputs {
			joined := strings.Join(splitCorpus(input, level), "")
			if joined != input {
				t.Errorf("level %d on %q reassembled to %q", level, input, joined)
			}
		}
	}
}

func TestSplitCorpus(t *testing.T) {
	t.Run("characters split on runes", func(t *testing.T) {
		if got := splitCorpus("abc", levelChar); len(got) != 3 {
			t.Fatalf("expected 3 pieces, got %v", got)
		}
		got := splitCorpus("你好", levelChar)
		if len(got) != 2 || got[0] != "你" || got[1] != "好" {
			t.Fatalf("multi-byte runes were torn apart: %v", got)
		}
	})

	t.Run("empty input yields no pieces", func(t *testing.T) {
		if got := splitCorpus("", levelParagraph); len(got) != 0 {
			t.Fatalf("expected no pieces, got %v", got)
		}
		if got := splitChars(""); len(got) != 0 {
			t.Fatalf("expected no pieces, got %v", got)
		}
	})

	t.Run("an unknown level returns the corpus whole", func(t *testing.T) {
		got := splitCorpus("anything", 99)
		if len(got) != 1 || got[0] != "anything" {
			t.Fatalf("expected the input unchanged, got %v", got)
		}
	})
}

func TestTrimApart(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"hello", []string{"hello"}},
		{"  hello  ", []string{"  ", "hello", "  "}},
		{"  ", []string{"  "}},
		{"hello  ", []string{"hello", "  "}},
		{"  hello", []string{"  ", "hello"}},
		{"\n\t x \t\n", []string{"\n\t ", "x", " \t\n"}},
	}

	for _, tc := range cases {
		got := trimApart(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("trimApart(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("trimApart(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestStitchPiecesDropsOnlyTheTrailingEmpty(t *testing.T) {
	cases := []struct {
		name   string
		parts  []string
		delims []string
		level  int
	}{
		{"word level", []string{"hello", "world"}, []string{" "}, levelWord},
		{"paragraph level trailing empty", []string{"hello", ""}, []string{"\n"}, levelParagraph},
		{"word level trailing empty", []string{"hello", "world", ""}, []string{" ", " "}, levelWord},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stitchPieces(tc.parts, tc.delims, tc.level)
			if len(got) == 0 {
				t.Fatal("expected some pieces")
			}
			for i, piece := range got {
				if piece == "" {
					t.Errorf("piece %d is empty; the trailing empty should have been dropped", i)
				}
			}
		})
	}
}

func TestHashDiff(t *testing.T) {
	t.Run("identical pieces all match", func(t *testing.T) {
		parts := hashDiff([]string{"a", "b", "c"}, []string{"a", "b", "c"})
		for _, p := range parts {
			if p.Type != "=" {
				t.Fatalf("expected every piece unchanged, got %+v", parts)
			}
		}
	})

	t.Run("empty input yields nothing", func(t *testing.T) {
		if parts := hashDiff(nil, nil); len(parts) != 0 {
			t.Fatalf("expected no segments, got %+v", parts)
		}
	})

	t.Run("interleaved new pieces are inserted in place", func(t *testing.T) {
		parts := hashDiff([]string{"a", "b"}, []string{"x", "a", "y", "b", "z"})
		inserts := 0
		for _, p := range parts {
			if p.Type == "+" {
				inserts++
			}
		}
		if inserts != 3 {
			t.Fatalf("expected x, y and z inserted, got %+v", parts)
		}
	})

	t.Run("matching is forward only", func(t *testing.T) {
		// Reversed pieces must not all read as unchanged: the cursor only
		// moves forward, so at most one of them can match.
		parts := hashDiff([]string{"aaa", "bbb", "ccc"}, []string{"ccc", "bbb", "aaa"})
		equals := 0
		for _, p := range parts {
			if p.Type == "=" {
				equals++
			}
		}
		if equals > 1 {
			t.Fatalf("a reordering should not read as unchanged, got %+v", parts)
		}
	})

	t.Run("duplicate content claims each piece once", func(t *testing.T) {
		parts := hashDiff([]string{"x", "x", "x"}, []string{"x", "y", "x"})
		var inserts, deletes int
		for _, p := range parts {
			switch p.Type {
			case "+":
				inserts++
			case "-":
				deletes++
			}
		}
		if inserts != 1 || deletes != 1 {
			t.Fatalf("expected one insert and one delete, got %+v", parts)
		}
	})
}

func TestEditDistanceDiff(t *testing.T) {
	repeat := func(s string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}

	kinds := func(parts []contracts.ProsePart) string {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Type)
		}
		return b.String()
	}

	t.Run("a mid-sequence change is isolated", func(t *testing.T) {
		parts, tooLarge := editDistanceDiff([]string{"a", "b", "c"}, []string{"a", "x", "c"}, levelWord)
		if tooLarge {
			t.Fatal("this input is well under the threshold")
		}
		if got := kinds(parts); got != "=-+=" {
			t.Fatalf("expected \"=-+=\", got %q in %+v", got, parts)
		}
	})

	t.Run("identical input is all unchanged", func(t *testing.T) {
		pieces := []string{"a", "b", "c"}
		parts, _ := editDistanceDiff(pieces, pieces, levelWord)
		if got := kinds(parts); got != "===" {
			t.Fatalf("expected \"===\", got %q", got)
		}
	})

	t.Run("one empty side degenerates cleanly", func(t *testing.T) {
		parts, _ := editDistanceDiff(nil, []string{"a", "b"}, levelSentence)
		if got := kinds(parts); got != "++" {
			t.Fatalf("expected \"++\", got %q", got)
		}
		parts, _ = editDistanceDiff([]string{"a", "b"}, nil, levelSentence)
		if got := kinds(parts); got != "--" {
			t.Fatalf("expected \"--\", got %q", got)
		}
	})

	t.Run("exactly maxEditDistance pieces still align", func(t *testing.T) {
		u := repeat("same", maxEditDistance)
		v := repeat("same", maxEditDistance)
		v[maxEditDistance/2] = "changed"
		parts, tooLarge := editDistanceDiff(u, v, levelSentence)
		if tooLarge {
			t.Fatalf("%d pieces is the limit, not past it", maxEditDistance)
		}
		if !strings.ContainsAny(kinds(parts), "-+") {
			t.Fatal("expected the changed piece to be reported")
		}
	})

	t.Run("past the threshold it degrades and says so", func(t *testing.T) {
		parts, tooLarge := editDistanceDiff(
			repeat("a", maxEditDistance+1), repeat("b", maxEditDistance+1), levelWord)
		if !tooLarge {
			t.Fatal("expected tooLarge to be reported")
		}
		// Every deletion first, then every insertion, with no alignment.
		got := kinds(parts)
		if strings.Contains(got, "+-") || strings.Contains(got, "=") {
			t.Fatalf("expected all deletes then all inserts, got %q", got)
		}
	})

	t.Run("a shared piece survives at the sentence level", func(t *testing.T) {
		// Sentences do not get smoothed, so the shared "b" stays visible.
		parts, _ := editDistanceDiff([]string{"a", "b", "c"}, []string{"x", "b", "y"}, levelSentence)
		found := false
		for _, p := range parts {
			if p.Type == "=" && p.Text == "b" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected \"b\" to remain unchanged, got %+v", parts)
		}
	})

	t.Run("a shared piece is absorbed at the word level", func(t *testing.T) {
		// Same input one level finer, where an isolated match is noise.
		parts, _ := editDistanceDiff([]string{"a", "b", "c"}, []string{"x", "b", "y"}, levelWord)
		for _, p := range parts {
			if p.Type == "=" {
				t.Fatalf("expected smoothing to absorb the lone match, got %+v", parts)
			}
		}
	})
}

func TestSmooth(t *testing.T) {
	cases := []struct {
		name      string
		ops, want string
	}{
		{"isolated match becomes a substitution", "dsi", "dxi"},
		{"consecutive matches are left alone", "dssi", "dssi"},
		{"all matches are left alone", "sss", "sss"},
		{"a leading match has no left neighbour", "si", "si"},
		{"a trailing match has no right neighbour", "ds", "ds"},
		{"empty input", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(smooth([]byte(tc.ops))); got != tc.want {
				t.Errorf("smooth(%q) = %q, want %q", tc.ops, got, tc.want)
			}
		})
	}
}

func TestReorderParts(t *testing.T) {
	part := func(kind, text string) contracts.ProsePart {
		return contracts.ProsePart{Type: kind, Text: text}
	}

	t.Run("deletions are moved ahead of insertions", func(t *testing.T) {
		got := reorderParts([]contracts.ProsePart{
			part("+", "new1"), part("-", "old1"), part("+", "new2"), part("-", "old2"),
		})
		if len(got) != 2 || got[0].Type != "-" || got[1].Type != "+" {
			t.Fatalf("expected one merged deletion then one merged insertion, got %+v", got)
		}
		if got[0].Text != "old1old2" || got[1].Text != "new1new2" {
			t.Fatalf("runs were not merged in order: %+v", got)
		}
	})

	t.Run("adjacent segments of one type merge", func(t *testing.T) {
		got := reorderParts([]contracts.ProsePart{part("=", "hello"), part("=", " world")})
		if len(got) != 1 || got[0].Text != "hello world" {
			t.Fatalf("expected a single merged segment, got %+v", got)
		}
	})

	t.Run("nil input", func(t *testing.T) {
		if got := reorderParts(nil); len(got) != 0 {
			t.Fatalf("expected no segments, got %+v", got)
		}
	})

	t.Run("an unchanged segment separates two runs", func(t *testing.T) {
		got := reorderParts([]contracts.ProsePart{
			part("+", "a"), part("=", "keep"), part("+", "b"),
		})
		if len(got) != 3 || got[1].Type != "=" {
			t.Fatalf("expected the runs to stay separate, got %+v", got)
		}
	})
}

func TestCombineRunsLiftsOnlyLayoutCharacters(t *testing.T) {
	part := func(kind, text string) []contracts.ProsePart {
		return []contracts.ProsePart{{Type: kind, Text: text}}
	}

	t.Run("a shared leading space is lifted out", func(t *testing.T) {
		got := combineRuns(part("-", " hello"), part("+", " world"))
		if len(got) < 2 || got[0].Type != "=" || got[0].Text != " " {
			t.Fatalf("expected a leading unchanged space, got %+v", got)
		}
	})

	t.Run("a shared trailing period is lifted out", func(t *testing.T) {
		got := combineRuns(part("-", "hello."), part("+", "world."))
		last := got[len(got)-1]
		if last.Type != "=" || last.Text != "." {
			t.Fatalf("expected a trailing unchanged period, got %+v", got)
		}
	})

	t.Run("both ends at once", func(t *testing.T) {
		got := combineRuns(part("-", " hello."), part("+", " world."))
		if len(got) != 4 {
			t.Fatalf("expected prefix, deletion, insertion and suffix, got %+v", got)
		}
		if got[0].Text != " " || got[3].Text != "." {
			t.Fatalf("wrong pieces lifted: %+v", got)
		}
		if got[1].Text != "hello" || got[2].Text != "world" {
			t.Fatalf("bodies were mis-sliced: %+v", got)
		}
	})

	t.Run("a shared ordinary prefix is NOT lifted", func(t *testing.T) {
		// The restriction that gives this function its name: claiming "abc"
		// survived would misrepresent a whole-token replacement.
		got := combineRuns(part("-", "abcX"), part("+", "abcY"))
		if len(got) != 2 || got[0].Type != "-" || got[1].Type != "+" {
			t.Fatalf("expected an unsplit deletion and insertion, got %+v", got)
		}
		if got[0].Text != "abcX" || got[1].Text != "abcY" {
			t.Fatalf("expected the tokens whole, got %+v", got)
		}
	})

	t.Run("nothing in common", func(t *testing.T) {
		got := combineRuns(part("-", "abc"), part("+", "xyz"))
		if len(got) != 2 {
			t.Fatalf("expected exactly a deletion and an insertion, got %+v", got)
		}
	})

	t.Run("one-sided runs pass through", func(t *testing.T) {
		if got := combineRuns(part("-", "removed"), nil); len(got) != 1 || got[0].Type != "-" {
			t.Fatalf("expected a lone deletion, got %+v", got)
		}
		if got := combineRuns(nil, part("+", "added")); len(got) != 1 || got[0].Type != "+" {
			t.Fatalf("expected a lone insertion, got %+v", got)
		}
	})
}

func TestMergeRunText(t *testing.T) {
	got := mergeRunText([]contracts.ProsePart{{Text: "hello"}, {Text: " "}, {Text: "world"}})
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
	if got := mergeRunText(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestMin3(t *testing.T) {
	cases := []struct {
		a, b, c, want int
	}{
		{1, 2, 3, 1},
		{3, 1, 2, 1},
		{3, 2, 1, 1},
		{1, 1, 1, 1},
		{1, 1, 2, 1},
		{2, 1, 1, 1},
	}
	for _, tc := range cases {
		if got := min3(tc.a, tc.b, tc.c); got != tc.want {
			t.Errorf("min3(%d,%d,%d) = %d, want %d", tc.a, tc.b, tc.c, got, tc.want)
		}
	}
}

func TestBuildProseDiff(t *testing.T) {
	t.Run("both sides empty", func(t *testing.T) {
		if got := buildProseDiff("", "", levelParagraph); len(got) != 0 {
			t.Fatalf("expected no segments, got %+v", got)
		}
	})

	t.Run("character level terminates", func(t *testing.T) {
		if got := buildProseDiff("abc", "axc", levelChar); len(got) == 0 {
			t.Fatal("expected segments at the character level")
		}
	})

	t.Run("whitespace-only corpus produces no change segments", func(t *testing.T) {
		for _, p := range buildProseDiff("\n\n", "\n\n", levelParagraph) {
			if p.Type != "=" {
				t.Fatalf("expected only unchanged segments, got %+v", p)
			}
		}
	})

	t.Run("a too-large level does not recurse", func(t *testing.T) {
		// Enough sentences to blow past maxEditDistance, so the level falls
		// back to delete-all/insert-all and the finer levels are skipped.
		// Had it recursed, the two sides would come back interleaved into
		// hundreds of word- and character-level segments.
		var oldText, newText strings.Builder
		for i := 0; i < maxEditDistance+50; i++ {
			oldText.WriteString("old. ")
			newText.WriteString("new. ")
		}
		got := buildProseDiff(oldText.String(), newText.String(), levelSentence)

		var kinds strings.Builder
		for _, p := range got {
			kinds.WriteString(p.Type)
		}
		// One deletion then one insertion. A trailing "=" is allowed and
		// expected: combineRuns still lifts the ". " both sides end with.
		if k := kinds.String(); k != "-+" && k != "-+=" && k != "=-+" && k != "=-+=" {
			t.Fatalf("expected a single delete/insert pair, got %q in %d segments", k, len(got))
		}
	})
}
