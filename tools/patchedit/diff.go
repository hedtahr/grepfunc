package patchedit

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const defaultDiffCtx = 3

func unifiedDiff(old, newContent []byte, path string, diffCtx int) string {
	if bytes.Equal(old, newContent) {
		return "(no changes)\n"
	}

	if diffCtx < 0 {
		diffCtx = 0
	}

	ctx := diffCtx
	if ctx == 0 {
		ctx = defaultDiffCtx
	}

	oldLines := strings.Split(string(old), "\n")
	newLines := strings.Split(string(newContent), "\n")

	// Strip leading slash: prevents "--- a//absolute/path"
	cleanPath := strings.TrimLeft(filepath.ToSlash(path), "/")

	var buf bytes.Buffer

	fmt.Fprintf(&buf, "--- a/%s\n+++ b/%s\n", cleanPath, cleanPath)

	// Narrow to dirty region + context before LCS → O(k²) not O(n²)
	pref := commonPrefixLines(oldLines, newLines)
	suf := commonSuffixLines(oldLines[pref:], newLines[pref:])
	lastOld := len(oldLines) - suf
	lastNew := len(newLines) - suf
	lo := max(pref-ctx, 0)
	hiOld := min(lastOld+ctx, len(oldLines))
	hiNew := min(lastNew+ctx, len(newLines))
	oldLines = oldLines[lo:hiOld]
	newLines = newLines[lo:hiNew]

	ops := buildDiffOps(oldLines, newLines)

	writeDiffHunks(&buf, ops, ctx)

	return buf.String()
}

func writeDiffHunks(buf *bytes.Buffer, ops []diffOp, ctx int) {
	idx := 0
	for idx < len(ops) {
		if ops[idx].kind == ' ' {
			idx++

			continue
		}

		start := max(idx-ctx, 0)

		for j := start; j < idx; j++ {
			if ops[j].kind == ' ' {
				buf.WriteString(" " + ops[j].text + "\n")
			}
		}

		sameCount := 0

		hunkEnd := idx
		for hunkEnd < len(ops) {
			if ops[hunkEnd].kind == ' ' {
				sameCount++
				if sameCount > ctx {
					break
				}
			} else {
				sameCount = 0
			}

			hunkEnd++
		}

		for j := idx; j < hunkEnd; j++ {
			buf.WriteByte(ops[j].kind)
			buf.WriteString(ops[j].text + "\n")
		}

		idx = hunkEnd
	}
}

func commonPrefixLines(a, b []string) int {
	commonLen := min(len(a), len(b))
	for i := range commonLen {
		if a[i] != b[i] {
			return i
		}
	}

	return commonLen
}

func commonSuffixLines(a, b []string) int {
	la, lb := len(a), len(b)

	commonLen := min(la, lb)
	for i := range commonLen {
		if a[la-1-i] != b[lb-1-i] {
			return i
		}
	}

	return commonLen
}

type diffOp struct {
	kind byte
	text string
}

func buildDiffOps(oldLines, newLines []string) []diffOp {
	lcs := buildLCS(oldLines, newLines)

	row, col := len(oldLines), len(newLines)

	var rev []diffOp

	for row > 0 || col > 0 {
		switch {
		case row > 0 && col > 0 && oldLines[row-1] == newLines[col-1]:
			rev = append(rev, diffOp{' ', oldLines[row-1]})
			row--
			col--

		case col > 0 && (row == 0 || lcs[row][col-1] >= lcs[row-1][col]):
			rev = append(rev, diffOp{'+', newLines[col-1]})
			col--

		default:
			rev = append(rev, diffOp{'-', oldLines[row-1]})
			row--
		}
	}

	slices.Reverse(rev)

	return rev
}

func buildLCS(oldLines, newLines []string) [][]int {
	rows, cols := len(oldLines), len(newLines)

	lcs := make([][]int, rows+1)
	for i := range lcs {
		lcs[i] = make([]int, cols+1)
	}

	for row := 1; row <= rows; row++ {
		for col := 1; col <= cols; col++ {
			if oldLines[row-1] == newLines[col-1] {
				lcs[row][col] = lcs[row-1][col-1] + 1
			} else {
				lcs[row][col] = max(lcs[row-1][col], lcs[row][col-1])
			}
		}
	}

	return lcs
}
