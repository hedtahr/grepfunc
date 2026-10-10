package patchedit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

const (
	unformattedGo = "package p\n\nfunc probeTmp(a int) int {\n\treturn a+1\n}\n"
	formattedGo   = "package p\n\nfunc probeTmp(a int) int {\n\treturn a + 1\n}\n"
)

func patchFile(t *testing.T, dir, path string, extra map[string]any) *server.ToolCallResult {
	t.Helper()

	patchArgs := map[string]any{
		"path":          path,
		"terse":         true,
		"skip_validate": true,
	}

	for key, value := range extra {
		patchArgs[key] = value
	}

	raw, err := json.Marshal(patchArgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	return result
}

// Regression: matching ran against a gofmt-formatted copy, so old_text copied
// from the file failed with a misleading NO_MATCH, and a match then rewrote the
// whole file formatted. Matching must use the bytes that are on disk.
func TestPatchMatchesOnDiskBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte(unformattedGo), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "return a+1", "new_text": "return a + 2"}},
	})

	if !strings.Contains(result.Content[0].Text, "[OK] 1/1") {
		t.Fatalf("edit against the on-disk text should apply: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	want := "package p\n\nfunc probeTmp(a int) int {\n\treturn a + 2\n}\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q (only the edit, no reformat)", data, want)
	}
}

// Regression: a Go file that did not match gofmt was silently rewritten formatted.
func TestPatchKeepsUnrelatedFormatting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte("package p\n\nvar  x   =  1\n"), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "var  x   =  1", "new_text": "var  y   =  1"}},
	})

	if !strings.Contains(result.Content[0].Text, "[OK] 1/1") {
		t.Fatalf("edit should apply: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	if string(data) != "package p\n\nvar  y   =  1\n" {
		t.Errorf("unrelated spacing was rewritten: %q", data)
	}
}

// format=true opts into reformatting the result.
func TestPatchFormatOptIn(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not installed")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte(unformattedGo), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits":  []map[string]any{{"old_text": "return a+1", "new_text": "return a+1"}},
		"format": true,
	})

	if !strings.Contains(result.Content[0].Text, "[OK] 1/1") {
		t.Fatalf("edit should apply: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	if string(data) != formattedGo {
		t.Errorf("format=true should gofmt the result, got %q", data)
	}
}

// Regression: a whitespace-fuzzy match whose replacement text is itself
// indented replaced only the text after the file's leading whitespace, so the
// file's indentation was kept and the new indentation doubled (8 + 16 = 24).
// The span must start at the line start in that case.
func TestWhitespaceFuzzyReplacesIndentation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte("package p\n\nfunc f() {\n\treturn nil\n}\n"), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "return  nil", "new_text": "\treturn err"}},
	})

	if !strings.Contains(result.Content[0].Text, "[OK] 1/1") {
		t.Fatalf("edit should apply: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	want := "package p\n\nfunc f() {\n\treturn err\n}\n"
	if string(data) != want {
		t.Errorf("indentation doubled: file = %q, want %q", data, want)
	}
}

// An unindented replacement keeps the file's own indentation.
func TestWhitespaceFuzzyKeepsIndentForPlainReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte("package p\n\nfunc f() {\n\treturn nil\n}\n"), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "return  nil", "new_text": "return err"}},
	})

	if !strings.Contains(result.Content[0].Text, "[OK] 1/1") {
		t.Fatalf("edit should apply: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	want := "package p\n\nfunc f() {\n\treturn err\n}\n"
	if string(data) != want {
		t.Errorf("file = %q, want %q", data, want)
	}
}

// Terse output must name the fuzzy tier used, so a fuzzy splice is never
// mistaken for an exact match.
func TestTerseReportsFuzzyTier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte("package p\n\nfunc f() {\n\treturn nil\n}\n"), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "return  nil", "new_text": "return err"}},
	})

	if !strings.Contains(result.Content[0].Text, "whitespace_fuzzy") {
		t.Errorf("terse output should name the match tier, got %q", result.Content[0].Text)
	}
}

func TestTerseReportsLineFuzzyTier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte("package p\n\nfunc NewDataStore(cfg StoreConfig) *DataStore {\n\treturn &DataStore{}\n}\n"), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{
			"old_text": "func NewDataStore(cfg StoreConfig) *DataStore { // constructor",
			"new_text": "func NewDataStore(cfg StoreConfig) *DataStore {",
		}},
	})

	if !strings.Contains(result.Content[0].Text, "line_fuzzy") {
		t.Errorf("terse output should name line_fuzzy, got %q", result.Content[0].Text)
	}
}

// Regression (end-to-end): the reported repro — a file line "w" must not
// stand in for a long search line, so the edit is NO_MATCH and the file is
// untouched.
func TestPatchRejectsTinySubsetLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repro.rs")

	repro := "fn g(t: T) -> Vec<u8> {\n    let w = t.words();\n    if w.len() <= 1 {\n        w\n    } else {\n        vec![8]\n    }\n}\n"

	_ = os.WriteFile(path, []byte(repro), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"dry_run": true,
		"edits": []map[string]any{{
			"old_text": "                kind: BindKind::View,",
			"new_text": "                kind: BindKind::View | BindKind::MutView,",
		}},
	})

	if !strings.Contains(result.Content[0].Text, "NO_MATCH") {
		t.Fatalf("tiny subset line must not match, got: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	if string(data) != repro {
		t.Errorf("file must be untouched, got %q", data)
	}
}

// The handler-level default must stay "never reformat": an edit that is a no-op
// on formatted content still writes the file back byte for byte.
func TestPatchNoReformatWithoutFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.go")

	_ = os.WriteFile(path, []byte(unformattedGo), 0600)

	result := patchFile(t, dir, path, map[string]any{
		"edits": []map[string]any{{"old_text": "return a+1", "new_text": "return a+1"}},
	})

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(path)

	if strings.Contains(result.Content[0].Text, "reformatted") {
		t.Errorf("output should not claim reformatting: %s", result.Content[0].Text)
	}

	if string(data) != unformattedGo {
		t.Errorf("file = %q, want it unchanged", data)
	}
}
