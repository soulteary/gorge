package unified

import (
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracts"
	"strings"
	"testing"
)

func singleLineChange(n int) (string, string) {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%d", i)
	}
	old := strings.Join(lines, "\n") + "\n"
	lines[n/2] = "changed"
	return old, strings.Join(lines, "\n") + "\n"
}
func TestLargeSingleLineChangePreservesFullContext(t *testing.T) {
	for _, n := range []int{2001, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			old, newText := singleLineChange(n)
			result, err := Generate(&contracts.DiffRequest{Old: old, New: newText})
			if err != nil {
				t.Fatal(err)
			}
			header := fmt.Sprintf("@@ -1,%d +1,%d @@\n", n, n)
			if !strings.Contains(result.Diff, header) || !strings.Contains(result.Diff, "-line-"+fmt.Sprint(n/2)+"\n+changed\n") {
				t.Fatal("large comparison lost hunk or replacement")
			}
			if got := strings.Count(result.Diff, "\n"); got != n+4 {
				t.Fatalf("context lost: newline count %d", got)
			}
		})
	}
}
func BenchmarkLargeSingleLineChange(b *testing.B) {
	for _, n := range []int{2001, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			old, newText := singleLineChange(n)
			req := &contracts.DiffRequest{Old: old, New: newText}
			b.ReportAllocs()
			b.SetBytes(int64(len(old) + len(newText)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := Generate(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
func FuzzBoundedEditsPreserveBothInputs(f *testing.F) {
	f.Add("a\nb\n", "a\nx\nb\n")
	f.Add("x\nsame", "y\nsame")
	f.Add("a\nb", "a\nb\n")
	f.Fuzz(func(t *testing.T, old, newText string) {
		if len(old)+len(newText) > 4096 {
			t.Skip()
		}
		a, b := splitLines(old), splitLines(newText)
		ops, err := boundedEdits(a, b)
		if err != nil {
			return
		}
		oi, ni := 0, 0
		edits := 0
		for _, op := range ops {
			switch op {
			case opEqual:
				if oi >= len(a) || ni >= len(b) || a[oi] != b[ni] {
					t.Fatal("equal operation changed content or newline")
				}
				oi++
				ni++
			case opDelete:
				oi++
				edits++
			case opInsert:
				ni++
				edits++
			default:
				t.Fatal("invalid operation")
			}
			if oi > len(a) || ni > len(b) {
				t.Fatal("script consumes beyond input")
			}
		}
		if oi != len(a) || ni != len(b) {
			t.Fatal("script loses input lines")
		}
		if len(a)*len(b) <= 10000 {
			want := 0
			for _, op := range lcs(a, b) {
				if op != opEqual {
					want++
				}
			}
			if edits != want {
				t.Fatalf("trim lost minimality: %d != %d", edits, want)
			}
		}
	})
}
