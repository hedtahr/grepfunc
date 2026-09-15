package grepreplace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

const testGlob = "*.go"

func TestReplaceNormal(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nfunc foo() {}\n"), 0600)

	raw, err := json.Marshal(map[string]any{keyPattern: "foo", keyReplacement: "bar", keyInclude: testGlob})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(result.Content[0].Text, "1 replacement") {
		t.Errorf("expected 1 replacement in output: %s", result.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)
	if !strings.Contains(string(data), "bar") || strings.Contains(string(data), "foo") {
		t.Errorf("file not replaced correctly: %s", data)
	}
}

// replacement supports capture groups ($1, $2) by design — verify they expand.
func TestReplaceCaptureGroup(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nvar count = 1\n"), 0600)

	raw, err := json.Marshal(map[string]any{keyPattern: "(co)unt", keyReplacement: "[$1]", keyInclude: testGlob})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)
	if !strings.Contains(string(data), "var [co] = 1") {
		t.Errorf("capture group should expand, got: %s", data)
	}
}

// The replacement string must expand \n into a real newline, not leave a
// literal backslash-n in the file.
func TestReplaceNewlineEscape(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nvar x = 1\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "var x = 1", keyReplacement: "var x = 1\nvar y = 2", keyInclude: testGlob,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)

	if strings.Contains(string(data), `\n`) {
		t.Errorf("replacement left a literal backslash-n: %q", data)
	}

	if !strings.Contains(string(data), "var x = 1\nvar y = 2\n") {
		t.Errorf("replacement did not insert a newline: %q\n%s", data, result.Content[0].Text)
	}
}

// include must match paths relative to the search root, so a plain sub-directory
// glob works without a "**/" prefix.
func TestReplaceIncludeRelativeToRoot(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	sub := filepath.Join(dir, "sql")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	filePath := filepath.Join(sub, "query.sql")
	_ = os.WriteFile(filePath, []byte("SELECT 1;\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "SELECT", keyReplacement: "select", keyInclude: "sql/query.sql",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(result.Content[0].Text, "1 files changed") {
		t.Errorf("expected the relative include to match: %s", result.Content[0].Text)
	}
}

// Brace sets must expand instead of silently matching nothing.
func TestReplaceIncludeBraceSet(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nvar t = 1\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "q.sql"), []byte("SELECT t;\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "notes.md"), []byte("t everywhere\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "t", keyReplacement: "T", keyInclude: "*.{go,sql}",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(result.Content[0].Text, "2 files changed") {
		t.Errorf("brace include should match a.go and q.sql only: %s", result.Content[0].Text)
	}
}

// ^ and $ are line anchors, matching the plain line text between them.
func TestReplaceLineAnchors(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.sql")
	_ = os.WriteFile(filePath, []byte("  REGIONAL AS REG,\nSELECT REGIONAL AS AGENCY,\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "^  REGIONAL AS REG,$", keyReplacement: "  REGIONAL AS REGION,",
		keyInclude: "*.sql", "case_sensitive": true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(result.Content[0].Text, "1 files changed") {
		t.Errorf("anchored pattern should match the indented line: %s", result.Content[0].Text)
	}
}

// A silent "0 files" is unhelpful: report the include glob and scanned count.
func TestReplaceNoMatchExplainsScope(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	_ = os.WriteFile(filepath.Join(dir, "x.go"), []byte("package p\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "absent", keyReplacement: "present", keyInclude: testGlob,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, want := range []string{noMatchMsgPrefix, "1 scanned files", testGlob} {
		if !strings.Contains(result.Content[0].Text, want) {
			t.Errorf("no-match output should contain %q: %s", want, result.Content[0].Text)
		}
	}
}

func TestReplaceDryRun(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nfunc foo() {}\n"), 0600)

	raw, err := json.Marshal(map[string]any{
		keyPattern: "foo", keyReplacement: "bar", keyInclude: testGlob, "dry_run": true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)
	if !strings.Contains(string(data), "foo") {
		t.Errorf("dry_run must not modify the file, got: %s", data)
	}
}
