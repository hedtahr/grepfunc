// Package multiread provides the multi_read MCP tool: read files by path, glob, or per-entry ranges.
package multiread

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

// Tool is the multi_read MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "multi_read",
	Description: "Read file contents. path accepts a file path OR a glob (any path containing * ? [). " +
		"For multi-file with per-entry ranges use the reads array. " +
		"Returns total line count. Defaults to last path in session.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			pathKey: {
				Type: typeString,
				Description: "File path or glob pattern (e.g. 'tools/**/*.go'). " +
					"Glob when contains * ? [. Defaults to last file in session.",
				Items: nil,
			},
			"reads": {
				Type: typeArray,
				Description: "Per-file ranges: [{path, start_line?, end_line?}]. path may be a glob — each glob " +
					"expands to matched files sharing the same range. Max 10 entries, max 20 total files.",
				Items: nil,
			},
			"lines": {
				Type:        typeInteger,
				Description: "Lines to read per file (mode 1 head / mode 3). Default 60, max 200.",
				Items:       nil,
			},
			"start": {Type: typeInteger, Description: "First line to read, 1-based (mode 1 range).", Items: nil},
			"end": {
				Type:        typeInteger,
				Description: "Last line to read, inclusive (mode 1 range).",
				Items:       nil,
			},
			"tail": {
				Type:        typeInteger,
				Description: "Read last N lines (mode 1). Mutually exclusive with start/end/lines.",
				Items:       nil,
			},
			"compact": {Type: typeBoolean, Description: "Terse output: no header, just code fences.", Items: nil},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

const (
	pathKey     = "path"
	typeString  = "string"
	typeArray   = "array"
	typeInteger = "integer"
	typeBoolean = "boolean"
	typeText    = "text"

	defaultLines = 60
	maxLines     = 200
	maxFileSize  = 2 * 1024 * 1024
	bufInitSize  = 64 * 1024
	bufMaxSize   = 4 * 1024 * 1024

	maxEntries    = 10
	maxTotalFiles = 20

	tailSkipLastByte = 2
)

var (
	errPathRequired = errors.New("path is required (no previous path in session)")
	errPathIsDir    = errors.New("path is a directory; use glob mode or file_stats/file_symbols instead")
	errGlobOutside  = errors.New("glob is outside project root")
	errStartExceeds = errors.New("start exceeds file length")
)

type readEntry struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type args struct {
	Path    string      `json:"path"`
	Reads   []readEntry `json:"reads"`
	Lines   int         `json:"lines"`
	Start   int         `json:"start"`
	End     int         `json:"end"`
	Tail    int         `json:"tail"`
	Compact bool        `json:"compact"`
}

// Handle serves the multi_read MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var input args

	err := json.Unmarshal(raw, &input)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	switch {
	case len(input.Reads) > 0:
		return handleMulti(input)
	case isGlob(input.Path):
		return handleGlob(input)
	default:
		return handleSingle(input)
	}
}

// ── mode 1: single file ──────────────────────────────────────────────────────

func handleSingle(input args) (*server.ToolCallResult, error) {
	if input.Path == "" {
		input.Path = server.LastPath
	}

	if input.Path == "" {
		return nil, errPathRequired
	}

	input.Path = server.ResolvePath(input.Path)

	err := server.CheckBounds(input.Path)
	if err != nil {
		return nil, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(input.Path)
	if err != nil {
		return nil, fmt.Errorf("check banned: %w", err)
	}

	server.SetLastPath(input.Path)

	info, err := os.Stat(input.Path)
	if err == nil && info.IsDir() {
		return nil, errPathIsDir
	}

	var buf strings.Builder

	if input.Tail > 0 {
		err = renderTail(input.Path, input.Tail, input.Compact, &buf)
	} else {
		err = renderRange(input.Path, input.Start, input.End, input.Lines, input.Compact, &buf)
	}

	if err != nil {
		return nil, err
	}

	return textResult(buf.String()), nil
}

// ── mode 2: reads array ──────────────────────────────────────────────────────

func handleMulti(input args) (*server.ToolCallResult, error) {
	if len(input.Reads) > maxEntries {
		input.Reads = input.Reads[:maxEntries]
	}

	err := validateAbsGlobs(input.Reads)
	if err != nil {
		return nil, err
	}

	// Expand entries in parallel (each glob does a WalkDir).
	slots := make([][]readEntry, len(input.Reads))

	var group sync.WaitGroup
	for idx, entry := range input.Reads {
		group.Add(1)

		go func(idx int, entry readEntry) {
			defer group.Done()

			if !isGlob(entry.Path) {
				slots[idx] = []readEntry{entry}

				return
			}

			slots[idx] = walkGlob(entry, maxTotalFiles)
		}(idx, entry)
	}

	group.Wait()

	// Flatten slots in order, cap total.
	expanded := flattenSlots(slots, maxTotalFiles)

	var buf strings.Builder

	for i, entry := range expanded {
		if i > 0 {
			buf.WriteByte('\n')
		}

		entry.Path = server.ResolvePath(entry.Path)

		err := server.CheckBounds(entry.Path)
		if err != nil {
			fmt.Fprintf(&buf, "%s\n[error: %v]\n", server.RelPath(entry.Path), err)

			continue
		}

		err = server.CheckBanned(entry.Path)
		if err != nil {
			fmt.Fprintf(&buf, "%s\n[error: %v]\n", server.RelPath(entry.Path), err)

			continue
		}

		renderEntry(&buf, entry, input.Compact)
	}

	return textResult(buf.String()), nil
}

// validateAbsGlobs rejects absolute globs that resolve outside the project root.
func validateAbsGlobs(reads []readEntry) error {
	for _, entry := range reads {
		if !isGlob(entry.Path) || !filepath.IsAbs(entry.Path) {
			continue
		}

		_, err := resolveGlob(entry.Path, server.ProjectRoot)
		if err != nil {
			return err
		}
	}

	return nil
}

// walkGlob expands one glob entry by walking the project root.
func walkGlob(entry readEntry, maxTotalFiles int) []readEntry {
	glob, err := resolveGlob(entry.Path, server.ProjectRoot)
	if err != nil {
		return nil
	}

	root := server.ProjectRoot

	var found []readEntry

	_ = filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil || dirEntry.IsDir() {
			if dirEntry != nil && dirEntry.IsDir() {
				if skipDir(dirEntry.Name()) {
					return filepath.SkipDir
				}
			}

			return err
		}

		ok, walkErr := matchGlobFile(root, path, glob, dirEntry)
		if walkErr != nil {
			return walkErr
		}

		if !ok {
			return nil
		}

		found = append(found, readEntry{Path: path, StartLine: entry.StartLine, EndLine: entry.EndLine})
		if len(found) >= maxTotalFiles {
			return filepath.SkipAll
		}

		return nil
	})

	return found
}

// flattenSlots merges per-entry expansions in order, capped at maxTotalFiles.
func flattenSlots(slots [][]readEntry, maxTotalFiles int) []readEntry {
	var expanded []readEntry

outer:
	for _, slot := range slots {
		for _, entry := range slot {
			expanded = append(expanded, entry)
			if len(expanded) >= maxTotalFiles {
				break outer
			}
		}
	}

	return expanded
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// resolveGlob makes an absolute in-root glob relative so MatchGlob works against rel paths.
func resolveGlob(glob, root string) (string, error) {
	if !filepath.IsAbs(glob) {
		return glob, nil
	}

	rel, err := filepath.Rel(root, glob)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%w %q %q", errGlobOutside, glob, root)
	}

	return rel, nil
}

// skipDir reports whether a directory should be pruned from glob walks.
func skipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".")
}

// matchGlobFile reports whether path matches glob and is a readable, non-binary,
// size-bounded regular file.
func matchGlobFile(root, path, glob string, dirEntry fs.DirEntry) (bool, error) {
	if !dirEntry.Type().IsRegular() || server.IsBannedPath(path) {
		return false, nil
	}

	rel, _ := filepath.Rel(root, path)
	if !grepfunc.MatchGlob(glob, rel) {
		return false, nil
	}

	info, err := dirEntry.Info()
	if err != nil {
		return false, fmt.Errorf("stat %q: %w", path, err)
	}

	if info.Size() > maxFileSize {
		return false, nil
	}

	if grepfunc.IsBinaryExt(strings.ToLower(filepath.Ext(path))) {
		return false, nil
	}

	return true, nil
}

// ── mode 3: glob ─────────────────────────────────────────────────────────────

func handleGlob(input args) (*server.ToolCallResult, error) {
	glob, err := resolveGlob(input.Path, server.ProjectRoot)
	if err != nil {
		return nil, err
	}

	root := server.ProjectRoot
	lines := lineLimit(input.Lines)

	var matched []string

	_ = filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if dirEntry.IsDir() {
			if skipDir(dirEntry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		ok, walkErr := matchGlobFile(root, path, glob, dirEntry)
		if walkErr != nil {
			return walkErr
		}

		if !ok {
			return nil
		}

		matched = append(matched, path)
		if len(matched) >= maxTotalFiles {
			return filepath.SkipAll
		}

		return nil
	})

	return renderGlobResults(matched, glob, lines, input.Compact), nil
}

// renderGlobResults formats the matched files with a count header, or a zero-match notice.
func renderGlobResults(matched []string, glob string, lines int, compact bool) *server.ToolCallResult {
	if len(matched) == 0 {
		return textResult(fmt.Sprintf("0 files matched %q", glob))
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "%d files matched %q\n\n", len(matched), glob)

	for i, path := range matched {
		if i > 0 {
			buf.WriteByte('\n')
		}

		renderEntry(&buf, readEntry{Path: path, StartLine: 0, EndLine: lines}, compact)
	}

	return textResult(buf.String())
}

// ── rendering helpers ─────────────────────────────────────────────────────────

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
}

// lineLimit clamps a requested line count to the default/max range.
func lineLimit(requested int) int {
	if requested <= 0 {
		return defaultLines
	}

	if requested > maxLines {
		return maxLines
	}

	return requested
}

func renderEntry(buf *strings.Builder, entry readEntry, compact bool) {
	const maxEntryLines = 300

	start := entry.StartLine
	if start <= 0 {
		start = 1
	}

	endAt := entry.EndLine

	rel := server.RelPath(entry.Path)

	file, err := os.Open(entry.Path)
	if err != nil {
		fmt.Fprintf(buf, "%s\n[error: %v]\n", rel, err)

		return
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, bufInitSize), bufMaxSize)

	var lines []string

	total := 0
	for scanner.Scan() {
		total++

		line := scanner.Text()
		if total >= start && (endAt <= 0 || total <= endAt) && len(lines) < maxEntryLines {
			lines = append(lines, line)
		}
	}

	actualEnd := start + len(lines) - 1
	if len(lines) == 0 {
		actualEnd = start
	}

	ext := extOf(entry.Path)

	writeEntryOutput(buf, ext, rel, start, actualEnd, total, lines, compact)
}

// writeEntryOutput formats the entry header, lines, and closing fence.
func writeEntryOutput(
	buf *strings.Builder, ext, rel string, start, actualEnd, total int, lines []string, compact bool,
) {
	if compact {
		fmt.Fprintf(buf, "```%s\n", ext)
	} else {
		fmt.Fprintf(buf, "%s (L%d-%d of %d)\n```%s\n", rel, start, actualEnd, total, ext)
	}

	for _, line := range lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}

	buf.WriteString("```\n")
}

func renderRange(path string, start, end, lines int, compact bool, buf *strings.Builder) error {
	if start > 0 || end > 0 {
		return renderRangePortion(path, start, end, compact, buf)
	}

	return renderHeadPortion(path, lines, compact, buf)
}

// renderRangePortion renders the requested line range.
func renderRangePortion(path string, start, end int, compact bool, buf *strings.Builder) error {
	if start <= 0 {
		start = 1
	}

	stored, _, total, err := scanLines(path, start, end)
	if err != nil {
		return err
	}

	if start > total {
		return fmt.Errorf("%w: start %d, file has %d lines", errStartExceeds, start, total)
	}

	if end <= 0 || end > total {
		end = total
	}

	if want := end - start + 1; len(stored) > want {
		stored = stored[:want]
	}

	writeRangeOutput(buf, extOf(path), strings.Join(stored, "\n"), server.RelPath(path), start, end, total, compact)

	return nil
}

// renderHeadPortion renders the first lines of the file.
func renderHeadPortion(path string, lines int, compact bool, buf *strings.Builder) error {
	lines = lineLimit(lines)

	stored, firstLine, total, err := scanLines(path, 1, lines)
	if err != nil {
		return err
	}

	ext := extOf(path)
	if ext == "text" && strings.HasPrefix(firstLine, "#!") {
		ext = shebangExt(firstLine)
	}

	writeHeadOutput(buf, ext, strings.Join(stored, "\n"), server.RelPath(path), lines, total, compact)

	return nil
}

// scanLines reads lines in [readStart, stopAt], capturing the first line and total count.
func scanLines(path string, readStart, stopAt int) ([]string, string, int, error) {
	// #nosec G304 -- paths bounds-checked by server
	file, err := os.Open(path)
	if err != nil {
		return nil, "", 0, fmt.Errorf("read failed: %w", err)
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, bufInitSize), bufMaxSize)

	var (
		stored    []string
		firstLine string
	)

	total := 0

	for scanner.Scan() {
		total++

		line := scanner.Text()
		if total == 1 {
			firstLine = line
		}

		if total >= readStart && (stopAt <= 0 || total <= stopAt) {
			stored = append(stored, line)
		}
	}

	scanErr := scanner.Err()
	if scanErr != nil {
		return nil, "", 0, fmt.Errorf("read failed: %w", scanErr)
	}

	return stored, firstLine, total, nil
}

// writeRangeOutput formats the rendered line-range output.
func writeRangeOutput(buf *strings.Builder, ext, content, rel string, start, end, total int, compact bool) {
	if compact {
		fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, content)
	} else {
		fmt.Fprintf(buf, "Lines %d-%d of %d — %s:\n\n```%s\n%s\n```\n", start, end, total, rel, ext, content)
	}
}

// writeHeadOutput formats the rendered head-of-file output.
func writeHeadOutput(buf *strings.Builder, ext, content, rel string, lines, total int, compact bool) {
	switch {
	case compact:
		fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, content)
	case lines < total:
		fmt.Fprintf(buf, "First %d of %d lines — %s:\n\n```%s\n%s\n```\n", lines, total, rel, ext, content)
	default:
		fmt.Fprintf(buf, "All %d lines — %s:\n\n```%s\n%s\n```\n", total, rel, ext, content)
	}
}

func renderTail(path string, count int, compact bool, buf *strings.Builder) error {
	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read failed: %w", err)
	}

	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}

	ext := extOf(path)
	rel := server.RelPath(path)

	show := min(count, total)
	newlines := 0
	cutAt := 0

	for i := len(data) - tailSkipLastByte; i >= 0; i-- {
		if data[i] == '\n' {
			newlines++
			if newlines == show {
				cutAt = i + 1

				break
			}
		}
	}

	selected := strings.TrimRight(string(data[cutAt:]), "\n")
	startLine := total - show + 1

	if compact {
		fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, selected)
	} else {
		fmt.Fprintf(buf, "Last %d of %d lines (L%d-%d) — %s:\n\n```%s\n%s\n```\n",
			show, total, startLine, total, rel, ext, selected)
	}

	return nil
}

func extOf(path string) string {
	if dot := strings.LastIndexByte(path, '.'); dot >= 0 {
		return path[dot+1:]
	}

	return "text"
}

func shebangExt(line string) string {
	switch {
	case strings.Contains(line, "python"):
		return "python"
	case strings.Contains(line, "bash"):
		return "bash"
	case strings.Contains(line, "node"):
		return "js"
	case strings.Contains(line, "ruby"):
		return "ruby"
	default:
		return "sh"
	}
}
