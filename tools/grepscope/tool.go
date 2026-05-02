package grepscope

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"mcp_patch_file/server"
	"mcp_patch_file/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "grep_scope",
	Description: "Search for a pattern inside a specific symbol's body (function or type). Returns only matching lines with context — ~5x cheaper than grep_func with body=true for targeted searches.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":        {Type: "string", Description: "Regex to search for within the symbol body."},
			"symbol":         {Type: "string", Description: "Function or type name to scope the search to."},
			"path":           {Type: "string", Description: "File or directory to search. Defaults to project root."},
			"include":        {Type: "string", Description: "Glob filter e.g. '**/*.go'. Defaults to all source files."},
			"case_sensitive": {Type: "boolean", Description: "Default false."},
			"context_lines":  {Type: "integer", Description: "Lines of context around each match. Default 2, max 8."},
			"kind":           {Type: "string", Description: "Symbol kind: 'func', 'type', or 'any' (default)."},
		},
		Required: []string{"pattern", "symbol"},
	},
}

type args struct {
	Pattern       string `json:"pattern"`
	Symbol        string `json:"symbol"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	CaseSensitive bool   `json:"case_sensitive"`
	ContextLines  int    `json:"context_lines"`
	Kind          string `json:"kind"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	if a.Symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if a.Include == "" {
		a.Include = "*"
	}
	if a.Kind == "" {
		a.Kind = "any"
	}
	if a.ContextLines <= 0 {
		a.ContextLines = 2
	}
	if a.ContextLines > 8 {
		a.ContextLines = 8
	}

	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}

	patRe, err := regexp.Compile(flags + a.Pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %v", err)
	}

	symbolRe, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.Symbol) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid symbol name: %v", err)
	}

	var symbols []grepfunc.FuncMatch
	if a.Kind == "any" || a.Kind == "func" {
		funcs, _ := grepfunc.Search(a.Path, a.Include, symbolRe, 20, grepfunc.IsFuncSig)
		for _, f := range funcs {
			if symbolRe.MatchString(f.Name) {
				symbols = append(symbols, f)
			}
		}
	}
	if a.Kind == "any" || a.Kind == "type" {
		types, _ := grepfunc.Search(a.Path, a.Include, symbolRe, 20, grepfunc.IsStructSig)
		for _, t := range types {
			if symbolRe.MatchString(t.Name) {
				symbols = append(symbols, t)
			}
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
		lines := strings.Split(sym.Body, "\n")
		ctx := a.ContextLines

		// Collect hit indices
		var hitIdxs []int
		for i, line := range lines {
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

		fmt.Fprintf(&buf, "%d match(es) for /%s/ in %q (%s:L%d-%d):\n\n",
			len(hitIdxs), a.Pattern, sym.Name, rel, startLine, sym.EndLine)

		// Build merged windows
		type window struct {
			start, end int
			matches    map[int]bool
		}
		var windows []window
		for _, idx := range hitIdxs {
			ws := max(0, idx-ctx)
			we := min(len(lines)-1, idx+ctx)
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
					fmt.Fprintf(&buf, "> L%d: %s\n", absLine, lines[i])
				} else {
					fmt.Fprintf(&buf, "  L%d: %s\n", absLine, lines[i])
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

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}
