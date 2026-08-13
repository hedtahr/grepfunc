// Package patchedit implements the patch_file and batch_patch MCP tools.
package patchedit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var (
	errGlobRequired  = errors.New("glob is required")
	errEditsRequired = errors.New("edits is required")
)

// BatchTool is the MCP tool definition for batch_patch.
//
//nolint:gochecknoglobals // MCP tool definition
var BatchTool = server.Tool{
	Name:        "batch_patch",
	Description: "Apply the same find-and-replace edits across ALL files matching a glob in one call. Per-file summary instead of N diffs.",
	InputSchema: server.InputSchema{
		Type:                 jsonTypeObject,
		AdditionalProperties: false,
		Properties: map[string]server.Property{
			"glob": {Type: jsonTypeString, Items: nil,
				Description: "Glob to select target files. E.g. '**/*.go', 'src/**/*.ts'."},
			propEdits: {Type: jsonTypeArray,
				Description: "Edit operations: {old_text, new_text, replace_all?}.",
				Items: &server.Property{Type: jsonTypeObject, Items: nil,
					Description: "Edit op: old_text, new_text, optional replace_all."}},
			"dry_run": {Type: jsonTypeBoolean, Items: nil,
				Description: "Preview which files would change (per-file match counts)."},
			"fail_fast": {Type: jsonTypeBoolean, Items: nil,
				Description: "Skip files where any edit fails to match (default true)."},
			"skip_validate": {Type: jsonTypeBoolean, Items: nil,
				Description: "Skip post-write validation (go vet / python ast)."},
			"no_diff": {Type: jsonTypeBoolean, Items: nil,
				Description: "Omit per-file diff output — summary only (saves tokens)."},
			propPath: {Type: jsonTypeString, Items: nil,
				Description: "Root directory to search. Defaults to project root."},
		},
		Required: []string{"glob", propEdits},
	},
}

type batchArgs struct {
	Glob         string   `json:"glob"`
	Edits        []EditOp `json:"edits"`
	DryRun       bool     `json:"dry_run"`
	FailFast     bool     `json:"fail_fast"`
	SkipValidate bool     `json:"skip_validate"`
	NoDiff       bool     `json:"no_diff"`
	Path         string   `json:"path"`
}

type fileResult struct {
	rel      string
	applied  int
	total    int
	failures []string
	diff     string
	skipped  bool
}

// BatchHandle processes a batch_patch request.
func BatchHandle(raw json.RawMessage) (*server.ToolCallResult, error) {
	args, err := parseBatchArgs(raw)
	if err != nil {
		return nil, err
	}

	root := server.ProjectRoot
	if args.Path != "" {
		root = server.ResolvePath(args.Path)
	}

	matchedPaths, err := collectMatchingPaths(root, args.Glob)
	if err != nil {
		return nil, err
	}

	var (
		mutex       sync.Mutex
		fileResults []fileResult
	)

	sem := make(chan struct{}, runtime.NumCPU())

	var waitGroup sync.WaitGroup

	for _, path := range matchedPaths {
		waitGroup.Add(1)

		sem <- struct{}{}

		go func(p string) {
			defer waitGroup.Done()
			defer func() { <-sem }()

			fr := processBatchFile(p, args)
			if fr != nil {
				mutex.Lock()

				fileResults = append(fileResults, *fr)
				mutex.Unlock()
			}
		}(path)
	}

	waitGroup.Wait()

	sort.Slice(fileResults, func(left, right int) bool {
		return fileResults[left].rel < fileResults[right].rel
	})

	if len(fileResults) == 0 {
		msg := fmt.Sprintf("No files matched %q or no edits applied.", args.Glob)

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: contentTypeText, Text: msg}},
			IsError: false,
		}, nil
	}

	return buildBatchSummary(args, fileResults), nil
}

func parseBatchArgs(raw json.RawMessage) (batchArgs, error) {
	var args batchArgs

	err := json.Unmarshal(raw, &args)
	if err != nil {
		return args, fmt.Errorf("invalid arguments: %w", err)
	}

	// default fail_fast=true (safe: don't partially edit files)
	var rawMap map[string]any
	if json.Unmarshal(raw, &rawMap) == nil {
		if _, ok := rawMap["fail_fast"]; !ok {
			args.FailFast = true
		}
	}

	if args.Glob == "" {
		return args, errGlobRequired
	}

	if len(args.Edits) == 0 {
		return args, errEditsRequired
	}

	return args, nil
}

func collectMatchingPaths(root, glob string) ([]string, error) {
	var matchedPaths []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		return collectPath(path, d, err, root, glob, &matchedPaths)
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	return matchedPaths, nil
}

func collectPath(path string, entry fs.DirEntry, err error, root, glob string, matchedPaths *[]string) error {
	if err != nil {
		return err
	}

	if entry.IsDir() {
		if skippedDir(entry.Name()) {
			return filepath.SkipDir
		}

		return nil
	}

	if skipFile(path, entry, root, glob) {
		return nil
	}

	*matchedPaths = append(*matchedPaths, path)

	return nil
}

func skippedDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "vendor" ||
		name == ".idea" || name == "__pycache__" || strings.HasPrefix(name, ".")
}

func skipFile(path string, entry fs.DirEntry, root, glob string) bool {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return true
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}

	if !grepfunc.MatchGlob(glob, rel) {
		return true
	}

	info, err := entry.Info()
	if err != nil {
		return true
	}

	if info.Size() > maxFileSize {
		return true
	}

	return grepfunc.IsBinaryExt(strings.ToLower(filepath.Ext(path)))
}

func buildBatchSummary(a batchArgs, fileResults []fileResult) *server.ToolCallResult {
	totalFiles, totalEdits, failCount := sumResults(fileResults)

	action := "Applied"
	if a.DryRun {
		action = "[DRY RUN] Would apply"
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, "%s %d edits across %d files", action, totalEdits, totalFiles)

	if failCount > 0 {
		fmt.Fprintf(&buf, " (%d files with failures)", failCount)
	}

	buf.WriteString("\n")

	for _, fileRes := range fileResults {
		writeBatchRow(&buf, fileRes)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: contentTypeText, Text: buf.String()}},
		IsError: false,
	}
}

func sumResults(fileResults []fileResult) (int, int, int) {
	totalFiles, totalEdits, failCount := 0, 0, 0

	for _, fileRes := range fileResults {
		totalEdits += fileRes.applied

		if fileRes.applied > 0 || fileRes.skipped {
			totalFiles++
		}

		if len(fileRes.failures) > 0 {
			failCount++
		}
	}

	return totalFiles, totalEdits, failCount
}

func writeBatchRow(buf *strings.Builder, fileRes fileResult) {
	if fileRes.skipped {
		fmt.Fprintf(buf, "- %s: SKIPPED (fail_fast) — %s\n", fileRes.rel, strings.Join(fileRes.failures, "; "))

		return
	}

	if fileRes.applied == fileRes.total && len(fileRes.failures) == 0 {
		fmt.Fprintf(buf, "- %s: %d/%d edits\n", fileRes.rel, fileRes.applied, fileRes.total)
	} else {
		fmt.Fprintf(buf, "- %s: %d/%d edits", fileRes.rel, fileRes.applied, fileRes.total)

		if len(fileRes.failures) > 0 {
			fmt.Fprintf(buf, " (%s)", strings.Join(fileRes.failures, "; "))
		}

		buf.WriteByte('\n')
	}

	if fileRes.diff != "" {
		fmt.Fprintf(buf, "```diff\n%s```\n", fileRes.diff)
	}
}

func processBatchFile(path string, args batchArgs) *fileResult {
	ext := strings.ToLower(filepath.Ext(path))

	// #nosec G304 -- paths bounds-checked by server
	original, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	content := original
	if formatted, didFmt := preFormat(path, content, ext); didFmt {
		content = formatted
	}

	results := computeResults(content, nil, args.Edits)

	fileRes := &fileResult{
		rel: server.RelPath(path), total: len(args.Edits),
		applied: 0, failures: nil, diff: "", skipped: false,
	}

	collectFailures(fileRes, results)

	if len(fileRes.failures) > 0 && args.FailFast {
		fileRes.skipped = true

		return fileRes
	}

	applied, current := applySuccessful(content, results)
	fileRes.applied = applied

	if applied == 0 {
		return nil
	}

	if !args.NoDiff {
		fileRes.diff = unifiedDiff(content, current, path, defaultDiffCtx)
	}

	if !args.DryRun {
		fileRes.failures = writeValidated(path, current, args.SkipValidate, fileRes.failures)
	}

	return fileRes
}

func collectFailures(fileRes *fileResult, results []editResult) {
	for _, r := range results {
		if !r.Success {
			fileRes.failures = append(fileRes.failures, fmt.Sprintf("edit %d: %s", r.Index, r.Error))
		}
	}
}

func writeValidated(path string, current []byte, skipValidate bool, failures []string) []string {
	err := atomicWrite(path, current)
	if err != nil {
		return append(failures, fmt.Sprintf("write error: %v", err))
	}

	if skipValidate {
		return failures
	}

	if warn := runValidate(path); warn != "" {
		failures = append(failures, "validate: "+warn)
	}

	return failures
}
