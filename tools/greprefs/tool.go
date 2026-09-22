// Package greprefs implements the grep_refs MCP tool.
package greprefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const (
	typeString  = "string"
	typeInteger = "integer"
	typeBoolean = "boolean"

	defaultMaxResults   = 20
	maxMaxResults       = 50
	defaultContextLines = 2
	maxContextLines     = 8
	maxFileSize         = 2 * 1024 * 1024
)

var errNameRequired = errors.New("name is required")

// Tool is the grep_refs MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_refs",
	Description: "Every reference to a symbol: call sites, type usages, assignments. calls_only=true → invocation lines only. Skips .gitignore'd dirs.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":           {Type: typeString, Description: "Symbol name (word-boundary match).", Items: nil},
			"path":           {Type: typeString, Description: "Directory to search. Defaults to project root.", Items: nil},
			"include":        {Type: typeString, Description: "Glob filter. Default: all source files.", Items: nil},
			"context_lines":  {Type: typeInteger, Description: "Lines before/after each reference. Default 2, max 8.", Items: nil},
			"case_sensitive": {Type: typeBoolean, Description: "Default false.", Items: nil},
			"max_results":    {Type: typeInteger, Description: "Max references. Default 20, max 50.", Items: nil},
			"offset":         {Type: typeInteger, Description: "Pagination offset (0-based).", Items: nil},
			"compact":        {Type: typeBoolean, Description: "Terse output.", Items: nil},
			"names_only":     {Type: typeBoolean, Description: "Only file:line — cheapest.", Items: nil},
			"scope":          {Type: typeBoolean, Description: "Annotate with enclosing function/method name.", Items: nil},
			"calls_only":     {Type: typeBoolean, Description: "Only invocation lines: Name( or x.Name().", Items: nil},
			"receiver":       {Type: typeString, Description: "With calls_only: only calls on this receiver/variable.", Items: nil},
			"token_budget":   {Type: typeInteger, Description: "Max output chars; truncates at line boundaries.", Items: nil},
		},
		Required:             []string{"name"},
		AdditionalProperties: false,
	},
}

type refSite struct {
	file    string
	refLine int
	start   int
	lines   []string
	scope   string
}

type args struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	ContextLines  int    `json:"context_lines"`
	CaseSensitive bool   `json:"case_sensitive"`
	MaxResults    int    `json:"max_results"`
	Offset        int    `json:"offset"`
	Compact       bool   `json:"compact"`
	NamesOnly     bool   `json:"names_only"`
	Scope         bool   `json:"scope"`
	CallsOnly     bool   `json:"calls_only"`
	Receiver      string `json:"receiver"`
	TokenBudget   int    `json:"token_budget"`
}

// Handle processes a grep_refs tool call and returns the result.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	arg, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	if err := server.CheckBounds(arg.Path); err != nil {
		return nil, err
	}

	refRe, err := compileRefPattern(arg)
	if err != nil {
		return nil, err
	}

	walker := newRefWalker(arg, refRe)
	_ = grepfunc.WalkDir(arg.Path, walker.step)

	output := renderRefs(arg, walker.sites, walker.total, walker.scannedAll)

	result := &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
		IsError: false,
	}

	return server.BudgetResult(result, arg.TokenBudget), nil
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
	if arg.MaxResults <= 0 {
		arg.MaxResults = defaultMaxResults
	}

	arg.MaxResults = min(arg.MaxResults, maxMaxResults)
	if arg.ContextLines <= 0 {
		arg.ContextLines = defaultContextLines
	}

	arg.ContextLines = min(arg.ContextLines, maxContextLines)
	if arg.Include == "" {
		arg.Include = "*"
	}

	return arg, nil
}

func compileRefPattern(arg args) (*regexp.Regexp, error) {
	flags := "(?i)"
	if arg.CaseSensitive {
		flags = ""
	}

	var (
		refRe *regexp.Regexp
		err   error
	)

	if arg.CallsOnly {
		if arg.Receiver != "" {
			refRe, err = regexp.Compile(flags + regexp.QuoteMeta(arg.Receiver) +
				`\s*\.\s*` + regexp.QuoteMeta(arg.Name) + `\s*[.(]`)
		} else {
			refRe, err = regexp.Compile(flags + `\b` + regexp.QuoteMeta(arg.Name) + `\s*[.(]`)
		}
	} else {
		refRe, err = regexp.Compile(flags + `\b` + regexp.QuoteMeta(arg.Name) + `\b`)
	}

	if err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}

	return refRe, nil
}

type refWalker struct {
	arg        args
	matcher    *grepfunc.GlobMatcher
	refRe      *regexp.Regexp
	declRe     *regexp.Regexp
	importRe   *regexp.Regexp
	fetchMax   int
	sites      []refSite
	total      int
	scannedAll bool
}

func newRefWalker(arg args, refRe *regexp.Regexp) *refWalker {
	flags := "(?i)"
	if arg.CaseSensitive {
		flags = ""
	}

	return &refWalker{
		arg:     arg,
		matcher: grepfunc.CompileGlob(arg.Include),
		refRe:   refRe,
		declRe: regexp.MustCompile(flags +
			`(?:(?:func|type|var|const|let|class|def|struct|interface|enum)\s+|func\s+\([^)]+\)\s+)` +
			regexp.QuoteMeta(arg.Name) + `\b`),
		importRe:   regexp.MustCompile(`^\s*(?:import\s|from\s+\w|use\s+\w|extern\s+crate\s)`),
		fetchMax:   arg.Offset + arg.MaxResults,
		sites:      nil,
		total:      0,
		scannedAll: true,
	}
}

func (w *refWalker) step(path string, entry fs.DirEntry, err error) error {
	if err != nil {
		return err
	}

	if entry.IsDir() {
		return nil
	}

	if w.skipFile(path, entry) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if w.unsupportedExt(ext) {
		return nil
	}

	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	w.scanFile(path, data)

	if len(w.sites) >= w.fetchMax {
		w.scannedAll = false

		return filepath.SkipAll
	}

	return nil
}

func (w *refWalker) skipFile(path string, entry fs.DirEntry) bool {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return true
	}

	rel, _ := filepath.Rel(w.arg.Path, path)

	return !w.matcher.Match(rel)
}

func (w *refWalker) unsupportedExt(ext string) bool {
	return grepfunc.IsBinaryExt(ext) || (w.arg.Include == "*" && grepfunc.IsNonSourceExt(ext))
}

func (w *refWalker) scanFile(path string, data []byte) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	var (
		fileLinesBytes [][]byte
		fileBoundaries grepfunc.BlockBoundaries
	)

	if w.arg.Scope {
		fileLinesBytes = make([][]byte, len(lines))
		for idx, l := range lines {
			fileLinesBytes[idx] = []byte(l)
		}

		fileBoundaries = grepfunc.MapBlockBoundaries(fileLinesBytes, combinedSig)
	}

	ctx := w.arg.ContextLines
	covered := -1

	for idx, line := range lines {
		if !w.refRe.MatchString(line) {
			continue
		}

		if w.skipLine(line) {
			continue
		}

		windowStart := max(0, idx-ctx)
		if windowStart <= covered {
			covered = min(len(lines)-1, idx+ctx)

			continue
		}

		w.total++
		if w.limitReached() {
			covered = min(len(lines)-1, idx+ctx)

			continue
		}

		windowEnd := min(len(lines)-1, idx+ctx)

		var siteScope string
		if w.arg.Scope {
			siteScope = grepfunc.EnclosingSymbol(fileLinesBytes, idx, fileBoundaries)
		}

		w.sites = append(w.sites, refSite{
			file:    path,
			refLine: idx + 1,
			start:   windowStart,
			lines:   lines[windowStart : windowEnd+1],
			scope:   siteScope,
		})
		covered = windowEnd
	}
}

func combinedSig(line []byte) bool {
	return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line)
}

func (w *refWalker) skipLine(line string) bool {
	if w.declRe.MatchString(line) || w.importRe.MatchString(line) {
		return true
	}

	trimmed := strings.TrimSpace(line)

	return strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
		strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#")
}

func (w *refWalker) limitReached() bool {
	return w.total <= w.arg.Offset || len(w.sites) >= w.arg.MaxResults
}

func renderRefs(arg args, sites []refSite, total int, scannedAll bool) string {
	var buf strings.Builder

	if len(sites) == 0 {
		if arg.CallsOnly {
			fmt.Fprintf(&buf, "No call sites found for %q.\n", arg.Name)
		} else {
			fmt.Fprintf(&buf, "No references found for %q.\n", arg.Name)
		}

		return buf.String()
	}

	if !arg.NamesOnly {
		writeRefsHeader(&buf, arg, total, len(sites), scannedAll)
	}

	for _, site := range sites {
		renderSite(&buf, arg, site)
	}

	if !arg.NamesOnly && (!scannedAll || arg.Offset+len(sites) < total) {
		fmt.Fprintf(&buf, "More results available. Use offset=%d.\n", arg.Offset+len(sites))
	}

	return buf.String()
}

func writeRefsHeader(buf *strings.Builder, arg args, total, shown int, scannedAll bool) {
	if arg.Compact {
		writeCompactRefsHeader(buf, arg, total, shown, scannedAll)
	} else {
		writeFullRefsHeader(buf, arg, total, shown, scannedAll)
	}
}

func writeCompactRefsHeader(buf *strings.Builder, arg args, total, shown int, scannedAll bool) {
	if scannedAll {
		fmt.Fprintf(buf, "%d refs %q\n", total, arg.Name)
	} else {
		fmt.Fprintf(buf, "%d+ refs %q (showing %d)\n", total, arg.Name, shown)
	}
}

func writeFullRefsHeader(buf *strings.Builder, arg args, total, shown int, scannedAll bool) {
	if scannedAll {
		fmt.Fprintf(buf, "%d refs for %q\n", total, arg.Name)
	} else {
		fmt.Fprintf(buf, "%d+ refs for %q (showing %d)\n\n", total, arg.Name, shown)
	}
}

func renderSite(buf *strings.Builder, arg args, site refSite) {
	rel := server.RelPath(site.file)

	if arg.NamesOnly {
		if site.scope != "" {
			fmt.Fprintf(buf, "%s:%d [%s]\n", rel, site.refLine, site.scope)
		} else {
			fmt.Fprintf(buf, "%s:%d\n", rel, site.refLine)
		}

		return
	}

	ext := strings.TrimPrefix(filepath.Ext(site.file), ".")
	if ext == "" {
		ext = "txt"
	}

	header := fmt.Sprintf("%s:%d", rel, site.refLine)
	if site.scope != "" {
		header += " [in " + site.scope + "]"
	}

	if arg.Compact {
		fmt.Fprintf(buf, "%s\n", header)
	} else {
		fmt.Fprintf(buf, "%s:\n", header)
	}

	fmt.Fprintf(buf, "```%s\n", ext)

	for idx, line := range site.lines {
		lineNum := site.start + idx + 1
		if lineNum == site.refLine {
			fmt.Fprintf(buf, "> %d: %s\n", lineNum, line)
		} else {
			fmt.Fprintf(buf, "  %d: %s\n", lineNum, line)
		}
	}

	fmt.Fprintf(buf, "```")

	if arg.Compact {
		buf.WriteByte('\n')
	} else {
		buf.WriteString("\n")
	}
}
