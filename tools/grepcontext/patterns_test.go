package grepcontext

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
	// Matches must be far apart: nearby hits share one context window.
	code := "alpha here\nfiller\nfiller\nfiller\nfiller\nbeta here\n" +
		"filler\nfiller\nfiller\nfiller\nfiller\ngamma here\n"

	err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		"patterns":   []string{"alpha", "gamma"},
		"path":       dir,
		"include":    "*.go",
		"names_only": true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	for _, want := range []string{"a.go:1", "a.go:12", `"alpha | gamma"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "a.go:6") {
		t.Errorf("output must not match the beta line:\n%s", text)
	}
}

func TestPatternsRequireAPattern(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"path": t.TempDir(), "patterns": []string{"", ""}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Handle(raw)
	if err == nil || !strings.Contains(err.Error(), "pattern or patterns") {
		t.Errorf("error = %v, want the pattern-required error", err)
	}
}
