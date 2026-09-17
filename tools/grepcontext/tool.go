// Package grepcontext returns matching lines with surrounding context.
package grepcontext

import (
	"bytes"
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

// Schema keys used in the tool definition.
const (
	schemaString  = "string"
	schemaInteger = "integer"
	schemaBoolean = "boolean"
	schemaText    = "text"
)

// Caps for result sizes.
const (
	maxResultsCap = 50
	maxContextCap = 10
)

// errPatternRequired is returned when no pattern is supplied.
var errPatternRequired = errors.New("pattern is required")

// Tool defines the grep_context MCP tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_context",
	Description: "Matching lines with context for non-function patterns (constants, imports, config). N lines before/after, deduplicated. Skips .gitignore'd dirs.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern": {
				Type:        schemaString,
				Items:       nil,
				Description: "Regex to search for.",
			},
			"path": {
				Type:        schemaString,
				Items:       nil,
				Description: "Directory to search. Defaults to project root.",
			},
			"include": {
				Type:        schemaString,
				Items:       nil,
				Description: "Glob filter. Default: all source files.",
			},
			"context_lines": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Lines before/after each match. Default 3, max 10.",
			},
			"case_sensitive": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Default false.",
			},
			"max_results": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Max matches. Default 20, max 50.",
			},
			"offset": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Pagination offset (0-based).",
			},
			"compact": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Terse output.",
			},
			"scope": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Annotate with enclosing function/type name.",
			},
			"group_by_file": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Group results under file headers.",
			},
			"names_only": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Only file:line — cheapest.",
			},
			"token_budget": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Max output chars; degrades to file:line, then truncates.",
			},
			"count_only": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Match counts per file only — cheapest.",
			},
		},
		AdditionalProperties: false,
		Required:             []string{"pattern"},
	},
}

type args struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	ContextLines  int    `json:"context_lines"`
	CaseSensitive bool   `json:"case_sensitive"`
	MaxResults    int    `json:"max_results"`
	Offset        int    `json:"offset"`
	Compact       bool   `json:"compact"`
	Scope         bool   `json:"scope"`
	GroupByFile   bool   `json:"group_by_file"`
	NamesOnly     bool   `json:"names_only"`
	TokenBudget   int    `json:"token_budget"`
	CountOnly     bool   `json:"count_only"`
}

type window struct {
	relPath   string
	matchLine int
	start     int
	end       int
	lines     []string
	scope     string
}

// Handle processes a grep_context tool call.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Pattern == "" {
		return nil, errPatternRequired
	}

	patternRe, err := grepfunc.CompilePattern(arg.Pattern, arg.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	resolved := server.ResolvePath(arg.Path)
	if err := server.CheckBounds(resolved); err != nil {
		return nil, err
	}

	applyDefaults(&arg)

	need := arg.Offset + arg.MaxResults

	all, scannedAll, err := scanWindows(arg, patternRe, resolved, need)
	if err != nil && len(all) == 0 {
		return nil, fmt.Errorf("walk %s: %w", resolved, err)
	}

	total := len(all)
	start := min(arg.Offset, total)
	end := min(arg.Offset+arg.MaxResults, total)
	page := all[start:end]

	if arg.CountOnly {
		return renderCountOnly(arg, all, total, scannedAll), nil
	}

	if total == 0 {
		var buf strings.Builder

		writeZeroMatches(&buf, arg)

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
			IsError: false,
		}, nil
	}

	suffix := ""
	if !scannedAll {
		suffix = "+"
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{
			Type: schemaText,
			Text: renderMatches(arg, resolved, page, total, start, end, suffix),
		}},
		IsError: false,
	}, nil
}

// renderMatches writes the match list with footer and budget handling.
func renderMatches(arg args, resolved string, page []window, total, start, end int, suffix string) string {
	var buf strings.Builder

	writeMatchHeader(&buf, arg, total, suffix, start, end)

	switch {
	case arg.NamesOnly:
		writeNamesOnly(&buf, page)
	case arg.GroupByFile:
		writeGrouped(&buf, arg, page)
	default:
		writeFlat(&buf, arg, page)
	}

	if end < total {
		fmt.Fprintf(&buf, "%d more. Use offset=%d.\n", total-end, end)
	}

	info, statErr := os.Stat(resolved)
	if statErr == nil && !info.IsDir() {
		server.SetLastPath(resolved)
	}

	output := buf.String()
	if arg.TokenBudget > 0 && len(output) > arg.TokenBudget {
		output = terseOutput(arg, page, total, start, end, suffix)
	}

	return output
}

// applyDefaults fills in default values for unset args.
func applyDefaults(arg *args) {
	if arg.MaxResults <= 0 {
		arg.MaxResults = 20
	}

	if arg.MaxResults > maxResultsCap {
		arg.MaxResults = maxResultsCap
	}

	if arg.ContextLines <= 0 {
		arg.ContextLines = 3
	}

	if arg.ContextLines > maxContextCap {
		arg.ContextLines = maxContextCap
	}
}

// scanWindows walks the tree collecting context windows around matches.
func scanWindows(arg args, patternRe *regexp.Regexp, resolved string, need int) ([]window, bool, error) {
	var all []window

	scannedAll := true

	glob := arg.Include

	if glob == "" {
		glob = "*"
	}

	matcher := grepfunc.CompileGlob(glob)

	err := filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if isSkippableDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		wins, err := scanWindowFile(arg, resolved, path, glob, matcher, entry, patternRe, need-len(all))
		if err != nil {
			return err
		}

		all = append(all, wins...)

		if len(all) >= need {
			scannedAll = false

			return filepath.SkipAll
		}

		return nil
	})
	if err != nil && len(all) == 0 {
		return nil, false, fmt.Errorf("walk %s: %w", resolved, err)
	}

	return all, scannedAll, nil
}

// scanWindowFile parses one file and builds its context windows.
func scanWindowFile(arg args, resolved, path, glob string, matcher *grepfunc.GlobMatcher, entry fs.DirEntry,
	patternRe *regexp.Regexp, need int) ([]window, error) {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil, nil
	}

	rel, _ := filepath.Rel(resolved, path)
	if !matcher.Match(rel) {
		return nil, nil
	}

	info, err := entry.Info()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Size() > 2*1024*1024 {
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if grepfunc.IsBinaryExt(ext) {
		return nil, nil
	}

	if glob == "*" && grepfunc.IsNonSourceExt(ext) {
		return nil, nil
	}

	data, err := os.ReadFile(path) // #nosec G122,G304 -- paths bounds-checked by server
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return matchWindows(data, rel, arg, patternRe, need), nil
}

// isSkippableDir reports whether a directory should be excluded from searches.
func isSkippableDir(base string) bool {
	return base == ".git" || base == "node_modules" || base == "vendor" || base == ".idea" ||
		base == "__pycache__" || strings.HasPrefix(base, ".")
}

// matchWindows builds context windows for all pattern hits in one file. Matching
// runs over the raw bytes first, so a file without hits is never split or
// materialised line by line.
func matchWindows(data []byte, rel string, arg args, patternRe *regexp.Regexp, need int) []window {
	if bytes.IndexByte(data, '\r') >= 0 {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	}

	hits := patternRe.FindAllIndex(data, -1)
	if len(hits) == 0 {
		return nil
	}

	starts := lineStarts(data)

	var (
		lines      [][]byte
		boundaries grepfunc.BlockBoundaries
	)

	if arg.Scope {
		lines = bytes.Split(data, []byte("\n"))
		boundaries = grepfunc.MapBlockBoundaries(lines, combinedSig)
	}

	windows := make([]window, 0, min(need, len(hits)))

	prevEnd := -1

	for _, hit := range hits {
		matchLineIdx := lineIndexAt(starts, hit[0])
		if matchLineIdx <= prevEnd {
			continue // covered by previous window
		}

		endLineIdx := lineIndexAt(starts, max(hit[1]-1, hit[0]))
		start := max(0, matchLineIdx-arg.ContextLines)
		end := min(len(starts)-1, endLineIdx+arg.ContextLines)

		windows = append(windows, window{
			relPath:   rel,
			matchLine: matchLineIdx + 1,
			start:     start,
			end:       end,
			lines:     windowLines(data, starts, start, end),
			scope:     scopeName(lines, boundaries, matchLineIdx),
		})

		prevEnd = end

		if len(windows) >= need {
			break
		}
	}

	return windows
}

// matchesScopeSig reports whether a line starts a function or type block.
func combinedSig(line []byte) bool {
	return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line)
}

// lineStarts returns the byte offset of every line start, matching the line
// count of bytes.Split(data, "\n").
func lineStarts(data []byte) []int {
	starts := make([]int, 1, 64+bytes.Count(data, []byte("\n")))

	for i, b := range data {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}

	return starts
}

// lineIndexAt returns the index of the line containing byte offset ofs.
func lineIndexAt(starts []int, ofs int) int {
	idx := sort.SearchInts(starts, ofs+1) - 1

	return max(idx, 0)
}

// windowLines materialises just the lines of one window.
func windowLines(data []byte, starts []int, start, end int) []string {
	lines := make([]string, 0, end-start+1)

	for i := start; i <= end; i++ {
		lineEnd := len(data)
		if i+1 < len(starts) {
			lineEnd = starts[i+1] - 1
		}

		lines = append(lines, string(data[starts[i]:lineEnd]))
	}

	return lines
}

// renderCountOnly builds the per-file match count output.
func renderCountOnly(arg args, all []window, total int, scannedAll bool) *server.ToolCallResult {
	counts := make(map[string]int)

	var files []string

	for _, win := range all {
		if _, seen := counts[win.relPath]; !seen {
			files = append(files, win.relPath)
		}

		counts[win.relPath]++
	}

	sort.Strings(files)

	suffix := ""
	if !scannedAll {
		suffix = "+"
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "%d%s matches for %q\n", total, suffix, arg.Pattern)

	if total == 0 {
		buf.WriteString(grepfunc.ZeroMatchHint(arg.Include))
	}

	buf.WriteString("```\n")

	for _, file := range files {
		fmt.Fprintf(&buf, "%s: %d\n", file, counts[file])
	}

	buf.WriteString("```\n")

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
		IsError: false,
	}
}

// writeZeroMatches writes the header for an empty result set.
func writeZeroMatches(buf *strings.Builder, arg args) {
	if arg.Compact {
		fmt.Fprintf(buf, "0 matches %q\n", arg.Pattern)
	} else {
		fmt.Fprintf(buf, "0 matches for %q\n", arg.Pattern)
	}

	buf.WriteString(grepfunc.ZeroMatchHint(arg.Include))
}

// writeMatchHeader writes the summary line and leading whitespace.
func writeMatchHeader(buf *strings.Builder, arg args, total int, suffix string, start, end int) {
	if arg.Compact {
		fmt.Fprintf(buf, "%d%s matches %q", total, suffix, arg.Pattern)
	} else {
		fmt.Fprintf(buf, "%d%s matches for %q", total, suffix, arg.Pattern)
	}

	if arg.Offset > 0 || end < total {
		fmt.Fprintf(buf, " (showing %d\u2013%d)", start+1, end)
	}

	buf.WriteByte('\n')

	if !arg.Compact {
		buf.WriteByte('\n')
	}
}

// writeNamesOnly writes file:line entries only.
func writeNamesOnly(buf *strings.Builder, page []window) {
	buf.WriteString("```\n")

	for _, win := range page {
		fmt.Fprintf(buf, "%s:%d\n", win.relPath, win.matchLine)
	}

	buf.WriteString("```\n")
}

// writeGrouped writes matches grouped under file headers.
func writeGrouped(buf *strings.Builder, arg args, page []window) {
	for _, group := range groupWindows(page) {
		if arg.Compact {
			fmt.Fprintf(buf, "%s (%d)\n", group.relPath, len(group.windows))
		} else {
			fmt.Fprintf(buf, "\n%s — %d matches\n", group.relPath, len(group.windows))
		}

		for _, win := range group.windows {
			renderWindow(buf, win, arg)
		}
	}
}

// writeFlat writes matches one window per file.
func writeFlat(buf *strings.Builder, arg args, page []window) {
	for _, win := range page {
		renderWindow(buf, win, arg)
	}
}

// fileGroup groups windows by file for the group_by_file output.
type fileGroup struct {
	relPath string
	windows []window
}

// groupWindows buckets windows by their relative path, preserving order.
func groupWindows(page []window) []fileGroup {
	var groups []fileGroup

	seen := map[string]int{}
	for _, win := range page {
		if idx, ok := seen[win.relPath]; ok {
			groups[idx].windows = append(groups[idx].windows, win)
		} else {
			seen[win.relPath] = len(groups)
			groups = append(groups, fileGroup{relPath: win.relPath, windows: []window{win}})
		}
	}

	return groups
}

// renderWindow writes one context window.
func renderWindow(buf *strings.Builder, win window, arg args) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(win.relPath)), ".")
	writeWindowHeader(buf, win, arg.GroupByFile)

	buf.WriteString("```")
	buf.WriteString(ext)
	buf.WriteByte('\n')

	for idx, line := range win.lines {
		lineNum := win.start + idx + 1
		if lineNum == win.matchLine {
			fmt.Fprintf(buf, "> %d: %s\n", lineNum, line)
		} else {
			fmt.Fprintf(buf, "  %d: %s\n", lineNum, line)
		}
	}

	buf.WriteString("```")

	if arg.Compact {
		buf.WriteByte('\n')
	} else {
		buf.WriteString("\n\n")
	}
}

// writeWindowHeader writes the location line of a window.
func writeWindowHeader(buf *strings.Builder, win window, groupByFile bool) {
	if groupByFile {
		if win.scope != "" {
			fmt.Fprintf(buf, ":%d [%s]\n", win.matchLine, win.scope)
		} else {
			fmt.Fprintf(buf, ":%d\n", win.matchLine)
		}

		return
	}

	if win.scope != "" {
		fmt.Fprintf(buf, "%s:%d [%s]:\n", win.relPath, win.matchLine, win.scope)
	} else {
		fmt.Fprintf(buf, "%s:%d:\n", win.relPath, win.matchLine)
	}
}

// terseOutput rebuilds output as file:line hits when the budget is exceeded.
func terseOutput(arg args, page []window, total, start, end int, suffix string) string {
	var terse strings.Builder

	fmt.Fprintf(&terse, "%d%s matches %q", total, suffix, arg.Pattern)

	if arg.Offset > 0 || end < total {
		fmt.Fprintf(&terse, " (showing %d\u2013%d)", start+1, end)
	}

	terse.WriteByte('\n')
	terse.WriteString("```\n")

	for _, win := range page {
		if win.scope != "" {
			fmt.Fprintf(&terse, "%s:%d [%s]\n", win.relPath, win.matchLine, win.scope)
		} else {
			fmt.Fprintf(&terse, "%s:%d\n", win.relPath, win.matchLine)
		}
	}

	terse.WriteString("```\n")

	if end < total {
		fmt.Fprintf(&terse, "%d more. Use offset=%d.\n", total-end, end)
	}

	output := terse.String()
	output = server.TruncateToBudget(output, arg.TokenBudget)

	output += fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. "+
		"Use names_only=true or reduce scope for more.]\n", arg.TokenBudget)

	return output
}

func scopeName(lines [][]byte, boundaries grepfunc.BlockBoundaries, lineIdx int) string {
	return grepfunc.EnclosingSymbol(lines, lineIdx, boundaries)
}
