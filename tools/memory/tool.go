// Package memory provides the memory MCP tool: recall and persist project conventions across sessions.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hedtahr/grepfunc/server"
)

// Tool is the memory MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "memory",
	Description: "Remember and recall project-specific conventions, style rules, and decisions " +
		"across sessions. Call WITHOUT arguments to recall everything you've learned. " +
		"Call with key and value to save something for next time. " +
		"Automatically evicts least-recently-used facts when at capacity.\n\n" +
		"Use this tool as your FIRST action when starting work on a project — it tells you " +
		"what you already figured out last time. " +
		"Save a fact whenever you discover a pattern, convention, or decision that would be " +
		"expensive to rediscover.\n\n" +
		"WARNING: Never store file paths, line numbers, function signatures, or code " +
		"locations. " +
		"Those go stale when files move or get refactored. " +
		"Store only invariants: coding style, naming conventions, architectural decisions, " +
		"tool preferences.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type: typeString,
				Description: "Project directory. Optional — defaults to the opened project root. " +
					"Memory is stored per-project at <project>/.llm/memory.json.",
				Items: nil,
			},
			"key": {
				Type: typeString,
				Description: "Fact key using dot notation (e.g. 'style.comments', 'conventions.naming', 'decisions.engine'). " +
					"Avoid keys like 'file.X' or 'location.Y' — those go stale.",
				Items: nil,
			},
			"value": {
				Type:        typeString,
				Description: "Value to store. Required when saving. Omit to just recall.",
				Items:       nil,
			},
			"delete": {Type: typeBoolean, Description: "Set true to forget this key.", Items: nil},
			"namespace": {
				Type: typeString,
				Description: "Optional namespace to scope keys (e.g. 'api', 'frontend'). " +
					"Useful in monorepos. Keys stored as 'namespace.key', recall-all filters to namespace.",
				Items: nil,
			},
			"prefix": {
				Type: typeString,
				Description: "When recalling (no key/value), filter returned facts to keys starting with this prefix. " +
					"E.g. 'decisions' returns only 'decisions.*' keys.",
				Items: nil,
			},
			"keys_only": {
				Type: typeBoolean,
				Description: "If true, return only key names (no values). " +
					"Useful for browsing what's stored before deciding what to recall.",
				Items: nil,
			},
			"search": {
				Type: typeString,
				Description: "Substring to search across all memory keys AND values. Case-insensitive. " +
					"Returns matching entries. Useful when you remember a fact but forgot the key.",
				Items: nil,
			},
			"merge": {
				Type: typeBoolean,
				Description: "If true and key exists, append value to existing value instead of overwriting. " +
					"Deduplicates entries.",
				Items: nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

const (
	maxEntries = 100
	memoryDir  = ".llm"
	memoryFile = "memory.json"

	memoryDirMode  = 0700
	memoryFileMode = 0600
	gitignoreMode  = 0600

	pathKey     = "path"
	keyField    = "key"
	valueField  = "value"
	typeString  = "string"
	typeBoolean = "boolean"
	typeText    = "text"
)

type entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	At    string `json:"at"`
}

type store struct {
	Entries []entry `json:"entries"`
}

type args struct {
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

// storePath returns the memory file path for a project directory. Overridable for tests.
var storePath = func(projectDir string) (string, error) { //nolint:gochecknoglobals // test override point
	root := server.FindProjectRoot(projectDir)
	if root == "" {
		root = projectDir
	}

	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err == nil {
			root = abs
		}
	}

	return filepath.Join(root, memoryDir, memoryFile), nil
}

// Handle serves the memory MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	input, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	if input.Path == "" {
		input.Path = server.ProjectRoot
	} else {
		resolveProject(&input)
	}

	storeFile, err := storePath(input.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot determine memory path: %w", err)
	}

	return dispatch(load(storeFile), storeFile, input)
}

// dispatch routes the request to the delete, save, recall, search, or list handler.
func dispatch(mem store, path string, input args) (*server.ToolCallResult, error) {
	// Apply namespace as key prefix.
	if input.Namespace != "" && input.Key != "" {
		input.Key = input.Namespace + "." + input.Key
	}

	// Delete.
	if input.Delete && input.Key != "" {
		return handleDelete(mem, path, input.Key)
	}

	// Save.
	if input.Key != "" && input.Value != "" {
		return handleSave(mem, path, input.Key, input.Value, input.Merge)
	}

	// Recall (no args, or key-only lookup).
	if input.Key != "" {
		return handleRecall(mem, input.Key), nil
	}

	// Search across keys and values.
	if input.Search != "" {
		return handleSearch(mem, input.Search), nil
	}

	return handleList(mem, input), nil
}

// parseArgs decodes the tool arguments.
func parseArgs(raw json.RawMessage) (args, error) {
	var input args

	err := json.Unmarshal(raw, &input)
	if err != nil {
		return input, fmt.Errorf("invalid arguments: %w", err)
	}

	return input, nil
}

// resolveProject pins ProjectRoot to the requested directory when it exists.
func resolveProject(input *args) {
	resolved := input.Path

	r, err := filepath.EvalSymlinks(input.Path)
	if err == nil {
		resolved = r
	}

	info, err := os.Stat(resolved)
	if err == nil && info.IsDir() {
		server.ProjectRoot = resolved
	}
}

// handleDelete removes a key and persists the store.
func handleDelete(mem store, path, key string) (*server.ToolCallResult, error) {
	mem = removeKey(mem, key)

	err := save(path, mem)
	if err != nil {
		return nil, err
	}

	return textResult(fmt.Sprintf("Forgot %q.", key)), nil
}

// handleSave upserts or merges a key/value and persists the store.
func handleSave(mem store, path, key, value string, merge bool) (*server.ToolCallResult, error) {
	if merge {
		mem = mergeEntry(mem, key, value)
	} else {
		mem = upsert(mem, key, value)
	}

	err := save(path, mem)
	if err != nil {
		return nil, err
	}

	root := filepath.Dir(filepath.Dir(path)) // memory.json is at root/.llm/memory.json.
	ensureGitignore(root)

	return textResult(fmt.Sprintf("Saved %q.", key)), nil
}

// handleRecall returns the value for an exact key match.
func handleRecall(mem store, key string) *server.ToolCallResult {
	for _, e := range mem.Entries {
		if e.Key == key {
			return textResult(e.Value)
		}
	}

	return textResult(fmt.Sprintf("No memory for %q.", key))
}

// handleSearch returns entries whose key or value contains the query.
func handleSearch(mem store, query string) *server.ToolCallResult {
	lower := strings.ToLower(query)

	var matched []entry

	for _, e := range mem.Entries {
		if strings.Contains(strings.ToLower(e.Key), lower) || strings.Contains(strings.ToLower(e.Value), lower) {
			matched = append(matched, e)
		}
	}

	if len(matched) == 0 {
		return textResult(fmt.Sprintf("No memories match %q.", query))
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "%d matches for %q:\n```\n", len(matched), query)

	for _, e := range matched {
		fmt.Fprintf(&buf, "%s → %s\n", e.Key, e.Value)
	}

	buf.WriteString("```")

	return textResult(buf.String())
}

// handleList returns all entries, optionally filtered by namespace or prefix.
func handleList(mem store, input args) *server.ToolCallResult {
	entries := mem.Entries

	if input.Namespace != "" {
		entries = filterPrefix(entries, input.Namespace+".")
	}

	if input.Prefix != "" {
		entries = filterPrefix(entries, input.Prefix)
	}

	if len(entries) == 0 {
		msg := "No memories yet. Use memory(key, value) to save conventions, " +
			"decisions, and patterns you discover."
		if input.Namespace != "" {
			msg = fmt.Sprintf("No memories for namespace %q.", input.Namespace)
		}

		return textResult(msg)
	}

	if input.KeysOnly {
		return textResult(keysOnlyText(entries))
	}

	return textResult(listText(entries, input.Namespace))
}

// filterPrefix keeps entries whose key starts with the prefix.
func filterPrefix(entries []entry, prefix string) []entry {
	var filtered []entry

	for _, e := range entries {
		if strings.HasPrefix(e.Key, prefix) {
			filtered = append(filtered, e)
		}
	}

	return filtered
}

// keysOnlyText renders the key list.
func keysOnlyText(entries []entry) string {
	var buf strings.Builder

	fmt.Fprintf(&buf, "%d key(s):\n", len(entries))

	for _, e := range entries {
		fmt.Fprintf(&buf, "- %s\n", e.Key)
	}

	return buf.String()
}

// listText renders the full memory listing, optionally namespaced.
func listText(entries []entry, namespace string) string {
	var buf strings.Builder

	if namespace != "" {
		fmt.Fprintf(&buf, "Project Memory (namespace: %s)\n```\n", namespace)
	} else {
		buf.WriteString("Project Memory\n```\n")
	}

	for _, e := range entries {
		fmt.Fprintf(&buf, "%s → %s\n", e.Key, e.Value)
	}

	buf.WriteString("```")

	return buf.String()
}

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
}

// load reads the store, returning an empty store when the file is missing or unreadable.
func load(path string) store {
	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return store{Entries: nil}
	}

	var mem store

	_ = json.Unmarshal(data, &mem)

	return mem
}

// save persists the store to the memory file with a private permissions mask.
func save(path string, mem store) error {
	err := os.MkdirAll(filepath.Dir(path), memoryDirMode)
	if err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}

	data, err := json.MarshalIndent(mem, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory: %w", err)
	}

	err = os.WriteFile(path, data, memoryFileMode)
	if err != nil {
		return fmt.Errorf("write memory: %w", err)
	}

	return nil
}

func upsert(mem store, key, value string) store {
	// Remove existing entry with same key.
	mem = removeKey(mem, key)

	// Prepend new entry (most recent = first).
	mem.Entries = append([]entry{{Key: key, Value: value, At: time.Now().UTC().Format(time.RFC3339)}}, mem.Entries...)

	// Evict oldest if over capacity.
	if len(mem.Entries) > maxEntries {
		mem.Entries = mem.Entries[:maxEntries]
	}

	return mem
}

func mergeEntry(mem store, key, value string) store {
	for _, e := range mem.Entries {
		if e.Key == key {
			existing := e.Value

			// Split existing by ", " or newlines.
			parts := strings.Split(existing, ", ")
			if len(parts) == 1 {
				parts = strings.Split(existing, "\n")
			}

			// Check if new value already present (case-insensitive exact match).
			newVal := strings.TrimSpace(value)
			for _, p := range parts {
				if strings.EqualFold(strings.TrimSpace(p), newVal) {
					return mem // already present, no change.
				}
			}

			// Append with ", " separator.
			newValue := existing + ", " + value
			mem = removeKey(mem, key)
			mem.Entries = append([]entry{{Key: key, Value: newValue, At: time.Now().UTC().Format(time.RFC3339)}}, mem.Entries...)

			return mem
		}
	}

	// Key doesn't exist, just upsert.
	return upsert(mem, key, value)
}

func removeKey(mem store, key string) store {
	var filtered []entry

	for _, e := range mem.Entries {
		if e.Key != key {
			filtered = append(filtered, e)
		}
	}

	mem.Entries = filtered

	return mem
}

// ensureGitignore adds a .llm/ entry to the project .gitignore so memories stay local.
func ensureGitignore(root string) {
	gitignorePath := filepath.Join(root, ".gitignore")

	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		data = nil
	}

	if strings.Contains(string(data), ".llm") {
		return
	}

	// #nosec G304 -- paths bounds-checked by server
	file, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, gitignoreMode)
	if err != nil {
		return
	}

	defer func() { _ = file.Close() }()

	// Append .llm/ entry.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, _ = file.WriteString("\n")
	}

	_, _ = file.WriteString(".llm/\n")
}
