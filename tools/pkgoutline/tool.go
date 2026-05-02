package pkgoutline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"mcp_patch_file/server"
	"mcp_patch_file/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "pkg_outline",
	Description: "List all symbols (functions + types) across an entire directory in one call, grouped by file. Replaces N sequential file_symbols calls with one. Essential for understanding a package before editing.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "Directory to outline. Defaults to project root."},
			"include": {Type: "string", Description: "Glob to filter files. E.g. '**/*.go'. Defaults to all source files."},
			"filter":  {Type: "string", Description: "Which symbols: 'func', 'type', or 'all' (default)."},
			"pattern": {Type: "string", Description: "Regex to filter symbol names/signatures. Case-insensitive."},
			"compact": {Type: "boolean", Description: "Terse output: no blank lines between files. Default false."},
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

type args struct {
	Path    string `json:"path"`
	Include string `json:"include"`
	Filter  string `json:"filter"`
	Pattern string `json:"pattern"`
	Compact bool   `json:"compact"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	a.Path = server.ResolvePath(a.Path)
	if a.Filter == "" {
		a.Filter = "all"
	}
	include := a.Include
	if include == "" {
		include = "*"
	}

	matchAll := regexp.MustCompile(`(?s).`)
	var syms []sym

	if a.Filter == "all" || a.Filter == "func" {
		funcs, _ := grepfunc.Search(a.Path, include, matchAll, 2000, grepfunc.IsFuncSig)
		for _, f := range funcs {
			syms = append(syms, sym{file: f.File, line: f.Line, endLine: f.EndLine, kind: "func", sig: firstSig(f.Body)})
		}
	}
	if a.Filter == "all" || a.Filter == "type" {
		types, _ := grepfunc.Search(a.Path, include, matchAll, 2000, grepfunc.IsStructSig)
		for _, t := range types {
			syms = append(syms, sym{file: t.File, line: t.Line, endLine: t.EndLine, kind: "type", sig: firstSig(t.Body)})
		}
	}

	// Dedup by file+line
	seen := map[string]bool{}
	unique := syms[:0]
	for _, s := range syms {
		k := fmt.Sprintf("%s:%d", s.file, s.line)
		if !seen[k] {
			seen[k] = true
			unique = append(unique, s)
		}
	}
	syms = removeNested(unique)

	if a.Pattern != "" {
		re, err := regexp.Compile("(?i)" + a.Pattern)
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

	byFile := map[string][]sym{}
	var fileOrder []string
	for _, s := range syms {
		rel := server.RelPath(s.file)
		if _, ok := byFile[rel]; !ok {
			fileOrder = append(fileOrder, rel)
		}
		byFile[rel] = append(byFile[rel], s)
	}
	sort.Strings(fileOrder)

	if len(syms) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No symbols found in %s.\n", server.RelPath(a.Path))}},
		}, nil
	}

	var buf strings.Builder
	totalFiles := len(fileOrder)
	if a.Compact {
		fmt.Fprintf(&buf, "%d symbols across %d files:\n", len(syms), totalFiles)
	} else {
		fmt.Fprintf(&buf, "%d symbol(s) across %d file(s) in %s:\n\n", len(syms), totalFiles, server.RelPath(a.Path))
	}

	for _, fr := range fileOrder {
		group := byFile[fr]
		sort.Slice(group, func(i, j int) bool { return group[i].line < group[j].line })
		if a.Compact {
			fmt.Fprintf(&buf, "### %s (%d)\n", fr, len(group))
		} else {
			fmt.Fprintf(&buf, "### %s — %d symbol(s)\n", fr, len(group))
		}
		for _, s := range group {
			fmt.Fprintf(&buf, "  L%-4d %-5s %s\n", s.line, s.kind, s.sig)
		}
		if !a.Compact {
			buf.WriteByte('\n')
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func firstSig(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSpace(line)
	if len(line) > 120 {
		return line[:120] + "..."
	}
	return line
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
