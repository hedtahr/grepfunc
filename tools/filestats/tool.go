// Package filestats implements the file_stats MCP tool.
package filestats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const (
	typeString       = "string"
	defaultDepth     = 2
	maxDepth         = 5
	defaultTopN      = 10
	maxTopN          = 30
	maxSearchResults = 50000
	topExts          = 3
)

// Tool is the file_stats MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "file_stats",
	Description: "Project overview: file/line counts, extension breakdown per dir. Mode 'top': files by symbol count. Skips .gitignore'd dirs.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: typeString, Description: "Root directory. Defaults to project root.", Items: nil},
			"include": {Type: typeString, Description: "Glob filter. Defaults to all source files.", Items: nil},
			"depth":   {Type: "integer", Description: "Directory depth to group by. Default 2, max 5.", Items: nil},
			"compact": {Type: "boolean", Description: "One line per dir, no header.", Items: nil},
			"mode":    {Type: typeString, Description: "Mode: 'stats' (default) or 'top' (files with most symbols).", Items: nil},
			"top_n":   {Type: "integer", Description: "Top files count. Default 10, max 30.", Items: nil},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

type args struct {
	Path    string `json:"path"`
	Include string `json:"include"`
	Depth   int    `json:"depth"`
	Compact bool   `json:"compact"`
	Mode    string `json:"mode"`
	TopN    int    `json:"top_n"`
}

type dirStats struct {
	files     int
	lines     int
	extCounts map[string]int
}

// Handle processes a file_stats tool call and returns the result.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	arg, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	if arg.Mode == "top" {
		return handleTop(arg)
	}

	stats, totalFiles, totalLines, err := collectStats(arg)
	if err != nil {
		return nil, err
	}

	output := renderStats(arg, stats, totalFiles, totalLines)

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
		IsError: false,
	}, nil
}

func parseArgs(raw json.RawMessage) (args, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return arg, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Path == "" {
		arg.Path = server.ProjectRoot
	} else {
		arg.Path = server.ResolvePath(arg.Path)
	}

	if err := server.CheckBounds(arg.Path); err != nil {
		return arg, fmt.Errorf("check bounds: %w", err)
	}

	if arg.Depth <= 0 {
		arg.Depth = defaultDepth
	}

	arg.Depth = min(arg.Depth, maxDepth)
	if arg.Include == "" {
		arg.Include = "*"
	}

	return arg, nil
}

func collectStats(arg args) (map[string]*dirStats, int, int, error) {
	walker := &statsWalker{
		arg:        arg,
		include:    grepfunc.CompileGlob(arg.Include),
		useGlob:    arg.Include != "*",
		stats:      map[string]*dirStats{},
		totalFiles: 0,
		totalLines: 0,
	}

	err := grepfunc.WalkDir(arg.Path, walker.step)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("walk: %w", err)
	}

	return walker.stats, walker.totalFiles, walker.totalLines, nil
}

type statsWalker struct {
	arg        args
	include    *grepfunc.GlobMatcher
	useGlob    bool
	stats      map[string]*dirStats
	totalFiles int
	totalLines int
}

func (w *statsWalker) step(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}

	if entry.IsDir() {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if w.skipFile(path, entry, ext) {
		return nil
	}

	rel, _ := filepath.Rel(w.arg.Path, path)
	rel = filepath.ToSlash(rel)

	if w.useGlob && !w.include.Match(rel) {
		return nil
	}

	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	lines := bytes.Count(data, []byte{'\n'})

	dirRel, _ := filepath.Rel(w.arg.Path, filepath.Dir(path))
	dirRel = filepath.ToSlash(dirRel)
	key := clampDir(dirRel, w.arg.Depth)

	count := w.stats[key]
	if count == nil {
		count = &dirStats{files: 0, lines: 0, extCounts: map[string]int{}}
		w.stats[key] = count
	}

	count.files++
	count.lines += lines

	extKey := ext
	if extKey == "" {
		extKey = "(none)"
	}

	count.extCounts[extKey]++
	w.totalFiles++
	w.totalLines += lines

	return nil
}

func (w *statsWalker) skipFile(path string, entry fs.DirEntry, ext string) bool {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return true
	}

	return grepfunc.IsBinaryExt(ext) || (!w.useGlob && grepfunc.IsNonSourceExt(ext))
}

func renderStats(arg args, stats map[string]*dirStats, totalFiles, totalLines int) string {
	dirs := make([]string, 0, len(stats))
	for key := range stats {
		dirs = append(dirs, key)
	}

	sort.Strings(dirs)

	var buf strings.Builder
	if arg.Compact {
		fmt.Fprintf(&buf, "%s %d files %d lines\n", server.RelPath(arg.Path), totalFiles, totalLines)
		buf.WriteString("```\n")

		for _, dir := range dirs {
			count := stats[dir]
			fmt.Fprintf(&buf, "%s %d files %d lines\n", dirLabel(dir), count.files, count.lines)
		}

		buf.WriteString("```\n")
	} else {
		fmt.Fprintf(&buf, "File stats for %s (%d files, %d total lines)\n", server.RelPath(arg.Path), totalFiles, totalLines)
		buf.WriteString("```\n")

		for _, dir := range dirs {
			count := stats[dir]
			fmt.Fprintf(&buf, "%-30s %6d files %6d lines   %s\n",
				dirLabel(dir), count.files, count.lines, formatExts(count.extCounts))
		}

		fmt.Fprintf(&buf, "%-30s %6d files %6d lines\n", "TOTAL", totalFiles, totalLines)
		buf.WriteString("```\n")
	}

	return buf.String()
}

func clampDir(rel string, depth int) string {
	if rel == "." || rel == "" {
		return "."
	}

	parts := strings.Split(rel, "/")
	if len(parts) <= depth {
		return rel
	}

	return strings.Join(parts[:depth], "/")
}

func dirLabel(dir string) string {
	if dir == "." {
		return "."
	}

	return dir + "/"
}

type extCount struct {
	ext   string
	count int
}

type fileSymCount struct {
	file       string
	funcs      int
	types      int
	totalLines int
}

func handleTop(arg args) (*server.ToolCallResult, error) {
	topN := arg.TopN
	if topN <= 0 {
		topN = defaultTopN
	}

	topN = min(topN, maxTopN)

	pattern, err := grepfunc.CompilePattern(".", false)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	funcs, types, err := grepfunc.SearchBoth(arg.Path, arg.Include, pattern, maxSearchResults)
	if err != nil {
		return nil, fmt.Errorf("search both: %w", err)
	}

	counts := countSymbols(funcs, types)

	sorted := make([]*fileSymCount, 0, len(counts))
	for _, count := range counts {
		sorted = append(sorted, count)
	}

	sort.Slice(sorted, func(left, right int) bool {
		leftTotal := sorted[left].funcs + sorted[left].types

		rightTotal := sorted[right].funcs + sorted[right].types
		if leftTotal != rightTotal {
			return leftTotal > rightTotal
		}

		return sorted[left].totalLines > sorted[right].totalLines
	})

	end := min(topN, len(sorted))
	sorted = sorted[:end]

	var buf strings.Builder

	fmt.Fprintf(&buf, "Top %d files by symbol count in %s\n\n", end, server.RelPath(arg.Path))
	fmt.Fprintf(&buf, "| File | Funcs | Types | Total Lines |\n")
	fmt.Fprintf(&buf, "|------|-------|-------|-------------|\n")

	for _, count := range sorted {
		rel := server.RelPath(count.file)
		fmt.Fprintf(&buf, "| %s | %d | %d | %d |\n", rel, count.funcs, count.types, count.totalLines)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		IsError: false,
	}, nil
}

func countSymbols(funcs, types []grepfunc.FuncMatch) map[string]*fileSymCount {
	counts := map[string]*fileSymCount{}

	for _, match := range funcs {
		count := counts[match.File]
		if count == nil {
			count = &fileSymCount{file: match.File, funcs: 0, types: 0, totalLines: 0}
			counts[match.File] = count
		}

		count.funcs++
	}

	for _, match := range types {
		count := counts[match.File]
		if count == nil {
			count = &fileSymCount{file: match.File, funcs: 0, types: 0, totalLines: 0}
			counts[match.File] = count
		}

		count.types++
	}

	// Count lines for files with symbols.
	for _, count := range counts {
		data, err := os.ReadFile(count.file)
		if err == nil {
			count.totalLines = bytes.Count(data, []byte{'\n'})
		}
	}

	return counts
}

func formatExts(m map[string]int) string {
	counts := make([]extCount, 0, len(m))
	for ext, n := range m {
		counts = append(counts, extCount{ext, n})
	}

	sort.Slice(counts, func(left, right int) bool {
		if counts[left].count != counts[right].count {
			return counts[left].count > counts[right].count
		}

		return counts[left].ext < counts[right].ext
	})

	show := min(topExts, len(counts))

	parts := make([]string, 0, show+1)
	for i := range show {
		parts = append(parts, fmt.Sprintf("%s(%d)", counts[i].ext, counts[i].count))
	}

	if len(counts) > topExts {
		parts = append(parts, fmt.Sprintf("+%d more", len(counts)-topExts))
	}

	return strings.Join(parts, " ")
}
