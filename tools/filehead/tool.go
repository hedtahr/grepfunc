package filehead

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "file_head",
	Description: "Read the FIRST lines of a file — the fastest way to orient yourself in unfamiliar code. Shows imports, package declaration, and top-level definitions without reading the entire file. Use FIRST when entering a new file — cheaper than reading everything, richer than a bare outline. Returns total line count so you know whether to read more.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "MUST be absolute path to the FILE (e.g. '/Users/you/project/src/main.go'). Defaults to last file operated on in this session."},
			"lines":   {Type: "integer", Description: "How many lines to read from the top. Default 60. Max 200. Ignored when start/end are set."},
			"start":   {Type: "integer", Description: "First line to read (1-based). Use with end to read a specific range anywhere in the file."},
			"end":     {Type: "integer", Description: "Last line to read (1-based, inclusive). Use with start to read a specific range."},
			"compact": {Type: "boolean", Description: "Terse output: no header line, just the code block. Default false."},
			"tail":    {Type: "integer", Description: "Read the LAST N lines of the file. Mutually exclusive with start/end/lines. Use for log files or append-heavy files where you need the bottom."},
		},
		Required: []string{},
	},
}

type args struct {
	Path    string `json:"path"`
	Lines   int    `json:"lines"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Compact bool   `json:"compact"`
	Tail    int    `json:"tail"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
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
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}
	server.SetLastPath(a.Path)

	// Check if path is a directory
	if info, err := os.Stat(a.Path); err == nil && info.IsDir() {
		return nil, fmt.Errorf("path is a directory, not a file: %s", a.Path)
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, fmt.Errorf("read failed: %v", err)
	}

	totalLines := 0
	if len(data) > 0 {
		totalLines = bytes.Count(data, []byte{'\n'}) + 1
		if data[len(data)-1] == '\n' {
			totalLines--
		}
	}

	ext := ""
	if dot := strings.LastIndexByte(a.Path, '.'); dot >= 0 {
		ext = a.Path[dot+1:]
	}
	// For extensionless files, detect from shebang
	if ext == "" && len(data) > 2 && data[0] == '#' && data[1] == '!' {
		firstNL := bytes.IndexByte(data, '\n')
		shebang := string(data[:firstNL])
		switch {
		case strings.Contains(shebang, "python"):
			ext = "python"
		case strings.Contains(shebang, "bash"):
			ext = "bash"
		case strings.Contains(shebang, "node"):
			ext = "js"
		case strings.Contains(shebang, "ruby"):
			ext = "ruby"
		default:
			ext = "sh"
		}
	}
	if ext == "" {
		ext = "text"
	}

	compact := a.Compact

	// Range mode: start/end override lines
	if a.Start > 0 || a.End > 0 {
		if a.Start <= 0 {
			a.Start = 1
		}
		if a.End <= 0 || a.End > totalLines {
			a.End = totalLines
		}
		if a.Start > totalLines {
			return nil, fmt.Errorf("start %d exceeds file length %d", a.Start, totalLines)
		}
		selected := extractLines(data, a.Start, a.End)
		var buf strings.Builder
		rel := server.RelPath(a.Path)
		if compact {
			fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
		} else {
			fmt.Fprintf(&buf, "Lines %d-%d of %d — %s:\n\n", a.Start, a.End, totalLines, rel)
			fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	// Tail mode: read last N lines
	if a.Tail > 0 {
		if totalLines == 0 {
			text := fmt.Sprintf("Last %d of 0 lines — %s (empty file)\n", a.Tail, server.RelPath(a.Path))
			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: "text", Text: text}},
			}, nil
		}
		show := min(a.Tail, totalLines)
		count := 0
		cutAt := len(data)
		start := len(data) - 1
		if start >= 0 && data[start] == '\n' {
			start--
		}
		for i := start; i >= 0; i-- {
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
		selected := data[cutAt:]
		startLine := totalLines - show + 1
		var buf strings.Builder
		rel := server.RelPath(a.Path)
		if compact {
			fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
		} else {
			fmt.Fprintf(&buf, "Last %d of %d lines (L%d-%d) — %s:\n\n", show, totalLines, startLine, totalLines, rel)
			fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	// Head mode
	if a.Lines <= 0 {
		a.Lines = 60
	}
	if a.Lines > 200 {
		a.Lines = 200
	}

	selected := data
	if a.Lines < totalLines {
		newlineCount := 0
		cutAt := len(data)
		for i, b := range data {
			if b == '\n' {
				newlineCount++
				if newlineCount == a.Lines {
					cutAt = i + 1
					break
				}
			}
		}
		selected = data[:cutAt]
	}

	var buf strings.Builder
	rel := server.RelPath(a.Path)
	if compact {
		fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
	} else {
		if a.Lines < totalLines {
			fmt.Fprintf(&buf, "First %d of %d lines — %s:\n\n",
				a.Lines, totalLines, rel)
		} else {
			fmt.Fprintf(&buf, "All %d lines — %s:\n\n", totalLines, rel)
		}
		fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(string(selected), "\n"))
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func extractLines(data []byte, start, end int) string {
	lines := strings.SplitAfter(string(data), "\n")
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "")
}
