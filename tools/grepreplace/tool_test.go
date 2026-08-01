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
