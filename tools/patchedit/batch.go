package patchedit

import (
	"encoding/json"
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

var BatchTool = server.Tool{
	Name:        "batch_patch",
	Description: "Apply the same find-and-replace edits across ALL files matching a glob pattern in one call. Eliminates N sequential patch_file calls for cross-file refactors (renames, API changes). Returns a compact per-file summary instead of N full diffs.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"glob":          {Type: "string", Description: "Glob pattern to select target files. E.g. '**/*.go', 'src/**/*.ts'. Matched against all files under project root."},
			"edits":         {Type: "array", Description: "Array of {old_text, new_text, replace_all?} edit operations to apply to every matched file.", Items: &server.Property{Type: "object", Description: "Edit op: old_text, new_text, optional replace_all."}},
			"dry_run":       {Type: "boolean", Description: "Preview which files would change without writing. Shows per-file match counts."},
			"fail_fast":     {Type: "boolean", Description: "If true (default), skip files where any edit fails to match — don't partially edit them. Set false to apply successful edits even when some fail."},
			"skip_validate": {Type: "boolean", Description: "Skip post-write validation (go vet / rustc / python ast)."},
			"no_diff":       {Type: "boolean", Description: "Omit per-file diff output. Set true for summary-only (saves tokens). Set false to see per-file diffs."},
			"path":          {Type: "string", Description: "MUST be absolute path — root directory to search. Always provide explicitly; do not rely on default."},
		},
		Required: []string{"glob", "edits"},
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

func BatchHandle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a batchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Glob == "" {
		return nil, fmt.Errorf("glob is required")
	}
	if len(a.Edits) == 0 {
		return nil, fmt.Errorf("edits is required")
	}

	root := server.ProjectRoot
	if a.Path != "" {
		root = server.ResolvePath(a.Path)
	}

	var matchedPaths []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(root, path)
		if !grepfunc.MatchGlob(a.Glob, rel) {
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
		matchedPaths = append(matchedPaths, path)
		return nil
	})

	var mu sync.Mutex
	var fileResults []fileResult
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup

	for _, path := range matchedPaths {
		wg.Add(1)
		sem <- struct{}{}
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			fr := processBatchFile(p, a)
			if fr != nil {
				mu.Lock()
				fileResults = append(fileResults, *fr)
				mu.Unlock()
			}
		}(path)
	}
	wg.Wait()

	sort.Slice(fileResults, func(i, j int) bool {
		return fileResults[i].rel < fileResults[j].rel
	})

	if len(fileResults) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No files matched %q or no edits applied.", a.Glob)}},
		}, nil
	}

	totalFiles := 0
	totalEdits := 0
	failCount := 0
	var buf strings.Builder

	for _, fr := range fileResults {
		totalEdits += fr.applied
		if fr.applied > 0 || fr.skipped {
			totalFiles++
		}
		if len(fr.failures) > 0 {
			failCount++
		}
	}

	action := "Applied"
	if a.DryRun {
		action = "[DRY RUN] Would apply"
	}
	fmt.Fprintf(&buf, "%s %d edits across %d files", action, totalEdits, totalFiles)
	if failCount > 0 {
		fmt.Fprintf(&buf, " (%d files with failures)", failCount)
	}
	buf.WriteString("\n")

	for _, fr := range fileResults {
		if fr.skipped {
			fmt.Fprintf(&buf, "- %s: SKIPPED (fail_fast) — %s\n", fr.rel, strings.Join(fr.failures, "; "))
			continue
		}
		if fr.applied == fr.total && len(fr.failures) == 0 {
			fmt.Fprintf(&buf, "- %s: %d/%d edits\n", fr.rel, fr.applied, fr.total)
		} else {
			fmt.Fprintf(&buf, "- %s: %d/%d edits", fr.rel, fr.applied, fr.total)
			if len(fr.failures) > 0 {
				fmt.Fprintf(&buf, " (%s)", strings.Join(fr.failures, "; "))
			}
			buf.WriteByte('\n')
		}
		if fr.diff != "" {
			fmt.Fprintf(&buf, "```diff\n%s```\n", fr.diff)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func processBatchFile(path string, a batchArgs) *fileResult {
	ext := strings.ToLower(filepath.Ext(path))
	original, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	content := original
	if formatted, didFmt, _ := preFormat(path, content, ext); didFmt {
		content = formatted
	}

	results := computeResults(content, nil, a.Edits)

	fr := &fileResult{rel: server.RelPath(path), total: len(a.Edits)}

	anyFail := false
	for _, r := range results {
		if !r.Success {
			fr.failures = append(fr.failures, fmt.Sprintf("edit %d: %s", r.Index, r.Error))
			anyFail = true
		}
	}

	if anyFail {
		fr.skipped = true
		return fr
	}

	// Sort descending by match offset so each applyReplacement doesn't shift subsequent offsets.
	applyOrder := make([]editResult, len(results))
	copy(applyOrder, results)
	sort.Slice(applyOrder, func(i, j int) bool {
		iOff, jOff := 0, 0
		if len(applyOrder[i].Matches) > 0 {
			iOff = applyOrder[i].Matches[0].Offset
		}
		if len(applyOrder[j].Matches) > 0 {
			jOff = applyOrder[j].Matches[0].Offset
		}
		return iOff > jOff
	})

	current := content
	applied := 0
	for _, r := range applyOrder {
		if r.Success {
			current = applyReplacement(current, r)
			applied++
		}
	}
	fr.applied = applied

	if applied == 0 {
		return nil
	}

	if !a.NoDiff {
		fr.diff = unifiedDiff(content, current, path, 3)
	}

	if !a.DryRun {
		os.WriteFile(path, current, 0644)
		if !a.SkipValidate {
			if warn := runValidate(path); warn != "" {
				fr.failures = append(fr.failures, "validate: "+warn)
			}
		}
	}

	return fr
}
