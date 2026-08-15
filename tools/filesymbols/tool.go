// Package filesymbols implements the file_symbols MCP tool.
package filesymbols

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
	"github.com/hedtahr/grepfunc/tools/internal/util"
)

const (
	filterAll   = "all"
	maxSymbols  = 500
	typeString  = "string"
	typeInteger = "integer"
	typeBoolean = "boolean"
	typeText    = "text"
)

var errPathRequired = errors.New("path is required (no previous path in session)")

// Tool is the file_symbols MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "file_symbols",
	Description: "List all function and type definitions in a file with line numbers — no bodies. Fastest way to map an unfamiliar file; use before editing.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":           {Type: typeString, Description: "File or directory to inspect. Defaults to last file operated on, then project root.", Items: nil},
			"filter":         {Type: typeString, Description: "Which symbols: 'func', 'type', or 'all' (default).", Items: nil},
			"count_only":     {Type: typeBoolean, Description: "If true, return only the count — cheapest check: 'worth inspecting?'.", Items: nil},
			"compact":        {Type: typeBoolean, Description: "Terse output, keeps syntax highlighting.", Items: nil},
			"pattern":        {Type: typeString, Description: "Regex to filter symbols by name or signature. Case-insensitive by default.", Items: nil},
			"case_sensitive": {Type: typeBoolean, Description: "Case-sensitive pattern filter. Default false.", Items: nil},
			"include":        {Type: typeString, Description: "Glob to filter files (directory path only). E.g. '**/*.go'.", Items: nil},
			"group_by_file":  {Type: typeBoolean, Description: "Group symbols under file headers (auto when path is a directory).", Items: nil},
			"token_budget":   {Type: typeInteger, Description: "Max output chars. Overflow → line-boundary truncation.", Items: nil},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

type args struct {
	Path          string `json:"path"`
	Filter        string `json:"filter"`
	CountOnly     bool   `json:"count_only"`
	Compact       bool   `json:"compact"`
	Pattern       string `json:"pattern"`
	CaseSensitive bool   `json:"case_sensitive"`
	Include       string `json:"include"`
	GroupByFile   bool   `json:"group_by_file"`
	TokenBudget   int    `json:"token_budget"`
}

type sym struct {
	file    string
	line    int
	endLine int
	kind    string
	sig     string
}

// Handle processes a file_symbols tool call and returns the result.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	arg, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	syms := collectSyms(arg)

	if arg.Pattern != "" {
		filtered, invalid := filterSyms(syms, arg.Pattern, arg.CaseSensitive)
		if invalid != nil {
			return invalid, nil
		}

		syms = filtered
	}

	info, err := os.Stat(arg.Path)
	isDir := err == nil && info.IsDir()

	rel := server.RelPath(arg.Path)
	ext := strings.TrimPrefix(filepath.Ext(arg.Path), ".")

	useGrouping := isDir || arg.GroupByFile

	if arg.CountOnly {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: typeText, Text: fmt.Sprintf("%d symbols in %s", len(syms), rel)}},
			IsError: false,
		}, nil
	}

	var buf strings.Builder

	if useGrouping {
		renderGrouped(&buf, arg, syms, rel)
	} else {
		renderFlat(&buf, arg, syms, rel, ext)
	}

	result := &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: buf.String()}},
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

	if arg.Path == "" {
		arg.Path = server.LastPath
	}

	if arg.Path == "" {
		return arg, errPathRequired
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

	server.SetLastPath(arg.Path)

	if arg.Filter == "" {
		arg.Filter = filterAll
	}

	if arg.Include == "" {
		arg.Include = "*"
	}

	return arg, nil
}

func collectSyms(arg args) []sym {
	matchAll := regexp.MustCompile(`(?s).`)

	var syms []sym

	if arg.Filter == filterAll || arg.Filter == "func" {
		funcs, _ := grepfunc.Search(arg.Path, arg.Include, matchAll, maxSymbols, grepfunc.IsFuncSig)
		syms = appendFuncSyms(syms, funcs)
	}

	if arg.Filter == filterAll || arg.Filter == "type" {
		types, _ := grepfunc.Search(arg.Path, arg.Include, matchAll, maxSymbols, grepfunc.IsStructSig)
		syms = appendTypeSyms(syms, types)
	}

	syms = dedupeSyms(syms)
	syms = removeNested(syms)

	sort.Slice(syms, func(left, right int) bool { return syms[left].line < syms[right].line })

	return syms
}

func appendFuncSyms(dst []sym, funcs []grepfunc.FuncMatch) []sym {
	for _, match := range funcs {
		sig := util.FirstSigLine(match.Body)
		dst = append(dst, sym{file: match.File, line: match.Line, endLine: match.EndLine, kind: "func", sig: sig})
	}

	return dst
}

func appendTypeSyms(dst []sym, types []grepfunc.FuncMatch) []sym {
	for _, match := range types {
		sig := util.FirstSigLine(match.Body)
		dst = append(dst, sym{file: match.File, line: match.Line, endLine: match.EndLine, kind: "type", sig: sig})
	}

	return dst
}

func dedupeSyms(syms []sym) []sym {
	// Deduplicate by (file, line).
	type fileLinePair struct {
		file string
		line int
	}

	seen := make(map[fileLinePair]bool)

	unique := syms[:0]

	for _, item := range syms {
		key := fileLinePair{item.file, item.line}
		if !seen[key] {
			seen[key] = true

			unique = append(unique, item)
		}
	}

	return unique
}

func filterSyms(syms []sym, pattern string, caseSensitive bool) ([]sym, *server.ToolCallResult) {
	flags := "(?i)"
	if caseSensitive {
		flags = ""
	}

	patternRe, err := regexp.Compile(flags + pattern)
	if err != nil {
		return nil, &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: typeText, Text: fmt.Sprintf("invalid pattern: %v", err)}},
			IsError: false,
		}
	}

	filtered := syms[:0]

	for _, item := range syms {
		if patternRe.MatchString(item.sig) {
			filtered = append(filtered, item)
		}
	}

	return filtered, nil
}

func renderGrouped(buf *strings.Builder, arg args, syms []sym, rel string) {
	byFile := map[string][]sym{}

	var fileOrder []string

	for _, item := range syms {
		fr := server.RelPath(item.file)
		if _, ok := byFile[fr]; !ok {
			fileOrder = append(fileOrder, fr)
		}

		byFile[fr] = append(byFile[fr], item)
	}

	if arg.Compact {
		fmt.Fprintf(buf, "%d symbols across %d files in %s:\n", len(syms), len(fileOrder), rel)
	} else {
		fmt.Fprintf(buf, "%d symbols across %d files in %s:\n\n", len(syms), len(fileOrder), rel)
	}

	for _, fr := range fileOrder {
		group := byFile[fr]
		fmt.Fprintf(buf, "\n%s (%d)\n```\n", fr, len(group))

		for _, item := range group {
			fmt.Fprintf(buf, "  L%-4d %-5s %s\n", item.line, item.kind, util.TrimKind(item.sig))
		}

		buf.WriteString("```\n")
	}
}

func renderFlat(buf *strings.Builder, arg args, syms []sym, rel, ext string) {
	if arg.Compact {
		fmt.Fprintf(buf, "%d symbols %s:\n", len(syms), rel)
	} else {
		fmt.Fprintf(buf, "%d symbols in %s:\n\n", len(syms), rel)
	}

	buf.WriteString("```\n")

	for _, item := range syms {
		fmt.Fprintf(buf, "L%-4d %-5s %s\n", item.line, item.kind, util.TrimKind(item.sig))
	}

	buf.WriteString("```\n")

	if len(syms) == 0 {
		fmt.Fprintf(buf, "(no %s symbols found — check filter or file extension)\n", ext)
	}
}

func removeNested(syms []sym) []sym {
	byFile := map[string][]sym{}

	var order []string

	for _, item := range syms {
		if _, ok := byFile[item.file]; !ok {
			order = append(order, item.file)
		}

		byFile[item.file] = append(byFile[item.file], item)
	}

	var out []sym

	for _, file := range order {
		group := byFile[file]
		for i, item := range group {
			nested := false

			for j, other := range group {
				if i != j && item.line > other.line && item.line <= other.endLine {
					nested = true

					break
				}
			}

			if !nested {
				out = append(out, item)
			}
		}
	}

	return out
}
