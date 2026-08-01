package deletesymbol

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

	testFuncDeletion(t, tmp)
	testStructDeletion(t, tmp)
	testIfaceDeletion(t, tmp)
}

func testFuncDeletion(t *testing.T, tmp string) {
	t.Helper()

	filePath := filepath.Join(tmp, "func.go")
	code := `package p

func Keep() int { return 1 }

func RemoveMe(x string) string {
	return "bye " + x
}
`
	_ = os.WriteFile(filePath, []byte(code), 0600)
	raw := json.RawMessage(`{"name":"RemoveMe","path":"` + filePath + `"}`)

	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("func: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "remove L5–L7, 3 lines") {
		t.Fatalf("func output: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(filePath)
	if strings.Contains(string(data), "RemoveMe") || !strings.Contains(string(data), "func Keep") {
		t.Fatal("func bad result")
	}
}

func testStructDeletion(t *testing.T, tmp string) {
	t.Helper()

	structPath := filepath.Join(tmp, "struct.go")
	_ = os.WriteFile(structPath, []byte(`package p

type KeepType struct {
	Val int
}

type RemoveType struct {
	Name string
}
`), 0600)

	raw := json.RawMessage(`{"name":"RemoveType","path":"` + structPath + `"}`)

	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("struct: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "remove L7–L9, 3 lines") {
		t.Fatalf("struct output: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, _ := os.ReadFile(structPath)
	if strings.Contains(string(data), "RemoveType") || !strings.Contains(string(data), "type KeepType") {
		t.Fatal("struct bad result")
	}
}

func testIfaceDeletion(t *testing.T, tmp string) {
	t.Helper()

	ifacePath := filepath.Join(tmp, "iface.go")
	_ = os.WriteFile(ifacePath, []byte(`package p

type KeepIface interface {
	A() error
}

type RemoveIface interface {
	B() error
}
`), 0600)

	raw := json.RawMessage(`{"name":"RemoveIface","path":"` + ifacePath + `"}`)

	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("interface: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "remove L7–L9, 3 lines") {
		t.Fatalf("interface output: %s", res.Content[0].Text)
	}
}
