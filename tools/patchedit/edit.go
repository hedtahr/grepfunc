package patchedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

func handleEditFile(raw json.RawMessage) (*server.ToolCallResult, error) {
	var args EditFileArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if args.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	args.Path = server.ResolvePath(args.Path)
	if err := server.CheckBanned(args.Path); err != nil {
		return nil, err
	}
	server.SetLastPath(args.Path)

	// insert_file: read file and treat as insert op
	if args.InsertFile != "" {
		insertPath := server.ResolvePath(args.InsertFile)
		if err := server.CheckBanned(insertPath); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(insertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read insert_file: %v", err)
		}
		line := args.InsertLine
		line = max(line, 1)
		args.Inserts = append(args.Inserts, InsertOp{
			Line: line,
			Text: string(data),
		})
	}

	if len(args.Edits) == 0 && len(args.Inserts) == 0 && args.AppendText == "" {
		return nil, fmt.Errorf("at least one edit, insert, or append_text is required")
	}

	// #7: 2MB size guard
	if fi, err := os.Stat(args.Path); err == nil && fi.Size() > 2*1024*1024 {
		return nil, fmt.Errorf("file too large (%d bytes). Max 2MB for patch_file; use batch_patch with path filter or edit manually.", fi.Size())
	}

	original, err := os.ReadFile(args.Path)
	if err != nil {
		if os.IsNotExist(err) && args.CreateIfMissing {
			original = nil
		} else {
			return nil, fmt.Errorf("failed to read file: %v", err)
		}
	}

	// #12: default echo_lines to 3 unless suppressed
	if args.EchoLines == 0 && !args.NoDiff {
		// Check if explicitly set to 0 in raw JSON
		var rawMap map[string]any
		if json.Unmarshal(raw, &rawMap) == nil {
			if _, ok := rawMap["echo_lines"]; !ok {
				args.EchoLines = 3
			}
		}
	}

	// #1: auto-number indices when all default (0) and multiple edits exist
	allDefaultIdx := true
	for _, e := range args.Edits {
		if e.Index != 0 {
			allDefaultIdx = false
			break
		}
	}
	if allDefaultIdx && len(args.Edits) >= 1 {
		for i := range args.Edits {
			args.Edits[i].Index = i + 1
		}
	}
	allDefaultIns := true
	for _, ins := range args.Inserts {
		if ins.Index != 0 {
			allDefaultIns = false
			break
		}
	}
	if allDefaultIns && len(args.Inserts) >= 1 {
		for i := range args.Inserts {
			args.Inserts[i].Index = i + len(args.Edits) + 1
		}
	}

	content := original
	ext := filepath.Ext(args.Path)
	if formatted, didFmt, _ := preFormat(args.Path, content, ext); didFmt {
		content = formatted
	}

	if args.AppendText != "" {
		sep := ""
		if len(content) > 0 && content[len(content)-1] != '\n' {
			sep = "\n"
		}
		args.Inserts = append(args.Inserts, InsertOp{
			Line:  1 << 30,
			Text:  sep + args.AppendText,
			Index: len(args.Edits) + len(args.Inserts) + 1,
		})
	}

	results := computeResults(content, args.Inserts, args.Edits)

	// #14: auto no_diff when >3 total ops
	showDiff := !args.NoDiff
	if showDiff && len(args.Edits)+len(args.Inserts) > 3 {
		showDiff = false
	}

	if args.FailFast {
		for _, r := range results {
			if !r.Success {
				// #2: mark as fail_fast blocked
				return buildResponse(args.Path, original, content, content, results, true, args.DiffContext, args.SkipValidate, showDiff, args.EchoLines, true, args.Terse)
			}
		}
	}

	toApply := make([]editResult, 0, len(results))
	for _, r := range results {
		if r.Success {
			toApply = append(toApply, r)
		}
	}
	sort.Slice(toApply, func(i, j int) bool {
		oi, oj := 0, 0
		if len(toApply[i].Matches) > 0 {
			oi = toApply[i].Matches[0].Offset
		}
		if len(toApply[j].Matches) > 0 {
			oj = toApply[j].Matches[0].Offset
		}
		return oi > oj
	})
	current := content
	for _, r := range toApply {
		current = applyReplacement(current, r)
	}

	return buildResponse(args.Path, original, content, current, results, args.DryRun, args.DiffContext, args.SkipValidate, showDiff, args.EchoLines, false, args.Terse)
}

func computeResults(content []byte, inserts []InsertOp, edits []EditOp) []editResult {
	var results []editResult

	sorted := make([]InsertOp, len(inserts))
	copy(sorted, inserts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Line > sorted[j].Line })

	for _, ins := range sorted {
		r := applyInsert(content, ins, ins.Index)
		results = append(results, r)
	}

	for _, op := range edits {
		r := findAndReplace(content, op, op.Index)
		results = append(results, r)
	}

	sort.Slice(results, func(i, j int) bool {
		iOrder := results[i].Index
		jOrder := results[j].Index
		if len(results[i].Matches) > 0 && results[i].Matches[0].Strategy == "insert" {
			iOrder -= 10000
		}
		if len(results[j].Matches) > 0 && results[j].Matches[0].Strategy == "insert" {
			jOrder -= 10000
		}
		return iOrder < jOrder
	})

	return results
}

func runValidate(path string) string {
	ext := filepath.Ext(path)
	var cmd *exec.Cmd
	switch ext {
	case ".go":
		cmd = exec.Command("go", "vet", path)
	case ".rs":
		cmd = exec.Command("rustc", "--edition", "2021", "--emit=metadata", path)
	case ".py":
		cmd = exec.Command("python3", "-c", "import ast, sys; ast.parse(open(sys.argv[1]).read())", path)
	default:
		return ""
	}
	if cmd == nil {
		return ""
	}
	if _, err := exec.LookPath(cmd.Path); err != nil {
		return ""
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stderr.String())
	}
	return ""
}

func applyInsert(content []byte, ins InsertOp, index int) editResult {
	r := editResult{Index: index, NewText: ins.Text}
	if ins.Line < 1 {
		r.Error = "insert line must be >= 1"
		return r
	}
	if ins.Text == "" {
		r.Error = "insert text is empty"
		return r
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
	r.Success = true
	r.Matches = []MatchLoc{{Offset: ofs, EndOffset: ofs, LineStart: ins.Line, LineEnd: ins.Line, Strategy: "insert"}}
	r.OldText = ""
	r.NewText = text
	r.LinesChanged = 1
	return r
}

func findAndReplace(content []byte, op EditOp, index int) editResult {
	r := editResult{Index: index, OldText: op.OldText, NewText: op.NewText}
	if op.OldText == "" {
		r.Error = "old_text is empty"
		return r
	}

	locs := findAllMatches(content, op.OldText)
	switch {
	case len(locs) == 0:
		n := findNearest(content, op.OldText)
		if len(content) == 0 || len(strings.TrimSpace(string(content))) == 0 {
			r.Error = "NO_MATCH: not found (file is empty or no meaningful content)"
		} else {
			r.Error = fmt.Sprintf("NO_MATCH: not found. Nearest: line %d (%q)", n.Line, n.Preview)
		}
	case len(locs) == 1:
		if locs[0].Strategy == "substring_fuzzy" {
			matchedLen := locs[0].EndOffset - locs[0].Offset
			coverage := float64(matchedLen) / float64(len(op.OldText))
			if coverage < 0.8 {
				n := findNearest(content, op.OldText)
				r.Error = fmt.Sprintf("NO_MATCH: substring_fuzzy only matched %d/%d chars (%.0f%%). Add more context. Nearest: line %d (%q)",
					matchedLen, len(op.OldText), 100*coverage, n.Line, n.Preview)
				r.Matches = locs
				return r
			}
		}
		r.Success = true
		r.Matches = locs
		r.LinesChanged = locs[0].LineEnd - locs[0].LineStart + 1
	default:
		if locs[0].Strategy != "exact" && op.ReplaceAll {
			r.Error = fmt.Sprintf("replace_all requires exact match (got %s). Remove replace_all or add context for exact match.", locs[0].Strategy)
			r.Matches = locs
			return r
		}
		if op.ReplaceAll {
			r.Success = true
			r.Matches = locs
			r.LinesChanged = locs[len(locs)-1].LineEnd - locs[0].LineStart + 1
		} else {
			var b strings.Builder
			show := locs
			omitted := 0
			if len(locs) > 10 {
				show = locs[:10]
				omitted = len(locs) - 10
			}
			fmt.Fprintf(&b, "AMBIGUOUS_MATCH: %d locations. Add more context to disambiguate:\n", len(locs))
			for _, l := range show {
				ctx := extractContext(content, l, 1)
				fmt.Fprintf(&b, "  L%d: %s\n", l.LineStart, ctx)
			}
			if omitted > 0 {
				fmt.Fprintf(&b, "  ... and %d more locations\n", omitted)
			}
			r.Error = b.String()
			r.Matches = locs
		}
	}
	return r
}

func extractContext(content []byte, loc MatchLoc, radius int) string {
	lines := strings.Split(string(content), "\n")
	start := loc.LineStart - 1 - radius
	start = max(start, 0)
	end := loc.LineEnd - 1 + radius
	if end >= len(lines) {
		end = len(lines) - 1
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		line := lines[i]
		if len(line) > 100 {
			line = line[:100] + "..."
		}
		b.WriteString(strings.TrimRight(line, " \t\r"))
		if i < end {
			b.WriteString(" ↵ ")
		}
	}
	return b.String()
}

func applyReplacement(content []byte, r editResult) []byte {
	if !r.Success || len(r.Matches) == 0 {
		return content
	}
	// Apply in reverse order so earlier offsets stay valid (handles replace_all multi-match)
	out := content
	for i := len(r.Matches) - 1; i >= 0; i-- {
		loc := r.Matches[i]
		oldLen := loc.EndOffset - loc.Offset
		if oldLen < 0 || loc.Offset > len(out) {
			continue
		}
		if loc.Offset+oldLen > len(out) {
			oldLen = len(out) - loc.Offset
		}
		var buf bytes.Buffer
		buf.Write(out[:loc.Offset])
		buf.WriteString(r.NewText)
		buf.Write(out[loc.Offset+oldLen:])
		out = buf.Bytes()
	}
	return out
}

func buildResponse(path string, original, formatted, current []byte, results []editResult, dryRun bool, diffCtx int, skipValidate, showDiff bool, echoLines int, failFastBlocked, terse bool) (*server.ToolCallResult, error) {
	var buf bytes.Buffer
	successCount := 0
	failCount := 0
	for _, r := range results {
		if r.Success {
			successCount++
		} else {
			failCount++
		}
	}

	if terse {
		if failCount > 0 {
			var errs []string
			for _, r := range results {
				if !r.Success {
					errs = append(errs, r.Error)
				}
			}
			return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("[FAIL] %d/%d edits: %s", successCount, len(results), strings.Join(errs, "; "))}}}, nil
		}
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("[OK] %d/%d edits applied to %s", successCount, len(results), path)}}}, nil
	}

	anyChange := successCount > 0

	// #2: fail_fast blocked overrides success count
	if failFastBlocked {
		anyChange = false
		successCount = 0
	}

	if !anyChange {
		if failFastBlocked {
			buf.WriteString("**fail_fast blocked all edits.**\n")
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
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
	}

	fmt.Fprintf(&buf, "**%s** — %d/%d edit(s) applied", path, successCount, len(results))
	if !bytes.Equal(original, formatted) {
		buf.WriteString(" (pre-formatted)")
	}
	buf.WriteString("\n\n")

	// #13: terse dry_run table
	if dryRun {
		for _, r := range results {
			n := r.Index
			if r.Success {
				switch {
				case r.Matches[0].Strategy == "insert" && r.Matches[0].LineStart >= 1<<30:
					fmt.Fprintf(&buf, "- Edit %d: \u2713 insert EOF\n", n)
				case r.Matches[0].Strategy == "insert":
					fmt.Fprintf(&buf, "- Edit %d: \u2713 insert L%d\n", n, r.Matches[0].LineStart)
				case len(r.Matches) > 1:
					fmt.Fprintf(&buf, "- Edit %d: \u2713 %d\u00d7 %s %s\n", n, len(r.Matches), r.Matches[0].Strategy, confTier(r.Matches[0].Strategy))
				default:
					fmt.Fprintf(&buf, "- Edit %d: \u2713 L%d %s %s\n", n, r.Matches[0].LineStart, r.Matches[0].Strategy, confTier(r.Matches[0].Strategy))
				}
			} else {
				fmt.Fprintf(&buf, "- Edit %d: \u274c %s\n", n, r.Error)
			}
		}
		buf.WriteString("\n[DRY RUN — file not modified]")
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
	}

	for _, r := range results {
		n := r.Index
		if r.Success {
			switch {
			case r.Matches[0].Strategy == "insert" && r.Matches[0].LineStart >= 1<<30:
				fmt.Fprintf(&buf, "- Edit %d: **matched** (insert, EOF)\n", n)
			case r.Matches[0].Strategy == "insert":
				fmt.Fprintf(&buf, "- Edit %d: **matched** (insert, L%d)\n", n, r.Matches[0].LineStart)
			case len(r.Matches) > 1:
				fmt.Fprintf(&buf, "- Edit %d: **matched** (%d\u00d7%s %s)\n",
					n, len(r.Matches), r.Matches[0].Strategy, confTier(r.Matches[0].Strategy))
			default:
				fmt.Fprintf(&buf, "- Edit %d: **matched** (%s %s, L%d-%d)\n",
					n, r.Matches[0].Strategy, confTier(r.Matches[0].Strategy), r.Matches[0].LineStart, r.Matches[0].LineEnd)
			}
		} else {
			fmt.Fprintf(&buf, "- Edit %d: \u274c %s\n", n, r.Error)
		}
	}

	for _, r := range results {
		if r.Success && len(r.Matches) > 0 && r.Matches[0].Strategy == "substring_fuzzy" {
			buf.WriteString("\n\u26a0\ufe0f LOW_CONFIDENCE: one or more edits matched via substring_fuzzy \u2014 verify the diff carefully.\n")
			break
		}
	}

	if showDiff {
		diff := unifiedDiff(formatted, current, path, diffCtx)
		buf.WriteString("\n```diff\n")
		buf.WriteString(diff)
		buf.WriteString("```")
	}

	if echoLines > 0 && anyChange {
		firstLine := 1
		for _, r := range results {
			if r.Success && len(r.Matches) > 0 && r.Matches[0].LineStart < 1<<30 {
				firstLine = r.Matches[0].LineStart
				break
			}
		}
		lines := strings.Split(string(current), "\n")
		start := max(0, firstLine-1-echoLines)
		end := min(len(lines)-1, firstLine-1+echoLines)
		ext := strings.TrimPrefix(filepath.Ext(path), ".")
		if ext == "" {
			ext = "txt"
		}
		fmt.Fprintf(&buf, "\n**Result (L%d\u00b1%d):**\n```%s\n", firstLine, echoLines, ext)
		for i := start; i <= end; i++ {
			fmt.Fprintf(&buf, "%4d: %s\n", i+1, lines[i])
		}
		buf.WriteString("```")
	}

	if dryRun {
		buf.WriteString("\n[DRY RUN — file not modified]")
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
	}

	// atomic write via tmp + rename
	tmpPath := path + ".tmp"
	os.Remove(tmpPath)
	if err := os.WriteFile(tmpPath, current, 0644); err != nil {
		fmt.Fprintf(&buf, "\nWARNING: write failed: %v", err)
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
			IsError: true,
		}, nil
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// fallback: direct write
		if err2 := os.WriteFile(path, current, 0644); err2 != nil {
			fmt.Fprintf(&buf, "\nWARNING: write failed: %v", err2)
			return &server.ToolCallResult{
				Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
				IsError: true,
			}, nil
		}
	}
	if result := runValidate(path); result != "" && !skipValidate {
		buf.WriteString("\n\n\u26a0\ufe0f Validation: " + result)
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
}

// #15: confidence tier annotation
func confTier(s string) string {
	switch s {
	case "exact":
		return "\u2713\u2713\u2713"
	case "whitespace_fuzzy":
		return "\u2713\u2713"
	case "line_fuzzy":
		return "\u2713\u2713"
	case "substring_fuzzy":
		return "\u2713"
	case "insert":
		return "(new)"
	default:
		return ""
	}
}
