package multiread

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestHandleSingleBannedPath(t *testing.T) {
	// Point project root at a temp dir so ResolvePath works predictably.
	orig := server.ProjectRoot
	server.ProjectRoot = t.TempDir()
	defer func() { server.ProjectRoot = orig }()

	// Write a real file named .env so ResolvePath can find it.
	banned := filepath.Join(server.ProjectRoot, ".env")
	_ = os.WriteFile(banned, []byte("SECRET=x\n"), 0600)

	raw, _ := json.Marshal(map[string]any{"path": banned})
	_, err := Handle(raw)
	if err == nil {
		t.Fatal("expected error for banned .env path, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should say 'access denied': %v", err)
	}
}

func TestHandleSingleNormalFile(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot
	server.ProjectRoot = dir
	defer func() { server.ProjectRoot = orig }()

	f := filepath.Join(dir, "hello.txt")
	_ = os.WriteFile(f, []byte("hello world\n"), 0644)

	raw, _ := json.Marshal(map[string]any{"path": f})
	result, err := Handle(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content[0].Text, "hello world") {
		t.Errorf("expected file content in output, got: %s", result.Content[0].Text)
	}
}
