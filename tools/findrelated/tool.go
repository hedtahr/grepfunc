// Package findrelated provides the find_related MCP tool: locate tests, mocks, and sibling files.
package findrelated

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

// Tool is the find_related MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "find_related",
	Description: "Find files RELATED to a file: tests (*_test.*, *.spec.*), mocks (mock_*, *_mock.*), siblings, same-named files nearby.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        "string",
				Description: "Source file to find related files for. Defaults to last file operated on.",
				Items:       nil,
			},
			"compact": {
				Type:        "boolean",
				Description: "Terse output: no category labels.",
				Items:       nil,
			},
			"with_symbols": {
				Type:        "boolean",
				Description: "Include top-level func/type names found in each related file.",
				Items:       nil,
			},
			"token_budget": {
				Type:        "integer",
				Description: "Max output chars. Overflow → line-boundary truncation.",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

const (
	pathKey     = "path"
	typeText    = "text"
	catTest     = "🧪 test"
	catMock     = "🎭 mock/stub"
	catSibling  = "📄 sibling"
	catOtherDir = "📁 other dir"

	maxSiblings = 5
	maxDepth    = 4
	maxOtherDir = 3
	maxResults  = 20
	maxSymbols  = 5
)

var errPathRequired = errors.New("path is required (no previous path in session)")

type args struct {
	Path        string `json:"path"`
	Compact     bool   `json:"compact"`
	WithSymbols bool   `json:"with_symbols"`
	TokenBudget int    `json:"token_budget"`
}

// Handle serves the find_related MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var input args

	err := json.Unmarshal(raw, &input)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if input.Path == "" {
		input.Path = server.LastPath
	}

	if input.Path == "" {
		return nil, errPathRequired
	}

	input.Path = server.ResolvePath(input.Path)

	err = server.CheckBounds(input.Path)
	if err != nil {
		return nil, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(input.Path)
	if err != nil {
		return nil, fmt.Errorf("check banned: %w", err)
	}

	server.SetLastPath(input.Path)
	related := findRelated(input.Path)

	return server.BudgetResult(textResult(renderResults(related, input)), input.TokenBudget), nil
}

// renderResults formats the related-file list, or a no-results notice.
func renderResults(related []string, input args) string {
	var buf strings.Builder

	if len(related) == 0 {
		fmt.Fprintf(&buf, "No related files found for %s.\n", input.Path)

		return buf.String()
	}

	if input.Compact {
		fmt.Fprintf(&buf, "%d related %s:\n", len(related), input.Path)
	} else {
		fmt.Fprintf(&buf, "%d files related to %s:\n", len(related), input.Path)
	}

	buf.WriteString("```\n")

	for _, relatedPath := range related {
		relPath := server.RelPath(relatedPath)
		if input.Compact {
			fmt.Fprintf(&buf, "%s\n", relPath)
		} else {
			symStr := ""
			if input.WithSymbols {
				symStr = extractSymbols(relatedPath)
			}

			fmt.Fprintf(&buf, "%s%s\n", relPath, symStr)
		}
	}

	buf.WriteString("```\n")

	return buf.String()
}

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
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

	results := sameDirRelated(dir, name, base, seen)

	// 1a. Same directory: siblings with same extension, capped and sorted by name similarity.
	results = append(results, siblingCandidates(dir, filePath, name, ext, seen)...)

	// 2. Walk up to find sibling directories with same-named files.
	root := searchRoot(dir)
	results = append(results, walkRelated(root, name, filePath, seen)...)

	return prioritizeResults(results, filePath)
}

// sameDirRelated finds test/mock/spec files next to the source file.
func sameDirRelated(dir, name, base string, seen map[string]bool) []string {
	entries, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil
	}

	var results []string

	for _, entry := range entries {
		entryBase := filepath.Base(entry)
		if entryBase == base {
			continue
		}

		if isRelatedName(name, entryBase) && !seen[entry] {
			seen[entry] = true

			results = append(results, entry)
		}
	}

	return results
}

// siblingCandidates finds same-extension files in the same directory, closest-name matches first.
func siblingCandidates(dir, filePath, name, ext string, seen map[string]bool) []string {
	if ext == "" {
		return nil
	}

	allSibs, _ := filepath.Glob(filepath.Join(dir, "*"+ext))

	candidates := make([]string, 0, len(allSibs))

	for _, sib := range allSibs {
		if sib == filePath || seen[sib] {
			continue
		}

		if !isRelatedName(name, filepath.Base(sib)) {
			candidates = append(candidates, sib)
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		first := strings.TrimSuffix(filepath.Base(candidates[i]), ext)
		second := strings.TrimSuffix(filepath.Base(candidates[j]), ext)

		return commonPrefixLen(name, first) > commonPrefixLen(name, second)
	})

	if len(candidates) > maxSiblings {
		candidates = candidates[:maxSiblings]
	}

	for _, sib := range candidates {
		seen[sib] = true
	}

	return candidates
}

// searchRoot returns the project root to walk, falling back to the source dir
// when no root is found or the walk would go too deep.
func searchRoot(dir string) string {
	root := server.FindProjectRoot(dir)
	if root == "" {
		root = dir
	}

	if filepath.Dir(root) == root {
		root = dir
	}

	// Cap depth: if projectRoot is >4 levels above dir, fall back to dir.
	if depthBetween(root, dir) > maxDepth {
		root = dir
	}

	return root
}

// walkRelated walks the project root for same-named files in sibling directories.
func walkRelated(root, name, filePath string, seen map[string]bool) []string {
	var results []string

	_ = filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}

		if dirEntry.IsDir() {
			if skipWalkDir(dirEntry.Name()) {
				return filepath.SkipDir
			}

			// Skip dirs >4 levels deep from projectRoot.
			if depthBetween(root, path) > maxDepth {
				return filepath.SkipDir
			}

			return nil
		}

		if seen[path] || path == filePath {
			return nil
		}

		if isRelatedName(name, filepath.Base(path)) {
			seen[path] = true

			results = append(results, path)
		}

		return nil
	})

	return results
}

// skipWalkDir reports whether a directory should be pruned from the related-file walk.
func skipWalkDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "vendor" ||
		name == ".idea" || name == "__pycache__" || strings.HasPrefix(name, ".")
}

// prioritizeResults orders test/mock/sibling hits first and caps the result list.
func prioritizeResults(results []string, filePath string) []string {
	var (
		prioritized []string
		otherDir    []string
	)

	for _, result := range results {
		if categorize(result, filePath) == catOtherDir {
			otherDir = append(otherDir, result)
		} else {
			prioritized = append(prioritized, result)
		}
	}

	if len(otherDir) > maxOtherDir {
		otherDir = otherDir[:maxOtherDir]
	}

	prioritized = append(prioritized, otherDir...)
	if len(prioritized) > maxResults {
		prioritized = prioritized[:maxResults]
	}

	return prioritized
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
		if isTestName(lower) {
			return catTest
		}

		if isMockName(lower) {
			return catMock
		}
	}

	if filepath.Dir(related) == filepath.Dir(original) {
		return catSibling
	}

	return catOtherDir
}

// isTestName reports whether a lowercased base name looks like a test file.
func isTestName(lower string) bool {
	return strings.Contains(lower, "_test") || strings.Contains(lower, ".test.") ||
		strings.HasPrefix(lower, "test_") ||
		strings.Contains(lower, ".spec") || strings.Contains(lower, "_spec") ||
		strings.HasPrefix(lower, "spec_")
}

// isMockName reports whether a lowercased base name looks like a mock/stub file.
func isMockName(lower string) bool {
	return strings.Contains(lower, "_mock") || strings.HasPrefix(lower, "mock_") ||
		strings.Contains(lower, "mock.")
}

func commonPrefixLen(a, b string) int {
	minLen := min(len(b), len(a))
	for i := range minLen {
		if a[i] != b[i] {
			return i
		}
	}

	return minLen
}

func toSnake(str string) string {
	var buf strings.Builder

	for i, char := range str {
		if char >= 'A' && char <= 'Z' {
			if i > 0 {
				buf.WriteByte('_')
			}

			buf.WriteRune(char - 'A' + 'a')
		} else {
			buf.WriteRune(char)
		}
	}

	return buf.String()
}

func toCamel(str string) string {
	var buf strings.Builder

	upper := true

	for _, char := range str {
		if char == '_' {
			upper = true

			continue
		}

		if upper {
			if char >= 'a' && char <= 'z' {
				buf.WriteRune(char - 'a' + 'A')
			} else {
				buf.WriteRune(char)
			}

			upper = false
		} else {
			buf.WriteRune(char)
		}
	}

	return buf.String()
}

func depthBetween(root, child string) int {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return 0
	}

	if rel == "." {
		return 0
	}

	return len(strings.Split(rel, string(filepath.Separator)))
}

var matchAll = regexp.MustCompile(`\w`)

func extractSymbols(filePath string) string {
	fi, err := os.Stat(filePath)
	if err != nil || fi.Size() > 100_000 {
		return ""
	}

	funcs, _ := grepfunc.Search(filePath, "*", matchAll, maxSymbols, grepfunc.IsFuncSig)
	types, _ := grepfunc.Search(filePath, "*", matchAll, maxSymbols, grepfunc.IsStructSig)

	var names []string

	seen := map[string]bool{}
	for _, fn := range funcs {
		if !seen[fn.Name] {
			seen[fn.Name] = true

			names = append(names, fn.Name)
		}
	}

	for _, tp := range types {
		if !seen[tp.Name] {
			seen[tp.Name] = true

			names = append(names, tp.Name)
		}
	}

	if len(names) == 0 {
		return ""
	}

	if len(names) > maxSymbols {
		names = names[:maxSymbols]
	}

	return " (" + strings.Join(names, ", ") + ")"
}
