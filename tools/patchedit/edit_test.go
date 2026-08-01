package patchedit

import (
	"sort"
	"strings"
	"testing"
)

func TestFindAndReplaceExact(t *testing.T) {
	content := []byte("\tcounter int64\n\titems   map[string]*Item\n")

	r := findAndReplace(content, EditOp{
		OldText: "\tcounter int64",
		NewText: "\tcounter uint64",
	}, 0)

	if !r.Success {
		t.Fatalf("expected success, got: %s", r.Error)
	}
	if r.Matches[0].Strategy != "exact" {
		t.Errorf("strategy = %s, want exact", r.Matches[0].Strategy)
	}
}

func TestFindAndReplaceNoMatch(t *testing.T) {
	content := []byte("package main\nfunc main() {}\n")

	r := findAndReplace(content, EditOp{
		OldText: "THIS_DOES_NOT_EXIST_12345",
		NewText: "// comment",
	}, 0)

	if r.Success {
		t.Fatal("expected failure")
	}
	if !strings.Contains(r.Error, "NO_MATCH") {
		t.Errorf("error does not say NO_MATCH: %s", r.Error)
	}
	if !strings.Contains(r.Error, "Nearest:") {
		t.Errorf("error does not show nearest match: %s", r.Error)
	}
}

func TestFindAndReplaceAmbiguous(t *testing.T) {
	content := []byte("return nil, false\n// code\nreturn nil, false\n// more\nreturn nil, false\n")

	r := findAndReplace(content, EditOp{
		OldText: "return nil, false",
		NewText: "return nil, true",
	}, 0)

	if r.Success {
		t.Fatal("expected failure for ambiguous match")
	}
	if !strings.Contains(r.Error, "AMBIGUOUS_MATCH") {
		t.Errorf("error does not say AMBIGUOUS_MATCH: %s", r.Error)
	}
	if len(r.Matches) != 3 {
		t.Errorf("expected 3 matches, got %d", len(r.Matches))
	}
}

func TestFindAndReplaceEmptyOldText(t *testing.T) {
	content := []byte("anything\n")

	r := findAndReplace(content, EditOp{
		OldText: "",
		NewText: "something",
	}, 0)

	if r.Success {
		t.Fatal("expected failure for empty old_text")
	}
}

func TestApplyReplacement(t *testing.T) {
	content := []byte("\tcounter int64\n\titems map[string]*Item\n")

	r := findAndReplace(content, EditOp{
		OldText: "counter int64",
		NewText: "counter uint64",
	}, 0)
	if !r.Success {
		t.Fatalf("match failed: %s", r.Error)
	}

	result := applyReplacement(content, r)
	resultStr := string(result)

	if !strings.Contains(resultStr, "counter uint64") {
		t.Errorf("replacement not applied: %s", resultStr)
	}
	if strings.Contains(resultStr, "counter int64") {
		t.Errorf("old text still present: %s", resultStr)
	}
}

func TestApplyReplacementPreservesSurrounding(t *testing.T) {
	content := []byte("lineA\nlineB\nlineC\n")

	r := findAndReplace(content, EditOp{
		OldText: "lineB",
		NewText: "lineB_changed",
	}, 0)
	if !r.Success {
		t.Fatalf("match failed: %s", r.Error)
	}

	result := string(applyReplacement(content, r))

	if !strings.Contains(result, "lineA") {
		t.Error("lineA missing")
	}
	if !strings.Contains(result, "lineC") {
		t.Error("lineC missing")
	}
	if !strings.Contains(result, "lineB_changed") {
		t.Error("change not applied")
	}
	if strings.Count(result, "\n") != strings.Count(string(content), "\n") {
		t.Error("line count changed unexpectedly")
	}
}

func TestApplyReplacementUnicode(t *testing.T) {
	content := []byte("// 🚀 constructor\nfunc New() {}\n")

	r := findAndReplace(content, EditOp{
		OldText: "// 🚀 constructor",
		NewText: "// 🚀 constructor v2",
	}, 0)

	if !r.Success {
		t.Fatalf("unicode match failed: %s", r.Error)
	}
	result := string(applyReplacement(content, r))
	if !strings.Contains(result, "v2") {
		t.Errorf("unicode replacement failed: %s", result)
	}
}

func TestApplyReplacementSubstringFuzzy(t *testing.T) {
	content := []byte("\titems: make(map[string]*Item, 256),")

	r := findAndReplace(content, EditOp{
		OldText: "\titems: make(map[string]*Item),",
		NewText: "\titems: make(map[string]*Item, 512),",
	}, 0)

	if !r.Success {
		t.Fatalf("match failed: %s", r.Error)
	}
	if r.Matches[0].Strategy == "exact" {
		t.Log("matched exact (no stale mismatch)")
	} else {
		t.Logf("matched via %s (stale content scenario)", r.Matches[0].Strategy)
	}
}

// TestMultiEditOffsetSafety verifies that multiple edits at different offsets
// are applied bottom-up so offsets computed against the original stay valid.
func TestMultiEditOffsetSafety(t *testing.T) {
	content := []byte("alpha\nbeta\ngamma\ndelta\n")

	edits := []EditOp{
		{OldText: "alpha", NewText: "ALPHA_REPLACED", Index: 0}, // low offset
		{OldText: "gamma", NewText: "GAMMA_REPLACED", Index: 1}, // higher offset
	}
	results := computeResults(content, nil, edits)

	for _, r := range results {
		if !r.Success {
			t.Fatalf("edit %d failed: %s", r.Index, r.Error)
		}
	}

	// Apply using the bottom-up sort (same logic as handleEditFile)
	toApply := make([]editResult, 0, len(results))
	for _, r := range results {
		if r.Success {
			toApply = append(toApply, r)
		}
	}
	sort.Slice(toApply, func(i, j int) bool {
		oi, oj := 0, 0
		if len(toApply[i].Matches) > 0 {
			oi = toApply[i].Matches[0].Offset
		}
		if len(toApply[j].Matches) > 0 {
			oj = toApply[j].Matches[0].Offset
		}
		return oi > oj
	})
	current := content
	for _, r := range toApply {
		current = applyReplacement(current, r)
	}

	got := string(current)
	if !strings.Contains(got, "ALPHA_REPLACED") {
		t.Errorf("low-offset edit missing: %q", got)
	}
	if !strings.Contains(got, "GAMMA_REPLACED") {
		t.Errorf("high-offset edit missing: %q", got)
	}
	if strings.Contains(got, "alpha") {
		t.Errorf("old low-offset text still present: %q", got)
	}
	if strings.Contains(got, "gamma") {
		t.Errorf("old high-offset text still present: %q", got)
	}
}

// Regression: matches whose text ends with a newline used to report
// LineEnd one line too high.
func TestLineEndWithTrailingNewline(t *testing.T) {
	content := []byte("line1\nline2\nline3\n")
	r := findAndReplace(content, EditOp{OldText: "line1\n", NewText: "X\n"}, 0)
	if !r.Success {
		t.Fatalf("match failed: %s", r.Error)
	}
	if r.Matches[0].LineStart != 1 || r.Matches[0].LineEnd != 2 {
		t.Errorf("line range = %d-%d, want 1-2", r.Matches[0].LineStart, r.Matches[0].LineEnd)
	}

	r = findAndReplace(content, EditOp{OldText: "line2\nline3", NewText: "Y"}, 0)
	if !r.Success {
		t.Fatalf("multi-line match failed: %s", r.Error)
	}
	if r.Matches[0].LineStart != 2 || r.Matches[0].LineEnd != 4 {
		t.Errorf("line range = %d-%d, want 2-4", r.Matches[0].LineStart, r.Matches[0].LineEnd)
	}
}

// Overlapping edits must be rejected instead of silently corrupting the file.
func TestOverlappingEditsRejected(t *testing.T) {
	content := []byte("foo bar baz\n")
	edits := []EditOp{
		{OldText: "foo bar", NewText: "FOO", Index: 1},
		{OldText: "bar baz", NewText: "BAZ", Index: 2},
	}
	results := computeResults(content, nil, edits)
	results = detectOverlaps(results)

	ok, failed := 0, 0
	for _, r := range results {
		if r.Success {
			ok++
		} else {
			failed++
			if !strings.Contains(r.Error, "OVERLAP") {
				t.Errorf("edit %d error should mention OVERLAP: %s", r.Index, r.Error)
			}
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("want 1 applied + 1 rejected, got %d + %d", ok, failed)
	}

	// Applying the surviving edit must produce clean output, no corruption.
	var out []byte = content
	for _, r := range results {
		if r.Success {
			out = applyReplacement(out, r)
		}
	}
	got := string(out)
	if !strings.Contains(got, "FOO") || strings.Contains(got, "bar") || strings.Contains(got, "BAZ") {
		t.Errorf("surviving edit corrupted the file: %q", got)
	}
}

// An insert landing inside an edit region conflicts; at the boundary it is fine.
func TestInsertInsideEditRegionRejected(t *testing.T) {
	content := []byte("line1\nline2\n")
	ins := []InsertOp{{Line: 2, Text: "mid\n", Index: 1}}
	edits := []EditOp{{OldText: "line1\nline2", NewText: "XXX", Index: 2}}
	results := detectOverlaps(computeResults(content, ins, edits))

	editOK, insertOK := false, false
	for _, r := range results {
		if r.Success {
			if r.Matches[0].Strategy == "insert" {
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
	content := []byte("alpha\nbeta\n")
	edits := []EditOp{
		{OldText: "alpha", NewText: "A", Index: 1},
		{OldText: "beta", NewText: "B", Index: 2},
	}
	results := detectOverlaps(computeResults(content, nil, edits))
	for _, r := range results {
		if !r.Success {
			t.Errorf("edit %d should pass: %s", r.Index, r.Error)
		}
	}
}
