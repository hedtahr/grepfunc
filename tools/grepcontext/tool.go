package grepcontext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"mcp_patch_file/server"
	"mcp_patch_file/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "grep_context",
	Description: "Search file contents and return matching lines WITH surrounding context. Eliminates the grep→file_head two-step for non-function patterns (constants, imports, config values, variable inits). Returns N lines before/after each match, deduplicates overlapping windows.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":        {Type: "string", Description: "Regex to search for"},
			"path":           {Type: "string", Description: "File or directory. Defaults to project root."},
			"include":        {Type: "string", Description: "Glob filter. E.g. **/*.go. Defaults to all source files."},
			"context_lines":  {Type: "integer", Description: "Lines before/after each match. Default 3, max 10."},
			"case_sensitive": {Type: "boolean", Description: "Default false."},
			"max_results":    {Type: "integer", Description: "Max matches. Default 20, max 50."},
			"offset":         {Type: "integer", Description: "Pagination offset (0-based)."},
			"compact":        {Type: "boolean", Description: "Terse output: less whitespace, shorter headers. Keeps syntax highlighting. Default false."},
			"scope":          {Type: "boolean", Description: "Annotate each match with enclosing function/type name. Default false."},
			"group_by_file":  {Type: "boolean", Description: "Group results under file headers instead of one header per match. Format: '### path/file.go (N matches)'. Reduces noise for multi-file searches. Default false."},
			"names_only":     {Type: "boolean", Description: "If true, return only file:line — no context, no code blocks. Cheapest mode."},
			"token_budget":  {Type: "integer", Description: "Max output chars. If exceeded, auto-switches to file:line only. No default (unlimited)."},
		},
		Required: []string{"pattern"},
	},
}

type args struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	ContextLines  int    `json:"context_lines"`
	CaseSensitive bool   `json:"case_sensitive"`
	MaxResults    int    `json:"max_results"`
	Offset        int    `json:"offset"`
	Compact       bool   `json:"compact"`
	Scope         bool   `json:"scope"`
	GroupByFile   bool   `json:"group_by_file"`
	NamesOnly     bool   `json:"names_only"`
	TokenBudget   int    `json:"token_budget"`
}

type window struct {
	relPath   string
	matchLine int
	start     int
	end       int
	lines     []string
	scope     string
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	re, err := grepfunc.CompilePattern(a.Pattern, a.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %v", err)
	}

	resolved := server.ResolvePath(a.Path)

	if a.MaxResults <= 0 {
		a.MaxResults = 20
	}
	if a.MaxResults > 50 {
		a.MaxResults = 50
	}
	if a.ContextLines <= 0 {
		a.ContextLines = 3
	}
	if a.ContextLines > 10 {
		a.ContextLines = 10
	}
	glob := a.Include
	if glob == "" {
		glob = "*"
	}

	var all []window
	need := a.Offset + a.MaxResults
	scannedAll := true

	filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
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

		rel, _ := filepath.Rel(resolved, path)
		if !grepfunc.MatchGlob(glob, rel) {
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
		if glob == "*" && grepfunc.IsNonSourceExt(ext) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		rawLines := bytes.Split(data, []byte("\n"))
		strs := make([]string, len(rawLines))
		for i, l := range rawLines {
			strs[i] = string(l)
		}

		prevEnd := -1
		var boundaries map[int]int
		if a.Scope {
			combinedSig := func(line []byte) bool {
				return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line)
			}
			boundaries = grepfunc.MapBlockBoundaries(strsToBytes(strs), combinedSig)
		}
		for i, l := range strs {
			if !re.MatchString(l) {
				continue
			}
			if i <= prevEnd {
				continue // covered by previous window
			}
			start := max(0, i-a.ContextLines)
			end := min(len(strs)-1, i+a.ContextLines)
			all = append(all, window{
				relPath:   rel,
				matchLine: i + 1,
				start:     start,
				end:       end,
				lines:     strs[start : end+1],
				scope:     scopeName(strs, i, boundaries),
			})
			prevEnd = end
			if len(all) >= need {
				scannedAll = false
				return filepath.SkipAll
			}
		}
		return nil
	})

	total := len(all)
	start := min(a.Offset, total)
	end := min(a.Offset+a.MaxResults, total)
	page := all[start:end]

	compact := a.Compact
	var sb strings.Builder
	if total == 0 {
		if compact {
			sb.WriteString(fmt.Sprintf("0 matches %q\n", a.Pattern))
		} else {
			sb.WriteString(fmt.Sprintf("0 match(es) for %q\n", a.Pattern))
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: sb.String()}},
		}, nil
	}

	suffix := ""
	if !scannedAll {
		suffix = "+"
	}
	if compact {
		sb.WriteString(fmt.Sprintf("%d%s matches %q", total, suffix, a.Pattern))
	} else {
		sb.WriteString(fmt.Sprintf("%d%s match(es) for %q", total, suffix, a.Pattern))
	}
	if a.Offset > 0 || end < total {
		sb.WriteString(fmt.Sprintf(" (showing %d\u2013%d)", start+1, end))
	}
	sb.WriteByte('\n')
	if !compact {
		sb.WriteByte('\n')
	}

	renderWindow := func(w window) {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(w.relPath)), ".")
		if !a.GroupByFile {
			label := w.relPath + ":" + fmt.Sprint(w.matchLine)
			if w.scope != "" {
				label += " [" + w.scope + "]"
			}
			sb.WriteString(label + ":\n")
		} else {
			label := ":" + fmt.Sprint(w.matchLine)
			if w.scope != "" {
				label += " [" + w.scope + "]"
			}
			sb.WriteString(label + "\n")
		}
		sb.WriteString("```" + ext + "\n")
		for idx, line := range w.lines {
			lineNum := w.start + idx + 1
			if lineNum == w.matchLine {
				sb.WriteString(fmt.Sprintf("> %d: %s\n", lineNum, line))
			} else {
				sb.WriteString(fmt.Sprintf("  %d: %s\n", lineNum, line))
			}
		}
		sb.WriteString("```")
		if compact {
			sb.WriteByte('\n')
		} else {
			sb.WriteString("\n\n")
		}
	}

	if a.NamesOnly {
		for _, w := range page {
			fmt.Fprintf(&sb, "%s:%d\n", w.relPath, w.matchLine)
		}
	} else if a.GroupByFile {
		type fileGroup struct {
			relPath string
			windows []window
		}
		var groups []fileGroup
		seen := map[string]int{}
		for _, w := range page {
			if idx, ok := seen[w.relPath]; ok {
				groups[idx].windows = append(groups[idx].windows, w)
			} else {
				seen[w.relPath] = len(groups)
				groups = append(groups, fileGroup{relPath: w.relPath, windows: []window{w}})
			}
		}
		for _, g := range groups {
			if compact {
				fmt.Fprintf(&sb, "### %s (%d)\n", g.relPath, len(g.windows))
			} else {
				fmt.Fprintf(&sb, "\n### %s — %d match(es)\n", g.relPath, len(g.windows))
			}
			for _, w := range g.windows {
				renderWindow(w)
			}
		}
	} else {
		for _, w := range page {
			renderWindow(w)
		}
	}

	if end < total {
		sb.WriteString(fmt.Sprintf("%d more. Use offset=%d.\n", total-end, end))
	}

	info, err2 := os.Stat(resolved)
	if err2 == nil && !info.IsDir() {
		server.SetLastPath(resolved)
	}

	output := sb.String()
	if a.TokenBudget > 0 && len(output) > a.TokenBudget {
		// Rebuild in names_only style: file:line only
		var terse strings.Builder
		suffix := ""
		if !scannedAll {
			suffix = "+"
		}
		fmt.Fprintf(&terse, "%d%s matches %q", total, suffix, a.Pattern)
		if a.Offset > 0 || end < total {
			fmt.Fprintf(&terse, " (showing %d\u2013%d)", start+1, end)
		}
		terse.WriteByte('\n')
		for _, w := range page {
			rel := w.relPath
			if w.scope != "" {
				fmt.Fprintf(&terse, "%s:%d [%s]\n", rel, w.matchLine, w.scope)
			} else {
				fmt.Fprintf(&terse, "%s:%d\n", rel, w.matchLine)
			}
		}
		if end < total {
			fmt.Fprintf(&terse, "%d more. Use offset=%d.\n", total-end, end)
		}
		output = terse.String()
		if len(output) > a.TokenBudget {
			output = output[:a.TokenBudget]
		}
		output += fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. Use names_only=true or reduce scope for more.]\n", a.TokenBudget)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output}},
	}, nil
}

func scopeName(lines []string, lineIdx int, boundaries map[int]int) string {
	if boundaries == nil {
		return ""
	}
	start, ok := boundaries[lineIdx]
	if !ok || start >= len(lines) {
		return ""
	}
	return grepfunc.EnclosingSymbol(strsToBytes(lines), lineIdx, boundaries)
}

func strsToBytes(lines []string) [][]byte {
	b := make([][]byte, len(lines))
	for i, s := range lines {
		b[i] = []byte(s)
	}
	return b
}
