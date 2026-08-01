package multiread

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

const helloText = "hello"

func marshalArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestHandleSingleBannedPath(t *testing.T) {
	// Point project root at a temp dir so ResolvePath works predictably.
	orig := server.ProjectRoot

	server.ProjectRoot = t.TempDir()
	defer func() { server.ProjectRoot = orig }()

	// Write a real file named .env so ResolvePath can find it.
	banned := filepath.Join(server.ProjectRoot, ".env")
	_ = os.WriteFile(banned, []byte("SECRET=x\n"), 0600)

	_, err := Handle(marshalArgs(t, map[string]any{pathKey: banned}))
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
	_ = os.WriteFile(f, []byte("hello world\n"), 0600)

	result, err := Handle(marshalArgs(t, map[string]any{pathKey: f}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result.Content[0].Text, "hello world") {
		t.Errorf("expected file content in output, got: %s", result.Content[0].Text)
	}
}

func TestTokenEstimateLine(t *testing.T) {
	var buf strings.Builder

	// Non-compact head output must carry a chars/4 estimate line.
	content := "hello world"
	writeHeadOutput(&buf, "txt", content, "hello.txt", 1, 1, false)

	got := buf.String()
	if !strings.Contains(got, "≈2 tokens (chars/4)") {
		t.Errorf("missing estimate line in output: %q", got)
	}

	// Compact output stays bare.
	buf.Reset()
	writeHeadOutput(&buf, "txt", content, "hello.txt", 1, 1, true)

	if got := buf.String(); got != "```txt\n"+content+"\n```\n" {
		t.Errorf("compact output should be unchanged, got %q", got)
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
		{"single line no newline", helloText, 1, helloText},
		{"single line with newline", "hello\n", 1, helloText},
		{"multi line trailing newline", "a\nb\nc\n", 1, "c"},
		{"multi line no trailing newline", "a\nb\nc", 1, "c"},
		{"full tail", "a\nb\nc\n", 3, "a\nb\nc"},
		{"empty file", "", 5, ""},
		{"only newline", "\n", 1, ""},
	}
	for _, tcase := range cases {
		t.Run(tcase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")

			err := os.WriteFile(path, []byte(tcase.content), 0600)
			if err != nil {
				t.Fatal(err)
			}

			var buf strings.Builder

			err = renderTail(path, tcase.tail, true, &buf)
			if err != nil {
				t.Fatalf("renderTail: %v", err)
			}

			got := buf.String()

			want := "```txt\n" + tcase.want + "\n```\n"
			if got != want {
				t.Errorf("renderTail(%q, %d) = %q, want %q", tcase.content, tcase.tail, got, want)
			}
		})
	}
}

func TestRenderTailLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")

	err := os.WriteFile(path, []byte("a\nb\nc\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder

	err = renderTail(path, 100, true, &buf)
	if err != nil {
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

	_, err := Handle(marshalArgs(t, map[string]any{pathKey: filepath.Join(outside, "**", "*.go")}))
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("expected outside-project-root error, got %v", err)
	}

	_, err = Handle(marshalArgs(t, map[string]any{"reads": []map[string]any{
		{pathKey: filepath.Join(outside, "**", "*.go")},
	}}))
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Errorf("reads mode: expected outside-project-root error, got %v", err)
	}
}
