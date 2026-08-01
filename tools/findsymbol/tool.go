package findsymbol

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
	"github.com/hedtahr/grepfunc/tools/internal/util"
)

var Tool = server.Tool{
	Name:        "find_symbol",
	Description: "Use when you know a symbol's NAME and need its location, signature, or full definition. Returns file:line + signature — set body=true for the complete body in one call (absorbs read_symbol). Word-boundary match first, substring fallback, then typo suggestions.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":           {Type: "string", Description: "Symbol name to find. Matches function/method/type names. Examples: 'Handle', 'UserService', 'findRelated'. Case-insensitive by default."},
			"path":           {Type: "string", Description: "Directory to search. Optional — defaults to the opened project root."},
			"include":        {Type: "string", Description: "Glob to filter files. Supports ** for recursive matching. If omitted, auto-filtered to common source extensions."},
			"kind":           {Type: "string", Description: "Filter by kind: 'func' (functions/methods only), 'type' (structs/classes/interfaces/enums only), or 'any' (default)."},
			"max_results":    {Type: "integer", Description: "Max results. Default 10, max 30."},
			"case_sensitive": {Type: "boolean", Description: "Case-sensitive matching. Default: false."},
			"compact":        {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"names_only":     {Type: "boolean", Description: "If true, return only file:line:name — no code blocks. Cheapest mode."},
			"body":           {Type: "boolean", Description: "If true, return the full body of each matched symbol — no second look-up call needed. Default false (signature only)."},
			"token_budget":   {Type: "integer", Description: "Max output chars. If exceeded, auto-switches to names_only. No default (unlimited)."},
			"summary":        {Type: "boolean", Description: "If true and body=true, truncate large bodies: shows first+last N lines with omission count."},
			"summary_lines":  {Type: "integer", Description: "Lines to show at start and end when summary=true. Default 5."},
		},
		Required: []string{"name"},
	},
}

type args struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	Kind          string `json:"kind"`
	MaxResults    int    `json:"max_results"`
	CaseSensitive bool   `json:"case_sensitive"`
	Compact       bool   `json:"compact"`
	NamesOnly     bool   `json:"names_only"`
	Body          bool   `json:"body"`
	Summary       bool   `json:"summary"`
	SummaryLines  int    `json:"summary_lines"`
	TokenBudget   int    `json:"token_budget"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBounds(a.Path); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 10
	}
	if a.MaxResults > 30 {
		a.MaxResults = 30
	}
	if a.Kind == "" {
		a.Kind = "any"
	}
	if a.Include == "" {
		a.Include = "*"
	}

	// Try exact word-boundary match
	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}
	pattern, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.Name) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid name pattern: %v", err)
	}

	results := searchSymbols(a, pattern)

	// Fallback: substring match
	if len(results) == 0 && len(a.Name) >= 3 {
		subPattern, _ := regexp.Compile(flags + regexp.QuoteMeta(a.Name))
		results = searchSymbols(a, subPattern)
	}

	// If still no results, suggest closest names via Levenshtein
	if len(results) == 0 {
		suggestions := suggestClosest(a, flags)
		if len(suggestions) > 0 {
			var buf strings.Builder
			fmt.Fprintf(&buf, "No symbols found for %q.\n\nDid you mean:\n", a.Name)
			for _, s := range suggestions {
				fmt.Fprintf(&buf, "- %s:%d: %s\n", server.RelPath(s.File), s.Line, s.Name)
			}
			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
			}, nil
		}
	}

	// Cache results for read_symbol
	for _, r := range results {
		server.CacheSet("symbol:"+r.File+":"+r.Name, r)
	}

	// Limit
	if len(results) > a.MaxResults {
		results = results[:a.MaxResults]
	}

	buildOutput := func(namesOnly bool) string {
		compact := a.Compact
		var buf strings.Builder
		if len(results) == 0 {
			fmt.Fprintf(&buf, "No symbols found for %q.\n", a.Name)
			return buf.String()
		}

		if !namesOnly {
			if compact {
				fmt.Fprintf(&buf, "%d symbols %q:\n", len(results), a.Name)
			} else {
				fmt.Fprintf(&buf, "%d symbols %q:\n", len(results), a.Name)
			}
		}
		for _, m := range results {
			rel := server.RelPath(m.File)
			if namesOnly {
				fmt.Fprintf(&buf, "%s:%d: %s\n", rel, m.Line, m.Name)
				continue
			}
			ext := strings.TrimPrefix(filepath.Ext(m.File), ".")
			if ext == "" {
				ext = "go"
			}
			fmt.Fprintf(&buf, "%s:%d-%d: %s\n", rel, m.Line, m.EndLine, m.Name)
			if a.Body {
				body := m.Body
				if a.Summary {
					sl := a.SummaryLines
					if sl <= 0 {
						sl = 5
					}
					body = grepfunc.SummarizeBody(body, sl)
				}
				fmt.Fprintf(&buf, "```%s\n%s\n```", ext, strings.TrimRight(body, "\n"))
			} else {
				sigLine := util.FirstSigLine(m.Body)
				if sigLine != "" {
					fmt.Fprintf(&buf, "```%s\n%s\n```", ext, sigLine)
				}
			}
			if compact {
				buf.WriteByte('\n')
			} else {
				buf.WriteByte('\n')
			}
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

func searchSymbols(a args, pattern *regexp.Regexp) []grepfunc.FuncMatch {
	var results []grepfunc.FuncMatch

	if a.Kind == "any" || a.Kind == "func" {
		funcs, _ := grepfunc.Search(a.Path, a.Include, pattern, a.MaxResults*2, grepfunc.IsFuncSig)
		for _, f := range funcs {
			if pattern.MatchString(f.Name) {
				results = append(results, f)
			}
		}
	}

	if a.Kind == "any" || a.Kind == "type" {
		types, _ := grepfunc.Search(a.Path, a.Include, pattern, a.MaxResults*2, grepfunc.IsStructSig)
		for _, t := range types {
			if pattern.MatchString(t.Name) {
				results = append(results, t)
			}
		}
	}

	// Deduplicate by file+line
	seen := make(map[string]bool)
	unique := results[:0]
	for _, r := range results {
		key := fmt.Sprintf("%s:%d", r.File, r.Line)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, r)
		}
	}
	return unique
}

func suggestClosest(a args, flags string) []grepfunc.FuncMatch {
	// Search with loose prefix pattern to collect candidates
	prefix := a.Name
	if len(prefix) > 3 {
		prefix = prefix[:3]
	}
	loose, _ := regexp.Compile(flags + regexp.QuoteMeta(prefix))
	candidates := searchSymbols(args{
		Path: a.Path, Include: a.Include, Kind: a.Kind,
		MaxResults: 50, CaseSensitive: a.CaseSensitive,
	}, loose)

	if len(candidates) == 0 {
		return nil
	}

	type pair struct {
		m grepfunc.FuncMatch
		d int
	}
	var pairs []pair
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(a.Name), strings.ToLower(c.Name))
		if d <= min(len(a.Name), 5) {
			pairs = append(pairs, pair{c, d})
		}
	}

	// Sort by distance
	for i := 0; i < len(pairs); i++ {
		for j := i + 1; j < len(pairs); j++ {
			if pairs[j].d < pairs[i].d {
				pairs[i], pairs[j] = pairs[j], pairs[i]
			}
		}
	}

	if len(pairs) > 3 {
		pairs = pairs[:3]
	}
	out := make([]grepfunc.FuncMatch, len(pairs))
	for i, p := range pairs {
		out[i] = p.m
	}
	return out
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	// Use two rows
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 0; i <= len(b); i++ {
		prev[i] = i
	}

	for i := 0; i < len(a); i++ {
		cur[0] = i + 1
		for j := 0; j < len(b); j++ {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j+1]+1, min(cur[j]+1, prev[j]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
