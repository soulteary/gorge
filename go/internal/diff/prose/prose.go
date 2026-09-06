// Package prose produces human-readable diffs of running text.
//
// It replaces PhutilProseDifferenceEngine, and unlike the unified package its
// output has no byte-exact reference to match: Phorge renders these segments
// as markup rather than parsing them back. What has to hold is the shape of
// the result, and one property in particular: concatenating the "=" and "-"
// segments must reproduce the old text exactly, and "=" with "+" the new one.
// Every split in here is therefore lossless, delimiters included.
//
// The engine walks four granularities, coarse to fine:
//
//	paragraphs (\n+) -> sentences ([\n,!;?.]+) -> words (\s+) -> characters
//
// Only the regions that actually changed descend to the next level, which is
// what keeps the cost bounded: an unchanged paragraph is compared once, by
// hash, and never looked at again. The alternative — running the character
// level over the whole corpus — is both quadratic and unreadable, because it
// finds coincidental single-character matches across unrelated words.
package prose

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Granularity levels, coarse to fine. levelChar terminates the recursion.
const (
	levelParagraph = 0
	levelSentence  = 1
	levelWord      = 2
	levelChar      = 3
)

// maxEditDistance caps how many pieces either side may have before the edit
// distance matrix is abandoned.
//
// The matrix is O(n*m), so 128 bounds a single level at 16384 cells. Beyond
// that the level degrades to "delete everything, insert everything", and the
// caller stops recursing: a level that could not align its pieces gives the
// finer levels nothing meaningful to refine, so descending would spend real
// time to produce a result no more readable than the fallback.
const maxEditDistance = 128

// Generate produces the prose diff between the request's two sides.
func Generate(req *contracts.ProseRequest) *contracts.ProseResult {
	parts := buildProseDiff(req.Old, req.New, levelParagraph)
	return &contracts.ProseResult{Parts: reorderParts(parts)}
}

func buildProseDiff(u, v string, level int) []contracts.ProsePart {
	uParts := splitCorpus(u, level)
	vParts := splitCorpus(v, level)

	var diff []contracts.ProsePart
	var tooLarge bool

	// The paragraph level aligns by content hash rather than edit distance.
	// Paragraphs are few but long, so equality is cheap to test and almost
	// always decisive, while an edit distance matrix over them would pay
	// O(n*m) string comparisons for no extra alignment quality.
	if level == levelParagraph {
		diff = hashDiff(uParts, vParts)
	} else {
		diff, tooLarge = editDistanceDiff(uParts, vParts, level)
	}

	diff = reorderParts(diff)

	if level == levelChar {
		return diff
	}

	// Group consecutive -/+ into change blocks and recurse at finer
	// granularity. Only these blocks descend; "=" runs are already aligned.
	type block struct {
		kind    string // "=" or "!"
		text    string
		oldText string
		newText string
	}

	var blocks []block
	var cur *block

	for _, p := range diff {
		switch p.Type {
		case "=":
			if cur != nil {
				blocks = append(blocks, *cur)
				cur = nil
			}
			blocks = append(blocks, block{kind: "=", text: p.Text})
		case "-":
			if cur == nil {
				cur = &block{kind: "!"}
			}
			cur.oldText += p.Text
		case "+":
			if cur == nil {
				cur = &block{kind: "!"}
			}
			cur.newText += p.Text
		}
	}
	if cur != nil {
		blocks = append(blocks, *cur)
	}

	var result []contracts.ProsePart
	for _, blk := range blocks {
		if blk.kind == "=" {
			result = append(result, contracts.ProsePart{Type: "=", Text: blk.text})
			continue
		}

		switch {
		case blk.oldText == "" && blk.newText == "":
			// Nothing on either side; drop the block rather than emitting an
			// empty segment.
		case blk.oldText == "":
			result = append(result, contracts.ProsePart{Type: "+", Text: blk.newText})
		case blk.newText == "":
			result = append(result, contracts.ProsePart{Type: "-", Text: blk.oldText})
		case tooLarge:
			result = append(result,
				contracts.ProsePart{Type: "-", Text: blk.oldText},
				contracts.ProsePart{Type: "+", Text: blk.newText})
		default:
			result = append(result, buildProseDiff(blk.oldText, blk.newText, level+1)...)
		}
	}

	return reorderParts(result)
}

// The split patterns capture their delimiters so stitchPieces can put them
// back: a piece that dropped its delimiter could not be reassembled into the
// original text, and reassembly is this package's one hard invariant.
var (
	reParagraph = regexp.MustCompile(`(\n+)`)
	reSentence  = regexp.MustCompile(`([\n,!;?.]+)`)
	reWord      = regexp.MustCompile(`(\s+)`)
)

func splitCorpus(corpus string, level int) []string {
	switch level {
	case levelParagraph:
		return stitchPieces(reParagraph.Split(corpus, -1), reParagraph.FindAllString(corpus, -1), level)
	case levelSentence:
		return stitchPieces(reSentence.Split(corpus, -1), reSentence.FindAllString(corpus, -1), level)
	case levelWord:
		return stitchPieces(reWord.Split(corpus, -1), reWord.FindAllString(corpus, -1), level)
	case levelChar:
		return splitChars(corpus)
	}
	return []string{corpus}
}

// splitChars splits on runes, not bytes, so a multi-byte character is never
// torn in half and reassembled into mojibake.
func splitChars(s string) []string {
	var result []string
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		result = append(result, string(r))
		s = s[size:]
	}
	return result
}

func stitchPieces(parts []string, delims []string, level int) []string {
	var results []string
	for i, part := range parts {
		piece := part
		if i < len(delims) {
			piece += delims[i]
		}

		// At the coarse levels the surrounding whitespace is peeled off into
		// pieces of its own, so it can align as unchanged even when the body
		// between it changed. Without this, editing a word drags the spaces
		// around it into the change and the rendered diff highlights padding.
		if level < levelWord {
			results = append(results, trimApart(piece)...)
		} else {
			results = append(results, piece)
		}
	}

	if len(results) > 0 && results[len(results)-1] == "" {
		results = results[:len(results)-1]
	}

	return results
}

// trimApart splits a string into [leading-whitespace, body,
// trailing-whitespace], omitting whichever of the three is empty.
func trimApart(input string) []string {
	if input == "" {
		return nil
	}

	var parts []string
	corpus := strings.TrimLeft(input, " \t\n\r")
	if len(corpus) != len(input) {
		parts = append(parts, input[:len(input)-len(corpus)])
	}

	trimmed := strings.TrimRight(corpus, " \t\n\r")
	if len(trimmed) > 0 {
		parts = append(parts, trimmed)
	}

	if len(trimmed) != len(corpus) {
		parts = append(parts, corpus[len(trimmed):])
	}

	return parts
}

// hashDiff aligns pieces by content, mirroring
// PhutilProseDifferenceEngine::newHashDiff. The match is greedy and forward
// only: a piece may pair with the first still-unclaimed identical piece at or
// after the cursor, never before it. That keeps the alignment monotonic, which
// is what stops two swapped paragraphs from being reported as unchanged.
func hashDiff(uParts, vParts []string) []contracts.ProsePart {
	vIndex := make(map[string][]int, len(vParts))
	for i, p := range vParts {
		vIndex[p] = append(vIndex[p], i)
	}

	vUsed := make([]bool, len(vParts))
	matches := make([]int, len(uParts))
	for i := range matches {
		matches[i] = -1
	}
	cursor := 0
	for i, up := range uParts {
		for _, vi := range vIndex[up] {
			if vi >= cursor && !vUsed[vi] {
				matches[i] = vi
				vUsed[vi] = true
				cursor = vi + 1
				break
			}
		}
	}

	var parts []contracts.ProsePart
	vi := 0
	for ui := 0; ui < len(uParts); ui++ {
		mi := matches[ui]
		if mi < 0 {
			parts = append(parts, contracts.ProsePart{Type: "-", Text: uParts[ui]})
			continue
		}
		// Emit the unmatched new pieces that sit before this match.
		for vi < mi {
			if !vUsed[vi] {
				parts = append(parts, contracts.ProsePart{Type: "+", Text: vParts[vi]})
			}
			vi++
		}
		parts = append(parts, contracts.ProsePart{Type: "=", Text: uParts[ui]})
		vi = mi + 1
	}

	for ; vi < len(vParts); vi++ {
		if !vUsed[vi] {
			parts = append(parts, contracts.ProsePart{Type: "+", Text: vParts[vi]})
		}
	}

	return parts
}

// editDistanceDiff aligns pieces by Levenshtein distance, mirroring
// PhutilProseDifferenceEngine::newEditDistanceMatrixDiff. The second return
// value reports that the inputs exceeded maxEditDistance and the result is the
// degraded delete-all/insert-all form; the caller uses it to stop recursing.
func editDistanceDiff(uParts, vParts []string, level int) ([]contracts.ProsePart, bool) {
	n := len(uParts)
	m := len(vParts)

	if n > maxEditDistance || m > maxEditDistance {
		var parts []contracts.ProsePart
		for _, p := range uParts {
			parts = append(parts, contracts.ProsePart{Type: "-", Text: p})
		}
		for _, p := range vParts {
			parts = append(parts, contracts.ProsePart{Type: "+", Text: p})
		}
		return parts, true
	}

	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
		dp[i][0] = i
	}
	for j := 0; j <= m; j++ {
		dp[0][j] = j
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if uParts[i-1] == vParts[j-1] {
				dp[i][j] = dp[i-1][j-1]
			} else {
				dp[i][j] = min3(dp[i-1][j-1]+1, dp[i-1][j]+1, dp[i][j-1]+1)
			}
		}
	}

	// Backtrack into an edit string. 'x' (substitute) is kept distinct from a
	// 'd' followed by an 'i' because smooth() below only has something to work
	// with if it can tell a paired replacement from an independent pair.
	var ops []byte
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && uParts[i-1] == vParts[j-1]:
			ops = append(ops, 's')
			i--
			j--
		case i > 0 && j > 0 && dp[i][j] == dp[i-1][j-1]+1:
			ops = append(ops, 'x')
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] <= dp[i-1][j]):
			ops = append(ops, 'i')
			j--
		default:
			ops = append(ops, 'd')
			i--
		}
	}

	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	// Sentences are large enough that an unchanged one between two changed
	// ones is worth reporting as unchanged. Words and characters are not: a
	// lone shared letter between two edits is noise, so it is folded into the
	// surrounding change.
	if level > levelSentence {
		ops = smooth(ops)
	}

	var parts []contracts.ProsePart
	ui, vi := 0, 0
	for _, c := range ops {
		switch c {
		case 's':
			parts = append(parts, contracts.ProsePart{Type: "=", Text: uParts[ui]})
			ui++
			vi++
		case 'd':
			parts = append(parts, contracts.ProsePart{Type: "-", Text: uParts[ui]})
			ui++
		case 'i':
			parts = append(parts, contracts.ProsePart{Type: "+", Text: vParts[vi]})
			vi++
		case 'x':
			parts = append(parts,
				contracts.ProsePart{Type: "-", Text: uParts[ui]},
				contracts.ProsePart{Type: "+", Text: vParts[vi]})
			ui++
			vi++
		}
	}

	return parts, false
}

// smooth rewrites an 's' that has a change on both sides into an 'x', so the
// run reads as one replacement instead of three fragments.
func smooth(ops []byte) []byte {
	result := make([]byte, len(ops))
	copy(result, ops)

	for i := range result {
		if result[i] != 's' {
			continue
		}
		prevChange := i > 0 && result[i-1] != 's'
		nextChange := i < len(result)-1 && result[i+1] != 's'
		if prevChange && nextChange {
			result[i] = 'x'
		}
	}
	return result
}

func min3(a, b, c int) int {
	if a <= b && a <= c {
		return a
	}
	if b <= c {
		return b
	}
	return c
}

// reorderParts normalises a segment list, mirroring
// PhutilProseDiff::reorderParts: within each run of changes every deletion
// comes before every insertion, and adjacent segments of one type merge. The
// ordering matters because interleaved -/+ segments read as an alternation
// that was never in either text.
func reorderParts(parts []contracts.ProsePart) []contracts.ProsePart {
	var oldRun, newRun, result []contracts.ProsePart

	flush := func() {
		if len(oldRun) > 0 || len(newRun) > 0 {
			result = append(result, combineRuns(oldRun, newRun)...)
			oldRun = nil
			newRun = nil
		}
	}

	for _, p := range parts {
		switch p.Type {
		case "-":
			oldRun = append(oldRun, p)
		case "+":
			newRun = append(newRun, p)
		default:
			flush()
			result = append(result, p)
		}
	}
	flush()

	var combined []contracts.ProsePart
	for _, p := range result {
		if len(combined) > 0 && combined[len(combined)-1].Type == p.Type {
			combined[len(combined)-1].Text += p.Text
		} else {
			combined = append(combined, p)
		}
	}

	return combined
}

// layoutChars are the characters combineRuns is willing to pull out of a
// change as unchanged: whitespace, sentence punctuation and brackets.
var layoutChars = [256]bool{
	' ': true, '\n': true, '.': true, '!': true, ',': true,
	'?': true, ']': true, '[': true, '(': true, ')': true,
	'<': true, '>': true,
}

// combineRuns merges a deletion run with an insertion run, lifting any shared
// prefix and suffix of layout characters out as unchanged segments.
//
// Only layout characters qualify, and that restriction is the point. Lifting
// an arbitrary common prefix would claim that the shared part survived the
// edit: "abcX" -> "abcY" would render as an unchanged "abc" plus a one-letter
// change, when the honest reading is that the whole token was replaced. A
// space or a comma carries no such meaning, so leaving it unhighlighted only
// removes noise.
func combineRuns(oldRun, newRun []contracts.ProsePart) []contracts.ProsePart {
	oldText := mergeRunText(oldRun)
	newText := mergeRunText(newRun)

	oldLen := len(oldText)
	newLen := len(newText)
	minLen := min(oldLen, newLen)

	prefixLen := 0
	for i := 0; i < minLen; i++ {
		if oldText[i] != newText[i] || !layoutChars[oldText[i]] {
			break
		}
		prefixLen++
	}

	suffixLen := 0
	for i := 0; i < minLen-prefixLen; i++ {
		o := oldText[oldLen-1-i]
		if o != newText[newLen-1-i] || !layoutChars[o] {
			break
		}
		suffixLen++
	}

	var result []contracts.ProsePart

	if prefixLen > 0 {
		result = append(result, contracts.ProsePart{Type: "=", Text: oldText[:prefixLen]})
	}
	if prefixLen < oldLen {
		result = append(result, contracts.ProsePart{Type: "-", Text: oldText[prefixLen : oldLen-suffixLen]})
	}
	if prefixLen < newLen {
		result = append(result, contracts.ProsePart{Type: "+", Text: newText[prefixLen : newLen-suffixLen]})
	}
	if suffixLen > 0 {
		result = append(result, contracts.ProsePart{Type: "=", Text: oldText[oldLen-suffixLen:]})
	}

	return result
}

func mergeRunText(parts []contracts.ProsePart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
