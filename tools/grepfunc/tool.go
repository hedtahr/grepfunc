package grepfunc

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "grep_func",
	Description: "Search for functions/methods matching a pattern and return complete function bodies with line numbers. Much more useful than grep when you need to understand or modify code — returns the full definition (signature + body), not just matching lines. Handles braces with string/comment awareness. Use BEFORE editing to understand code structure. Use to find function implementations, call sites, or patterns inside function bodies.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":         {Type: "string", Description: "Regex pattern to match against function names or code. Matches anywhere inside a function's lines — not just the signature. Examples: 'func.*Handler', 'def process', 'return', 'TODO'."},
			"path":            {Type: "string", Description: "MUST be absolute path to file or directory to search"},
			"include":         {Type: "string", Description: "Glob to filter files. Supports ** for recursive matching. Examples: '**/*.go', '**/*.ts', '**/*.py'. If omitted, auto-filtered to common source extensions."},
			"max_results":     {Type: "integer", Description: "Max functions to return. Default 15, max 50. Use lower values for large codebases to reduce token usage."},
			"offset":          {Type: "integer", Description: "Starting position for paginated results (0-based)."},
			"body":            {Type: "boolean", Description: "Include full function body in output. Default: false (signature + location only)."},
			"case_sensitive":  {Type: "boolean", Description: "Case-sensitive regex. Default: false (case-insensitive)."},
			"summary":         {Type: "boolean", Description: "If true and body=true, truncate large functions: shows first+last N lines with omission count. Reduces token cost for large handlers."},
			"summary_lines":   {Type: "integer", Description: "Lines to show at start and end when summary=true. Default 5."},
			"names_only":      {Type: "boolean", Description: "If true, return only file:line:name — no body, no signature. Cheapest mode (~20x fewer tokens than body=true). For table-of-contents scans."},
			"include_types":   {Type: "boolean", Description: "If true, also return type definitions (struct/class/interface/enum) in the results. Combines grep_func + grep_struct in one call."},
			"sig_lines":       {Type: "integer", Description: "Lines of signature when body=false. Default 1 (first line only). Use 2-3 for multi-line signatures."},
			"compact":         {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"receiver":        {Type: "string", Description: "Filter to methods on this receiver type (e.g. 'Server' finds func (s *Server) Method). Applies to Go, Rust, Python classes."},
			"group_by_file":   {Type: "boolean", Description: "Group results under file headers instead of a flat list. Reduces navigation overhead in large multi-file scans."},
			"token_budget":    {Type: "integer", Description: "Max output chars. If exceeded, auto-switches to names_only/summary mode. No default (unlimited)."},
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
	IncludeTypes   bool   `json:"include_types"`
	SigLines       int    `json:"sig_lines"`
	Compact        bool   `json:"compact"`
	Receiver       string `json:"receiver"`
	GroupByFile    bool   `json:"group_by_file"`
	TokenBudget    int    `json:"token_budget"`
	ExcludePattern string `json:"exclude_pattern"`
}

type FuncMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
	Name    string `json:"name"`
	Lines   int    `json:"lines"`
	Body    string `json:"body"`
	Kind    string `json:"kind,omitempty"`
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
	if a.SigLines <= 0 {
		a.SigLines = 1
	}

	pattern, err := CompilePattern(a.Pattern, a.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %v", err)
	}

	fetchMax := a.MaxResults
	if a.Offset > 0 {
		fetchMax = min(a.Offset+a.MaxResults, 200)
	}
	var results, typeResults []FuncMatch
	if a.IncludeTypes {
		var err error
		results, typeResults, err = SearchBoth(a.Path, a.Include, pattern, fetchMax)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		results, err = Search(a.Path, a.Include, pattern, fetchMax, IsFuncSig)
		if err != nil {
			return nil, err
		}
	}
	for i := range results {
		results[i].Kind = "func"
	}
	for i := range typeResults {
		typeResults[i].Kind = "type"
	}

	all := append(results, typeResults...)

	// Post-filter by receiver type name
	if a.Receiver != "" {
		all = filterByReceiver(all, a.Receiver)
	}

	// Exclude pattern filter
	if a.ExcludePattern != "" {
		all, err = filterByExclude(all, a.ExcludePattern)
		if err != nil {
			return nil, fmt.Errorf("exclude_pattern: %v", err)
		}
	}

	total := len(all)
	start := min(a.Offset, total)
	end := min(start+a.MaxResults, total)
	page := all[start:end]

	// Defer token_budget check: build full, then rebuild terse if needed
	buildOutput := func(namesOnly bool) string {
		var buf strings.Builder
		includeBody := !namesOnly && a.Body
		compact := a.Compact
		label := "match"
		if !a.IncludeTypes {
			label = "function"
		}
		if compact {
			fmt.Fprintf(&buf, "%d %ss %q", total, label, a.Pattern)
		} else {
			fmt.Fprintf(&buf, "%d %ss matching %q", total, label, a.Pattern)
		}
		if total > a.MaxResults || a.Offset > 0 {
			fmt.Fprintf(&buf, " (showing %d\u2013%d)", start+1, end)
		}
		if compact {
			buf.WriteString("\n")
		} else {
			buf.WriteString("\n")
		}
		type fileGroup struct {
			file    string
			matches []FuncMatch
		}
		groupResults := func(ms []FuncMatch) []fileGroup {
			seen := map[string]int{}
			var groups []fileGroup
			for _, m := range ms {
				rel := server.RelPath(m.File)
				if idx, ok := seen[rel]; ok {
					groups[idx].matches = append(groups[idx].matches, m)
				} else {
					seen[rel] = len(groups)
					groups = append(groups, fileGroup{file: rel, matches: []FuncMatch{m}})
				}
			}
			return groups
		}

		if a.GroupByFile {
			for _, g := range groupResults(page) {
				if compact {
					fmt.Fprintf(&buf, "%s (%d)\n", g.file, len(g.matches))
				} else {
					fmt.Fprintf(&buf, "\n%s — %d matches\n", g.file, len(g.matches))
				}
				if !includeBody {
					buf.WriteString("```\n")
				}
				for _, m := range g.matches {
					if namesOnly {
						nm := m.Name
						if a.IncludeTypes && m.Kind != "" {
							nm = "[" + m.Kind + "] " + nm
						}
						if compact {
							fmt.Fprintf(&buf, "L%d: %s\n", m.Line, nm)
						} else {
							fmt.Fprintf(&buf, "L%d: %s (%dL)\n", m.Line, nm, m.Lines)
						}
						continue
					}
					if includeBody {
						fmt.Fprintf(&buf, "L%d-%d: %s\n", m.Line, m.EndLine, firstLine(m.Body, true))
					} else {
						sig := sigPreview(m.Body, a.SigLines)
						if a.IncludeTypes && m.Kind != "" {
							sig = "[" + m.Kind + "] " + sig
						}
						fmt.Fprintf(&buf, "L%d-%d: %s\n", m.Line, m.EndLine, sig)
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
							body = SummarizeBody(body, sl)
						}
						fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(body, "\n"))
					}
				}
				if !includeBody {
					buf.WriteString("```\n")
				}
			}
		} else {
			if !includeBody {
				buf.WriteString("```\n")
			}
			for _, m := range page {
				rel := server.RelPath(m.File)
				if namesOnly {
					nm := m.Name
					if a.IncludeTypes && m.Kind != "" {
						nm = "[" + m.Kind + "] " + nm
					}
					if compact {
						fmt.Fprintf(&buf, "%s:%d: %s\n", rel, m.Line, nm)
					} else {
						fmt.Fprintf(&buf, "%s:%d: %s (%dL)\n", rel, m.Line, nm, m.Lines)
					}
					continue
				}
				if includeBody {
					fmt.Fprintf(&buf, "%s:%d-%d: %s\n", rel, m.Line, m.EndLine, firstLine(m.Body, true))
				} else {
					sig := sigPreview(m.Body, a.SigLines)
					if a.IncludeTypes && m.Kind != "" {
						sig = "[" + m.Kind + "] " + sig
					}
					fmt.Fprintf(&buf, "%s:%d-%d: %s\n", rel, m.Line, m.EndLine, sig)
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
						body = SummarizeBody(body, sl)
					}
					fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(body, "\n"))
				}
			}
			if !includeBody {
				buf.WriteString("```\n")
			}
		} // end group_by_file else
		if end < total {
			fmt.Fprintf(&buf, "\n%d more results. Use offset=%d for next page.", total-end, end)
		}
		return buf.String()
	}

	output := buildOutput(a.NamesOnly)
	if a.TokenBudget > 0 && len(output) > a.TokenBudget && !a.NamesOnly {
		output = buildOutput(true)
		if len(output) > a.TokenBudget {
			output = output[:a.TokenBudget]
		}
		output += fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. Use names_only=true or reduce scope for more.]\n", a.TokenBudget)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
	}, nil
}

func filterByReceiver(matches []FuncMatch, name string) []FuncMatch {
	// Match Go: func (ident Type) method or func (ident *Type) method
	pat := `\(\s*\*?\s*` + regexp.QuoteMeta(name) + `\s*\)`
	re := regexp.MustCompile(pat)
	out := matches[:0]
	for _, m := range matches {
		if re.MatchString(m.Body) {
			out = append(out, m)
		}
	}
	return out
}

func filterByExclude(matches []FuncMatch, excludePattern string) ([]FuncMatch, error) {
	re, err := CompilePattern(excludePattern, false)
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

func sigPreview(body string, n int) string {
	if n <= 1 {
		return firstLine(body, false)
	}
	lines := strings.SplitN(body, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	s := strings.TrimSpace(strings.Join(lines, " ↵ "))
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
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

func SummarizeBody(body string, n int) string {
	lines := strings.Split(body, "\n")
	if len(lines) <= n*2+1 {
		return body
	}
	omitted := len(lines) - n*2
	var buf strings.Builder
	for i := range n {
		buf.WriteString(lines[i])
		buf.WriteByte('\n')
	}
	fmt.Fprintf(&buf, "// ... (%d lines omitted)\n", omitted)
	for i := len(lines) - n; i < len(lines); i++ {
		buf.WriteString(lines[i])
		if i < len(lines)-1 {
			buf.WriteByte('\n')
		}
	}
	return buf.String()
}
