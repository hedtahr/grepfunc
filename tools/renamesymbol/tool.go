// Package renamesymbol renames a symbol across the project.
package renamesymbol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const (
	keyOldName  = "old_name"
	keyNewName  = "new_name"
	typeString  = "string"
	maxFileSize = 2 * 1024 * 1024
)

var (
	errOldNameRequired = errors.New("old_name is required")
	errNewNameRequired = errors.New("new_name is required")
)

// Tool describes the rename_symbol tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "rename_symbol",
	Description: "Rename a function/type/variable project-wide. Word-boundary matching.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			keyOldName: {Type: typeString, Description: "Symbol name to rename.", Items: nil},
			keyNewName: {Type: typeString, Description: "New name for the symbol.", Items: nil},
			"path": {
				Type:        typeString,
				Description: "Root directory. Defaults to project root.",
				Items:       nil,
			},
			"include": {
				Type:        typeString,
				Description: "Glob filter. Default: all source files.",
				Items:       nil,
			},
			"kind": {
				Type:        typeString,
				Description: "Declaration kind: 'func', 'type', 'any' (default).",
				Items:       nil,
			},
			"dry_run":        {Type: "boolean", Description: "Preview changes without writing.", Items: nil},
			"case_sensitive": {Type: "boolean", Description: "Case-sensitive. Default false.", Items: nil},
		},
		Required:             []string{keyOldName, keyNewName},
		AdditionalProperties: false,
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

// Handle renames a symbol across the project.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.OldName == "" {
		return nil, errOldNameRequired
	}

	if req.NewName == "" {
		return nil, errNewNameRequired
	}

	req.Path = server.ResolvePath(req.Path)
	if err := server.CheckBounds(req.Path); err != nil {
		return nil, err
	}

	if req.Include == "" {
		req.Include = "*"
	}

	flags := "(?i)"
	if req.CaseSensitive {
		flags = ""
	}

	namePattern, err := regexp.Compile(flags + `\b` + regexp.QuoteMeta(req.OldName) + `\b`)
	if err != nil {
		return nil, fmt.Errorf("invalid old_name: %w", err)
	}

	walker := &renameWalker{
		root:    req.Path,
		re:      namePattern,
		matcher: grepfunc.CompileGlob(req.Include),
		include: req.Include,
		newName: req.NewName,
		dryRun:  req.DryRun,
		changes: nil,
		total:   0,
	}

	walkErr := grepfunc.WalkDir(req.Path, walker.walk)
	if walkErr != nil {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("Walk error: %v", walkErr)}},
			IsError: true,
		}, nil
	}

	return renderSummary(req, walker.changes, walker.total), nil
}

type renameWalker struct {
	root    string
	re      *regexp.Regexp
	matcher *grepfunc.GlobMatcher
	include string
	newName string
	dryRun  bool
	changes []fileChange
	total   int
}

func (w *renameWalker) walk(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}

	if entry.IsDir() {
		return nil
	}

	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil
	}

	return w.applyFile(path, entry)
}

func (w *renameWalker) applyFile(path string, entry fs.DirEntry) error {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return fmt.Errorf("rel: %w", err)
	}

	if !w.matcher.Match(rel) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("file info: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if skipExt(w.include, ext) {
		return nil
	}

	// #nosec G304,G122 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	matches := w.re.FindAllIndex(data, -1)
	if len(matches) == 0 {
		return nil
	}

	newData := w.re.ReplaceAll(data, []byte(strings.ReplaceAll(w.newName, "$", "$$")))

	w.changes = append(w.changes, fileChange{rel: rel, count: len(matches)})
	w.total += len(matches)

	if !w.dryRun {
		return w.writeFile(path, rel, newData, info.Mode())
	}

	return nil
}

func skipExt(include, ext string) bool {
	if grepfunc.IsBinaryExt(ext) {
		return true
	}

	return include == "*" && grepfunc.IsNonSourceExt(ext)
}

func (w *renameWalker) writeFile(path, rel string, newData []byte, mode fs.FileMode) error {
	// Atomic write via tmp + rename.
	tmpPath := path + ".tmp"
	// #nosec G122 -- path from WalkDir under resolved root
	_ = os.Remove(tmpPath)

	// #nosec G122,G703 -- paths bounds-checked by server
	err := os.WriteFile(tmpPath, newData, mode)
	if err != nil {
		return fmt.Errorf("write failed for %s: %w", rel, err)
	}

	// #nosec G122 -- path from WalkDir under resolved root
	err = os.Rename(tmpPath, path)
	if err != nil {
		// #nosec G122,G703 -- paths bounds-checked by server
		_ = os.WriteFile(path, newData, mode)

		return fmt.Errorf("rename failed for %s: %w", rel, err)
	}

	return nil
}

func renderSummary(req args, changes []fileChange, totalReplacements int) *server.ToolCallResult {
	var output strings.Builder
	if len(changes) == 0 {
		fmt.Fprintf(&output, "No occurrences of %q found.\n", req.OldName)
	} else {
		fmt.Fprintf(&output, "Renamed %q → %q: %d replacements across %d files\n",
			req.OldName, req.NewName, totalReplacements, len(changes))

		for _, change := range changes {
			plural := "replacements"
			if change.count == 1 {
				plural = "replacement"
			}

			fmt.Fprintf(&output, "- %s: %d %s\n", change.rel, change.count, plural)
		}

		if req.DryRun {
			output.WriteString("\n[DRY RUN — no files written]\n")
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: output.String()}},
		IsError: false,
	}
}
