package counttokens

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "count_tokens",
	Description: "Estimate the token cost of reading a file or line range BEFORE committing to reading it. Uses a chars/4 heuristic. Prevents accidentally reading large generated files. Zero content overhead — just stat + line scan.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":       {Type: "string", Description: "MUST be absolute path to file to estimate. Required."},
			"start_line": {Type: "integer", Description: "First line of range (1-based). Optional."},
			"end_line":   {Type: "integer", Description: "Last line of range (1-based, inclusive). Optional."},
		},
		Required: []string{"path"},
	},
}

type args struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

const (
	warnThreshold   = 4000
	dangerThreshold = 12000
)

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %v", err)
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	totalLines := len(lines)

	var target []string
	rangeDesc := ""
	if a.StartLine > 0 || a.EndLine > 0 {
		start := max(1, a.StartLine) - 1
		end := totalLines
		if a.EndLine > 0 {
			end = min(totalLines, a.EndLine)
		}
		if start > end {
			start = 0
		}
		target = lines[start:end]
		rangeDesc = fmt.Sprintf(" (L%d–L%d)", start+1, end)
	} else {
		target = lines
	}

	charCount := 0
	for _, l := range target {
		charCount += len(l) + 1
	}
	estTokens := charCount / 4

	var buf strings.Builder
	rel := server.RelPath(a.Path)
	fmt.Fprintf(&buf, "%s%s\n", rel, rangeDesc)
	fmt.Fprintf(&buf, "- Lines: %d", len(target))
	if a.StartLine <= 0 && a.EndLine <= 0 {
		fmt.Fprintf(&buf, " (of %d total)", totalLines)
	}
	fmt.Fprintf(&buf, "\n- Chars: %d\n- Est. tokens: ~%d\n", charCount, estTokens)

	switch {
	case estTokens > dangerThreshold:
		fmt.Fprintf(&buf, "\n⛔ VERY LARGE — reading full file costs ~%d tokens. Use start_line/end_line ranges or file_symbols instead.\n", estTokens)
	case estTokens > warnThreshold:
		fmt.Fprintf(&buf, "\n⚠️  Large file — consider reading in ranges (file_head start/end) to reduce token cost.\n")
	default:
		fmt.Fprintf(&buf, "\n✓ Safe to read.\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}
