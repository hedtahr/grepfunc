package multiread

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name: "multi_read",
	Description: "Read file contents. path accepts a file path OR a glob (any path containing * ? [). " +
		"For multi-file with per-entry ranges use the reads array. " +
		"Returns total line count. Defaults to last path in session.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "File path or glob pattern (e.g. 'tools/**/*.go'). Glob when contains * ? [. Defaults to last file in session."},
			"reads":   {Type: "array", Description: "Per-file ranges: [{path, start_line?, end_line?}]. path may be a glob — each glob expands to matched files sharing the same range. Max 10 entries, max 20 total files."},
			"lines":   {Type: "integer", Description: "Lines to read per file (mode 1 head / mode 3). Default 60, max 200."},
			"start":   {Type: "integer", Description: "First line to read, 1-based (mode 1 range)."},
			"end":     {Type: "integer", Description: "Last line to read, inclusive (mode 1 range)."},
			"tail":    {Type: "integer", Description: "Read last N lines (mode 1). Mutually exclusive with start/end/lines."},
			"compact": {Type: "boolean", Description: "Terse output: no header, just code fences."},
		},
		Required: []string{},
	},
}

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

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}

	switch {
	case len(a.Reads) > 0:
		return handleMulti(a)
	case isGlob(a.Path):
		return handleGlob(a)
	default:
		return handleSingle(a)
	}
}

// ── mode 1: single file ──────────────────────────────────────────────────────

func handleSingle(a args) (*server.ToolCallResult, error) {
	if a.Path == "" {
		a.Path = server.LastPath
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required (no previous path in session)")
	}
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBounds(a.Path); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}
	server.SetLastPath(a.Path)

	if info, err := os.Stat(a.Path); err == nil && info.IsDir() {
		return nil, fmt.Errorf("path is a directory; use glob mode or pkg_outline instead")
	}

	var buf strings.Builder
	var err error
	if a.Tail > 0 {
		err = renderTail(a.Path, a.Tail, a.Compact, &buf)
	} else {
		err = renderRange(a.Path, a.Start, a.End, a.Lines, a.Compact, &buf)
	}
	if err != nil {
		return nil, err
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
}

// ── mode 2: reads array ──────────────────────────────────────────────────────

func handleMulti(a args) (*server.ToolCallResult, error) {
	const maxEntries = 10
	const maxTotalFiles = 20
	if len(a.Reads) > maxEntries {
		a.Reads = a.Reads[:maxEntries]
	}

	// Expand entries in parallel (each glob does a WalkDir)
	slots := make([][]readEntry, len(a.Reads))
	var wg sync.WaitGroup
	for i, e := range a.Reads {
		wg.Add(1)
		go func(i int, e readEntry) {
			defer wg.Done()
			if !isGlob(e.Path) {
				slots[i] = []readEntry{e}
				return
			}
			glob := e.Path
			root := server.ProjectRoot
			if filepath.IsAbs(glob) {
				if rel, err := filepath.Rel(root, glob); err == nil && !strings.HasPrefix(rel, "..") {
					glob = rel
				}
			}
			var found []readEntry
			_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					if d != nil && d.IsDir() {
						base := d.Name()
						if base == ".git" || base == "node_modules" || base == "vendor" || strings.HasPrefix(base, ".") {
							return filepath.SkipDir
						}
					}
					return err
				}
				if !d.Type().IsRegular() || server.IsBannedPath(path) {
					return nil
				}
				rel, _ := filepath.Rel(root, path)
				if !grepfunc.MatchGlob(glob, rel) {
					return nil
				}
				if info, err2 := d.Info(); err2 != nil || info.Size() > 2*1024*1024 {
					return nil
				}
				if grepfunc.IsBinaryExt(strings.ToLower(filepath.Ext(path))) {
					return nil
				}
				found = append(found, readEntry{Path: path, StartLine: e.StartLine, EndLine: e.EndLine})
				if len(found) >= maxTotalFiles {
					return filepath.SkipAll
				}
				return nil
			})
			slots[i] = found
		}(i, e)
	}
	wg.Wait()

	// Flatten slots in order, cap total
	var expanded []readEntry
outer:
	for _, slot := range slots {
		for _, e := range slot {
			expanded = append(expanded, e)
			if len(expanded) >= maxTotalFiles {
				break outer
			}
		}
	}

	var sb strings.Builder
	for i, e := range expanded {
		if i > 0 {
			sb.WriteByte('\n')
		}
		e.Path = server.ResolvePath(e.Path)
		if err := server.CheckBounds(e.Path); err != nil {
			fmt.Fprintf(&sb, "%s\n[error: %v]\n", server.RelPath(e.Path), err)
			continue
		}
		if err := server.CheckBanned(e.Path); err != nil {
			fmt.Fprintf(&sb, "%s\n[error: %v]\n", server.RelPath(e.Path), err)
			continue
		}
		renderEntry(&sb, e, a.Compact)
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: sb.String()}}}, nil
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// ── mode 3: glob ─────────────────────────────────────────────────────────────

func handleGlob(a args) (*server.ToolCallResult, error) {
	const maxFiles = 20
	lines := a.Lines
	if lines <= 0 {
		lines = 60
	}
	if lines > 200 {
		lines = 200
	}

	glob := a.Path
	root := server.ProjectRoot

	// If glob is absolute and inside root, make it relative so MatchGlob works against rel paths
	if filepath.IsAbs(glob) {
		if rel, err := filepath.Rel(root, glob); err == nil && !strings.HasPrefix(rel, "..") {
			glob = rel
		}
	}

	var matched []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" ||
				strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || server.IsBannedPath(path) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !grepfunc.MatchGlob(glob, rel) {
			return nil
		}
		if info, err2 := d.Info(); err2 != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		if grepfunc.IsBinaryExt(strings.ToLower(filepath.Ext(path))) {
			return nil
		}
		matched = append(matched, path)
		if len(matched) >= maxFiles {
			return filepath.SkipAll
		}
		return nil
	})

	if len(matched) == 0 {
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("0 files matched %q", glob)}}}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d files matched %q\n\n", len(matched), glob)
	for i, path := range matched {
		if i > 0 {
			sb.WriteByte('\n')
		}
		renderEntry(&sb, readEntry{Path: path, EndLine: lines}, a.Compact)
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: sb.String()}}}, nil
}

// ── rendering helpers ─────────────────────────────────────────────────────────

func renderEntry(sb *strings.Builder, e readEntry, compact bool) {
	const maxLines = 300
	start := e.StartLine
	if start <= 0 {
		start = 1
	}
	endAt := e.EndLine

	rel := server.RelPath(e.Path)
	f, err := os.Open(e.Path)
	if err != nil {
		fmt.Fprintf(sb, "%s\n[error: %v]\n", rel, err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	total := 0
	for scanner.Scan() {
		total++
		line := scanner.Text()
		if total >= start && (endAt <= 0 || total <= endAt) && len(lines) < maxLines {
			lines = append(lines, line)
		}
	}

	actualEnd := start + len(lines) - 1
	if len(lines) == 0 {
		actualEnd = start
	}
	ext := extOf(e.Path)

	if compact {
		fmt.Fprintf(sb, "```%s\n", ext)
	} else {
		fmt.Fprintf(sb, "%s (L%d-%d of %d)\n```%s\n", rel, start, actualEnd, total, ext)
	}
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	sb.WriteString("```\n")
}

func renderRange(path string, start, end, lines int, compact bool, buf *strings.Builder) error {
	isRange := start > 0 || end > 0
	if !isRange {
		if lines <= 0 {
			lines = 60
		}
		if lines > 200 {
			lines = 200
		}
	} else if start <= 0 {
		start = 1
	}

	readStart := start
	if readStart <= 0 {
		readStart = 1
	}
	stopAt := end
	if !isRange {
		stopAt = lines
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read failed: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var stored []string
	var firstLine string
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
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read failed: %v", err)
	}

	ext := extOf(path)
	if ext == "text" && strings.HasPrefix(firstLine, "#!") {
		ext = shebangExt(firstLine)
	}

	rel := server.RelPath(path)
	content := strings.Join(stored, "\n")

	if isRange {
		if start > total {
			return fmt.Errorf("start %d exceeds file length %d", start, total)
		}
		if end <= 0 || end > total {
			end = total
		}
		if want := end - start + 1; len(stored) > want {
			stored = stored[:want]
			content = strings.Join(stored, "\n")
		}
		if compact {
			fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, content)
		} else {
			fmt.Fprintf(buf, "Lines %d-%d of %d — %s:\n\n```%s\n%s\n```\n", start, end, total, rel, ext, content)
		}
	} else {
		if compact {
			fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, content)
		} else if lines < total {
			fmt.Fprintf(buf, "First %d of %d lines — %s:\n\n```%s\n%s\n```\n", lines, total, rel, ext, content)
		} else {
			fmt.Fprintf(buf, "All %d lines — %s:\n\n```%s\n%s\n```\n", total, rel, ext, content)
		}
	}
	return nil
}

func renderTail(path string, n int, compact bool, buf *strings.Builder) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read failed: %v", err)
	}
	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	ext := extOf(path)
	rel := server.RelPath(path)

	show := min(n, total)
	count := 0
	cutAt := len(data)
	for i := len(data) - 2; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == show {
				cutAt = i + 1
				break
			}
		}
		if i == 0 {
			cutAt = 0
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
