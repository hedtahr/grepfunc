package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp_patch_file/server"
)

var Tool = server.Tool{
	Name: "memory",
	Description: `Remember and recall project-specific conventions, style rules, and decisions across sessions. Call WITHOUT arguments to recall everything you've learned. Call with key and value to save something for next time. Automatically evicts least-recently-used facts when at capacity.

Use this tool as your FIRST action when starting work on a project — it tells you what you already figured out last time. Save a fact whenever you discover a pattern, convention, or decision that would be expensive to rediscover.

WARNING: Never store file paths, line numbers, function signatures, or code locations. Those go stale when files move or get refactored. Store only invariants: coding style, naming conventions, architectural decisions, tool preferences.`,
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":      {Type: "string", Description: "Project directory. Defaults to current working directory. Memory is stored per-project at <project>/.llm/memory.json."},
			"key":       {Type: "string", Description: "Fact key using dot notation (e.g. 'style.comments', 'conventions.naming', 'decisions.engine'). Avoid keys like 'file.X' or 'location.Y' — those go stale."},
			"value":     {Type: "string", Description: "Value to store. Required when saving. Omit to just recall."},
			"delete":    {Type: "boolean", Description: "Set true to forget this key."},
			"namespace": {Type: "string", Description: "Optional namespace to scope keys (e.g. 'api', 'frontend'). Useful in monorepos. Keys stored as 'namespace.key', recall-all filters to namespace."},
			"prefix":    {Type: "string", Description: "When recalling (no key/value), filter returned facts to keys starting with this prefix. E.g. 'decisions' returns only 'decisions.*' keys."},
			"keys_only": {Type: "boolean", Description: "If true, return only key names (no values). Useful for browsing what's stored before deciding what to recall."},
			"search":    {Type: "string", Description: "Substring to search across all memory keys AND values. Case-insensitive. Returns matching entries. Useful when you remember a fact but forgot the key."},
			"merge":     {Type: "boolean", Description: "If true and key exists, append value to existing value instead of overwriting. Deduplicates entries."},
		},
		Required: []string{},
	},
}

type entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	At    string `json:"at"`
}

type store struct {
	Entries []entry `json:"entries"`
}

const (
	maxEntries = 100
	memoryDir  = ".llm"
	memoryFile = "memory.json"
)

// storePath returns the memory file path for a project directory. Overridable for tests.
var storePath = func(projectDir string) (string, error) {
	root := server.FindProjectRoot(projectDir)
	return filepath.Join(root, memoryDir, memoryFile), nil
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a struct {
		Path      string `json:"path"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		Delete    bool   `json:"delete"`
		Namespace string `json:"namespace"`
		Prefix    string `json:"prefix"`
		KeysOnly  bool   `json:"keys_only"`
		Search    string `json:"search"`
		Merge     bool   `json:"merge"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		a.Path = "."
	}

	fp, err := storePath(a.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot determine memory path: %v", err)
	}

	s := load(fp)

	// Apply namespace as key prefix
	if a.Namespace != "" && a.Key != "" {
		a.Key = a.Namespace + "." + a.Key
	}

	// Delete
	if a.Delete && a.Key != "" {
		s = removeKey(s, a.Key)
		if err := save(fp, s); err != nil {
			return nil, err
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("Forgot %q.", a.Key)}},
		}, nil
	}

	// Save
	if a.Key != "" && a.Value != "" {
		if a.Merge {
			s = mergeEntry(s, a.Key, a.Value)
		} else {
			s = upsert(s, a.Key, a.Value)
		}
		if err := save(fp, s); err != nil {
			return nil, err
		}
		root := filepath.Dir(filepath.Dir(fp)) // memory.json is at root/.llm/memory.json
		ensureGitignore(root)
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("Saved %q.", a.Key)}},
		}, nil
	}

	// Recall (no args, or key-only lookup)
	if a.Key != "" {
		for _, e := range s.Entries {
			if e.Key == a.Key {
				return &server.ToolCallResult{
					Content: []server.ToolCallContent{{Type: "text", Text: e.Value}},
				}, nil
			}
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No memory for %q.", a.Key)}},
		}, nil
	}

	// Search across keys and values
	if a.Search != "" {
		lower := strings.ToLower(a.Search)
		var matched []entry
		for _, e := range s.Entries {
			if strings.Contains(strings.ToLower(e.Key), lower) || strings.Contains(strings.ToLower(e.Value), lower) {
				matched = append(matched, e)
			}
		}
		if len(matched) == 0 {
			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No memories match %q.", a.Search)}},
			}, nil
		}
		var buf strings.Builder
		fmt.Fprintf(&buf, "%d match(es) for %q:\n\n", len(matched), a.Search)
		for _, e := range matched {
			fmt.Fprintf(&buf, "**%s** → %s\n", e.Key, e.Value)
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	// List all (optionally filtered to namespace)
	entries := s.Entries
	if a.Namespace != "" {
		prefix := a.Namespace + "."
		var filtered []entry
		for _, e := range entries {
			if strings.HasPrefix(e.Key, prefix) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if a.Prefix != "" {
		var filtered []entry
		for _, e := range entries {
			if strings.HasPrefix(e.Key, a.Prefix) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if len(entries) == 0 {
		msg := "No memories yet. Use memory(key, value) to save conventions, decisions, and patterns you discover."
		if a.Namespace != "" {
			msg = fmt.Sprintf("No memories for namespace %q.", a.Namespace)
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: msg}},
		}, nil
	}

	if a.KeysOnly {
		var buf strings.Builder
		fmt.Fprintf(&buf, "%d key(s):\n", len(entries))
		for _, e := range entries {
			fmt.Fprintf(&buf, "- %s\n", e.Key)
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	var buf strings.Builder
	if a.Namespace != "" {
		fmt.Fprintf(&buf, "## Project Memory (namespace: %s)\n\n", a.Namespace)
	} else {
		buf.WriteString("## Project Memory\n\n")
	}
	for _, e := range entries {
		fmt.Fprintf(&buf, "**%s** → %s\n", e.Key, e.Value)
	}
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func load(fp string) store {
	data, err := os.ReadFile(fp)
	if err != nil {
		return store{}
	}
	var s store
	json.Unmarshal(data, &s)
	return s
}

func save(fp string, s store) error {
	os.MkdirAll(filepath.Dir(fp), 0755)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(fp, data, 0644)
}

func upsert(s store, key, value string) store {
	// Remove existing entry with same key
	s = removeKey(s, key)
	// Prepend new entry (most recent = first)
	s.Entries = append([]entry{{Key: key, Value: value, At: time.Now().UTC().Format(time.RFC3339)}}, s.Entries...)
	// Evict oldest if over capacity
	if len(s.Entries) > maxEntries {
		s.Entries = s.Entries[:maxEntries]
	}
	return s
}

func mergeEntry(s store, key, value string) store {
	// Find existing entry
	for _, e := range s.Entries {
		if e.Key == key {
			existing := e.Value
			// Split existing by ", " or newlines
			parts := strings.Split(existing, ", ")
			if len(parts) == 1 {
				parts = strings.Split(existing, "\n")
			}
			// Check if new value already present (case-insensitive substring)
			newLower := strings.ToLower(strings.TrimSpace(value))
			for _, p := range parts {
				if strings.Contains(strings.ToLower(strings.TrimSpace(p)), newLower) || strings.Contains(newLower, strings.ToLower(strings.TrimSpace(p))) {
					return s // already present, no change
				}
			}
			// Append with ", " separator
			newValue := existing + ", " + value
			s = removeKey(s, key)
			s.Entries = append([]entry{{Key: key, Value: newValue, At: time.Now().UTC().Format(time.RFC3339)}}, s.Entries...)
			return s
		}
	}
	// Key doesn't exist, just upsert
	return upsert(s, key, value)
}

func removeKey(s store, key string) store {
	var filtered []entry
	for _, e := range s.Entries {
		if e.Key != key {
			filtered = append(filtered, e)
		}
	}
	s.Entries = filtered
	return s
}

func ensureGitignore(root string) {
	fp := filepath.Join(root, ".gitignore")
	data, err := os.ReadFile(fp)
	if err != nil {
		data = nil
	}
	if strings.Contains(string(data), ".llm") {
		return
	}
	// Append .llm/ entry
	f, err := os.OpenFile(fp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(data) > 0 && data[len(data)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString(".llm/\n")
}
