package patchedit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

const (
	maxFileSize       = 2 * 1024 * 1024
	minCoverage       = 0.8
	percentScale      = 100
	maxAmbiguousShown = 10
	maxContextLineLen = 100
	defaultFileMode   = os.FileMode(0644)
	maxOpsForDiff     = 3
	eofLine           = 1 << 30
)

var (
	errPathRequired = errors.New("path is required")
	errNoOperations = errors.New("at least one edit, insert, or append_text is required")
	errFileTooLarge = errors.New("file too large")
)

func handleEditFile(raw json.RawMessage) (*server.ToolCallResult, error) {
	args, err := parseEditArgs(raw)
	if err != nil {
		return nil, err
	}

	server.SetLastPath(args.Path)

	defaultEchoLines(raw, args)
	indexEdits(args)

	original, err := readOriginal(args)
	if err != nil {
		return nil, err
	}

	content := original

	ext := filepath.Ext(args.Path)
	if formatted, didFmt := preFormat(args.Path, content, ext); didFmt {
		content = formatted
	}

	appendEOFInsert(args, content)

	results := computeResults(content, args.Inserts, args.Edits)
	results = detectOverlaps(results)

	showDiff := !args.NoDiff && len(args.Edits)+len(args.Inserts) <= maxOpsForDiff

	if args.FailFast {
		for _, r := range results {
			if !r.Success {
				return buildResponse(args.Path, original, content, content, results, true,
					args.DiffContext, args.SkipValidate, showDiff, args.EchoLines, true, args.Terse)
			}
		}
	}

	current := applyResults(content, results)

	return buildResponse(args.Path, original, content, current, results, args.DryRun,
		args.DiffContext, args.SkipValidate, showDiff, args.EchoLines, false, args.Terse)
}

func parseEditArgs(raw json.RawMessage) (*EditFileArgs, error) {
	var args EditFileArgs

	err := json.Unmarshal(raw, &args)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Path == "" {
		return nil, errPathRequired
	}

	args.Path = server.ResolvePath(args.Path)

	err = server.CheckBounds(args.Path)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	err = server.CheckBanned(args.Path)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	err = expandInsertFile(&args)
	if err != nil {
		return nil, err
	}

	if len(args.Edits) == 0 && len(args.Inserts) == 0 && args.AppendText == "" {
		return nil, errNoOperations
	}

	return &args, nil
}

// expandInsertFile reads the insert_file and appends it as an insert operation.
func expandInsertFile(args *EditFileArgs) error {
	if args.InsertFile == "" {
		return nil
	}

	insertPath := server.ResolvePath(args.InsertFile)

	err := server.CheckBounds(insertPath)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = server.CheckBanned(insertPath)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	data, err := os.ReadFile(insertPath) // #nosec G304 -- insert_file path bounds-checked above
	if err != nil {
		return fmt.Errorf("failed to read insert_file: %w", err)
	}

	line := max(args.InsertLine, 1)
	args.Inserts = append(args.Inserts, InsertOp{
		Line:  line,
		Text:  string(data),
		Index: 0,
	})

	return nil
}

// #12: default echo_lines to 3 unless suppressed or explicitly set in raw JSON.
func defaultEchoLines(raw json.RawMessage, args *EditFileArgs) {
	if args.EchoLines != 0 || args.NoDiff {
		return
	}

	var rawMap map[string]any
	if json.Unmarshal(raw, &rawMap) != nil {
		return
	}

	if _, ok := rawMap["echo_lines"]; ok {
		return
	}

	args.EchoLines = 3
}

// #1: auto-number indices when all default (0) and multiple edits exist.
func indexEdits(args *EditFileArgs) {
	autoNumberEdits(args)
	autoNumberInserts(args)
}

func autoNumberEdits(args *EditFileArgs) {
	if len(args.Edits) == 0 {
		return
	}

	for _, e := range args.Edits {
		if e.Index != 0 {
			return
		}
	}

	for i := range args.Edits {
		args.Edits[i].Index = i + 1
	}
}

func autoNumberInserts(args *EditFileArgs) {
	if len(args.Inserts) == 0 {
		return
	}

	for _, ins := range args.Inserts {
		if ins.Index != 0 {
			return
		}
	}

	for i := range args.Inserts {
		args.Inserts[i].Index = i + len(args.Edits) + 1
	}
}

// #7: 2MB size guard, then read the file (create_if_missing → empty).
func readOriginal(args *EditFileArgs) ([]byte, error) {
	fi, err := os.Stat(args.Path)
	if err == nil && fi.Size() > maxFileSize {
		return nil, fmt.Errorf(
			"%w (%d bytes). Max 2MB for patch_file; "+
				"use batch_patch with path filter or edit manually",
			errFileTooLarge, fi.Size())
	}

	original, err := os.ReadFile(args.Path)
	if err != nil {
		if os.IsNotExist(err) && args.CreateIfMissing {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	return original, nil
}

func appendEOFInsert(args *EditFileArgs, content []byte) {
	if args.AppendText == "" {
		return
	}

	sep := ""
	if len(content) > 0 && content[len(content)-1] != '\n' {
		sep = "\n"
	}

	args.Inserts = append(args.Inserts, InsertOp{
		Line:  eofLine,
		Text:  sep + args.AppendText,
		Index: len(args.Edits) + len(args.Inserts) + 1,
	})
}

// detectOverlaps fails edits whose match regions intersect. Applying overlapping
// edits against shifting content would silently corrupt the file, so the
// higher-offset edit is rejected instead.
func detectOverlaps(results []editResult) []editResult {
	type locRef struct {
		res *editResult
		loc MatchLoc
	}

	var list []locRef

	for i := range results {
		if results[i].Success && len(results[i].Matches) > 0 {
			for _, loc := range results[i].Matches {
				list = append(list, locRef{res: &results[i], loc: loc})
			}
		}
	}

	for listIdx := range list {
		for otherIdx := listIdx + 1; otherIdx < len(list); otherIdx++ {
			if !rangesOverlap(list[listIdx].loc, list[otherIdx].loc) {
				continue
			}

			fail := list[otherIdx].res
			if list[listIdx].loc.Offset > list[otherIdx].loc.Offset {
				fail = list[listIdx].res
			}

			if fail.Success {
				fail.Success = false
				fail.Error = fmt.Sprintf("OVERLAP: region %d-%d intersects region %d-%d of another edit; remove or merge one.",
					list[listIdx].loc.Offset, list[listIdx].loc.EndOffset, list[otherIdx].loc.Offset, list[otherIdx].loc.EndOffset)
			}
		}
	}

	return results
}

// rangesOverlap reports whether two match regions intersect (or one point lies inside the other).
func rangesOverlap(a, b MatchLoc) bool {
	return a.Offset < b.EndOffset && b.Offset < a.EndOffset
}

func computeResults(content []byte, inserts []InsertOp, edits []EditOp) []editResult {
	results := make([]editResult, 0, len(inserts)+len(edits))

	sorted := make([]InsertOp, len(inserts))
	copy(sorted, inserts)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Line > sorted[right].Line })

	for _, ins := range sorted {
		r := applyInsert(content, ins, ins.Index)
		results = append(results, r)
	}

	for _, op := range edits {
		r := findAndReplace(content, op, op.Index)
		results = append(results, r)
	}

	sort.Slice(results, func(left, right int) bool {
		leftOrder := results[left].Index

		rightOrder := results[right].Index

		if len(results[left].Matches) > 0 && results[left].Matches[0].Strategy == strategyInsert {
			leftOrder -= 10000
		}

		if len(results[right].Matches) > 0 && results[right].Matches[0].Strategy == strategyInsert {
			rightOrder -= 10000
		}

		return leftOrder < rightOrder
	})

	return results
}

func runValidate(path string) string {
	ext := filepath.Ext(path)

	var cmd *exec.Cmd

	switch ext {
	case ".go":
		cmd = exec.CommandContext(context.Background(), "go", "vet", ".")
		cmd.Dir = filepath.Dir(path)
	case ".py":
		// #nosec G204 -- paths bounds-checked by server
		cmd = exec.CommandContext(context.Background(), "python3", "-c",
			"import ast, sys; ast.parse(open(sys.argv[1]).read())", path)
	default:
		return ""
	}

	if cmd == nil {
		return ""
	}

	_, err := exec.LookPath(cmd.Path)
	if err != nil {
		return ""
	}

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		return strings.TrimSpace(stderr.String())
	}

	return ""
}

func applyInsert(content []byte, ins InsertOp, index int) editResult {
	result := editResult{
		Index:        index,
		Success:      false,
		Matches:      nil,
		Error:        "",
		OldText:      "",
		NewText:      ins.Text,
		LinesChanged: 0,
	}
	if ins.Line < 1 {
		result.Error = "insert line must be >= 1"

		return result
	}

	if ins.Text == "" {
		result.Error = "insert text is empty"

		return result
	}

	lineNum := 1

	ofs := 0
	for ofs < len(content) {
		if lineNum == ins.Line {
			break
		}

		if content[ofs] == '\n' {
			lineNum++
		}

		ofs++
	}

	text := ins.Text
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	result.Success = true
	result.Matches = []MatchLoc{{Offset: ofs, EndOffset: ofs,
		LineStart: ins.Line, LineEnd: ins.Line, Strategy: strategyInsert}}
	result.OldText = ""
	result.NewText = text
	result.LinesChanged = 1

	return result
}

func findAndReplace(content []byte, editOp EditOp, index int) editResult {
	result := editResult{
		Index:        index,
		Success:      false,
		Matches:      nil,
		Error:        "",
		OldText:      editOp.OldText,
		NewText:      editOp.NewText,
		LinesChanged: 0,
	}
	if editOp.OldText == "" {
		result.Error = "old_text is empty"

		return result
	}

	locs := findAllMatches(content, editOp.OldText)

	switch {
	case len(locs) == 0:
		result.Error = noMatchError(content, editOp.OldText)
	case len(locs) == 1:
		result = singleMatchResult(result, content, editOp, locs)
	default:
		result = multiMatchResult(result, content, editOp, locs)
	}

	return result
}

func noMatchError(content []byte, oldText string) string {
	nearest := findNearest(content, oldText)

	if len(content) == 0 || len(strings.TrimSpace(string(content))) == 0 {
		return "NO_MATCH: not found (file is empty or no meaningful content)"
	}

	return fmt.Sprintf("NO_MATCH: not found. Nearest: line %d (%q)", nearest.Line, nearest.Preview)
}

func singleMatchResult(result editResult, content []byte, editOp EditOp, locs []MatchLoc) editResult {
	if locs[0].Strategy == strategySubstringFuzzy {
		matchedLen := locs[0].EndOffset - locs[0].Offset

		coverage := float64(matchedLen) / float64(len(editOp.OldText))
		if coverage < minCoverage {
			n := findNearest(content, editOp.OldText)
			result.Error = fmt.Sprintf(
				"NO_MATCH: substring_fuzzy only matched %d/%d chars (%.0f%%). "+
					"Add more context. Nearest: line %d (%q)",
				matchedLen, len(editOp.OldText), percentScale*coverage, n.Line, n.Preview)
			result.Matches = locs

			return result
		}
	}

	result.Success = true
	result.Matches = locs
	result.LinesChanged = locs[0].LineEnd - locs[0].LineStart + 1

	return result
}

func multiMatchResult(result editResult, content []byte, editOp EditOp, locs []MatchLoc) editResult {
	if locs[0].Strategy != strategyExact && editOp.ReplaceAll {
		result.Error = fmt.Sprintf(
			"replace_all requires exact match (got %s). "+
				"Remove replace_all or add context for exact match.",
			locs[0].Strategy)
		result.Matches = locs

		return result
	}

	if editOp.ReplaceAll {
		result.Success = true
		result.Matches = locs
		result.LinesChanged = locs[len(locs)-1].LineEnd - locs[0].LineStart + 1
	} else {
		result.Error = ambiguousError(content, locs)
		result.Matches = locs
	}

	return result
}

func ambiguousError(content []byte, locs []MatchLoc) string {
	var buf strings.Builder

	show := locs
	omitted := 0

	if len(locs) > maxAmbiguousShown {
		show = locs[:maxAmbiguousShown]
		omitted = len(locs) - maxAmbiguousShown
	}

	fmt.Fprintf(&buf, "AMBIGUOUS_MATCH: %d locations. Add more context to disambiguate:\n", len(locs))

	for _, l := range show {
		ctx := extractContext(content, l, 1)
		fmt.Fprintf(&buf, "  L%d: %s\n", l.LineStart, ctx)
	}

	if omitted > 0 {
		fmt.Fprintf(&buf, "  ... and %d more locations\n", omitted)
	}

	return buf.String()
}

func extractContext(content []byte, loc MatchLoc, radius int) string {
	lines := strings.Split(string(content), "\n")
	start := loc.LineStart - 1 - radius
	start = max(start, 0)

	end := loc.LineEnd - 1 + radius
	if end >= len(lines) {
		end = len(lines) - 1
	}

	var buf strings.Builder

	for idx := start; idx <= end; idx++ {
		line := lines[idx]
		if len(line) > maxContextLineLen {
			line = line[:maxContextLineLen] + "..."
		}

		buf.WriteString(strings.TrimRight(line, " \t\r"))

		if idx < end {
			buf.WriteString(" ↵ ")
		}
	}

	return buf.String()
}

// replacement is one text splice at a fixed offset of the original content.
type replacement struct {
	start int
	end   int
	text  string
}

// applyReplacement splices one edit's matches into content.
func applyReplacement(content []byte, result editResult) []byte {
	if !result.Success || len(result.Matches) == 0 {
		return content
	}

	return applyReplacements(content, resultReplacements(result))
}

func resultReplacements(result editResult) []replacement {
	reps := make([]replacement, 0, len(result.Matches))

	for _, loc := range result.Matches {
		reps = append(reps, replacement{start: loc.Offset, end: loc.EndOffset, text: result.NewText})
	}

	return reps
}

func applyResults(content []byte, results []editResult) []byte {
	_, current := applySuccessful(content, results)

	return current
}

func applySuccessful(content []byte, results []editResult) (int, []byte) {
	applied := 0
	reps := make([]replacement, 0, len(results))

	for _, r := range results {
		if !r.Success {
			continue
		}

		applied++
		reps = append(reps, resultReplacements(r)...)
	}

	return applied, applyReplacements(content, reps)
}

// applyReplacements splices every match in one bottom-up pass. All offsets were
// computed against content pre-edit, so per-result application makes the later
// matches of a replace_all stale as soon as another edit lands between them.
func applyReplacements(content []byte, reps []replacement) []byte {
	sort.SliceStable(reps, func(i, j int) bool { return reps[i].start > reps[j].start })

	kept := selectReplacements(reps, len(content))
	if len(kept) == 0 {
		return content
	}

	// Applied bottom-up: ascending order keeps same-offset splices in argument order.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].start < kept[j].start })

	size := len(content)
	for _, rep := range kept {
		size += len(rep.text) - (rep.end - rep.start)
	}

	out := make([]byte, 0, size)

	prev := 0
	for _, rep := range kept {
		out = append(out, content[prev:rep.start]...)
		out = append(out, rep.text...)

		prev = rep.end
	}

	return append(out, content[prev:]...)
}

// selectReplacements keeps the highest-offset non-overlapping subset of reps
// (sorted by descending start) and clamps ends to content length.
func selectReplacements(reps []replacement, contentLen int) []replacement {
	kept := make([]replacement, 0, len(reps))
	limit := contentLen + 1

	for _, rep := range reps {
		if rep.start < 0 || rep.end < rep.start || rep.end > limit {
			continue
		}

		if rep.end > contentLen {
			rep.end = contentLen
		}

		kept = append(kept, rep)
		limit = rep.start
	}

	return kept
}

func buildResponse(path string, original, formatted, current []byte, results []editResult, dryRun bool,
	diffCtx int, skipValidate, showDiff bool, echoLines int, failFastBlocked, terse bool) (*server.ToolCallResult, error) {
	successCount, failCount := countResults(results)

	if terse {
		return terseResponse(path, current, results, dryRun, skipValidate, successCount, failCount)
	}

	if successCount == 0 || failFastBlocked {
		return noChangeResponse(results, failFastBlocked)
	}

	var buf bytes.Buffer

	writeHeader(&buf, path, original, formatted, successCount, len(results))

	if dryRun {
		writeEditTable(&buf, results, true)
		buf.WriteString("\n[DRY RUN — file not modified]")

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: contentTypeText, Text: buf.String()}},
			IsError: false,
		}, nil
	}

	writeEditTable(&buf, results, false)
	writeLowConfidence(&buf, results)

	if showDiff {
		writeDiffBlock(&buf, formatted, current, path, diffCtx)
	}

	if echoLines > 0 {
		writeEchoBlock(&buf, current, path, results, echoLines)
	}

	err := atomicWrite(path, current)
	if err != nil {
		fmt.Fprintf(&buf, "\nWARNING: write failed: %v", err)

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: contentTypeText, Text: buf.String()}},
			IsError: true,
		}, nil
	}

	if result := runValidate(path); result != "" && !skipValidate {
		buf.WriteString("\n\n\u26a0\ufe0f Validation: " + result)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: contentTypeText, Text: buf.String()}},
		IsError: false,
	}, nil
}

func terseResponse(path string, current []byte, results []editResult, dryRun, skipValidate bool,
	successCount, failCount int) (*server.ToolCallResult, error) {
	if failCount > 0 {
		msg := fmt.Sprintf("[FAIL] %d/%d edits: %s", successCount, len(results), joinErrors(results))

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: contentTypeText, Text: msg}},
			IsError: false,
		}, nil
	}

	if !dryRun {
		err := atomicWrite(path, current)
		if err != nil {
			msg := fmt.Sprintf("[FAIL] write error: %v", err)

			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: contentTypeText, Text: msg}},
				IsError: true,
			}, nil
		}

		if !skipValidate {
			if result := runValidate(path); result != "" {
				msg := fmt.Sprintf("[OK] %d/%d applied — validation: %s", successCount, len(results), result)

				return &server.ToolCallResult{
					Content: []server.ToolCallContent{{Type: contentTypeText, Text: msg}},
					IsError: false,
				}, nil
			}
		}
	}

	msg := fmt.Sprintf("[OK] %d/%d edits applied to %s", successCount, len(results), path)
	if dryRun {
		msg = fmt.Sprintf("[DRY RUN] %d/%d edits would apply to %s", successCount, len(results), path)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: contentTypeText, Text: msg}},
		IsError: false,
	}, nil
}

func noChangeResponse(results []editResult, failFastBlocked bool) (*server.ToolCallResult, error) {
	var buf bytes.Buffer

	if failFastBlocked {
		buf.WriteString("[BLOCKED] all edits\n")

		for _, r := range results {
			if r.Success {
				fmt.Fprintf(&buf, "Edit %d: would have matched (%s, L%d-%d)\n",
					r.Index, r.Matches[0].Strategy, r.Matches[0].LineStart, r.Matches[0].LineEnd)
			} else {
				fmt.Fprintf(&buf, "Edit %d: %s\n", r.Index, r.Error)
			}
		}
	} else {
		buf.WriteString("No edits were applied.\n")

		for _, r := range results {
			fmt.Fprintf(&buf, "Edit %d: %s\n", r.Index, r.Error)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: contentTypeText, Text: buf.String()}},
		IsError: false,
	}, nil
}

func countResults(results []editResult) (int, int) {
	successCount, failCount := 0, 0

	for _, r := range results {
		if r.Success {
			successCount++
		} else {
			failCount++
		}
	}

	return successCount, failCount
}

func joinErrors(results []editResult) string {
	var errs []string

	for _, r := range results {
		if !r.Success {
			errs = append(errs, r.Error)
		}
	}

	return strings.Join(errs, "; ")
}

func writeHeader(buf *bytes.Buffer, path string, original, formatted []byte, successCount, total int) {
	fmt.Fprintf(buf, "%s — %d/%d", server.RelPath(path), successCount, total)

	if !bytes.Equal(original, formatted) {
		buf.WriteString(" (pre-formatted)")
	}

	buf.WriteString("\n")
}

func writeEditTable(buf *bytes.Buffer, results []editResult, dryRun bool) {
	for _, r := range results {
		writeEditRow(buf, r, dryRun)
	}
}

func writeEditRow(buf *bytes.Buffer, result editResult, dryRun bool) {
	editIndex := result.Index

	if !result.Success {
		fmt.Fprintf(buf, "- Edit %d: \u274c %s\n", editIndex, result.Error)

		return
	}

	loc := result.Matches[0]
	tier := confTier(loc.Strategy)

	switch {
	case loc.Strategy == strategyInsert && loc.LineStart >= eofLine:
		fmt.Fprintf(buf, "- Edit %d: ✓ insert EOF\n", editIndex)
	case loc.Strategy == strategyInsert:
		fmt.Fprintf(buf, "- Edit %d: ✓ insert L%d\n", editIndex, loc.LineStart)
	case len(result.Matches) > 1:
		fmt.Fprintf(buf, "- Edit %d: ✓ %d×%s %s\n", editIndex, len(result.Matches), loc.Strategy, tier)
	case dryRun:
		fmt.Fprintf(buf, "- Edit %d: ✓ L%d %s %s\n", editIndex, loc.LineStart, loc.Strategy, tier)
	default:
		fmt.Fprintf(buf, "- Edit %d: ✓ L%d-%d %s %s\n", editIndex, loc.LineStart, loc.LineEnd, loc.Strategy, tier)
	}
}

func writeLowConfidence(buf *bytes.Buffer, results []editResult) {
	for _, r := range results {
		if r.Success && len(r.Matches) > 0 && r.Matches[0].Strategy == strategySubstringFuzzy {
			buf.WriteString("\n\u26a0\ufe0f LOW_CONFIDENCE: one or more edits matched via substring_fuzzy ")
			buf.WriteString("\u2014 verify the diff carefully.\n")

			break
		}
	}
}

func writeDiffBlock(buf *bytes.Buffer, formatted, current []byte, path string, diffCtx int) {
	diff := unifiedDiff(formatted, current, path, diffCtx)

	buf.WriteString("\n```diff\n")
	buf.WriteString(diff)
	buf.WriteString("```")
}

func writeEchoBlock(buf *bytes.Buffer, current []byte, path string, results []editResult, echoLines int) {
	firstLine := firstChangedLine(results)
	lines := strings.Split(string(current), "\n")
	start := max(0, firstLine-1-echoLines)
	end := min(len(lines)-1, firstLine-1+echoLines)

	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		ext = "txt"
	}

	fmt.Fprintf(buf, "\n**Result (L%d\u00b1%d):**\n```%s\n", firstLine, echoLines, ext)

	for i := start; i <= end; i++ {
		fmt.Fprintf(buf, "%4d: %s\n", i+1, lines[i])
	}

	buf.WriteString("```")
}

func firstChangedLine(results []editResult) int {
	for _, r := range results {
		if r.Success && len(r.Matches) > 0 && r.Matches[0].LineStart < eofLine {
			return r.Matches[0].LineStart
		}
	}

	return 1
}

// atomicWrite writes via a temp file then renames, preserving the existing file mode.
func atomicWrite(path string, content []byte) error {
	tmpPath := path + ".tmp"
	_ = os.Remove(tmpPath)

	mode := defaultFileMode

	info, err := os.Stat(path)
	if err == nil {
		mode = info.Mode()
	}

	// #nosec G703 -- paths bounds-checked by server
	err = os.WriteFile(tmpPath, content, mode)
	if err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	err = os.Rename(tmpPath, path)
	if err != nil {
		// #nosec G703 -- paths bounds-checked by server
		fallbackErr := os.WriteFile(path, content, mode)
		if fallbackErr != nil {
			return fmt.Errorf("rename %s: %w", tmpPath, errors.Join(err, fallbackErr))
		}
	}

	return nil
}

// #15: confidence tier annotation.
func confTier(s string) string {
	switch s {
	case strategyExact:
		return "\u2713\u2713\u2713"
	case strategyWhitespaceFuzzy:
		return "\u2713\u2713"
	case strategyLineFuzzy:
		return "\u2713\u2713"
	case strategySubstringFuzzy:
		return "\u2713"
	case strategyInsert:
		return "(new)"
	default:
		return ""
	}
}
