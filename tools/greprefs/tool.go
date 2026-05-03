package greprefs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "grep_refs",
	Description: "Find all references to a named symbol — type usages, assignments, function arguments, and call sites. Unlike find_callers (call-syntax only), grep_refs finds every non-declaration usage: passing a func as a value, using a type in an annotation, referencing a constant. Filters declarations, imports, and comments automatically.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":           {Type: "string", Description: "Symbol name to find references to. Matched as a whole word (word-boundary)."},
			"path":           {Type: "string", Description: "MUST be absolute path to file or directory to search."},
			"include":        {Type: "string", Description: "Glob filter. Defaults to all source files."},
			"context_lines":  {Type: "integer", Description: "Lines before/after each reference. Default 2, max 8."},
			"case_sensitive": {Type: "boolean", Description: "Default false."},
			"max_results":    {Type: "integer", Description: "Max references. Default 20, max 50."},
			"offset":         {Type: "integer", Description: "Pagination offset (0-based)."},
			"compact":        {Type: "boolean", Description: "Terse output. Default false."},
			"names_only":     {Type: "boolean", Description: "Return only file:line — no context. Cheapest mode."},
			"scope":          {Type: "boolean", Description: "Annotate each reference with the enclosing function/method name."},
		},
		Required: []string{"name"},
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
	if a.MaxResults <= 0 {
		a.MaxResults = 20
	}
	a.MaxResults = min(a.MaxResults, 50)
	if a.ContextLines <= 0 {
		a.ContextLines = 2
	}
	a.ContextLines = min(a.ContextLines, 8)
	if a.Include == "" {
		a.Include = "*"
	}

	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}
	refRe, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.Name) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid name: %v", err)
	}
	declRe := regexp.MustCompile(flags + `(?:(?:func|type|var|const|let|class|def|struct|interface|enum)\s+|func\s+\([^)]+\)\s+)` + regexp.QuoteMeta(a.Name) + `\b`)
	importRe := regexp.MustCompile(`^\s*(?:import\s|from\s+\w|use\s+\w|extern\s+crate\s)`)

	var sites []refSite
	total := 0
	scannedAll := true
	fetchMax := a.Offset + a.MaxResults

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
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(a.Path, path)
		if !grepfunc.MatchGlob(a.Include, rel) {
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
		if a.Include == "*" && grepfunc.IsNonSourceExt(ext) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

		var fileLinesBytes [][]byte
		var fileBoundaries map[int]int
		if a.Scope {
			fileLinesBytes = make([][]byte, len(lines))
			for i, l := range lines {
				fileLinesBytes[i] = []byte(l)
			}
			combinedSig := func(line []byte) bool {
				return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line)
			}
			fileBoundaries = grepfunc.MapBlockBoundaries(fileLinesBytes, combinedSig)
		}

		ctx := a.ContextLines
		covered := -1

		for i, line := range lines {
			if !refRe.MatchString(line) {
				continue
			}
			if declRe.MatchString(line) {
				continue
			}
			if importRe.MatchString(line) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
				continue
			}

			windowStart := max(0, i-ctx)
			if windowStart <= covered {
				covered = min(len(lines)-1, i+ctx)
				continue
			}

			total++
			if total <= a.Offset {
				covered = min(len(lines)-1, i+ctx)
				continue
			}
			if len(sites) >= a.MaxResults {
				covered = min(len(lines)-1, i+ctx)
				continue
			}

			windowEnd := min(len(lines)-1, i+ctx)
			var siteScope string
			if a.Scope && fileBoundaries != nil {
				siteScope = grepfunc.EnclosingSymbol(fileLinesBytes, i, fileBoundaries)
			}
			sites = append(sites, refSite{
				file:    path,
				refLine: i + 1,
				start:   windowStart,
				lines:   lines[windowStart : windowEnd+1],
				scope:   siteScope,
			})
			covered = windowEnd
		}

		if len(sites) >= fetchMax {
			scannedAll = false
			return filepath.SkipAll
		}
		return nil
	})

	var buf strings.Builder

	if len(sites) == 0 {
		fmt.Fprintf(&buf, "No references found for %q.\n", a.Name)
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	shown := len(sites)
	if !a.NamesOnly {
		if a.Compact {
			if scannedAll {
				fmt.Fprintf(&buf, "%d refs %q\n", total, a.Name)
			} else {
				fmt.Fprintf(&buf, "%d+ refs %q (showing %d)\n", total, a.Name, shown)
			}
		} else {
			if scannedAll {
				fmt.Fprintf(&buf, "%d reference(s) to %q\n\n", total, a.Name)
			} else {
				fmt.Fprintf(&buf, "%d+ reference(s) to %q (showing %d)\n\n", total, a.Name, shown)
			}
		}
	}

	for _, s := range sites {
		rel := server.RelPath(s.file)
		if a.NamesOnly {
			if s.scope != "" {
				fmt.Fprintf(&buf, "%s:%d [%s]\n", rel, s.refLine, s.scope)
			} else {
				fmt.Fprintf(&buf, "%s:%d\n", rel, s.refLine)
			}
			continue
		}
		ext := strings.TrimPrefix(filepath.Ext(s.file), ".")
		if ext == "" {
			ext = "txt"
		}
		header := fmt.Sprintf("%s:%d", rel, s.refLine)
		if s.scope != "" {
			header += " [in " + s.scope + "]"
		}
		if a.Compact {
			fmt.Fprintf(&buf, "%s\n", header)
		} else {
			fmt.Fprintf(&buf, "%s:\n", header)
		}
		fmt.Fprintf(&buf, "```%s\n", ext)
		for j, line := range s.lines {
			lineNum := s.start + j + 1
			if lineNum == s.refLine {
				fmt.Fprintf(&buf, "> %d: %s\n", lineNum, line)
			} else {
				fmt.Fprintf(&buf, "  %d: %s\n", lineNum, line)
			}
		}
		fmt.Fprintf(&buf, "```")
		if a.Compact {
			buf.WriteByte('\n')
		} else {
			buf.WriteString("\n\n")
		}
	}

	if !a.NamesOnly && (!scannedAll || a.Offset+shown < total) {
		fmt.Fprintf(&buf, "More results available. Use offset=%d.\n", a.Offset+shown)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}
