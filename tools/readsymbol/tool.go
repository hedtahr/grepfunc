package readsymbol

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "read_symbol",
	Description: "Read a specific function, method, struct, class, or interface body by name from a file. Combines find_symbol + file_head range read into a single efficient call. Returns the complete body with line numbers. Use when you know the symbol name and the file it's in — avoids the two-step find+read pattern that wastes tokens.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":           {Type: "string", Description: "MUST be absolute path to the file to read the symbol from. Defaults to last file operated on in this session."},
			"name":           {Type: "string", Description: "Symbol name to find and read. Matches function/method/type names. Case-insensitive."},
			"kind":           {Type: "string", Description: "Filter by kind: 'func' (functions/methods only), 'type' (structs/interfaces/enums only), or 'any' (default)."},
			"case_sensitive": {Type: "boolean", Description: "Case-sensitive matching. Default: false."},
			"summary":        {Type: "boolean", Description: "If true, truncate large bodies to first+last N lines with omission count. Default false."},
			"summary_lines":  {Type: "integer", Description: "Lines to show at start and end when summary=true. Default 5."},
		},
		Required: []string{"name"},
	},
}

type args struct {
	Path          string `json:"path"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	CaseSensitive bool   `json:"case_sensitive"`
	Summary       bool   `json:"summary"`
	SummaryLines  int    `json:"summary_lines"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		a.Path = server.LastPath
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required (no previous path in session)")
	}
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}
	server.SetLastPath(a.Path)

	if a.Kind == "" {
		a.Kind = "any"
	}

	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}
	pattern, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.Name) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid name: %v", err)
	}

	rel := server.RelPath(a.Path)
	ext := strings.TrimPrefix(filepath.Ext(a.Path), ".")
	if ext == "" {
		ext = "go"
	}

	var matches []grepfunc.FuncMatch

	// Search funcs in this file only
	if a.Kind == "any" || a.Kind == "func" {
		funcs, _ := grepfunc.Search(a.Path, "*", pattern, 50, grepfunc.IsFuncSig)
		for _, f := range funcs {
			if pattern.MatchString(f.Name) {
				f.Kind = "func"
				matches = append(matches, f)
			}
		}
	}

	// Search types in this file only
	if a.Kind == "any" || a.Kind == "type" {
		types, _ := grepfunc.Search(a.Path, "*", pattern, 50, grepfunc.IsStructSig)
		for _, t := range types {
			if pattern.MatchString(t.Name) {
				t.Kind = "type"
				matches = append(matches, t)
			}
		}
	}

	if len(matches) == 0 {
		// Try substring fallback
		subPattern, _ := regexp.Compile(flags + regexp.QuoteMeta(a.Name))
		if a.Kind == "any" || a.Kind == "func" {
			funcs, _ := grepfunc.Search(a.Path, "*", subPattern, 50, grepfunc.IsFuncSig)
			for _, f := range funcs {
				if subPattern.MatchString(f.Name) {
					f.Kind = "func"
					matches = append(matches, f)
				}
			}
		}
		if len(matches) == 0 && (a.Kind == "any" || a.Kind == "type") {
			types, _ := grepfunc.Search(a.Path, "*", subPattern, 50, grepfunc.IsStructSig)
			for _, t := range types {
				if subPattern.MatchString(t.Name) {
					t.Kind = "type"
					matches = append(matches, t)
				}
			}
		}
	}

	if len(matches) == 0 {
		// Check session cache as last resort
		cacheKey := "symbol:" + a.Path + ":" + a.Name
		if cached, ok := server.CacheGet(cacheKey); ok {
			if cm, ok := cached.(grepfunc.FuncMatch); ok {
				matches = append(matches, cm)
			}
		}
	}

	if len(matches) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{
				Type: "text",
				Text: fmt.Sprintf("Symbol %q not found in %s.\n", a.Name, rel),
			}},
		}, nil
	}

	// Dedup
	seen := make(map[string]bool)
	unique := matches[:0]
	for _, m := range matches {
		key := fmt.Sprintf("%d", m.Line)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, m)
		}
	}
	matches = unique

	var buf strings.Builder
	plural := ""
	if len(matches) > 1 {
		plural = "s"
	}
	fmt.Fprintf(&buf, "%d symbol%s matching %q in %s:\n", len(matches), plural, a.Name, rel)

	for _, m := range matches {
		fmt.Fprintf(&buf, "%s:%d-%d: **[%s]** %s\n", rel, m.Line, m.EndLine, m.Kind, firstLine(m.Body, false))

		body := m.Body
		if a.Summary {
			sl := a.SummaryLines
			if sl <= 0 {
				sl = 5
			}
			body = grepfunc.SummarizeBody(body, sl)
		}
		fmt.Fprintf(&buf, "```%s\n%s\n```\n", ext, strings.TrimRight(body, "\n"))
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func firstLine(s string, includeBody bool) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		s = strings.TrimSpace(before)
	}
	if !includeBody && len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
