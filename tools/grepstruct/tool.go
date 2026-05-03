package grepstruct

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "grep_struct",
	Description: "Search for struct/class/interface/enum/type definitions matching a pattern and return complete bodies with line numbers. Returns the full definition (signature + body), not just matching lines. Use BEFORE editing to understand data models, type hierarchies, or class structures.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":         {Type: "string", Description: "Regex pattern to match against type names or body contents. Matches anywhere inside the definition. Examples: 'User', 'Handler', 'interface', 'class.*Service'."},
			"path":            {Type: "string", Description: "File or directory to search. Relative to project root or absolute. Defaults to project root."},
			"include":         {Type: "string", Description: "Glob to filter files. Supports ** for recursive matching. Examples: '**/*.go', '**/*.ts', '**/*.java'. If omitted, auto-filtered to common source extensions."},
			"max_results":     {Type: "integer", Description: "Max definitions to return. Default 15, max 50."},
			"offset":          {Type: "integer", Description: "Starting position for paginated results (0-based)."},
			"body":            {Type: "boolean", Description: "Include full body in output. Default: false (name + location only)."},
			"case_sensitive":  {Type: "boolean", Description: "Case-sensitive regex. Default: false (case-insensitive)."},
			"summary":         {Type: "boolean", Description: "If true and body=true, truncate large definitions: shows first+last N lines with omission count. Reduces token cost for large definitions."},
			"summary_lines":   {Type: "integer", Description: "Lines to show at start and end when summary=true. Default 5."},
			"names_only":      {Type: "boolean", Description: "If true, return only file:line:name — no body, no signature. Cheapest mode (~20x fewer tokens than body=true). For table-of-contents scans."},
			"compact":         {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"exclude_pattern": {Type: "string", Description: "Regex to exclude matching results. Filters on body and name."},
		},
		Required: []string{"pattern"},
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

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if a.MaxResults <= 0 {
		a.MaxResults = 15
	}
	if a.MaxResults > 50 {
		a.MaxResults = 50
	}
	if a.Include == "" {
		a.Include = "*"
	}

	pattern, err := grepfunc.CompilePattern(a.Pattern, a.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %v", err)
	}

	fetchMax := a.MaxResults
	if a.Offset > 0 {
		fetchMax = a.Offset + a.MaxResults
		if fetchMax > 200 {
			fetchMax = 200
		}
	}
	results, err := grepfunc.Search(a.Path, a.Include, pattern, fetchMax, grepfunc.IsStructSig)
	if err != nil {
		return nil, err
	}

	if a.ExcludePattern != "" {
		results, err = filterByExclude(results, a.ExcludePattern)
		if err != nil {
			return nil, fmt.Errorf("exclude_pattern: %v", err)
		}
	}

	total := len(results)
	start := min(a.Offset, total)
	end := min(start+a.MaxResults, total)
	page := results[start:end]

	var buf strings.Builder
	namesOnly := a.NamesOnly
	includeBody := !namesOnly && a.Body
	compact := a.Compact
	if compact {
		fmt.Fprintf(&buf, "%d defs %q", total, a.Pattern)
	} else {
		fmt.Fprintf(&buf, "%d definition(s) matching %q", total, a.Pattern)
	}
	if total > a.MaxResults || a.Offset > 0 {
		fmt.Fprintf(&buf, " (showing %d\u2013%d)", start+1, end)
	}
	if compact {
		buf.WriteString("\n")
	} else {
		buf.WriteString("\n\n")
	}
	for _, m := range page {
		rel := server.RelPath(m.File)
		if namesOnly {
			if compact {
				fmt.Fprintf(&buf, "%s:%d: %s\n", rel, m.Line, m.Name)
			} else {
				fmt.Fprintf(&buf, "%s:%d: %s (%dL)\n", rel, m.Line, m.Name, m.Lines)
			}
			continue
		}
		if includeBody {
			fmt.Fprintf(&buf, "%s:%d-%d: %s\n", rel, m.Line, m.EndLine, firstLine(m.Body, true))
		} else {
			fmt.Fprintf(&buf, "%s:%d-%d: %s\n", rel, m.Line, m.EndLine, firstLine(m.Body, false))
		}
		if includeBody {
			ext := strings.TrimPrefix(filepath.Ext(m.File), ".")
			if ext == "" {
				ext = "go"
			}
			body := m.Body
			if a.Summary {
				sl := a.SummaryLines
				if sl <= 0 {
					sl = 5
				}
				body = grepfunc.SummarizeBody(body, sl)
			}
			fmt.Fprintf(&buf, "```%s\n%s\n```", ext, strings.TrimRight(body, "\n"))
			if compact {
				buf.WriteString("\n")
			} else {
				buf.WriteString("\n\n")
			}
		}
	}
	if end < total {
		fmt.Fprintf(&buf, "\n%d more results. Use offset=%d for next page.", total-end, end)
	}
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func firstLine(s string, includeBody bool) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		s = strings.TrimSpace(before)
	}
	if !includeBody && len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func filterByExclude(matches []grepfunc.FuncMatch, excludePattern string) ([]grepfunc.FuncMatch, error) {
	re, err := grepfunc.CompilePattern(excludePattern, false)
	if err != nil {
		return nil, err
	}
	out := matches[:0]
	for _, m := range matches {
		if re.MatchString(m.Body) || re.MatchString(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
