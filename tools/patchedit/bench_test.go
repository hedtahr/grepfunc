package patchedit

import (
	"fmt"
	"strings"
	"testing"
)

// diffFixture returns original content plus a version with `changed` scattered
// lines rewritten, so prefix/suffix narrowing cannot help.
func diffFixture(total, changed int) ([]byte, []byte) {
	var oldBuf, newBuf strings.Builder

	for i := range total {
		line := fmt.Sprintf("line %d: some stable content here\n", i)

		oldBuf.WriteString(line)

		if i%max(1, total/changed) == 0 {
			fmt.Fprintf(&newBuf, "line %d: REWRITTEN content here\n", i)
		} else {
			newBuf.WriteString(line)
		}
	}

	return []byte(oldBuf.String()), []byte(newBuf.String())
}

func BenchmarkUnifiedDiff(b *testing.B) {
	oldContent, newContent := diffFixture(2000, 200)

	b.ReportAllocs()

	for b.Loop() {
		if unifiedDiff(oldContent, newContent, "pkg/f.go", 3) == "" {
			b.Fatal("empty diff")
		}
	}
}

// Pathological shape: a whole-file rewrite, i.e. the region the LCS matrix sees.
func BenchmarkUnifiedDiffLargeRegion(b *testing.B) {
	oldContent, newContent := diffFixture(3000, 3000)

	b.ReportAllocs()

	for b.Loop() {
		if unifiedDiff(oldContent, newContent, "pkg/f.go", 3) == "" {
			b.Fatal("empty diff")
		}
	}
}
