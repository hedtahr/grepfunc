package grepstruct

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patterns must union with pattern and render as one label.
func TestHandlePatternsUnion(t *testing.T) {
	dir := t.TempDir()
	code := "package p\n\ntype Alpha struct {\n\tA int\n}\n\ntype Beta struct {\n\tB int\n}\n\ntype Gamma struct {\n\tG int\n}\n"

	err := os.WriteFile(filepath.Join(dir, "types.go"), []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		keyPatterns: []string{"Alpha", "Gamma"},
		keyPath:     dir,
		keyInclude:  "*.go",
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	for _, want := range []string{"Alpha", "Gamma", `"Alpha | Gamma"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "Beta") {
		t.Errorf("output must not contain Beta:\n%s", text)
	}
}

func TestPatternsRequireAPattern(t *testing.T) {
	raw, err := json.Marshal(map[string]any{keyPath: t.TempDir(), keyPatterns: []string{""}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Handle(raw)
	if err == nil || !strings.Contains(err.Error(), "pattern or patterns") {
		t.Errorf("error = %v, want the pattern-required error", err)
	}
}
