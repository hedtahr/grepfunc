// Package grepreplace replaces regex matches across files.
package grepreplace

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
	keyPattern     = "pattern"
	keyReplacement = "replacement"
	keyInclude     = "include"

	typeString  = "string"
	typeBoolean = "boolean"
	typeInteger = "integer"

	maxFilesLimit    = 200
	defaultMaxFiles  = 50
	newFileMode      = 0600
	maxFileSize      = 2 * 1024 * 1024
	noMatchMsgPrefix = "0 files matched pattern "
)

var errPatternRequired = errors.New("pattern is required")

// Tool describes the grep_replace tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_replace",
	Description: "Regex find-and-replace across many files in one call. Per-file change summary + dry_run preview. Go regex with capture groups ($1, $2).",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			keyPattern:     {Type: typeString, Description: "Go regex to find.", Items: nil},
			keyReplacement: {Type: typeString, Description: "Replacement string. Supports $1 $2 capture groups.", Items: nil},
			"path": {
				Type:        typeString,
				Description: "Absolute root directory. Defaults to project root.",
				Items:       nil,
			},
			keyInclude:       {Type: typeString, Description: "Glob filter (e.g. **/*.go). Default: *.", Items: nil},
			"dry_run":        {Type: typeBoolean, Description: "Preview without writing.", Items: nil},
			"case_sensitive": {Type: typeBoolean, Description: "Default false (case-insensitive).", Items: nil},
			"max_files":      {Type: typeInteger, Description: "Max files to process. Default 50, max 200.", Items: nil},
		},
		Required:             []string{keyPattern, keyReplacement},
		AdditionalProperties: false,
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

type result struct {
	rel   string
	count int
}

// Handle replaces regex matches across files.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Pattern == "" {
		return nil, errPatternRequired
	}

	pat := req.Pattern
	if !req.CaseSensitive {
		pat = "(?i)" + pat
	}

	pattern, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	root, glob, maxFiles := resolveScope(req)
	walker := &replaceWalker{
		re:          pattern,
		glob:        glob,
		maxFiles:    maxFiles,
		dryRun:      req.DryRun,
		replacement: req.Replacement,
		filesWalked: 0,
		results:     nil,
	}

	walkErr := filepath.WalkDir(root, walker.walk)
	if walkErr != nil {
		return nil, fmt.Errorf("walk error: %w", walkErr)
	}

	if len(walker.results) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: noMatchMsgPrefix + req.Pattern}},
			IsError: false,
		}, nil
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: renderResults(walker.results, req.DryRun)}},
		IsError: false,
	}, nil
}

func resolveScope(req args) (string, string, int) {
	root := req.Path
	if root == "" {
		root = server.ProjectRoot
	}

	root = server.ResolvePath(root)

	glob := req.Include
	if glob == "" {
		glob = "*"
	}

	maxFiles := req.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}

	if maxFiles > maxFilesLimit {
		maxFiles = maxFilesLimit
	}

	return root, glob, maxFiles
}

type replaceWalker struct {
	re          *regexp.Regexp
	glob        string
	maxFiles    int
	dryRun      bool
	replacement string
	filesWalked int
	results     []result
}

func (w *replaceWalker) walk(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}

	if entry.IsDir() {
		if skipDir(entry.Name()) {
			return filepath.SkipDir
		}

		return nil
	}

	if w.filesWalked >= w.maxFiles {
		return fs.SkipAll
	}

	if server.IsBannedPath(path) {
		return nil
	}

	return w.applyFile(path, entry)
}

func (w *replaceWalker) applyFile(path string, entry fs.DirEntry) error {
	ext := strings.ToLower(filepath.Ext(path))
	if grepfunc.IsBinaryExt(ext) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("file info: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil
	}

	if !grepfunc.MatchGlob(w.glob, path) {
		return nil
	}

	// #nosec G304,G122 -- paths bounds-checked by server
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	matches := w.re.FindAll(content, -1)
	if len(matches) == 0 {
		return nil
	}

	w.filesWalked++

	if !w.dryRun {
		newContent := w.re.ReplaceAll(content, []byte(w.replacement))

		writeErr := atomicWrite(path, newContent)
		if writeErr != nil {
			return writeErr
		}
	}

	w.results = append(w.results, result{rel: server.RelPath(path), count: len(matches)})

	return nil
}

func skipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".")
}

func renderResults(results []result, dryRun bool) string {
	label := "changed"
	if dryRun {
		label = "would change (dry_run)"
	}

	var output strings.Builder

	fmt.Fprintf(&output, "%d files %s\n\n```\n", len(results), label)

	for _, r := range results {
		fmt.Fprintf(&output, "- %s: %d replacement", r.rel, r.count)

		if r.count != 1 {
			output.WriteByte('s')
		}

		output.WriteByte('\n')
	}

	output.WriteString("```\n")

	return output.String()
}

func atomicWrite(path string, data []byte) error {
	mode := fs.FileMode(newFileMode)

	info, statErr := os.Stat(path)
	if statErr == nil {
		mode = info.Mode()
	}

	tmp := path + ".tmp"
	// #nosec G703 -- paths bounds-checked by server
	err := os.WriteFile(tmp, data, mode)
	if err != nil {
		return writeDirect(path, data, mode)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		_ = os.Remove(tmp)

		return writeDirect(path, data, mode)
	}

	return nil
}

func writeDirect(path string, data []byte, mode fs.FileMode) error {
	// #nosec G703 -- paths bounds-checked by server
	err := os.WriteFile(path, data, mode)
	if err != nil {
		return fmt.Errorf("direct write: %w", err)
	}

	return nil
}
