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
	Description: "Use when you need a function/method's full definition to read or edit its code. Returns complete brace-aware bodies with line numbers — native grep only returns matching lines, forcing a follow-up read. body=true returns full bodies; default returns signature + location. Set symbol=<name> to search for a pattern inside one specific symbol's body. For plain-text search (constants, config, prose) use grep instead.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":         {Type: "string", Description: "Regex pattern to match against function names or code. Matches anywhere inside a function's lines — not just the signature. Examples: 'func.*Handler', 'def process', 'return', 'TODO'."},
			"path":            {Type: "string", Description: "Directory to search. Optional — defaults to the opened project root."},
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
			"symbol":          {Type: "string", Description: "If set, search INSIDE the named symbol's body instead of listing functions. Finds the symbol (function or type) by name, then returns matching lines with context — ~5x cheaper than body=true for targeted searches."},
			"context_lines":   {Type: "integer", Description: "Lines of context around each match when symbol is set. Default 2, max 8."},
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
	Symbol         string `json:"symbol"`
	ContextLines   int    `json:"context_lines"`
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
	if a.Symbol != "" {
		return scopedSearch(a, pattern)
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

func scopedSearch(a args, patRe *regexp.Regexp) (*server.ToolCallResult, error) {
	if a.ContextLines <= 0 {
		a.ContextLines = 2
	}
	if a.ContextLines > 8 {
		a.ContextLines = 8
	}
	symbolRe, err := CompilePattern(`\b`+regexp.QuoteMeta(a.Symbol)+`\b`, a.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid symbol name: %v", err)
	}

	var symbols []FuncMatch
	funcs, _ := Search(a.Path, a.Include, symbolRe, 20, IsFuncSig)
	for _, f := range funcs {
		if symbolRe.MatchString(f.Name) {
			symbols = append(symbols, f)
		}
	}
	types, _ := Search(a.Path, a.Include, symbolRe, 20, IsStructSig)
	for _, t := range types {
		if symbolRe.MatchString(t.Name) {
			symbols = append(symbols, t)
		}
	}

	if len(symbols) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("Symbol %q not found.", a.Symbol)}},
		}, nil
	}

	var buf strings.Builder
	totalMatches := 0

	for _, sym := range symbols {
		rel := server.RelPath(sym.File)
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(sym.File)), ".")
		ctx := a.ContextLines
		bodyLines := strings.Split(sym.Body, "\n")
		var hitIdxs []int
		for i, line := range bodyLines {
			if patRe.MatchString(line) {
				hitIdxs = append(hitIdxs, i)
			}
		}
		if len(hitIdxs) == 0 {
			continue
		}
		totalMatches += len(hitIdxs)

		startLine := sym.Line
		if startLine == 0 {
			startLine = 1
		}

		fmt.Fprintf(&buf, "%d matches for /%s/ in %q (%s:L%d-%d):\n\n",
			len(hitIdxs), a.Pattern, sym.Name, rel, startLine, sym.EndLine)

		type window struct {
			start, end int
			matches    map[int]bool
		}
		var windows []window
		for _, idx := range hitIdxs {
			ws := max(0, idx-ctx)
			we := min(len(bodyLines)-1, idx+ctx)
			if len(windows) > 0 && ws <= windows[len(windows)-1].end+1 {
				last := &windows[len(windows)-1]
				last.end = max(last.end, we)
				last.matches[idx] = true
			} else {
				windows = append(windows, window{start: ws, end: we, matches: map[int]bool{idx: true}})
			}
		}

		for _, w := range windows {
			fmt.Fprintf(&buf, "```%s\n", ext)
			for i := w.start; i <= w.end; i++ {
				absLine := startLine + i
				if w.matches[i] {
					fmt.Fprintf(&buf, "> L%d: %s\n", absLine, bodyLines[i])
				} else {
					fmt.Fprintf(&buf, "  L%d: %s\n", absLine, bodyLines[i])
				}
			}
			buf.WriteString("```\n\n")
		}
	}

	if totalMatches == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No matches for /%s/ inside %q.", a.Pattern, a.Symbol)}},
		}, nil
	}

	output := buf.String()
	if a.TokenBudget > 0 && len(output) > a.TokenBudget {
		var terse strings.Builder
		for _, sym := range symbols {
			rel := server.RelPath(sym.File)
			lines := strings.Split(sym.Body, "\n")
			startLine := sym.Line
			if startLine == 0 {
				startLine = 1
			}
			for i, line := range lines {
				if patRe.MatchString(line) {
					fmt.Fprintf(&terse, "%s:%d: %s\n", rel, startLine+i, strings.TrimSpace(line))
				}
			}
		}
		output = terse.String()
		if len(output) > a.TokenBudget {
			output = output[:a.TokenBudget]
		}
		output += fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. Use names_only=true or reduce scope for more.]\n", a.TokenBudget)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
	}, nil
}