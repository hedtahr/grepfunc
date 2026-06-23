package counttokens

import (
	"bufio"
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
	if err := server.CheckBounds(a.Path); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}

	fi, err := os.Stat(a.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot stat: %v", err)
	}
	charCount := int(fi.Size())
	totalLines := 0
	rangeInfo := ""
	rangeLines := 0

	if a.StartLine > 0 || a.EndLine > 0 {
		f, err := os.Open(a.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot open: %v", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		ln := 0
		rngStart := max(1, a.StartLine)
		rngEnd := a.EndLine
		if rngEnd <= 0 {
			rngEnd = 1<<31 - 1
		}
		charCount = 0
		for sc.Scan() {
			ln++
			if ln < rngStart {
				continue
			}
			if ln > rngEnd {
				break
			}
			charCount += len(sc.Bytes()) + 1
			rangeLines++
		}
		totalLines = ln
		if rngEnd > totalLines {
			rngEnd = totalLines
		}
		rangeInfo = fmt.Sprintf(" (L%d\u2013L%d)", rngStart, rngEnd)
	} else {
		f, err := os.Open(a.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot open: %v", err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			totalLines++
		}
	}

	estTokens := charCount / 4

	var buf strings.Builder
	rel := server.RelPath(a.Path)
	fmt.Fprintf(&buf, "%s%s\n```\n", rel, rangeInfo)
	if a.StartLine > 0 || a.EndLine > 0 {
		fmt.Fprintf(&buf, "Lines: %d (of %d total)\n", rangeLines, totalLines)
	} else {
		fmt.Fprintf(&buf, "Lines: %d\n", totalLines)
	}
	fmt.Fprintf(&buf, "Chars: %d\nEst. tokens: ~%d\n```\n", charCount, estTokens)

	switch {
	case estTokens > dangerThreshold:
		fmt.Fprintf(&buf, "\n\u26d4 VERY LARGE \u2014 reading full file costs ~%d tokens. Use start_line/end_line ranges or file_symbols instead.\n", estTokens)
	case estTokens > warnThreshold:
		fmt.Fprintf(&buf, "\n\u26a0\ufe0f  Large file \u2014 consider reading in ranges (file_head start/end) to reduce token cost.\n")
	default:
		fmt.Fprintf(&buf, "\n\u2713 Safe to read.\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}
