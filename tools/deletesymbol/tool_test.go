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

	// Test function deletion.
	fp := filepath.Join(tmp, "func.go")
	code := `package p

func Keep() int { return 1 }

func RemoveMe(x string) string {
	return "bye " + x
}
`
	os.WriteFile(fp, []byte(code), 0644)
	raw := json.RawMessage(`{"name":"RemoveMe","path":"` + fp + `"}`)
	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("func: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "remove L5–L7, 3 lines") {
		t.Fatalf("func output: %s", res.Content[0].Text)
	}
	data, _ := os.ReadFile(fp)
	if strings.Contains(string(data), "RemoveMe") || !strings.Contains(string(data), "func Keep") {
		t.Fatal("func bad result")
	}

	// Test struct deletion.
	fp2 := filepath.Join(tmp, "struct.go")
	os.WriteFile(fp2, []byte(`package p

type KeepType struct {
	Val int
}

type RemoveType struct {
	Name string
}
`), 0644)
	raw = json.RawMessage(`{"name":"RemoveType","path":"` + fp2 + `"}`)
	res, err = Handle(raw)
	if err != nil {
		t.Fatalf("struct: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "remove L7–L9, 3 lines") {
		t.Fatalf("struct output: %s", res.Content[0].Text)
	}
	data, _ = os.ReadFile(fp2)
	if strings.Contains(string(data), "RemoveType") || !strings.Contains(string(data), "type KeepType") {
		t.Fatal("struct bad result")
	}

	// Test interface deletion.
	fp3 := filepath.Join(tmp, "iface.go")
	os.WriteFile(fp3, []byte(`package p

type KeepIface interface {
	A() error
}

type RemoveIface interface {
	B() error
}
`), 0644)
	raw = json.RawMessage(`{"name":"RemoveIface","path":"` + fp3 + `"}`)
	res, err = Handle(raw)
	if err != nil {
		t.Fatalf("interface: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, "remove L7–L9, 3 lines") {
		t.Fatalf("interface output: %s", res.Content[0].Text)
	}
}
