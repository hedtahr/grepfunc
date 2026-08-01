package grepreplace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestReplaceNormal(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nfunc foo() {}\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"pattern": "foo", "replacement": "bar", "include": "*.go"})
	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "1 replacement") {
		t.Errorf("expected 1 replacement in output: %s", result.Content[0].Text)
	}
	data, _ := os.ReadFile(fp)
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

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nvar count = 1\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"pattern": "(co)unt", "replacement": "[$1]", "include": "*.go"})
	if _, err := Handle(raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "var [co] = 1") {
		t.Errorf("capture group should expand, got: %s", data)
	}
}

func TestReplaceDryRun(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	fp := filepath.Join(dir, "x.go")
	os.WriteFile(fp, []byte("package p\n\nfunc foo() {}\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"pattern": "foo", "replacement": "bar", "include": "*.go", "dry_run": true})
	if _, err := Handle(raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	data, _ := os.ReadFile(fp)
	if !strings.Contains(string(data), "foo") {
		t.Errorf("dry_run must not modify the file, got: %s", data)
	}
}
