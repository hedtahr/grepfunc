package movesymbol

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
	src := filepath.Join(tmp, "src.go")
	dst := filepath.Join(tmp, "dst.go")

	// Setup: src with two functions, dst with existing code.
	os.WriteFile(src, []byte(`package p

func Keep() int { return 1 }

func MoveMe(x string) string {
	return "moved: " + x
}
`), 0644)
	os.WriteFile(dst, []byte(`package p

func Existing() bool { return true }
`), 0644)

	// Move MoveMe from src to dst, append mode (no line specified).
	raw := json.RawMessage(`{"name":"MoveMe","src":"` + src + `","dst":"` + dst + `"}`)
	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "remove L5–L7, 3 lines") {
		t.Fatalf("expected remove info, got: %s", text)
	}

	// Verify src: MoveMe removed, Keep remains.
	srcAfter, _ := os.ReadFile(src)
	if strings.Contains(string(srcAfter), "MoveMe") {
		t.Fatal("MoveMe still in src")
	}
	if !strings.Contains(string(srcAfter), "func Keep") {
		t.Fatal("Keep missing from src")
	}

	// Verify dst: MoveMe appended after Existing.
	dstAfter, _ := os.ReadFile(dst)
	if !strings.Contains(string(dstAfter), "func Existing") {
		t.Fatal("Existing missing from dst")
	}
	if !strings.Contains(string(dstAfter), "func MoveMe") {
		t.Fatal("MoveMe not appended to dst")
	}

	// MoveMe should be after Existing.
	idxExisting := strings.Index(string(dstAfter), "func Existing")
	idxMoved := strings.Index(string(dstAfter), "func MoveMe")
	if idxMoved < idxExisting {
		t.Fatal("MoveMe should be after Existing")
	}
}
