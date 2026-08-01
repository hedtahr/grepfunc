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

// renderTail must handle 1-byte files, files without trailing newlines,
// empty files, and files ending in newlines.
func TestRenderTailEdges(t *testing.T) {
	cases := []struct {
		name    string
		content string
		tail    int
		want    string
	}{
		{"single byte no newline", "x", 1, "x"},
		{"single line no newline", "hello", 1, "hello"},
		{"single line with newline", "hello\n", 1, "hello"},
		{"multi line trailing newline", "a\nb\nc\n", 1, "c"},
		{"multi line no trailing newline", "a\nb\nc", 1, "c"},
		{"full tail", "a\nb\nc\n", 3, "a\nb\nc"},
		{"empty file", "", 5, ""},
		{"only newline", "\n", 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(fp, []byte(c.content), 0644); err != nil {
				t.Fatal(err)
			}
			var buf strings.Builder
			if err := renderTail(fp, c.tail, true, &buf); err != nil {
				t.Fatalf("renderTail: %v", err)
			}
			got := buf.String()
			want := "```txt\n" + c.want + "\n```\n"
			if got != want {
				t.Errorf("renderTail(%q, %d) = %q, want %q", c.content, c.tail, got, want)
			}
		})
	}
}

func TestRenderTailLimit(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(fp, []byte("a\nb\nc\n"), 0644)
	var buf strings.Builder
	if err := renderTail(fp, 100, true, &buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "```txt\na\nb\nc\n```\n" {
		t.Errorf("tail beyond length should show whole file, got %q", got)
	}
}

func TestGlobOutsideRootRejected(t *testing.T) {
	orig := server.ProjectRoot
	server.ProjectRoot = t.TempDir()
	defer func() { server.ProjectRoot = orig }()

	outside := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"path": filepath.Join(outside, "**", "*.go")})
	_, err := Handle(raw)
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("expected outside-project-root error, got %v", err)
	}

	raw, _ = json.Marshal(map[string]any{"reads": []map[string]any{{"path": filepath.Join(outside, "**", "*.go")}}})
	_, err = Handle(raw)
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("reads mode: expected outside-project-root error, got %v", err)
	}
}
