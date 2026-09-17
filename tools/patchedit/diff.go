package patchedit

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const defaultDiffCtx = 3

// LCS cells are O(rows×cols) ints, so a large rewrite would allocate gigabytes.
// Regions above the cap are split on identical lines and only oversized stretches
// are summarised.
const (
	maxDiffCells       = 1 << 20
	maxDiffSummaryRows = 5
	syncWindow         = 256
)

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

	var ops []diffOp

	if int64(len(oldLines))*int64(len(newLines)) > maxDiffCells {
		ops = splitDiffOps(oldLines, newLines)
	} else {
		ops = buildDiffOps(oldLines, newLines)
	}

	writeDiffHunks(&buf, ops, ctx)

	return buf.String()
}

// splitDiffOps diffs a large region by synchronising on identical lines, so each
// stretch handed to the quadratic DP is small. A stretch with no identical line
// within syncWindow is summarised: describing it exactly would mean the whole
// region, which is the allocation this exists to avoid.
func splitDiffOps(oldLines, newLines []string) []diffOp {
	var ops []diffOp

	i, j := 0, 0
	for i < len(oldLines) || j < len(newLines) {
		if i < len(oldLines) && j < len(newLines) && oldLines[i] == newLines[j] {
			ops = append(ops, diffOp{' ', oldLines[i]})
			i++
			j++

			continue
		}

		di, dj, found := findSync(oldLines, newLines, i, j)
		if !found {
			return append(ops, changeOps(oldLines[i:], newLines[j:])...)
		}

		ops = append(ops, changeOps(oldLines[i:i+di], newLines[j:j+dj])...)
		i += di
		j += dj
	}

	return ops
}

// findSync returns the offsets of the nearest pair of identical lines within
// syncWindow, scanning forward in the old lines first.
func findSync(oldLines, newLines []string, i, j int) (int, int, bool) {
	limitOld := min(len(oldLines), i+syncWindow)
	limitNew := min(len(newLines), j+syncWindow)

	for di := 0; i+di < limitOld; di++ {
		for dj := 0; j+dj < limitNew; dj++ {
			if oldLines[i+di] == newLines[j+dj] {
				return di, dj, true
			}
		}
	}

	return 0, 0, false
}

// changeOps diffs one stretch, or summarises it when the DP would be too large.
func changeOps(oldLines, newLines []string) []diffOp {
	if int64(len(oldLines))*int64(len(newLines)) <= maxDiffCells {
		return buildDiffOps(oldLines, newLines)
	}

	ops := make([]diffOp, 0, maxDiffSummaryRows*2+2)

	for i, line := range oldLines {
		if i == maxDiffSummaryRows {
			ops = append(ops, diffOp{'~', fmt.Sprintf("(%d more removed lines)", len(oldLines)-i)})

			break
		}

		ops = append(ops, diffOp{'-', line})
	}

	for i, line := range newLines {
		if i == maxDiffSummaryRows {
			ops = append(ops, diffOp{'~', fmt.Sprintf("(%d more added lines)", len(newLines)-i)})

			break
		}

		ops = append(ops, diffOp{'+', line})
	}

	return ops
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
				buf.WriteByte(' ')
				buf.WriteString(ops[j].text)
				buf.WriteByte('\n')
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
			buf.WriteString(ops[j].text)
			buf.WriteByte('\n')
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
