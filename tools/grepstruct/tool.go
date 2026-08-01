// Package grepstruct implements the grep_struct MCP tool.
package grepstruct

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
	"github.com/hedtahr/grepfunc/tools/internal/util"
)

const (
	keyPattern          = "pattern"
	keyInclude          = "include"
	keyPath             = "path"
	keyBody             = "body"
	keyName             = "Name"
	keyGoGlob           = "*.go"
	typeString          = "string"
	typeInteger         = "integer"
	typeBoolean         = "boolean"
	defaultMaxResults   = 15
	maxMaxResults       = 50
	maxFetchMax         = 200
	defaultSummaryLines = 5
)

var errPatternRequired = errors.New("pattern is required")

// Tool is the grep_struct MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "grep_struct",
	Description: "Use when you need data models — struct/class/interface/enum/type definitions to read or edit. " +
		"Returns full type bodies with line numbers, brace-aware — native grep only returns matching lines. " +
		"body=true returns full bodies; default returns name + location. For plain-text search use grep instead.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			keyPattern: {Type: typeString, Description: "Regex pattern to match against type names or body contents. " +
				"Matches anywhere inside the definition. Examples: 'User', 'Handler', 'interface', 'class.*Service'.", Items: nil},
			keyPath: {Type: typeString, Description: "Directory to search. " +
				"Optional — defaults to the opened project root.", Items: nil},
			keyInclude: {Type: typeString, Description: "Glob to filter files. Supports ** for recursive matching. " +
				"Examples: '**/*.go', '**/*.ts', '**/*.java'. If omitted, auto-filtered to common source extensions.", Items: nil},
			"max_results": {Type: typeInteger, Description: "Max definitions to return. Default 15, max 50.", Items: nil},
			"offset":      {Type: typeInteger, Description: "Starting position for paginated results (0-based).", Items: nil},
			keyBody: {Type: typeBoolean, Description: "Include full body in output. " +
				"Default: false (name + location only).", Items: nil},
			"case_sensitive": {Type: typeBoolean, Description: "Case-sensitive regex. " +
				"Default: false (case-insensitive).", Items: nil},
			"summary": {Type: typeBoolean, Description: "If true and body=true, truncate large definitions: " +
				"shows first+last N lines with omission count. Reduces token cost for large definitions.", Items: nil},
			"summary_lines": {Type: typeInteger, Description: "Lines to show at start and end " +
				"when summary=true. Default 5.", Items: nil},
			"names_only": {Type: typeBoolean, Description: "If true, return only file:line:name — no body, no signature. " +
				"Cheapest mode (~20x fewer tokens than body=true). For table-of-contents scans.", Items: nil},
			"compact": {Type: typeBoolean, Description: "Terse output: less whitespace, shorter headers. " +
				"Keeps syntax highlighting. Default false.", Items: nil},
			"exclude_pattern": {Type: typeString, Description: "Regex to exclude matching results. " +
				"Filters on body and name.", Items: nil},
		},
		Required:             []string{keyPattern},
		AdditionalProperties: false,
	},
}

type args struct {
	Pattern        string `json:"pattern"`
	Path           string `json:"path"`
	Include        string `json:"include"`
	MaxResults     int    `json:"max_results"`
	Offset         int    `json:"offset"`
	Body           bool   `json:"body"`
	CaseSensitive  bool   `json:"case_sensitive"`
	Summary        bool   `json:"summary"`
	SummaryLines   int    `json:"summary_lines"`
	NamesOnly      bool   `json:"names_only"`
	Compact        bool   `json:"compact"`
	ExcludePattern string `json:"exclude_pattern"`
}

// Handle processes a grep_struct tool call and returns the result.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	arg, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	fetchMax := arg.MaxResults
	if arg.Offset > 0 {
		fetchMax = min(arg.Offset+arg.MaxResults, maxFetchMax)
	}

	pattern, err := grepfunc.CompilePattern(arg.Pattern, arg.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	results, err := grepfunc.Search(arg.Path, arg.Include, pattern, fetchMax, grepfunc.IsStructSig)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	if arg.ExcludePattern != "" {
		results, err = util.FilterByExclude(results, arg.ExcludePattern)
		if err != nil {
			return nil, fmt.Errorf("exclude_pattern: %w", err)
		}
	}

	total := len(results)
	start := min(arg.Offset, total)
	end := min(start+arg.MaxResults, total)
	page := results[start:end]

	output := renderResults(arg, page, total, start, end)

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
		IsError: false,
	}, nil
}

func renderResults(arg args, page []grepfunc.FuncMatch, total, start, end int) string {
	var buf strings.Builder

	if arg.Compact {
		fmt.Fprintf(&buf, "%d defs %q", total, arg.Pattern)
	} else {
		fmt.Fprintf(&buf, "%d definitions matching %q", total, arg.Pattern)
	}

	if total > arg.MaxResults || arg.Offset > 0 {
		fmt.Fprintf(&buf, " (showing %d\u2013%d)", start+1, end)
	}

	buf.WriteString("\n")

	includeBody := !arg.NamesOnly && arg.Body
	if !includeBody {
		buf.WriteString("```\n")
	}

	renderMatches(&buf, arg, page)

	if !includeBody {
		buf.WriteString("```\n")
	}

	if end < total {
		fmt.Fprintf(&buf, "\n%d more results. Use offset=%d for next page.", total-end, end)
	}

	return buf.String()
}

func parseArgs(raw json.RawMessage) (args, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return arg, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Pattern == "" {
		return arg, errPatternRequired
	}

	arg.Path = server.ResolvePath(arg.Path)
	if arg.MaxResults <= 0 {
		arg.MaxResults = defaultMaxResults
	}

	if arg.MaxResults > maxMaxResults {
		arg.MaxResults = maxMaxResults
	}

	if arg.Include == "" {
		arg.Include = "*"
	}

	return arg, nil
}

func renderMatches(buf *strings.Builder, arg args, page []grepfunc.FuncMatch) {
	namesOnly := arg.NamesOnly
	includeBody := !namesOnly && arg.Body

	for _, match := range page {
		rel := server.RelPath(match.File)

		if namesOnly {
			if arg.Compact {
				fmt.Fprintf(buf, "%s:%d: %s\n", rel, match.Line, match.Name)
			} else {
				fmt.Fprintf(buf, "%s:%d: %s (%dL)\n", rel, match.Line, match.Name, match.Lines)
			}

			continue
		}

		if includeBody {
			fmt.Fprintf(buf, "%s:%d-%d: %s\n", rel, match.Line, match.EndLine, util.FirstLine(match.Body, true))
		} else {
			fmt.Fprintf(buf, "%s:%d-%d: %s\n", rel, match.Line, match.EndLine, util.FirstLine(match.Body, false))
		}

		if includeBody {
			ext := strings.TrimPrefix(filepath.Ext(match.File), ".")
			if ext == "" {
				ext = "go"
			}

			body := match.Body

			if arg.Summary {
				sl := arg.SummaryLines
				if sl <= 0 {
					sl = defaultSummaryLines
				}

				body = grepfunc.SummarizeBody(body, sl)
			}

			fmt.Fprintf(buf, "```%s\n%s\n```\n", ext, strings.TrimRight(body, "\n"))
		}
	}
}
