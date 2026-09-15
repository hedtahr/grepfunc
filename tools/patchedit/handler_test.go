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
