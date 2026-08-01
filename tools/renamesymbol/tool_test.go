package renamesymbol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

const testSymbol = "foo"

func TestRenameNormal(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nfunc foo() int { return 1 }\n"), 0600)

	raw, err := json.Marshal(map[string]any{keyOldName: testSymbol, keyNewName: "bar"})
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

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nvar foo = 1\n"), 0600)

	raw, err := json.Marshal(map[string]any{keyOldName: testSymbol, keyNewName: "$1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)
	if !strings.Contains(string(data), "var $1 = 1") {
		t.Errorf("$ in new_name should be literal, got: %s", data)
	}
}

func TestRenameDryRun(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	filePath := filepath.Join(dir, "x.go")
	_ = os.WriteFile(filePath, []byte("package p\n\nfunc foo() {}\n"), 0600)

	raw, err := json.Marshal(map[string]any{keyOldName: testSymbol, keyNewName: "bar", "dry_run": true})
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
