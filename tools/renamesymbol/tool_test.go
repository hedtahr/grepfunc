package renamesymbol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestRenameNormal(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nfunc foo() int { return 1 }\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"old_name": "foo", "new_name": "bar"})
	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "1 replacement") {
		t.Errorf("expected 1 replacement in output: %s", result.Content[0].Text)
	}
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "func bar()") || strings.Contains(string(data), "foo") {
		t.Errorf("file not renamed correctly: %s", data)
	}
}

// new_name containing $ must be inserted literally, not expanded as a
// regexp capture-group reference.
func TestRenameDollarInNewName(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nvar foo = 1\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"old_name": "foo", "new_name": "$1"})
	if _, err := Handle(raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "var $1 = 1") {
		t.Errorf("$ in new_name should be literal, got: %s", data)
	}
}

func TestRenameDryRun(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nfunc foo() {}\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"old_name": "foo", "new_name": "bar", "dry_run": true})
	if _, err := Handle(raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "foo") {
		t.Errorf("dry_run must not modify the file, got: %s", data)
	}
}
