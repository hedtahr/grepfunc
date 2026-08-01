package patchedit

import (
	"strings"
	"testing"
)

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
