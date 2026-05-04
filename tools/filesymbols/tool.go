package filesymbols

import (
	"encoding/json"
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

var Tool = server.Tool{
	Name: "file_symbols",
	Description: "List all function and type definitions in a single file with line numbers — no bodies. " +
		"The fastest way to orient in an unfamiliar file: one call instead of grep_func + grep_struct. " +
		"Returns symbols sorted by line number. Use before editing to get a full map of what's in the file.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":           {Type: "string", Description: "MUST be absolute path to file or directory to inspect. Defaults to last file operated on in this session."},
			"filter":         {Type: "string", Description: "Which symbols to return: 'func' (functions/methods only), 'type' (structs/interfaces/enums only), or 'all' (default)."},
			"count_only":     {Type: "boolean", Description: "If true, return only the count of symbols — no names, no lines. Cheapest check: 'is this file worth inspecting?'"},
			"compact":        {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"pattern":        {Type: "string", Description: "Regex to filter symbols by name or signature. E.g. '^Handle' for exported handlers, 'Error' for error types. Case-insensitive by default."},
			"case_sensitive": {Type: "boolean", Description: "Make pattern filter case-sensitive. Default false."},
			"include":        {Type: "string", Description: "Glob to filter files. Only used when path is a directory. E.g. '**/*.go'. Defaults to all source files."},
			"group_by_file":  {Type: "boolean", Description: "When path is a directory, group symbols under file headers. Auto-enabled when path is a directory."},
		},
		Required: []string{},
	},
}

type sym struct {
	file    string
	line    int
	endLine int
	kind    string
	sig     string
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a struct {
		Path          string `json:"path"`
		Filter        string `json:"filter"`
		CountOnly     bool   `json:"count_only"`
		Compact       bool   `json:"compact"`
		Pattern       string `json:"pattern"`
		CaseSensitive bool   `json:"case_sensitive"`
		Include       string `json:"include"`
		GroupByFile   bool   `json:"group_by_file"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		a.Path = server.LastPath
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required (no previous path in session)")
	}
	a.Path = server.ResolvePath(a.Path)
	server.SetLastPath(a.Path)

	if a.Filter == "" {
		a.Filter = "all"
	}

	matchAll := regexp.MustCompile(`(?s).`)

	include := a.Include
	if include == "" {
		include = "*"
	}

	var syms []sym

	if a.Filter == "all" || a.Filter == "func" {
		funcs, _ := grepfunc.Search(a.Path, include, matchAll, 500, grepfunc.IsFuncSig)
		for _, f := range funcs {
			sig := util.FirstSigLine(f.Body)
			syms = append(syms, sym{file: f.File, line: f.Line, endLine: f.EndLine, kind: "func", sig: sig})
		}
	}

	if a.Filter == "all" || a.Filter == "type" {
		types, _ := grepfunc.Search(a.Path, include, matchAll, 500, grepfunc.IsStructSig)
		for _, t := range types {
			sig := util.FirstSigLine(t.Body)
			syms = append(syms, sym{file: t.File, line: t.Line, endLine: t.EndLine, kind: "type", sig: sig})
		}
	}

	// Deduplicate by (file, line), then remove nested symbols
	type fileLinePair struct {
		file string
		line int
	}
	seen := make(map[fileLinePair]bool)
	unique := syms[:0]
	for _, s := range syms {
		key := fileLinePair{s.file, s.line}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, s)
		}
	}
	syms = removeNested(unique)

	isDir := false
	if info, err := os.Stat(a.Path); err == nil && info.IsDir() {
		isDir = true
	}

	sort.Slice(syms, func(i, j int) bool { return syms[i].line < syms[j].line })

	if a.Pattern != "" {
		flags := "(?i)"
		if a.CaseSensitive {
			flags = ""
		}
		re, err := regexp.Compile(flags + a.Pattern)
		if err != nil {
			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("invalid pattern: %v", err)}},
			}, nil
		}
		filtered := syms[:0]
		for _, s := range syms {
			if re.MatchString(s.sig) {
				filtered = append(filtered, s)
			}
		}
		syms = filtered
	}

	rel := server.RelPath(a.Path)
	ext := strings.TrimPrefix(filepath.Ext(a.Path), ".")

	useGrouping := isDir || a.GroupByFile

	if a.CountOnly {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("%d symbols in %s", len(syms), rel)}},
		}, nil
	}

	compact := a.Compact
	var buf strings.Builder

	if useGrouping {
		byFile := map[string][]sym{}
		var fileOrder []string
		for _, s := range syms {
			fr := server.RelPath(s.file)
			if _, ok := byFile[fr]; !ok {
				fileOrder = append(fileOrder, fr)
			}
			byFile[fr] = append(byFile[fr], s)
		}
		totalFiles := len(fileOrder)
		if compact {
			fmt.Fprintf(&buf, "%d symbols across %d files in %s:\n", len(syms), totalFiles, rel)
		} else {
			fmt.Fprintf(&buf, "%d symbols across %d files in %s:\n\n", len(syms), totalFiles, rel)
		}
		for _, fr := range fileOrder {
			group := byFile[fr]
			fmt.Fprintf(&buf, "\n%s (%d)\n```\n", fr, len(group))
			for _, s := range group {
				fmt.Fprintf(&buf, "  L%-4d %-5s %s\n", s.line, s.kind, util.TrimKind(s.sig))
			}
			buf.WriteString("```\n")
		}
	} else {
		if compact {
			fmt.Fprintf(&buf, "%d symbols %s:\n", len(syms), rel)
		} else {
			fmt.Fprintf(&buf, "%d symbols in %s:\n\n", len(syms), rel)
		}
		buf.WriteString("```\n")
		for _, s := range syms {
			fmt.Fprintf(&buf, "L%-4d %-5s %s\n", s.line, s.kind, util.TrimKind(s.sig))
		}
		buf.WriteString("```\n")
		if len(syms) == 0 {
			fmt.Fprintf(&buf, "(no %s symbols found — check filter or file extension)\n", ext)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func removeNested(syms []sym) []sym {
	byFile := map[string][]sym{}
	var order []string
	for _, s := range syms {
		if _, ok := byFile[s.file]; !ok {
			order = append(order, s.file)
		}
		byFile[s.file] = append(byFile[s.file], s)
	}
	var out []sym
	for _, f := range order {
		group := byFile[f]
		for i, s := range group {
			nested := false
			for j, other := range group {
				if i != j && s.line > other.line && s.line <= other.endLine {
					nested = true
					break
				}
			}
			if !nested {
				out = append(out, s)
			}
		}
	}
	return out
}
