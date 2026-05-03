package grepreplace

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
	Name:        "grep_replace",
	Description: "Regex find-and-replace across files matching a glob. Like sed -i 's/pattern/replacement/g'. Uses Go regex syntax — supports capture groups ($1, $2). Returns per-file change summary. Use dry_run=true to preview.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"pattern":        {Type: "string", Description: "Go regex pattern to find."},
			"replacement":    {Type: "string", Description: "Replacement string. Supports $1 $2 capture groups."},
			"path":           {Type: "string", Description: "Absolute root directory to search. Defaults to project root."},
			"include":        {Type: "string", Description: "Glob filter (e.g. **/*.go). Default: *."},
			"dry_run":        {Type: "boolean", Description: "Preview without writing. Default false."},
			"case_sensitive": {Type: "boolean", Description: "Default false (case-insensitive)."},
			"max_files":      {Type: "integer", Description: "Max files to process. Default 50, max 200."},
		},
		Required: []string{"pattern", "replacement"},
	},
}

type args struct {
	Pattern       string `json:"pattern"`
	Replacement   string `json:"replacement"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	DryRun        bool   `json:"dry_run"`
	CaseSensitive bool   `json:"case_sensitive"`
	MaxFiles      int    `json:"max_files"`
}

const maxFilesLimit = 200

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	pat := a.Pattern
	if !a.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %v", err)
	}

	root := a.Path
	if root == "" {
		root = server.ProjectRoot
	}
	root = server.ResolvePath(root)

	glob := a.Include
	if glob == "" {
		glob = "*"
	}

	maxFiles := a.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 50
	}
	if maxFiles > maxFilesLimit {
		maxFiles = maxFilesLimit
	}

	type result struct {
		rel   string
		count int
	}
	var results []result
	filesWalked := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filesWalked >= maxFiles {
			return fs.SkipAll
		}
		if server.IsBannedPath(path) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if grepfunc.IsBinaryExt(ext) {
			return nil
		}
		info, err2 := d.Info()
		if err2 != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		if !grepfunc.MatchGlob(glob, path) {
			return nil
		}

		content, err2 := os.ReadFile(path)
		if err2 != nil {
			return nil
		}
		matches := re.FindAll(content, -1)
		if len(matches) == 0 {
			return nil
		}
		filesWalked++

		if !a.DryRun {
			newContent := re.ReplaceAll(content, []byte(a.Replacement))
			if writeErr := atomicWrite(path, newContent); writeErr != nil {
				return nil
			}
		}
		results = append(results, result{rel: server.RelPath(path), count: len(matches)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk error: %v", err)
	}

	if len(results) == 0 {
		text := fmt.Sprintf("0 files matched pattern %s", a.Pattern)
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: text}}}, nil
	}

	label := "changed"
	if a.DryRun {
		label = "would change (dry_run)"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d files %s\n\n```\n", len(results), label)
	for _, r := range results {
		fmt.Fprintf(&sb, "- %s: %d replacement", r.rel, r.count)
		if r.count != 1 {
			sb.WriteByte('s')
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("```\n")

	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: sb.String()}}}, nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return os.WriteFile(path, data, 0644)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return os.WriteFile(path, data, 0644)
	}
	return nil
}
