package grepfunc

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

// Schema keys and content types shared by the tool definition and callers.
const (
	schemaPattern = "pattern"
	schemaPath    = "path"
	schemaInclude = "include"
	schemaString  = "string"
	schemaInteger = "integer"
	schemaBoolean = "boolean"
	schemaBody    = "body"
	schemaText    = "text"
)

// Caps for search result sizes and previews.
const (
	maxResultsCap       = 50
	fetchCap            = 200
	scopedSearchMax     = 20
	sigPreviewCap       = 240
	maxContextCap       = 8
	summarySides        = 2
	defaultSummaryLines = 5
)

// errPatternRequired is returned when no pattern is supplied.
var errPatternRequired = errors.New("pattern is required")

// Tool defines the grep_func MCP tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_func",
	Description: "Function/method search, brace-aware bodies. body=true → full bodies; default signature+location. Skips .gitignore'd dirs.",
	InputSchema: server.InputSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]server.Property{
			schemaPattern: {
				Type:        schemaString,
				Items:       nil,
				Description: "Regex on function names or bodies.",
			},
			schemaPath: {
				Type:        schemaString,
				Items:       nil,
				Description: "Directory to search. Defaults to project root.",
			},
			schemaInclude: {
				Type:        schemaString,
				Items:       nil,
				Description: "Glob filter (**). Auto: source extensions.",
			},
			"max_results": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Max functions to return. Default 15, max 50.",
			},
			"offset": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Pagination offset (0-based).",
			},
			schemaBody: {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Include full body. Default false (signature+location).",
			},
			"case_sensitive": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Case-sensitive regex. Default false.",
			},
			"summary": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "First+last N lines of bodies + omission count.",
			},
			"summary_lines": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "N lines at start/end. Default 5.",
			},
			"names_only": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Only file:line:name — cheapest.",
			},
			"include_types": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Also return type definitions.",
			},
			"sig_lines": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Signature lines when body=false. Default 1; 2-3 for multi-line.",
			},
			"compact": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Terse output.",
			},
			"receiver": {
				Type:        schemaString,
				Items:       nil,
				Description: "Only methods on this receiver type (e.g. 'Server').",
			},
			"group_by_file": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Group results under file headers.",
			},
			"token_budget": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Max output chars; degrades to names_only, then truncates.",
			},
			"exclude_pattern": {
				Type:        schemaString,
				Items:       nil,
				Description: "Regex to exclude results (body/name).",
			},
			"symbol": {
				Type:        schemaString,
				Items:       nil,
				Description: "Search INSIDE this symbol body (~5x cheaper than body=true).",
			},
			"context_lines": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Context lines around matches when symbol set. Default 2, max 8.",
			},
		},
		Required: []string{schemaPattern},
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

// FuncMatch is a matched function or type block extracted from a file.
type FuncMatch struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
	Name    string `json:"name"`
	Lines   int    `json:"lines"`
	Body    string `json:"body"`
	Kind    string `json:"kind,omitempty"`
}

// Handle processes a grep_func tool call.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Pattern == "" {
		return nil, errPatternRequired
	}

	applyDefaults(&arg)

	if err := server.CheckBounds(arg.Path); err != nil {
		return nil, err
	}

	pattern, err := CompilePattern(arg.Pattern, arg.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	if arg.Symbol != "" {
		return scopedSearch(arg, pattern)
	}

	fetchMax := arg.MaxResults
	if arg.Offset > 0 {
		fetchMax = min(arg.Offset+arg.MaxResults, fetchCap)
	}

	results, typeResults, err := searchMatches(arg, pattern, fetchMax)
	if err != nil {
		return nil, err
	}

	all := setKinds(results, typeResults)

	// Post-filter by receiver type name
	if arg.Receiver != "" {
		all = filterByReceiver(all, arg.Receiver)
	}

	// Exclude pattern filter
	if arg.ExcludePattern != "" {
		all, err = filterByExclude(all, arg.ExcludePattern)
		if err != nil {
			return nil, fmt.Errorf("exclude_pattern: %w", err)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: schemaText, Text: renderPaged(arg, all)}},
		IsError: false,
	}, nil
}

// applyDefaults fills in default values for unset args.
func applyDefaults(arg *args) {
	arg.Path = server.ResolvePath(arg.Path)
	if arg.MaxResults <= 0 {
		arg.MaxResults = 15
	}

	if arg.MaxResults > maxResultsCap {
		arg.MaxResults = maxResultsCap
	}

	if arg.Include == "" {
		arg.Include = "*"
	}

	if arg.SigLines <= 0 {
		arg.SigLines = 1
	}
}

// setKinds labels func and type matches, then merges them into one slice.
func setKinds(results, typeResults []FuncMatch) []FuncMatch {
	for i := range results {
		results[i].Kind = "func"
	}

	for i := range typeResults {
		typeResults[i].Kind = "type"
	}

	all := make([]FuncMatch, 0, len(results)+len(typeResults))
	all = append(all, results...)
	all = append(all, typeResults...)

	return all
}

// searchMatches runs Search or SearchBoth depending on includeTypes.
func searchMatches(arg args, pattern *regexp.Regexp, fetchMax int) ([]FuncMatch, []FuncMatch, error) {
	if arg.IncludeTypes {
		return SearchBoth(arg.Path, arg.Include, pattern, fetchMax)
	}

	results, err := Search(arg.Path, arg.Include, pattern, fetchMax, IsFuncSig)

	return results, nil, err
}

// renderPaged applies pagination and the token budget to the final output.
func renderPaged(arg args, all []FuncMatch) string {
	total := len(all)
	start := min(arg.Offset, total)
	end := min(start+arg.MaxResults, total)
	page := all[start:end]

	// When full bodies are requested and can't possibly fit the budget, render
	// the names_only form directly — no point building output we'd throw away.
	if arg.TokenBudget > 0 && arg.Body && !arg.NamesOnly && estBodyBytes(page) > arg.TokenBudget {
		return renderBudgetTerse(arg, page, total, start, end)
	}

	output := renderResults(arg, page, total, start, end, arg.NamesOnly)
	if arg.TokenBudget > 0 && len(output) > arg.TokenBudget && !arg.NamesOnly {
		output = renderResults(arg, page, total, start, end, true)
		output = server.TruncateToBudget(output, arg.TokenBudget) + budgetTip(arg.TokenBudget)
	}

	return output
}

// budgetTip returns the standard trimmed-output notice for grep_func renders.
func budgetTip(budget int) string {
	return server.BudgetHint(budget, "Use names_only=true or reduce scope for more.")
}

// estBodyBytes estimates the rendered bytes of a page's bodies plus per-match overhead.
func estBodyBytes(page []FuncMatch) int {
	bytes := 0

	for _, m := range page {
		bytes += len(m.Body) + 80
	}

	return bytes
}

// renderBudgetTerse builds names_only output bounded by the token budget.
func renderBudgetTerse(arg args, page []FuncMatch, total, start, end int) string {
	output := renderResults(arg, page, total, start, end, true)
	output = server.TruncateToBudget(output, arg.TokenBudget)
	output += server.BudgetHint(arg.TokenBudget, "Bodies exceed the budget — shown as names_only. Use summary=true or a narrower pattern.")

	return output
}

// ZeroMatchHint explains an empty result set: include globs match root-relative
// paths and the default "*" searches known source extensions only.
func ZeroMatchHint(include string) string {
	return fmt.Sprintf("no matches \u2014 include %q matches paths relative to the search root; "+
		"the default \"*\" searches known source extensions only "+
		"(pass include, e.g. \"**/*.plsql\", to widen).\n", include)
}

// renderResults builds the text output for a page of matches.
func renderResults(arg args, page []FuncMatch, total, start, end int, namesOnly bool) string {
	var buf strings.Builder

	includeBody := !namesOnly && arg.Body
	compact := arg.Compact

	label := "match"
	if !arg.IncludeTypes {
		label = "function"
	}

	if compact {
		fmt.Fprintf(&buf, "%d %ss %q", total, label, arg.Pattern)
	} else {
		fmt.Fprintf(&buf, "%d %ss matching %q", total, label, arg.Pattern)
	}

	if total > arg.MaxResults || arg.Offset > 0 {
		fmt.Fprintf(&buf, " (showing %d\u2013%d)", start+1, end)
	}

	buf.WriteString("\n")

	if total == 0 {
		buf.WriteString(ZeroMatchHint(arg.Include))
	}

	if arg.GroupByFile {
		renderGrouped(&buf, arg, page, namesOnly, includeBody, compact)
	} else {
		renderFlat(&buf, arg, page, namesOnly, includeBody, compact)
	}

	if end < total {
		fmt.Fprintf(&buf, "\n%d more results. Use offset=%d for next page.", total-end, end)
	}

	return buf.String()
}

// fileGroup groups matches by file for the group_by_file output.
type fileGroup struct {
	file    string
	matches []FuncMatch
}

// groupResults buckets matches by their relative path, preserving order.
func groupResults(ms []FuncMatch) []fileGroup {
	seen := map[string]int{}

	var groups []fileGroup

	for _, match := range ms {
		rel := server.RelPath(match.File)
		if idx, ok := seen[rel]; ok {
			groups[idx].matches = append(groups[idx].matches, match)
		} else {
			seen[rel] = len(groups)
			groups = append(groups, fileGroup{file: rel, matches: []FuncMatch{match}})
		}
	}

	return groups
}

// renderGrouped writes matches grouped under file headers.
func renderGrouped(buf *strings.Builder, arg args, page []FuncMatch, namesOnly, includeBody, compact bool) {
	for _, group := range groupResults(page) {
		if compact {
			fmt.Fprintf(buf, "%s (%d)\n", group.file, len(group.matches))
		} else {
			fmt.Fprintf(buf, "\n%s — %d matches\n", group.file, len(group.matches))
		}

		if !includeBody {
			buf.WriteString("```\n")
		}

		for _, match := range group.matches {
			appendMatch(buf, match, "L", namesOnly, includeBody, compact, arg)
		}

		if !includeBody {
			buf.WriteString("```\n")
		}
	}
}

// renderFlat writes matches one per line.
func renderFlat(buf *strings.Builder, arg args, page []FuncMatch, namesOnly, includeBody, compact bool) {
	if !includeBody {
		buf.WriteString("```\n")
	}

	for _, match := range page {
		rel := server.RelPath(match.File)
		appendMatch(buf, match, rel+":", namesOnly, includeBody, compact, arg)
	}

	if !includeBody {
		buf.WriteString("```\n")
	}
}

// appendMatch writes a single match line (and body block when requested).
func appendMatch(buf *strings.Builder, match FuncMatch, loc string, namesOnly, includeBody, compact bool, arg args) {
	if namesOnly {
		name := kindPrefix(match.Kind, arg.IncludeTypes) + match.Name

		if compact {
			fmt.Fprintf(buf, "%s%d: %s\n", loc, match.Line, name)
		} else {
			fmt.Fprintf(buf, "%s%d: %s (%dL)\n", loc, match.Line, name, match.Lines)
		}

		return
	}

	if includeBody {
		fmt.Fprintf(buf, "%s%d-%d: %s\n", loc, match.Line, match.EndLine, server.FirstLine(match.Body, 0))
	} else {
		sig := kindPrefix(match.Kind, arg.IncludeTypes) + sigPreview(match.Body, arg.SigLines)
		fmt.Fprintf(buf, "%s%d-%d: %s\n", loc, match.Line, match.EndLine, sig)
	}

	if includeBody {
		body := match.Body

		if arg.Summary {
			body = SummarizeBody(body, summarizeLines(arg.SummaryLines))
		}

		fmt.Fprintf(buf, "```%s\n%s\n```\n", codeFenceExt(match.File), strings.TrimRight(body, "\n"))
	}
}

// kindPrefix returns the "[kind] " prefix when includeTypes is set and kind is known.
func kindPrefix(kind string, includeTypes bool) string {
	if includeTypes && kind != "" {
		return "[" + kind + "] "
	}

	return ""
}

// codeFenceExt returns the code-fence language tag for a file, defaulting to go.
func codeFenceExt(file string) string {
	ext := strings.TrimPrefix(filepath.Ext(file), ".")
	if ext == "" {
		return "go"
	}

	return ext
}

// summarizeLines returns the number of summary lines, defaulting to 5.
func summarizeLines(n int) int {
	if n <= 0 {
		return defaultSummaryLines
	}

	return n
}

func filterByReceiver(matches []FuncMatch, name string) []FuncMatch {
	// Match Go: func (ident Type) method or func (ident *Type) method
	pat := `\(\s*\*?\s*` + regexp.QuoteMeta(name) + `\s*\)`
	patRe := regexp.MustCompile(pat)

	out := matches[:0]

	for _, m := range matches {
		if patRe.MatchString(m.Body) {
			out = append(out, m)
		}
	}

	return out
}

func filterByExclude(matches []FuncMatch, excludePattern string) ([]FuncMatch, error) {
	excludeRe, err := CompilePattern(excludePattern, false)
	if err != nil {
		return nil, err
	}

	out := matches[:0]

	for _, m := range matches {
		if excludeRe.MatchString(m.Body) || excludeRe.MatchString(m.Name) {
			continue
		}

		out = append(out, m)
	}

	return out, nil
}

func sigPreview(body string, maxLines int) string {
	if maxLines <= 1 {
		return server.FirstLine(body, 120)
	}

	lines := strings.SplitN(body, "\n", maxLines+1)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}

	s := strings.TrimSpace(strings.Join(lines, " ↵ "))
	if len(s) > sigPreviewCap {
		return s[:sigPreviewCap] + "..."
	}

	return s
}

// SummarizeBody keeps the first and last maxLines lines of a body, marking omitted lines.
func SummarizeBody(body string, maxLines int) string {
	lines := strings.Split(body, "\n")
	if len(lines) <= maxLines*summarySides+1 {
		return body
	}

	omitted := len(lines) - maxLines*summarySides

	var buf strings.Builder
	for i := range maxLines {
		buf.WriteString(lines[i])
		buf.WriteByte('\n')
	}

	fmt.Fprintf(&buf, "// ... (%d lines omitted)\n", omitted)

	for i := len(lines) - maxLines; i < len(lines); i++ {
		buf.WriteString(lines[i])

		if i < len(lines)-1 {
			buf.WriteByte('\n')
		}
	}

	return buf.String()
}

// scopedSearch searches inside a named symbol's body for pattern matches.
func scopedSearch(arg args, patRe *regexp.Regexp) (*server.ToolCallResult, error) {
	if arg.ContextLines <= 0 {
		arg.ContextLines = 2
	}

	if arg.ContextLines > maxContextCap {
		arg.ContextLines = maxContextCap
	}

	symbolRe, err := CompilePattern(`\b`+regexp.QuoteMeta(arg.Symbol)+`\b`, arg.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid symbol name: %w", err)
	}

	symbols := findSymbols(arg, symbolRe)

	if len(symbols) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{
				Type: schemaText,
				Text: fmt.Sprintf("Symbol %q not found.", arg.Symbol),
			}},
			IsError: false,
		}, nil
	}

	var buf strings.Builder

	totalMatches := 0

	for _, sym := range symbols {
		totalMatches += renderScopedSymbol(&buf, sym, arg, patRe)
	}

	if totalMatches == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{
				Type: schemaText,
				Text: fmt.Sprintf("No matches for /%s/ inside %q.", arg.Pattern, arg.Symbol),
			}},
			IsError: false,
		}, nil
	}

	output := buf.String()
	if arg.TokenBudget > 0 && len(output) > arg.TokenBudget {
		output = renderScopedTerse(arg, symbols, patRe)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: schemaText, Text: output}},
		IsError: false,
	}, nil
}

// findSymbols locates funcs and types whose name matches symbolRe.
func findSymbols(arg args, symbolRe *regexp.Regexp) []FuncMatch {
	var symbols []FuncMatch

	funcs, _ := Search(arg.Path, arg.Include, symbolRe, scopedSearchMax, IsFuncSig)
	for _, f := range funcs {
		if symbolRe.MatchString(f.Name) {
			symbols = append(symbols, f)
		}
	}

	types, _ := Search(arg.Path, arg.Include, symbolRe, scopedSearchMax, IsStructSig)
	for _, t := range types {
		if symbolRe.MatchString(t.Name) {
			symbols = append(symbols, t)
		}
	}

	return symbols
}

// window is a context window around a hit line inside a symbol body.
type window struct {
	start, end int
	matches    map[int]bool
}

// buildWindows merges overlapping context windows around hit lines.
func buildWindows(hitIdxs []int, ctx, lastIdx int) []window {
	var windows []window

	for _, idx := range hitIdxs {
		wStart := max(0, idx-ctx)
		wEnd := min(lastIdx, idx+ctx)

		if len(windows) > 0 && wStart <= windows[len(windows)-1].end+1 {
			last := &windows[len(windows)-1]
			last.end = max(last.end, wEnd)
			last.matches[idx] = true
		} else {
			windows = append(windows, window{start: wStart, end: wEnd, matches: map[int]bool{idx: true}})
		}
	}

	return windows
}

// renderScopedSymbol writes one symbol's matches and returns the number of hits.
func renderScopedSymbol(buf *strings.Builder, sym FuncMatch, arg args, patRe *regexp.Regexp) int {
	rel := server.RelPath(sym.File)
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(sym.File)), ".")
	ctx := arg.ContextLines
	bodyLines := strings.Split(sym.Body, "\n")

	var hitIdxs []int

	for i, line := range bodyLines {
		if patRe.MatchString(line) {
			hitIdxs = append(hitIdxs, i)
		}
	}

	if len(hitIdxs) == 0 {
		return 0
	}

	startLine := sym.Line
	if startLine == 0 {
		startLine = 1
	}

	fmt.Fprintf(buf, "%d matches for /%s/ in %q (%s:L%d-%d):\n\n",
		len(hitIdxs), arg.Pattern, sym.Name, rel, startLine, sym.EndLine)

	for _, w := range buildWindows(hitIdxs, ctx, len(bodyLines)-1) {
		fmt.Fprintf(buf, "```%s\n", ext)

		for i := w.start; i <= w.end; i++ {
			absLine := startLine + i
			if w.matches[i] {
				fmt.Fprintf(buf, "> L%d: %s\n", absLine, bodyLines[i])
			} else {
				fmt.Fprintf(buf, "  L%d: %s\n", absLine, bodyLines[i])
			}
		}

		buf.WriteString("```\n\n")
	}

	return len(hitIdxs)
}

// renderScopedTerse rebuilds output as file:line hits when the budget is exceeded.
func renderScopedTerse(arg args, symbols []FuncMatch, patRe *regexp.Regexp) string {
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

	output := terse.String()
	output = server.TruncateToBudget(output, arg.TokenBudget) +
		server.BudgetHint(arg.TokenBudget, "Reduce context_lines or scope for more.")

	return output
}
