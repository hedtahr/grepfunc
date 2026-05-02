package memory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleEmptyList(t *testing.T) {
	// Override storePath
	origPath := storePath
	storePath = func(_ string) (string, error) {
		return filepath.Join(t.TempDir(), "memory.json"), nil
	}
	defer func() { storePath = origPath }()

	raw, _ := json.Marshal(map[string]any{})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content[0].Text, "No memories yet") {
		t.Error("empty state should show helpful message")
	}
}

func TestSaveAndRecall(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")
	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	// Save
	raw, _ := json.Marshal(map[string]any{
		"key":   "style.comments",
		"value": "one-liners only",
	})
	_, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	// Recall
	raw, _ = json.Marshal(map[string]any{})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "style.comments") {
		t.Error("should list saved key")
	}
	if !strings.Contains(text, "one-liners only") {
		t.Error("should list saved value")
	}
}

func TestRecallSpecificKey(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")
	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	// Save two
	saveKV(t, "a", "alpha")
	saveKV(t, "b", "beta")

	// Recall one
	raw, _ := json.Marshal(map[string]any{"key": "a"})
	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[0].Text != "alpha" {
		t.Errorf("got %q, want alpha", result.Content[0].Text)
	}
}

func TestDelete(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")
	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	saveKV(t, "x", "y")

	raw, _ := json.Marshal(map[string]any{"key": "x", "delete": true})
	_, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	raw, _ = json.Marshal(map[string]any{})
	result, _ := Handle(raw)
	if strings.Contains(result.Content[0].Text, "x") {
		t.Error("deleted key should not appear")
	}
}

func TestUpsertOverwrites(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")
	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	saveKV(t, "theme", "dark")
	saveKV(t, "theme", "light")

	raw, _ := json.Marshal(map[string]any{"key": "theme"})
	result, _ := Handle(raw)
	if result.Content[0].Text != "light" {
		t.Errorf("got %q, want light", result.Content[0].Text)
	}
}

func TestLRUEviction(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")
	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	// Fill beyond maxEntries (50)
	for i := 0; i < 55; i++ {
		saveKV(t, "k"+string(rune('a'+i%26))+string(rune('0'+i/26)), "v")
	}

	raw, _ := json.Marshal(map[string]any{})
	result, _ := Handle(raw)
	text := result.Content[0].Text

	// Should have at most 100 entries
	count := 0
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(l, "**") {
			count++
		}
	}
	if count > 100 {
		t.Errorf("got %d entries, max is 100", count)
	}
}

func TestStorePath(t *testing.T) {
	path, err := storePath(".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, ".llm") {
		t.Errorf("path should contain .llm: %s", path)
	}
	if !strings.HasSuffix(path, "memory.json") {
		t.Errorf("path should end with memory.json: %s", path)
	}
}

func saveKV(t *testing.T, key, value string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"key": key, "value": value})
	_, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}
}
