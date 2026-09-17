package patchedit

import (
	"fmt"
	"strings"
	"testing"
)

// snippet returns a bounded view of a diff for failure messages.
func snippet(diff string) string {
	return diff[:min(len(diff), 400)]
}

// ops must describe both sides exactly, except where a stretch is summarised.
func reconstructingSides(t *testing.T, oldContent, newContent []byte) {
	t.Helper()

	oldLines := strings.Split(string(oldContent), "\n")
	newLines := strings.Split(string(newContent), "\n")

	oldLines = oldLines[:len(oldLines)-1]
	newLines = newLines[:len(newLines)-1]

	var ops []diffOp
	if int64(len(oldLines))*int64(len(newLines)) > maxDiffCells {
		ops = splitDiffOps(oldLines, newLines)
	} else {
		ops = buildDiffOps(oldLines, newLines)
	}

	var gotOld, gotNew []string

	for _, op := range ops {
		switch op.kind {
		case ' ':
			gotOld = append(gotOld, op.text)
			gotNew = append(gotNew, op.text)
		case '-':
			gotOld = append(gotOld, op.text)
		case '+':
			gotNew = append(gotNew, op.text)
		}
	}

	if strings.Join(gotOld, "\n") != strings.Join(oldLines, "\n") {
		t.Errorf("old side not reconstructed: got %d lines, want %d", len(gotOld), len(oldLines))
	}

	if strings.Join(gotNew, "\n") != strings.Join(newLines, "\n") {
		t.Errorf("new side not reconstructed: got %d lines, want %d", len(gotNew), len(newLines))
	}
}

func TestDiffOpsReconstructBothSides(t *testing.T) {
	big := func(n int, prefix string) []string {
		lines := make([]string, n)
		for i := range lines {
			lines[i] = fmt.Sprintf("%s line %d", prefix, i)
		}

		return lines
	}

	scattered := big(3000, "stable")

	scatteredNew := make([]string, len(scattered))
	copy(scatteredNew, scattered)

	for i := 0; i < len(scatteredNew); i += 7 {
		scatteredNew[i] = fmt.Sprintf("REWRITTEN %d", i)
	}

	cases := map[string][2][]string{
		"scattered rewrite above cap": {scattered, scatteredNew},
		"insertions and deletions": {
			[]string{"a", "b", "c", "d", "e", "f", "g"},
			[]string{"a", "c", "c2", "d", "g", "h"},
		},
		"empty to content": {[]string{""}, []string{"x", "y"}},
		"content to empty": {[]string{"x", "y"}, []string{""}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reconstructingSides(t, []byte(strings.Join(tc[0], "\n")+"\n"), []byte(strings.Join(tc[1], "\n")+"\n"))
		})
	}
}

// A scattered rewrite must still produce a real diff, not a summary.
func TestUnifiedDiffScatteredLargeRegion(t *testing.T) {
	scattered := make([]string, 3000)
	newLines := make([]string, 3000)

	for i := range scattered {
		scattered[i] = fmt.Sprintf("line %d stable", i)
		newLines[i] = scattered[i]

		if i%7 == 0 {
			newLines[i] = fmt.Sprintf("line %d CHANGED", i)
		}
	}

	diff := unifiedDiff([]byte(strings.Join(scattered, "\n")), []byte(strings.Join(newLines, "\n")), "pkg/f.go", 3)

	if strings.Contains(diff, "more removed lines") {
		t.Errorf("scattered changes should not be summarised: %s", snippet(diff))
	}

	if !strings.Contains(diff, "-line 14 stable") || !strings.Contains(diff, "+line 14 CHANGED") {
		t.Errorf("expected a real hunk for the changed line, got: %s", snippet(diff))
	}
}

// A full rewrite has no line to synchronise on, so it is summarised instead of
// allocating an O(n²) matrix.
func TestUnifiedDiffSummarisesUnsyncableRegion(t *testing.T) {
	oldLines := make([]string, 0, 2000)
	newLines := make([]string, 0, 2000)

	for i := range 2000 {
		oldLines = append(oldLines, fmt.Sprintf("old line %d", i))
		newLines = append(newLines, fmt.Sprintf("new line %d", i))
	}

	oldContent := []byte(strings.Join(oldLines, "\n") + "\n")
	newContent := []byte(strings.Join(newLines, "\n") + "\n")

	diff := unifiedDiff(oldContent, newContent, "pkg/f.go", 3)

	if !strings.Contains(diff, "more removed lines") || !strings.Contains(diff, "more added lines") {
		t.Errorf("expected a summary for an unsyncable hunk: %s", snippet(diff))
	}

	if len(diff) > 4096 {
		t.Errorf("summary should stay small, got %d bytes", len(diff))
	}
}

func TestUnifiedDiffNoChange(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	newContent := []byte("line1\nline2\nline3\n")

	result := unifiedDiff(old, newContent, "test.txt", 0)
	if !strings.Contains(result, "(no changes)") {
		t.Errorf("expected no changes, got: %s", result)
	}
}

func TestUnifiedDiffSingleLine(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	newContent := []byte("line1\nline2_changed\nline3\n")

	result := unifiedDiff(old, newContent, "test.txt", 0)
	if strings.Contains(result, "(no changes)") {
		t.Error("expected changes")
	}

	if !strings.Contains(result, "line2_changed") {
		t.Errorf("missing changed line: %s", result)
	}
}

func TestUnifiedDiffAddition(t *testing.T) {
	old := []byte("line1\nline2\n")
	newContent := []byte("line1\nline2\nline3\n")

	result := unifiedDiff(old, newContent, "test.txt", 0)
	if !strings.Contains(result, "line3") {
		t.Errorf("missing added line: %s", result)
	}
}

func TestUnifiedDiffDeletion(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	newContent := []byte("line1\nline3\n")

	result := unifiedDiff(old, newContent, "test.txt", 0)
	if !strings.Contains(result, "line2") {
		t.Errorf("missing deleted line: %s", result)
	}
}
