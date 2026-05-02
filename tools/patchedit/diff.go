package patchedit

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

func unifiedDiff(old, new []byte, path string, diffCtx int) string {
	if bytes.Equal(old, new) {
		return "(no changes)\n"
	}

	if diffCtx < 0 {
		diffCtx = 0
	}
	ctx := diffCtx
	if ctx == 0 {
		ctx = 3 // default
	}

	oldLines := strings.Split(string(old), "\n")
	newLines := strings.Split(string(new), "\n")

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

	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		for j := start; j < i; j++ {
			if ops[j].kind == ' ' {
				buf.WriteString(" " + ops[j].text + "\n")
			}
		}
		sameCount := 0
		hunkEnd := i
		for hunkEnd < len(ops) {
			if ops[hunkEnd].kind == ' ' {
				sameCount++
				if sameCount > ctx {
					sameCount--
					break
				}
			} else {
				sameCount = 0
			}
			hunkEnd++
		}
		for j := i; j < hunkEnd; j++ {
			buf.WriteByte(ops[j].kind)
			buf.WriteString(ops[j].text + "\n")
		}
		i = hunkEnd
	}
	return buf.String()
}

func commonPrefixLines(a, b []string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func commonSuffixLines(a, b []string) int {
	la, lb := len(a), len(b)
	n := min(la, lb)
	for i := 0; i < n; i++ {
		if a[la-1-i] != b[lb-1-i] {
			return i
		}
	}
	return n
}

type diffOp struct {
	kind byte
	text string
}

func buildDiffOps(a, b []string) []diffOp {
	m, n := len(a), len(b)
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else {
				lcs[i][j] = max(lcs[i-1][j], lcs[i][j-1])
			}
		}
	}

	var ops []diffOp
	i, j := m, n
	var rev []diffOp
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && a[i-1] == b[j-1] {
			rev = append(rev, diffOp{' ', a[i-1]})
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			rev = append(rev, diffOp{'+', b[j-1]})
			j--
		} else {
			rev = append(rev, diffOp{'-', a[i-1]})
			i--
		}
	}
	for k := len(rev) - 1; k >= 0; k-- {
		ops = append(ops, rev[k])
	}
	return ops
}
