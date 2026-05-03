package renamesymbol

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
	Name:        "rename_symbol",
	Description: "Rename a function, type, or variable across the entire project in one call. Uses word-boundary matching to avoid partial renames. Replaces the grep_refs → batch_patch → verify multi-call pattern.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"old_name":       {Type: "string", Description: "Symbol name to rename."},
			"new_name":       {Type: "string", Description: "New name for the symbol."},
			"path":           {Type: "string", Description: "MUST be absolute path to root directory to search."},
			"include":        {Type: "string", Description: "Glob filter e.g. '**/*.go'. Defaults to all source files."},
			"kind":           {Type: "string", Description: "Filter declaration kind: 'func', 'type', or 'any' (default). Only affects dry-run declaration count; replacements always use word-boundary."},
			"dry_run":        {Type: "boolean", Description: "Preview changes without writing files."},
			"case_sensitive": {Type: "boolean", Description: "Case-sensitive name matching. Default false."},
		},
		Required: []string{"old_name", "new_name"},
	},
}

type args struct {
	OldName       string `json:"old_name"`
	NewName       string `json:"new_name"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	Kind          string `json:"kind"`
	DryRun        bool   `json:"dry_run"`
	CaseSensitive bool   `json:"case_sensitive"`
}

type fileChange struct {
	rel   string
	count int
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.OldName == "" {
		return nil, fmt.Errorf("old_name is required")
	}
	if a.NewName == "" {
		return nil, fmt.Errorf("new_name is required")
	}
	a.Path = server.ResolvePath(a.Path)
	if a.Include == "" {
		a.Include = "*"
	}

	flags := "(?i)"
	if a.CaseSensitive {
		flags = ""
	}
	re, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(a.OldName) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid old_name: %v", err)
	}

	var changes []fileChange
	totalReplacements := 0

	walkErr := filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
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

		matches := re.FindAllIndex(data, -1)
		if len(matches) == 0 {
			return nil
		}

		newData := re.ReplaceAll(data, []byte(a.NewName))
		changes = append(changes, fileChange{rel: rel, count: len(matches)})
		totalReplacements += len(matches)

		if !a.DryRun {
			// Atomic write via tmp + rename
			tmpPath := path + ".tmp"
			os.Remove(tmpPath)
			if err := os.WriteFile(tmpPath, newData, info.Mode()); err != nil {
				return fmt.Errorf("write failed for %s: %v", rel, err)
			}
			if err := os.Rename(tmpPath, path); err != nil {
				_ = os.WriteFile(path, newData, info.Mode())
				return fmt.Errorf("rename failed for %s: %v", rel, err)
			}
		}
		return nil
	})
	if walkErr != nil {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("Walk error: %v", walkErr)}},
			IsError: true,
		}, nil
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].rel < changes[j].rel })

	var sb strings.Builder
	if len(changes) == 0 {
		fmt.Fprintf(&sb, "No occurrences of %q found.\n", a.OldName)
	} else {
		fmt.Fprintf(&sb, "Renamed %q → %q: %d replacement(s) across %d file(s)\n\n",
			a.OldName, a.NewName, totalReplacements, len(changes))
		for _, c := range changes {
			plural := "replacements"
			if c.count == 1 {
				plural = "replacement"
			}
			fmt.Fprintf(&sb, "- %s: %d %s\n", c.rel, c.count, plural)
		}
		if a.DryRun {
			sb.WriteString("\n[DRY RUN — no files written]\n")
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: sb.String()}},
	}, nil
}
