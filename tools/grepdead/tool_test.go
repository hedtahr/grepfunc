package grepdead

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

// A private symbol used from its own file is live; only an exported one has to
// be used from elsewhere to count as referenced.
func TestHandleCountsSameFileReferences(t *testing.T) {
	dir := t.TempDir()

	orig := server.ProjectRoot
	server.ProjectRoot = dir

	t.Cleanup(func() { server.ProjectRoot = orig })

	source := "package p\n\n" +
		"func usedHelper() {\n\tn := 1\n\t_ = n\n}\n\n" +
		"func orphanHelper() {\n\tn := 1\n\t_ = n\n}\n\n" +
		"func Root() {\n\tusedHelper()\n\treturn\n}\n"

	// #nosec G304 -- t.TempDir test fixture
	err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(source), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(args{Path: dir, MinLines: 2})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text

	if strings.Contains(text, "usedHelper") {
		t.Errorf("usedHelper is called from its own file, must not be reported:\n%s", text)
	}

	for _, want := range []string{"orphanHelper", "Root"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s missing from the report:\n%s", want, text)
		}
	}
}
