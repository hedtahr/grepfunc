// Package findsymbol implements the find_symbol MCP tool.
package findsymbol

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const (
	typeString              = "string"
	typeInteger             = "integer"
	typeBoolean             = "boolean"
	kindAny                 = "any"
	defaultMaxResults       = 10
	maxMaxResults           = 30
	searchMultiplier        = 2
	minSuggestionLen        = 3
	suggestionPrefixLen     = 3
	maxSuggestionCandidates = 50
	maxSuggestionDist       = 5
	maxSuggestions          = 3
	defaultSummaryLines     = 5
)

var errNameRequired = errors.New("name is required")

// Tool is the find_symbol MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "find_symbol",
	Description: "Find a symbol by name: file:line + signature. body=true for full body.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":           {Type: typeString, Description: "Symbol name (function/method/type). Case-insensitive.", Items: nil},
			"path":           {Type: typeString, Description: "Directory to search. Defaults to project root.", Items: nil},
			"include":        {Type: typeString, Description: "Glob filter. Auto: source extensions.", Items: nil},
			"kind":           {Type: typeString, Description: "Filter by kind: 'func', 'type', or 'any' (default).", Items: nil},
			"max_results":    {Type: typeInteger, Description: "Max results. Default 10, max 30.", Items: nil},
			"case_sensitive": {Type: typeBoolean, Description: "Case-sensitive. Default false.", Items: nil},
			"compact":        {Type: typeBoolean, Description: "Terse output.", Items: nil},
			"names_only":     {Type: typeBoolean, Description: "Only file:line:name — cheapest.", Items: nil},
			"body":           {Type: typeBoolean, Description: "Full body of each match. Default false.", Items: nil},
			"token_budget":   {Type: typeInteger, Description: "Max output chars; degrades to names_only, then truncates.", Items: nil},
			"summary":        {Type: typeBoolean, Description: "First+last N lines of bodies + omission count.", Items: nil},
			"summary_lines":  {Type: typeInteger, Description: "N lines at start/end. Default 5.", Items: nil},
		},
		Required:             []string{"name"},
		AdditionalProperties: false,
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

// Handle processes a find_symbol tool call and returns the result.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	arg, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	pattern, err := compileNamePattern(arg)
	if err != nil {
		return nil, err
	}

	results := searchSymbols(arg, pattern)
	results = substringFallback(arg, pattern, results)

	// If still no results, suggest closest names via Levenshtein.
	if len(results) == 0 {
		suggestions := suggestClosest(arg, flagsFor(arg))
		if len(suggestions) > 0 {
			return suggestionResult(arg, suggestions), nil
		}
	}

	// Limit.
	if len(results) > arg.MaxResults {
		results = results[:arg.MaxResults]
	}

	output := buildOutput(arg, results, arg.NamesOnly)
	output = applyTokenBudget(arg, results, output)

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
		IsError: false,
	}, nil
}

func substringFallback(arg args, _ *regexp.Regexp, results []grepfunc.FuncMatch) []grepfunc.FuncMatch {
	// Fallback: substring match.
	if len(results) == 0 && len(arg.Name) >= minSuggestionLen {
		subPattern, _ := regexp.Compile(flagsFor(arg) + regexp.QuoteMeta(arg.Name))

		return searchSymbols(arg, subPattern)
	}

	return results
}

func applyTokenBudget(arg args, results []grepfunc.FuncMatch, output string) string {
	if arg.TokenBudget > 0 && len(output) > arg.TokenBudget && !arg.NamesOnly {
		return trimOutput(arg, results)
	}

	return output
}

func suggestionResult(arg args, suggestions []grepfunc.FuncMatch) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: renderSuggestions(arg, suggestions)}},
		IsError: false,
	}
}

func trimOutput(arg args, results []grepfunc.FuncMatch) string {
	output := buildOutput(arg, results, true)
	output = server.TruncateToBudget(output, arg.TokenBudget)

	return output + fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. "+
		"Use names_only=true or reduce scope for more.]\n", arg.TokenBudget)
}

func parseArgs(raw json.RawMessage) (args, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return arg, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Name == "" {
		return arg, errNameRequired
	}

	arg.Path = server.ResolvePath(arg.Path)

	err = server.CheckBounds(arg.Path)
	if err != nil {
		return arg, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(arg.Path)
	if err != nil {
		return arg, fmt.Errorf("check banned: %w", err)
	}

	if arg.MaxResults <= 0 {
		arg.MaxResults = defaultMaxResults
	}

	if arg.MaxResults > maxMaxResults {
		arg.MaxResults = maxMaxResults
	}

	if arg.Kind == "" {
		arg.Kind = kindAny
	}

	if arg.Include == "" {
		arg.Include = "*"
	}

	return arg, nil
}

func compileNamePattern(arg args) (*regexp.Regexp, error) {
	pattern, err := regexp.Compile(flagsFor(arg) + `\b` + regexp.QuoteMeta(arg.Name) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid name pattern: %w", err)
	}

	return pattern, nil
}

func flagsFor(arg args) string {
	if arg.CaseSensitive {
		return ""
	}

	return "(?i)"
}

func searchSymbols(arg args, pattern *regexp.Regexp) []grepfunc.FuncMatch {
	var results []grepfunc.FuncMatch

	if arg.Kind == kindAny || arg.Kind == "func" {
		funcs, _ := grepfunc.Search(arg.Path, arg.Include, pattern, arg.MaxResults*searchMultiplier, grepfunc.IsFuncSig)
		results = appendMatches(results, funcs, pattern)
	}

	if arg.Kind == kindAny || arg.Kind == "type" {
		types, _ := grepfunc.Search(arg.Path, arg.Include, pattern, arg.MaxResults*searchMultiplier, grepfunc.IsStructSig)
		results = appendMatches(results, types, pattern)
	}

	return dedupeMatches(results)
}

func appendMatches(dst []grepfunc.FuncMatch, src []grepfunc.FuncMatch, pattern *regexp.Regexp) []grepfunc.FuncMatch {
	for _, match := range src {
		if pattern.MatchString(match.Name) {
			dst = append(dst, match)
		}
	}

	return dst
}

func dedupeMatches(results []grepfunc.FuncMatch) []grepfunc.FuncMatch {
	// Deduplicate by file+line.
	seen := make(map[string]bool)

	unique := results[:0]

	for _, match := range results {
		key := fmt.Sprintf("%s:%d", match.File, match.Line)
		if !seen[key] {
			seen[key] = true

			unique = append(unique, match)
		}
	}

	return unique
}

func suggestClosest(arg args, flags string) []grepfunc.FuncMatch {
	// Search with loose prefix pattern to collect candidates.
	prefix := arg.Name
	if len(prefix) > suggestionPrefixLen {
		prefix = prefix[:suggestionPrefixLen]
	}

	loose, _ := regexp.Compile(flags + regexp.QuoteMeta(prefix))
	candidates := searchSymbols(args{
		Name:          "",
		Path:          arg.Path,
		Include:       arg.Include,
		Kind:          arg.Kind,
		MaxResults:    maxSuggestionCandidates,
		CaseSensitive: arg.CaseSensitive,
		Compact:       false,
		NamesOnly:     false,
		Body:          false,
		Summary:       false,
		SummaryLines:  0,
		TokenBudget:   0,
	}, loose)

	if len(candidates) == 0 {
		return nil
	}

	type pair struct {
		match grepfunc.FuncMatch
		dist  int
	}

	var pairs []pair

	for _, candidate := range candidates {
		dist := levenshtein(strings.ToLower(arg.Name), strings.ToLower(candidate.Name))
		if dist <= min(len(arg.Name), maxSuggestionDist) {
			pairs = append(pairs, pair{candidate, dist})
		}
	}

	// Sort by distance.
	sort.SliceStable(pairs, func(left, right int) bool {
		return pairs[left].dist < pairs[right].dist
	})

	if len(pairs) > maxSuggestions {
		pairs = pairs[:maxSuggestions]
	}

	out := make([]grepfunc.FuncMatch, len(pairs))
	for i, item := range pairs {
		out[i] = item.match
	}

	return out
}

func renderSuggestions(arg args, suggestions []grepfunc.FuncMatch) string {
	var buf strings.Builder

	fmt.Fprintf(&buf, "No symbols found for %q.\n\nDid you mean:\n", arg.Name)

	for _, suggestion := range suggestions {
		fmt.Fprintf(&buf, "- %s:%d: %s\n", server.RelPath(suggestion.File), suggestion.Line, suggestion.Name)
	}

	return buf.String()
}

func buildOutput(arg args, results []grepfunc.FuncMatch, namesOnly bool) string {
	var buf strings.Builder

	if len(results) == 0 {
		fmt.Fprintf(&buf, "No symbols found for %q.\n", arg.Name)

		return buf.String()
	}

	if !namesOnly {
		fmt.Fprintf(&buf, "%d symbols %q:\n", len(results), arg.Name)
	}

	for _, match := range results {
		renderMatch(&buf, arg, match, namesOnly)
	}

	return buf.String()
}

func renderMatch(buf *strings.Builder, arg args, match grepfunc.FuncMatch, namesOnly bool) {
	rel := server.RelPath(match.File)
	if namesOnly {
		fmt.Fprintf(buf, "%s:%d: %s\n", rel, match.Line, match.Name)

		return
	}

	ext := strings.TrimPrefix(filepath.Ext(match.File), ".")
	if ext == "" {
		ext = "go"
	}

	fmt.Fprintf(buf, "%s:%d-%d: %s\n", rel, match.Line, match.EndLine, match.Name)

	if arg.Body {
		renderBody(buf, arg, ext, match.Body)
	} else {
		renderSigLine(buf, ext, match.Body)
	}

	buf.WriteByte('\n')
}

func renderBody(buf *strings.Builder, arg args, ext, body string) {
	if arg.Summary {
		sl := arg.SummaryLines
		if sl <= 0 {
			sl = defaultSummaryLines
		}

		body = grepfunc.SummarizeBody(body, sl)
	}

	fmt.Fprintf(buf, "```%s\n%s\n```", ext, strings.TrimRight(body, "\n"))
}

func renderSigLine(buf *strings.Builder, ext, body string) {
	sigLine := server.FirstLine(body, 120)
	if sigLine != "" {
		fmt.Fprintf(buf, "```%s\n%s\n```", ext, sigLine)
	}
}

func levenshtein(left, right string) int {
	if len(left) == 0 {
		return len(right)
	}

	if len(right) == 0 {
		return len(left)
	}

	// Use two rows.
	prev := make([]int, len(right)+1)

	cur := make([]int, len(right)+1)

	for i := range len(right) + 1 {
		prev[i] = i
	}

	for i := range len(left) {
		cur[0] = i + 1

		for col := range len(right) {
			cost := 1
			if left[i] == right[col] {
				cost = 0
			}

			cur[col+1] = min(prev[col+1]+1, min(cur[col]+1, prev[col]+cost))
		}

		prev, cur = cur, prev
	}

	return prev[len(right)]
}
