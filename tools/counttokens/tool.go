// Package counttokens provides the count_tokens MCP tool: estimate the token cost of reading files.
package counttokens

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

// Tool is the count_tokens MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "count_tokens",
	Description: "Estimate the token cost of reading a file or line range BEFORE committing to reading it. " +
		"Uses a chars/4 heuristic. Prevents accidentally reading large generated files. " +
		"Zero content overhead — just stat + line scan.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type: "string",
				Description: "Target file to estimate. Absolute path or project-relative " +
					"(resolved against the opened project root).",
				Items: nil,
			},
			"start_line": {Type: "integer", Description: "First line of range (1-based). Optional.", Items: nil},
			"end_line":   {Type: "integer", Description: "Last line of range (1-based, inclusive). Optional.", Items: nil},
		},
		Required:             []string{"path"},
		AdditionalProperties: false,
	},
}

type args struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

const (
	typeText        = "text"
	warnThreshold   = 4000
	dangerThreshold = 12000
	charsPerToken   = 4
	maxInt32        = 1<<31 - 1
)

var errPathRequired = errors.New("path is required")

// Handle serves the count_tokens MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var input args

	err := json.Unmarshal(raw, &input)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
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

	info, err := os.Stat(input.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot stat: %w", err)
	}

	charCount, totalLines, rangeLines, rangeInfo, err :=
		estimate(input.Path, info.Size(), input.StartLine, input.EndLine)
	if err != nil {
		return nil, err
	}

	return textResult(renderOutput(input, charCount, totalLines, rangeLines, rangeInfo)), nil
}

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
}

// renderOutput formats the estimate for the whole file or the requested line range.
func renderOutput(input args, charCount, totalLines, rangeLines int, rangeInfo string) string {
	estTokens := charCount / charsPerToken

	var buf strings.Builder

	rel := server.RelPath(input.Path)
	fmt.Fprintf(&buf, "%s%s\n```\n", rel, rangeInfo)

	if input.StartLine > 0 || input.EndLine > 0 {
		fmt.Fprintf(&buf, "Lines: %d (of %d total)\n", rangeLines, totalLines)
	} else {
		fmt.Fprintf(&buf, "Lines: %d\n", totalLines)
	}

	fmt.Fprintf(&buf, "Chars: %d\nEst. tokens: ~%d\n```\n", charCount, estTokens)

	switch {
	case estTokens > dangerThreshold:
		fmt.Fprintf(&buf, "\n\u26d4 VERY LARGE \u2014 reading full file costs ~%d tokens. "+
			"Use start_line/end_line ranges or file_symbols instead.\n", estTokens)
	case estTokens > warnThreshold:
		fmt.Fprintf(&buf, "\n\u26a0\ufe0f  Large file \u2014 consider reading in ranges "+
			"(file_head start/end) to reduce token cost.\n")
	default:
		fmt.Fprintf(&buf, "\n\u2713 Safe to read.\n")
	}

	return buf.String()
}

// estimate counts characters and lines for the whole file or the requested line range.
func estimate(path string, size int64, startLine, endLine int) (int, int, int, string, error) {
	if startLine > 0 || endLine > 0 {
		return countRange(path, startLine, endLine)
	}

	totalLines, err := countTotalLines(path)
	if err != nil {
		return 0, 0, 0, "", err
	}

	return int(size), totalLines, 0, "", nil
}

// countRange scans only the requested line range and returns its char count.
func countRange(path string, startLine, endLine int) (int, int, int, string, error) {
	// #nosec G304 -- paths bounds-checked by server
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, "", fmt.Errorf("cannot open: %w", err)
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	lineNo := 0
	rngStart := max(1, startLine)

	rngEnd := endLine
	if rngEnd <= 0 {
		rngEnd = maxInt32
	}

	charCount := 0
	rangeLines := 0

	for scanner.Scan() {
		lineNo++
		if lineNo < rngStart {
			continue
		}

		if lineNo > rngEnd {
			break
		}

		charCount += len(scanner.Bytes()) + 1
		rangeLines++
	}

	totalLines := lineNo
	if rngEnd > totalLines {
		rngEnd = totalLines
	}

	rangeInfo := fmt.Sprintf(" (L%d\u2013L%d)", rngStart, rngEnd)

	return charCount, totalLines, rangeLines, rangeInfo, nil
}

// countTotalLines scans the whole file and returns its line count.
func countTotalLines(path string) (int, error) {
	// #nosec G304 -- paths bounds-checked by server
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("cannot open: %w", err)
	}

	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	totalLines := 0

	for scanner.Scan() {
		totalLines++
	}

	return totalLines, nil
}
