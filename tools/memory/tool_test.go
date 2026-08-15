package memory

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func marshalArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestHandleEmptyList(t *testing.T) {
	// Override storePath.
	origPath := storePath

	storePath = func(_ string) (string, error) {
		return filepath.Join(t.TempDir(), "memory.json"), nil
	}
	defer func() { storePath = origPath }()

	result, err := Handle(marshalArgs(t, map[string]any{}))
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

	// Save.
	_, err := Handle(marshalArgs(t, map[string]any{
		keyField:   "style.comments",
		valueField: "one-liners only",
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Recall.
	result, err := Handle(marshalArgs(t, map[string]any{}))
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

	// Save two.
	saveKV(t, "a", "alpha")
	saveKV(t, "b", "beta")

	// Recall one.
	result, err := Handle(marshalArgs(t, map[string]any{keyField: "a"}))
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

	_, err := Handle(marshalArgs(t, map[string]any{keyField: "x", "delete": true}))
	if err != nil {
		t.Fatal(err)
	}

	result, _ := Handle(marshalArgs(t, map[string]any{}))
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

	result, _ := Handle(marshalArgs(t, map[string]any{keyField: "theme"}))
	if result.Content[0].Text != "light" {
		t.Errorf("got %q, want light", result.Content[0].Text)
	}
}

// merge must keep distinct values even when one is a substring of the other,
// while still deduplicating exact repeats.
func TestMergeDistinctSubstringsKept(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")

	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	merge(t, "langs", "golang")
	merge(t, "langs", "go")

	result, _ := Handle(marshalArgs(t, map[string]any{keyField: "langs"}))

	text := result.Content[0].Text
	if !strings.Contains(text, "golang") || !strings.Contains(text, "go") {
		t.Errorf("merge dropped a distinct value: %q", text)
	}

	// Exact repeat is deduplicated.
	merge(t, "langs", "golang")

	result, _ = Handle(marshalArgs(t, map[string]any{keyField: "langs"}))
	if strings.Count(result.Content[0].Text, "golang") != 1 {
		t.Errorf("exact repeat should be deduped: %q", result.Content[0].Text)
	}
}

func merge(t *testing.T, key, value string) {
	t.Helper()

	_, err := Handle(marshalArgs(t, map[string]any{keyField: key, valueField: value, "merge": true}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestLRUEviction(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")

	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	// Fill beyond maxEntries (50).
	for i := range 55 {
		saveKV(t, "k"+string(rune('a'+i%26))+string(rune('0'+i/26)), "v")
	}

	result, _ := Handle(marshalArgs(t, map[string]any{}))
	text := result.Content[0].Text

	// Should have at most 100 entries.
	count := 0

	for l := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
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

func TestRecallCodeFenceFormat(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")

	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	saveKV(t, "style.indent", "tabs")

	result, err := Handle(marshalArgs(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if !strings.Contains(text, "```\n") {
		t.Errorf("recall list output should be wrapped in code fences, got:\n%s", text)
	}

	if !strings.HasSuffix(strings.TrimSpace(text), "```") {
		t.Errorf("recall list output should end with closing code fence, got:\n%s", text)
	}
}

func saveKV(t *testing.T, key, value string) {
	t.Helper()

	_, err := Handle(marshalArgs(t, map[string]any{keyField: key, valueField: value}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestListDefaultCap(t *testing.T) {
	origPath := storePath
	fp := filepath.Join(t.TempDir(), "memory.json")

	storePath = func(_ string) (string, error) { return fp, nil }
	defer func() { storePath = origPath }()

	for i := range 60 {
		_, err := Handle(marshalArgs(t, map[string]any{
			keyField:   fmt.Sprintf("key%d", i),
			valueField: fmt.Sprintf("value%d", i),
		}))
		if err != nil {
			t.Fatal(err)
		}
	}

	result, err := Handle(marshalArgs(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if got := strings.Count(text, "→"); got != defaultListCap {
		t.Errorf("default recall shows %d entries, want %d", got, defaultListCap)
	}

	if !strings.Contains(text, "10 more entries") {
		t.Errorf("missing pagination hint, got %q", text)
	}
}
