// Package unified generates unified diffs with full context.
//
// The output is a hard contract: Phorge feeds it to ArcanistDiffParser and
// DifferentialHunkParser, which reach it through
// PhabricatorDifferenceEngine::generateRawDiffFromFileContent. That method
// shells out to `diff -U65535 -L <name> -L <name>`, so this package has to
// reproduce what GNU (and Apple/FreeBSD) diff writes, byte for byte, rather
// than merely produce a valid unified diff. A divergence does not raise an
// error anywhere: the parser accepts a wrong hunk header and silently
// misplaces every line after it.
package unified

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// defaultName is the placeholder Phorge passes for content with no path.
const defaultName = "/dev/universe"

// nameSuffix is appended to both -L labels. PHP hands diff a fixed sentinel
// instead of a real mtime so the output is reproducible; the parser ignores it
// but its presence and shape are part of the format.
const nameSuffix = " 9999-99-99"

// noNewlineMarker is the line diff writes for a side whose last line is not
// newline-terminated.
const noNewlineMarker = `\ No newline at end of file`

// maxCells caps the size of the dynamic-programming table lcs allocates.
//
// The bound is on the product n*m rather than on either side's line count,
// because that product is what gets allocated: 100 lines against 100000 is
// cheap and must not be rejected, while 10000 against 10000 would ask for
// 800MB. At 4 million cells the table is 32MB of int on a 64-bit build, which
// is what a single request is allowed to spend.
//
// GNU diff has no such ceiling because it runs Myers' O(ND) algorithm rather
// than a full LCS table. Replacing the algorithm is the real fix; until then
// this is a guard, not a tuning knob.
const maxCells = 4_000_000

// ErrTooLarge reports that the two inputs have too many lines between them to
// diff within maxCells. Callers should surface it as 413, not 500: it
// describes the request, not a failure of the service.
var ErrTooLarge = errors.New("too many lines to diff")

// line is one input line together with whether the input terminated it.
//
// hasNewline is not presentation detail, and that is the whole reason this
// type exists instead of a plain string. GNU diff considers an unterminated
// line different from the same text terminated, so comparing "a\nb" with
// "a\nb\nc\n" reports -b and +b rather than keeping b as shared context. Both
// fields therefore take part in equality, and both drive the marker below.
type line struct {
	text       string
	hasNewline bool
}

// Generate produces the unified diff between the request's two sides.
func Generate(req *contracts.DiffRequest) (*contracts.DiffResult, error) {
	oldName := req.OldName
	if oldName == "" {
		oldName = defaultName
	}
	newName := req.NewName
	if newName == "" {
		newName = defaultName
	}

	oldText := req.Old
	newText := req.New

	if req.Normalize {
		oldText = normalizeText(oldText)
		newText = normalizeText(newText)
	}

	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	if linesEqual(oldLines, newLines) {
		// buildIdenticalDiff works from the text, not the lines, because PHP
		// does. See its comment.
		return &contracts.DiffResult{
			Diff:  buildIdenticalDiff(oldName, newName, oldText),
			Equal: true,
		}, nil
	}

	if n, m := len(oldLines), len(newLines); n > 0 && m > 0 && n > maxCells/m {
		// Division rather than multiplication: n*m is what overflows.
		return nil, ErrTooLarge
	}

	ops := lcs(oldLines, newLines)

	return &contracts.DiffResult{
		Diff:  formatUnified(oldName, newName, oldLines, newLines, ops),
		Equal: false,
	}, nil
}

// normalizeText mirrors PhabricatorDifferenceEngine::normalizeFile, which
// strips every space and tab anywhere in the content: humans do not read a
// whitespace-only change as a different line, even where it is semantic.
func normalizeText(s string) string {
	return strings.NewReplacer(" ", "", "\t", "").Replace(s)
}

// splitLines breaks text into lines, recording for each whether a newline
// terminated it. Only the last line of an unterminated input can carry
// hasNewline == false, so "a\nb\n" yields two terminated lines while "a\nb"
// yields one terminated and one not.
func splitLines(s string) []line {
	var lines []line
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return append(lines, line{text: s})
		}
		lines = append(lines, line{text: s[:i], hasNewline: true})
		s = s[i+1:]
	}
	return lines
}

func linesEqual(a, b []line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// buildIdenticalDiff reproduces the changeless diff PHP synthesises itself,
// and is deliberately *not* aligned with GNU diff. This asymmetry is the one
// place in this package where GNU is not the reference.
//
// generateRawDiffFromFileContent only trusts diff's output when the files
// differ. On exit status 0 it builds a string of its own, so that callers can
// still render the unchanged file rather than being told only that nothing
// changed. That synthetic string is what ArcanistDiffParser has always
// received for this case, so its literal behaviour is the contract, quirks
// included:
//
//   - it splits on "\n" without dropping the empty trailing element, so
//     "a\nb\n" is three lines and the last one renders as a single space
//   - the counts are hardcoded to "-1,{len} +1,{len}", keeping the ",1" that
//     GNU would omit for a single line
//   - empty input still splits to one element, so it yields a one-line hunk
//     rather than a header-only diff
func buildIdenticalDiff(oldName, newName, text string) string {
	parts := strings.Split(text, "\n")

	var b strings.Builder
	writeFileHeader(&b, oldName, newName)
	fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", len(parts), len(parts))
	for _, part := range parts {
		b.WriteByte(' ')
		b.WriteString(part)
		b.WriteByte('\n')
	}

	return b.String()
}

// editOp represents an operation in the edit script.
type editOp byte

const (
	opEqual  editOp = '='
	opDelete editOp = '-'
	opInsert editOp = '+'
)

// lcs computes a diff using an LCS-based (longest common subsequence) DP
// approach. This is O(n*m) but simple and correct; callers must apply the
// maxCells guard before calling it.
func lcs(a, b []line) []editOp {
	n := len(a)
	m := len(b)

	if n == 0 {
		ops := make([]editOp, m)
		for i := range ops {
			ops[i] = opInsert
		}
		return ops
	}
	if m == 0 {
		ops := make([]editOp, n)
		for i := range ops {
			ops[i] = opDelete
		}
		return ops
	}

	// DP table
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to produce edit operations. Preferring insert over delete on a
	// tie is what makes a replaced line come out as -old before +new, the
	// order GNU emits and the order the hunk parser expects.
	var ops []editOp
	i, j := n, m
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && a[i-1] == b[j-1] {
			ops = append(ops, opEqual)
			i--
			j--
		} else if j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]) {
			ops = append(ops, opInsert)
			j--
		} else {
			ops = append(ops, opDelete)
			i--
		}
	}

	// Reverse
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	return ops
}

// formatRange renders one side of a hunk header. Every hunk starts at line 1
// because -U65535 asks for the whole file as context, so the count is all that
// varies — in three shapes, none of them cosmetic. ArcanistDiffParser reads
// these numbers to place the lines that follow, so writing "-1,1" where diff
// writes "-1" is enough to shift the whole hunk.
func formatRange(count int) string {
	switch count {
	case 0:
		// An empty side starts at 0, not 1: there is no line 1 to point at.
		return "0,0"
	case 1:
		// A single line omits the count entirely.
		return "1"
	default:
		return "1," + strconv.Itoa(count)
	}
}

func formatUnified(oldName, newName string, oldLines, newLines []line, ops []editOp) string {
	var b strings.Builder
	writeFileHeader(&b, oldName, newName)

	// The header carries both totals and comes first, so the ops are counted
	// before any of them is rendered.
	oldCount, newCount := 0, 0
	for _, op := range ops {
		switch op {
		case opEqual:
			oldCount++
			newCount++
		case opDelete:
			oldCount++
		case opInsert:
			newCount++
		}
	}
	fmt.Fprintf(&b, "@@ -%s +%s @@\n", formatRange(oldCount), formatRange(newCount))

	oi, ni := 0, 0
	for _, op := range ops {
		var sigil byte
		var l line
		switch op {
		case opEqual:
			sigil, l = ' ', oldLines[oi]
			oi++
			ni++
		case opDelete:
			sigil, l = '-', oldLines[oi]
			oi++
		case opInsert:
			sigil, l = '+', newLines[ni]
			ni++
		}

		b.WriteByte(sigil)
		b.WriteString(l.text)
		b.WriteByte('\n')

		// The marker follows whichever record carried the unterminated line,
		// which is what makes the three observable cases fall out of one rule:
		// a context line missing its newline on both sides produces the marker
		// once, a -/+ pair where both sides lack it produces it twice, and a
		// pair where only one side lacks it produces it in that side's
		// position. Note that every diff line, the marker included, is
		// newline-terminated — the diff itself never lacks a trailing newline.
		if !l.hasNewline {
			b.WriteString(noNewlineMarker)
			b.WriteByte('\n')
		}
	}

	return b.String()
}

func writeFileHeader(b *strings.Builder, oldName, newName string) {
	fmt.Fprintf(b, "--- %s%s\n", oldName, nameSuffix)
	fmt.Fprintf(b, "+++ %s%s\n", newName, nameSuffix)
}
