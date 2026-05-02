package findcallers

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mcp_patch_file/server"
	"mcp_patch_file/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "find_callers",
	Description: "Find all call sites of a named function or method. Returns only invocation lines (not declarations, not comments) with surrounding context. Faster than grep+manual filtering — understands declaration vs. call-site syntax.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":           {Type: "string", Description: "Function/method name to find callers of."},
			"path":           {Type: "string", Description: "File or directory to search. Defaults to project root."},
			"include":        {Type: "string", Description: "Glob filter. Defaults to all source files."},
			"context_lines":  {Type: "integer", Description: "Lines before/after each call site. Default 2, max 8."},
			"case_sensitive": {Type: "boolean", Description: "Default false."},
			"max_results":    {Type: "integer", Description: "Max call sites. Default 15, max 50."},
			"offset":         {Type: "integer", Description: "Pagination offset (0-based)."},
			"compact":        {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"receiver":       {Type: "string", Description: "Filter call sites to method calls on this receiver/variable name. E.g. 'receiver: \"s\"' finds 's.MethodName('. Useful to narrow results for common method names."},
			"names_only":     {Type: "boolean", Description: "If true, return only file:line — no context, no code blocks. Cheapest mode (~20x fewer tokens). For quick scan of where a function is called."},
			"scope":          {Type: "boolean", Description: "Annotate each call site with the enclosing function/method name. Default false."},
			"token_budget":  {Type: "integer", Description: "Max output chars. If exceeded, auto-switches to names_only. No default (unlimited)."},
		},
		Required: []string{"name"},
	},
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
	Receiver      string `json:"receiver"`
	NamesOnly     bool   `json:"names_only"`
	Scope         bool   `json:"scope"`
	TokenBudget   int    `json:"token_budget"`
}

type callSite struct {
	file    string
	callIdx int // 0-based index into lines
	lines   []string
	scope   string
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
		a.MaxResults = 15
	}
	if a.MaxResults > 50 {
		a.MaxResults = 50
	}
	if a.ContextLines <= 0 {
		a.ContextLines = 2
	}
	if a.ContextLines > 8 {
		a.ContextLines = 8
	}
	if a.Include == "" {
		a.Include = "*"
	}

	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}
	callRe, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.Name) + `\s*[.(]`)
	if err != nil {
		return nil, fmt.Errorf("invalid name: %v", err)
	}
	if a.Receiver != "" {
		callRe, err = regexp.Compile(flags + regexp.QuoteMeta(a.Receiver) + `\s*\.\s*` + regexp.QuoteMeta(a.Name) + `\s*[.(]`)
		if err != nil {
			return nil, fmt.Errorf("invalid receiver: %v", err)
		}
	}
	declRe := regexp.MustCompile(flags + `(?:(?:func|type|class|def|interface|struct)\s+|func\s+\([^)]+\)\s+)` + regexp.QuoteMeta(a.Name) + `\b`)

	var sites []callSite
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
		ctx := a.ContextLines
		covered := -1 // last line index covered by a previous window

		var fileLinesBytes [][]byte
		var fileBoundaries map[int]int
		if a.Scope {
			fileLinesBytes = make([][]byte, len(lines))
			for idx, l := range lines {
				fileLinesBytes[idx] = []byte(l)
			}
			combinedSig := func(line []byte) bool {
				return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line)
			}
			fileBoundaries = grepfunc.MapBlockBoundaries(fileLinesBytes, combinedSig)
		}

		for i, line := range lines {
			if !callRe.MatchString(line) {
				continue
			}
			if declRe.MatchString(line) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
				continue
			}

			windowStart := max(0, i-ctx)
			if windowStart <= covered {
				// extend coverage but don't emit a new window
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
			sites = append(sites, callSite{
				file:    path,
				callIdx: i,
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

	compact := a.Compact
	var buf strings.Builder

	if len(sites) == 0 {
		fmt.Fprintf(&buf, "No call sites found for %q. (declarations excluded)\n", a.Name)
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		}, nil
	}

	buildOutput := func(namesOnly bool) string {
		var out strings.Builder
		shown := len(sites)

		if !namesOnly {
			if compact {
				if scannedAll {
					fmt.Fprintf(&out, "%d callers %q\n", total, a.Name)
				} else {
					fmt.Fprintf(&out, "%d+ callers %q (showing %d)\n", total, a.Name, shown)
				}
			} else {
				if scannedAll {
					fmt.Fprintf(&out, "%d call site(s) for %q\n\n", total, a.Name)
				} else {
					fmt.Fprintf(&out, "%d+ call site(s) for %q (showing %d)\n\n", total, a.Name, shown)
				}
			}
		}

		for _, s := range sites {
			rel := server.RelPath(s.file)
			callLineNum := s.callIdx + 1

			if namesOnly {
				if s.scope != "" {
					fmt.Fprintf(&out, "%s:%d [%s]\n", rel, callLineNum, s.scope)
				} else {
					fmt.Fprintf(&out, "%s:%d\n", rel, callLineNum)
				}
				continue
			}

			ext := strings.TrimPrefix(filepath.Ext(s.file), ".")
			if ext == "" {
				ext = "txt"
			}

			windowStart := s.callIdx - a.ContextLines
			if windowStart < 0 {
				windowStart = 0
			}

			header := fmt.Sprintf("%s:%d", rel, callLineNum)
			if s.scope != "" {
				header += " [in " + s.scope + "]"
			}
			if compact {
				fmt.Fprintf(&out, "%s\n", header)
			} else {
				fmt.Fprintf(&out, "%s:\n", header)
			}
			fmt.Fprintf(&out, "```%s\n", ext)
			for j, line := range s.lines {
				lineNum := windowStart + j + 1
				if lineNum == callLineNum {
					fmt.Fprintf(&out, "> %d: %s\n", lineNum, line)
				} else {
					fmt.Fprintf(&out, "  %d: %s\n", lineNum, line)
				}
			}
			fmt.Fprintf(&out, "```")
			if compact {
				out.WriteByte('\n')
			} else {
				out.WriteString("\n\n")
			}
		}

		if !namesOnly && (!scannedAll || a.Offset+shown < total) {
			fmt.Fprintf(&out, "More results available. Use offset=%d.\n", a.Offset+shown)
		}
		return out.String()
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
