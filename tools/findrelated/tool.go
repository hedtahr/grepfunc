package findrelated

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"mcp_patch_file/server"
)

var Tool = server.Tool{
	Name:        "find_related",
	Description: "Find files RELATED to a given file — tests, mocks, sibling implementations, config files. The fastest way to answer 'where's the test for this?' or 'what other files do I need to touch?' Stops you from guessing file names and wasting tokens on failed reads. Finds: *_test.*, *.test.*, *_mock.*, mock_*, *.spec.*, and same-named files in nearby directories.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "Path to the source file. Relative to project root or absolute. Defaults to last file from file_head/patch_file/find_related in this session."},
			"compact": {Type: "boolean", Description: "Terse output: less whitespace, no category labels. Default false."},
		},
		Required: []string{},
	},
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a struct {
		Path    string `json:"path"`
		Compact bool   `json:"compact"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		a.Path = server.LastPath
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required (no previous path in session)")
	}
	a.Path = server.ResolvePath(a.Path)
	server.SetLastPath(a.Path)
	related := findRelated(a.Path)

	compact := a.Compact
	var buf strings.Builder
	if len(related) == 0 {
		fmt.Fprintf(&buf, "No related files found for %s.\n", a.Path)
	} else {
		if compact {
			fmt.Fprintf(&buf, "%d related %s:\n", len(related), a.Path)
		} else {
			fmt.Fprintf(&buf, "%d file(s) related to %s:\n\n", len(related), a.Path)
		}
		for _, r := range related {
			if compact {
				fmt.Fprintf(&buf, "- `%s`\n", server.RelPath(r))
			} else {
				category := categorize(r, a.Path)
				fmt.Fprintf(&buf, "- %s → `%s`\n", category, server.RelPath(r))
			}
		}
	}
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func findRelated(filePath string) []string {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	if ext == "" {
		return nil
	}

	seen := make(map[string]bool)
	var results []string

	// 1. Same directory: test/mock/spec files
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err == nil {
		for _, e := range entries {
			entryBase := filepath.Base(e)
			if entryBase == base {
				continue
			}
			if isRelatedName(name, entryBase) {
				if !seen[e] {
					seen[e] = true
					results = append(results, e)
				}
			}
		}
	}

	// 1a. Same directory: siblings with same extension, capped at 5, sorted by name similarity
	if ext != "" {
		allSibs, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
		var candSibs []string
		for _, s := range allSibs {
			if s == filePath || seen[s] {
				continue
			}
			if !isRelatedName(name, filepath.Base(s)) {
				candSibs = append(candSibs, s)
			}
		}
		sort.Slice(candSibs, func(i, j int) bool {
			ni := strings.TrimSuffix(filepath.Base(candSibs[i]), ext)
			nj := strings.TrimSuffix(filepath.Base(candSibs[j]), ext)
			return commonPrefixLen(name, ni) > commonPrefixLen(name, nj)
		})
		if len(candSibs) > 5 {
			candSibs = candSibs[:5]
		}
		for _, s := range candSibs {
			seen[s] = true
			results = append(results, s)
		}
	}

	// 2. Walk up to find sibling directories with same-named files
	projectRoot := server.FindProjectRoot(dir)
	if filepath.Dir(projectRoot) == projectRoot {
		projectRoot = dir
	}

	filepath.WalkDir(projectRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" ||
				base == ".idea" || base == "__pycache__" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if seen[p] || p == filePath {
			return nil
		}
		entryBase := filepath.Base(p)
		if isRelatedName(name, entryBase) {
			if !seen[p] {
				seen[p] = true
				results = append(results, p)
			}
		}
		return nil
	})

	sort.Strings(results)

	var prioritized []string
	var otherDir []string
	for _, r := range results {
		cat := categorize(r, filePath)
		if cat == "📁 other dir" {
			otherDir = append(otherDir, r)
		} else {
			prioritized = append(prioritized, r)
		}
	}
	if len(otherDir) > 3 {
		otherDir = otherDir[:3]
	}
	results = append(prioritized, otherDir...)
	if len(results) > 20 {
		results = results[:20]
	}
	return results
}

func isRelatedName(name, entry string) bool {
	entryExt := filepath.Ext(entry)
	entryName := strings.TrimSuffix(entry, entryExt)

	testPatterns := []string{
		name + "_test", name + ".test", "test_" + name,
		name + "Test", name + ".spec", name + "_spec", name + "Spec",
	}
	mockPatterns := []string{
		name + "_mock", "mock_" + name, name + "Mock",
	}
	if len(name) > 0 {
		mockPatterns = append(mockPatterns, "mock"+strings.ToUpper(name[:1])+name[1:])
	}
	samePatterns := []string{name, toSnake(name), toCamel(name)}

	for _, p := range append(append(testPatterns, mockPatterns...), samePatterns...) {
		if strings.EqualFold(entryName, p) {
			return true
		}
	}
	return false
}

func categorize(related, original string) string {
	origBase := filepath.Base(original)
	origExt := filepath.Ext(origBase)
	origName := strings.TrimSuffix(origBase, origExt)
	base := filepath.Base(related)

	if isRelatedName(origName, base) {
		lower := strings.ToLower(base)
		if strings.Contains(lower, "_test") || strings.Contains(lower, ".test.") ||
			strings.HasPrefix(lower, "test_") ||
			strings.Contains(lower, ".spec") || strings.Contains(lower, "_spec") ||
			strings.HasPrefix(lower, "spec_") {
			return "🧪 test"
		}
		if strings.Contains(lower, "_mock") || strings.HasPrefix(lower, "mock_") ||
			strings.Contains(lower, "mock.") {
			return "🎭 mock/stub"
		}
	}
	if filepath.Dir(related) == filepath.Dir(original) {
		return "📄 sibling"
	}
	return "📁 other dir"
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func toSnake(s string) string {
	var buf strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				buf.WriteByte('_')
			}
			buf.WriteRune(r + 32)
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func toCamel(s string) string {
	var buf strings.Builder
	upper := true
	for _, r := range s {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			if r >= 'a' && r <= 'z' {
				buf.WriteRune(r - 32)
			} else {
				buf.WriteRune(r)
			}
			upper = false
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}
