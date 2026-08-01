package grepdead

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "grep_dead",
	Description: "Use when you need to identify dead code — declared symbols with zero references outside their declaring file. Two-pass analysis (find declarations, then verify references) that native grep can't do in a single query. Can be slow on large codebases; cap with max_symbols.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":        {Type: "string", Description: "Root directory to search. Optional — defaults to the opened project root."},
			"include":     {Type: "string", Description: "Glob to filter files for both declaration and reference search. E.g. '**/*.go'."},
			"kind":        {Type: "string", Description: "Symbol kind to check: 'func', 'type', or 'all' (default)."},
			"min_lines":   {Type: "integer", Description: "Skip symbols with body shorter than this many lines. Default 2 (skips trivial one-liners)."},
			"max_symbols": {Type: "integer", Description: "Max symbols to check (cap to avoid timeouts). Default 200."},
			"compact":     {Type: "boolean", Description: "Terse output. Default false."},
		},
		Required: []string{},
	},
}

type args struct {
	Path       string `json:"path"`
	Include    string `json:"include"`
	Kind       string `json:"kind"`
	MinLines   int    `json:"min_lines"`
	MaxSymbols int    `json:"max_symbols"`
	Compact    bool   `json:"compact"`
}

type deadSym struct {
	file string
	line int
	name string
	kind string
}

type symWithKind struct {
	m    grepfunc.FuncMatch
	kind string
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	a.Path = server.ResolvePath(a.Path)
	if a.Kind == "" {
		a.Kind = "all"
	}
	if a.MinLines <= 0 {
		a.MinLines = 2
	}
	if a.MaxSymbols <= 0 {
		a.MaxSymbols = 200
	}
	include := a.Include
	if include == "" {
		include = "*"
	}

	matchAll := regexp.MustCompile(`(?s).`)
	var rawCandidates []symWithKind

	if a.Kind == "all" || a.Kind == "func" {
		funcs, _ := grepfunc.Search(a.Path, include, matchAll, a.MaxSymbols*2, grepfunc.IsFuncSig)
		for _, f := range funcs {
			rawCandidates = append(rawCandidates, symWithKind{m: f, kind: "func"})
		}
	}
	if a.Kind == "all" || a.Kind == "type" {
		types, _ := grepfunc.Search(a.Path, include, matchAll, a.MaxSymbols*2, grepfunc.IsStructSig)
		for _, t := range types {
			rawCandidates = append(rawCandidates, symWithKind{m: t, kind: "type"})
		}
	}

	// Dedup by file+line
	seen := map[string]bool{}
	unique := rawCandidates[:0]
	for _, c := range rawCandidates {
		k := fmt.Sprintf("%s:%d", c.m.File, c.m.Line)
		if !seen[k] {
			seen[k] = true
			unique = append(unique, c)
		}
	}
	rawCandidates = unique

	// Filter by min_lines
	filtered := rawCandidates[:0]
	for _, c := range rawCandidates {
		if c.m.Lines >= a.MinLines {
			filtered = append(filtered, c)
		}
	}
	rawCandidates = filtered

	if len(rawCandidates) > a.MaxSymbols {
		rawCandidates = rawCandidates[:a.MaxSymbols]
	}

	type declInfo struct {
		file string
		line int
		kind string
	}
	nameToDecl := make(map[string]string, len(rawCandidates))
	nameToInfo := make(map[string]declInfo, len(rawCandidates))
	refCount := make(map[string]int, len(rawCandidates))

	for _, c := range rawCandidates {
		if c.m.Name == "" || c.m.Name == "<fn>" {
			continue
		}
		nameToDecl[c.m.Name] = c.m.File
		nameToInfo[c.m.Name] = declInfo{c.m.File, c.m.Line, c.kind}
		refCount[c.m.Name] = 0
	}

	var dead []deadSym
	checked := len(nameToDecl)

	if checked > 0 {
		declRe := regexp.MustCompile(`(?i)(?:(?:func|type|var|const|let|class|def|struct|interface|enum)\s+|func\s+\([^)]+\)\s+)\w+\b`)

		// Chunk names into batches of 30 to avoid giant regex
		names := make([]string, 0, len(nameToDecl))
		for name := range nameToDecl {
			names = append(names, name)
		}
		sort.Strings(names)
		chunkSize := 30
		for i := 0; i < len(names); i += chunkSize {
			end := min(i+chunkSize, len(names))
			chunk := names[i:end]
			parts := make([]string, len(chunk))
			for j, name := range chunk {
				parts[j] = regexp.QuoteMeta(name)
			}
			chunkRe := regexp.MustCompile(`(?i)\b(` + strings.Join(parts, "|") + `)\b`)

			_ = filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					base := d.Name()
					if base == ".git" || base == "node_modules" || base == "vendor" ||
						base == ".idea" || base == "__pycache__" || strings.HasPrefix(base, ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() || server.IsBannedPath(path) {
					return nil
				}
				rel, _ := filepath.Rel(a.Path, path)
				if !grepfunc.MatchGlob(include, rel) {
					return nil
				}
				info, err := d.Info()
				if err != nil || info.Size() > 2*1024*1024 {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(path))
				if grepfunc.IsBinaryExt(ext) {
					return nil
				}
				if include == "*" && grepfunc.IsNonSourceExt(ext) {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return nil
				}
				for line := range strings.SplitSeq(string(data), "\n") {
					if !chunkRe.MatchString(line) {
						continue
					}
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") ||
						strings.HasPrefix(trimmed, "*") {
						continue
					}
					if declRe.MatchString(line) {
						continue
					}
					for _, m := range chunkRe.FindAllStringSubmatch(line, -1) {
						name := m[1]
						if declFile, ok := nameToDecl[name]; ok && path != declFile {
							refCount[name]++
						}
					}
				}
				return nil
			})
		}

		for name, count := range refCount {
			if count == 0 {
				info := nameToInfo[name]
				dead = append(dead, deadSym{file: info.file, line: info.line, name: name, kind: info.kind})
			}
		}
		sort.Slice(dead, func(i, j int) bool {
			if dead[i].file != dead[j].file {
				return dead[i].file < dead[j].file
			}
			return dead[i].line < dead[j].line
		})
	}

	if len(dead) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No dead symbols found among %d checked.\n", checked)}},
		}, nil
	}

	var buf strings.Builder
	if a.Compact {
		fmt.Fprintf(&buf, "%d potentially dead symbols (of %d checked):\n", len(dead), checked)
	} else {
		fmt.Fprintf(&buf, "%d potentially dead symbols (of %d checked):\n\n", len(dead), checked)
	}
	buf.WriteString("```\n")
	for _, d := range dead {
		rel := server.RelPath(d.file)
		kind := d.kind
		if kind == "" {
			kind = "?"
		}
		fmt.Fprintf(&buf, "%s:%d [%s] %s\n", rel, d.line, kind, d.name)
	}
	buf.WriteString("```\n")

	if len(rawCandidates) == a.MaxSymbols {
		fmt.Fprintf(&buf, "\n(capped at %d symbols — increase max_symbols for full scan)\n", a.MaxSymbols)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}
