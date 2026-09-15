package patchedit

import (
	"strings"
	"testing"
)

const contentAlpha = "alpha"

func TestFindAndReplaceExact(t *testing.T) {
	content := []byte("\tcounter int64\n\titems   map[string]*Item\n")

	result := findAndReplace(content, EditOp{
		OldText:    "\tcounter int64",
		NewText:    "\tcounter uint64",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if !result.Success {
		t.Fatalf("expected success, got: %s", result.Error)
	}

	if result.Matches[0].Strategy != strategyExact {
		t.Errorf("strategy = %s, want exact", result.Matches[0].Strategy)
	}
}

func TestFindAndReplaceNoMatch(t *testing.T) {
	content := []byte("package main\nfunc main() {}\n")

	result := findAndReplace(content, EditOp{
		OldText:    "THIS_DOES_NOT_EXIST_12345",
		NewText:    "// comment",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if result.Success {
		t.Fatal("expected failure")
	}

	if !strings.Contains(result.Error, "NO_MATCH") {
		t.Errorf("error does not say NO_MATCH: %s", result.Error)
	}

	if !strings.Contains(result.Error, "Nearest:") {
		t.Errorf("error does not show nearest match: %s", result.Error)
	}
}

func TestFindAndReplaceAmbiguous(t *testing.T) {
	content := []byte("return nil, false\n// code\nreturn nil, false\n// more\nreturn nil, false\n")

	result := findAndReplace(content, EditOp{
		OldText:    "return nil, false",
		NewText:    "return nil, true",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if result.Success {
		t.Fatal("expected failure for ambiguous match")
	}

	if !strings.Contains(result.Error, "AMBIGUOUS_MATCH") {
		t.Errorf("error does not say AMBIGUOUS_MATCH: %s", result.Error)
	}

	if len(result.Matches) != 3 {
		t.Errorf("expected 3 matches, got %d", len(result.Matches))
	}
}

func TestFindAndReplaceEmptyOldText(t *testing.T) {
	content := []byte("anything\n")

	result := findAndReplace(content, EditOp{
		OldText:    "",
		NewText:    "something",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if result.Success {
		t.Fatal("expected failure for empty old_text")
	}
}

func TestApplyReplacement(t *testing.T) {
	content := []byte("\tcounter int64\n\titems map[string]*Item\n")

	result := findAndReplace(content, EditOp{
		OldText:    "counter int64",
		NewText:    "counter uint64",
		Index:      0,
		ReplaceAll: false,
	}, 0)
	if !result.Success {
		t.Fatalf("match failed: %s", result.Error)
	}

	resultStr := string(applyReplacement(content, result))

	if !strings.Contains(resultStr, "counter uint64") {
		t.Errorf("replacement not applied: %s", resultStr)
	}

	if strings.Contains(resultStr, "counter int64") {
		t.Errorf("old text still present: %s", resultStr)
	}
}

func TestApplyReplacementPreservesSurrounding(t *testing.T) {
	content := []byte("lineA\nlineB\nlineC\n")

	result := findAndReplace(content, EditOp{
		OldText:    "lineB",
		NewText:    "lineB_changed",
		Index:      0,
		ReplaceAll: false,
	}, 0)
	if !result.Success {
		t.Fatalf("match failed: %s", result.Error)
	}

	resultStr := string(applyReplacement(content, result))

	if !strings.Contains(resultStr, "lineA") {
		t.Error("lineA missing")
	}

	if !strings.Contains(resultStr, "lineC") {
		t.Error("lineC missing")
	}

	if !strings.Contains(resultStr, "lineB_changed") {
		t.Error("change not applied")
	}

	if strings.Count(resultStr, "\n") != strings.Count(string(content), "\n") {
		t.Error("line count changed unexpectedly")
	}
}

func TestApplyReplacementUnicode(t *testing.T) {
	content := []byte("// 🚀 constructor\nfunc New() {}\n")

	result := findAndReplace(content, EditOp{
		OldText:    "// 🚀 constructor",
		NewText:    "// 🚀 constructor v2",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if !result.Success {
		t.Fatalf("unicode match failed: %s", result.Error)
	}

	resultStr := string(applyReplacement(content, result))
	if !strings.Contains(resultStr, "v2") {
		t.Errorf("unicode replacement failed: %s", resultStr)
	}
}

func TestApplyReplacementSubstringFuzzy(t *testing.T) {
	content := []byte("\titems: make(map[string]*Item, 256),")

	result := findAndReplace(content, EditOp{
		OldText:    "\titems: make(map[string]*Item),",
		NewText:    "\titems: make(map[string]*Item, 512),",
		Index:      0,
		ReplaceAll: false,
	}, 0)

	if !result.Success {
		t.Fatalf("match failed: %s", result.Error)
	}

	if result.Matches[0].Strategy == strategyExact {
		t.Log("matched exact (no stale mismatch)")
	} else {
		t.Logf("matched via %s (stale content scenario)", result.Matches[0].Strategy)
	}
}

// TestMultiEditOffsetSafety verifies that multiple edits at different offsets
// are applied bottom-up so offsets computed against the original stay valid.
func TestMultiEditOffsetSafety(t *testing.T) {
	content := []byte(contentAlpha + "\nbeta\ngamma\ndelta\n")

	edits := []EditOp{
		{OldText: contentAlpha, NewText: "ALPHA_REPLACED", Index: 0, ReplaceAll: false}, // low offset
		{OldText: "gamma", NewText: "GAMMA_REPLACED", Index: 1, ReplaceAll: false},      // higher offset
	}
	results := computeResults(content, nil, edits)

	for _, r := range results {
		if !r.Success {
			t.Fatalf("edit %d failed: %s", r.Index, r.Error)
		}
	}

	got := string(applyResults(content, results))

	checks := []struct {
		want    string
		present bool
	}{
		{"ALPHA_REPLACED", true},
		{"GAMMA_REPLACED", true},
		{contentAlpha, false},
		{"gamma", false},
	}
	for _, check := range checks {
		if strings.Contains(got, check.want) != check.present {
			t.Errorf("content %q presence=%v, want %v in %q", check.want, strings.Contains(got, check.want), check.present, got)
		}
	}
}

// Regression: a replace_all result was applied from offsets computed against the
// original content, so any other edit landing between its match regions shifted
// the later matches — duplicating fragments and truncating neighbours.
func TestReplaceAllWithInterveningEdit(t *testing.T) {
	content := []byte("foo AAAAAAAAAAAAAAAAAAAA bar ------ foo\n")

	edits := []EditOp{
		{OldText: "foo", NewText: "FOO", Index: 1, ReplaceAll: true},
		{OldText: "bar", NewText: "BARBARBAR", Index: 2, ReplaceAll: false},
	}
	results := detectOverlaps(computeResults(content, nil, edits))

	want := "FOO AAAAAAAAAAAAAAAAAAAA BARBARBAR ------ FOO\n"
	if got := string(applyResults(content, results)); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Regression: matches whose text ends with a newline used to report
// LineEnd one line too high.
func TestLineEndWithTrailingNewline(t *testing.T) {
	content := []byte("line1\nline2\nline3\n")

	result := findAndReplace(content, EditOp{OldText: "line1\n", NewText: "X\n", Index: 0, ReplaceAll: false}, 0)
	if !result.Success {
		t.Fatalf("match failed: %s", result.Error)
	}

	if result.Matches[0].LineStart != 1 || result.Matches[0].LineEnd != 2 {
		t.Errorf("line range = %d-%d, want 1-2", result.Matches[0].LineStart, result.Matches[0].LineEnd)
	}

	result = findAndReplace(content, EditOp{OldText: "line2\nline3", NewText: "Y", Index: 0, ReplaceAll: false}, 0)
	if !result.Success {
		t.Fatalf("multi-line match failed: %s", result.Error)
	}

	if result.Matches[0].LineStart != 2 || result.Matches[0].LineEnd != 4 {
		t.Errorf("line range = %d-%d, want 2-4", result.Matches[0].LineStart, result.Matches[0].LineEnd)
	}
}

// Overlapping edits must be rejected instead of silently corrupting the file.
func TestOverlappingEditsRejected(t *testing.T) {
	content := []byte("foo bar baz\n")
	edits := []EditOp{
		{OldText: "foo bar", NewText: "FOO", Index: 1, ReplaceAll: false},
		{OldText: "bar baz", NewText: "BAZ", Index: 2, ReplaceAll: false},
	}
	results := detectOverlaps(computeResults(content, nil, edits))

	for _, r := range results {
		if !r.Success && !strings.Contains(r.Error, "OVERLAP") {
			t.Errorf("edit %d error should mention OVERLAP: %s", r.Index, r.Error)
		}
	}

	successCount, failCount := countResults(results)
	if successCount != 1 || failCount != 1 {
		t.Fatalf("want 1 applied + 1 rejected, got %d + %d", successCount, failCount)
	}

	// Applying the surviving edit must produce clean output, no corruption.
	got := string(applyResults(content, results))
	if !strings.Contains(got, "FOO") || strings.Contains(got, "bar") || strings.Contains(got, "BAZ") {
		t.Errorf("surviving edit corrupted the file: %q", got)
	}
}

// An insert landing inside an edit region conflicts; at the boundary it is fine.
func TestInsertInsideEditRegionRejected(t *testing.T) {
	content := []byte("line1\nline2\n")
	ins := []InsertOp{{Line: 2, Text: "mid\n", Index: 1}}
	edits := []EditOp{{OldText: "line1\nline2", NewText: "XXX", Index: 2, ReplaceAll: false}}
	results := detectOverlaps(computeResults(content, ins, edits))

	editOK, insertOK := false, false

	for _, r := range results {
		if r.Success {
			if r.Matches[0].Strategy == strategyInsert {
				insertOK = true
			} else {
				editOK = true
			}
		}
	}

	if !editOK {
		t.Error("edit should survive; the mid-region insert must be rejected")
	}

	if insertOK {
		t.Error("insert landing inside the edit region should be rejected")
	}

	// Insert exactly at the edit region boundary (line 1) is fine.
	ins = []InsertOp{{Line: 1, Text: "pre\n", Index: 1}}
	results = detectOverlaps(computeResults(content, ins, edits))
	applied := 0

	for _, r := range results {
		if r.Success {
			applied++
		}
	}

	if applied != 2 {
		t.Errorf("boundary insert should coexist with the edit, got %d applied", applied)
	}
}

// Non-overlapping edits pass overlap detection untouched.
func TestNonOverlappingEditsPass(t *testing.T) {
	content := []byte(contentAlpha + "\nbeta\n")
	edits := []EditOp{
		{OldText: contentAlpha, NewText: "A", Index: 1, ReplaceAll: false},
		{OldText: "beta", NewText: "B", Index: 2, ReplaceAll: false},
	}

	results := detectOverlaps(computeResults(content, nil, edits))
	for _, r := range results {
		if !r.Success {
			t.Errorf("edit %d should pass: %s", r.Index, r.Error)
		}
	}
}
