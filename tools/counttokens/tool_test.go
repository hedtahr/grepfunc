package counttokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestHandle(t *testing.T) {
	tmp := t.TempDir()
	origRoot := server.ProjectRoot
	server.ProjectRoot = tmp
	defer func() { server.ProjectRoot = origRoot }()

	// Full-file: os.Stat for size, bufio.Scanner for line count.
	fullPath := filepath.Join(tmp, "full.go")
	os.WriteFile(fullPath, []byte("line1\nline2\n"), 0644)
	res, err := Handle(json.RawMessage(`{"path":"` + fullPath + `"}`))
	if err != nil {
		t.Fatalf("full file: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "Lines: 2") {
		t.Fatalf("expected Lines: 2, got %s", text)
	}
	if !strings.Contains(text, "Chars: 12") {
		t.Fatalf("expected Chars: 12, got %s", text)
	}

	// Line range: scan and count only target lines.
	os.WriteFile(fullPath, []byte("aaa\nbbb\nccc\nddd\n"), 0644)
	res, err = Handle(json.RawMessage(`{"path":"` + fullPath + `","start_line":2,"end_line":3}`))
	if err != nil {
		t.Fatalf("line range: %v", err)
	}
	text = res.Content[0].Text
	if !strings.Contains(text, "Lines: 2 (of 4 total)") {
		t.Fatalf("expected Lines: 2 (of 4 total), got %s", text)
	}
	if !strings.Contains(text, "Chars: 8") {
		t.Fatalf("expected Chars: 8, got %s", text)
	}
}
