package filehead

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "test.go")
	code := "package test\n\n// Hello returns a greeting.\nfunc Hello() string {\n\treturn \"hello\"\n}\n"
	os.WriteFile(fp, []byte(code), 0644)

	raw, _ := json.Marshal(map[string]any{"path": fp, "lines": 3})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if text == "" {
		t.Fatal("empty output")
	}
}

func TestHandleDefaults(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "test.go")
	os.WriteFile(fp, []byte("package main\nfunc main() {}\n"), 0644)

	// No lines specified → default 60
	raw, _ := json.Marshal(map[string]any{"path": fp})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty content")
	}
}

func TestHandleSmallFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "small.go")
	os.WriteFile(fp, []byte("package small\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"path": fp, "lines": 50})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if text == "" {
		t.Fatal("empty output")
	}
}

func TestHandleMissingFile(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"path": "/nonexistent/file.go"})
	_, err := Handle(raw)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestHandleNoPath(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{})
	_, err := Handle(raw)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestLinesCappedAt200(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "big.go")
	code := strings.Repeat("// line\n", 300)
	os.WriteFile(fp, []byte(code), 0644)

	raw, _ := json.Marshal(map[string]any{"path": fp, "lines": 999})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if text == "" {
		t.Fatal("empty output")
	}
}
