// Package bookmark provides the bookmark MCP tool: a session-handoff pointer to
// the doc sections where current work is described.
package bookmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hedtahr/grepfunc/server"
)

// Tool describes the bookmark MCP tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "bookmark",
	Description: "Bookmark current state/progress of work: the doc sections recording what is being " +
		"done, where it stands, what is next. Call with no args at session start: line ranges + " +
		"git drift; moved sections repaired, missing dropped. set: path + refs \"label=heading\" " +
		"(max 4). clear: forget.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        typeString,
				Description: "Doc path, project-relative; set only.",
				Items:       nil,
			},
			"refs": {
				Type:        typeArray,
				Description: "\"label=heading\" strings, max 4, replaces all refs.",
				Items:       &server.Property{Type: typeString},
			},
			"clear": {
				Type:        typeBoolean,
				Description: "Remove the bookmark (doc untouched).",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

const (
	typeString  = "string"
	typeBoolean = "boolean"
	typeArray   = "array"
	typeText    = "text"

	maxRefs     = 4
	markCap     = 60
	labelCap    = 24
	pathScanCap = 40
	evidenceCap = 3
	commitCap   = 40
	dirtyCap    = 200

	bookmarkDir  = ".llm"
	bookmarkFile = "bookmark.json"

	dirMode  = 0o700
	fileMode = 0o600
)

var errPathRequired = errors.New("path is required: set needs the doc path")

// pathTokenRe finds path-shaped tokens in doc sections (e.g. tools/grepfunc/walk.go).
//
//nolint:gochecknoglobals // compiled once
var pathTokenRe = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_./-]{3,}`)

// wordRe finds words used for commit-message keyword matching.
//
//nolint:gochecknoglobals // compiled once
var wordRe = regexp.MustCompile(`[A-Za-z]{4,}`)

type ref struct {
	Label   string `json:"label"`
	Heading string `json:"heading"`
	Mark    string `json:"mark"`
}

type bookmark struct {
	Path     string   `json:"path"`
	Refs     []ref    `json:"refs"`
	Branch   string   `json:"branch,omitempty"`
	SHA      string   `json:"sha,omitempty"`
	Dirty    []string `json:"dirty,omitempty"`
	At       string   `json:"at,omitempty"`
	Verified string   `json:"verified,omitempty"`
}

type args struct {
	Path  string   `json:"path"`
	Refs  []string `json:"refs"`
	Clear bool     `json:"clear"`
}

// span is a resolved ref: label plus inclusive line indexes into the doc.
type span struct {
	label string
	start int
	end   int
}

// storePath returns the bookmark file path for a project directory. Overridable for tests.
var storePath = func(projectDir string) (string, error) { //nolint:gochecknoglobals // test override point
	root := server.FindProjectRoot(projectDir)
	if root == "" {
		root = projectDir
	}

	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err == nil {
			root = abs
		}
	}

	return filepath.Join(root, bookmarkDir, bookmarkFile), nil
}

// Handle serves the bookmark MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	storeFile, err := storePath(server.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot determine bookmark path: %w", err)
	}

	switch {
	case req.Clear:
		return clear(storeFile)
	case len(req.Refs) > 0:
		return set(storeFile, req)
	default:
		return read(storeFile)
	}
}

// set validates the refs against the doc and stores the bookmark.
// A ref that cannot resolve at write time is refused: no dead-on-arrival anchors.
func set(storeFile string, req args) (*server.ToolCallResult, error) {
	if req.Path == "" {
		return nil, errPathRequired
	}

	abs := server.ResolvePath(req.Path)

	err := server.CheckBounds(abs)
	if err != nil {
		return nil, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(abs)
	if err != nil {
		return nil, fmt.Errorf("check banned: %w", err)
	}

	lines, err := readDoc(abs)
	if err != nil {
		return nil, fmt.Errorf("bookmark not saved: doc %s: %w", server.RelPath(abs), err)
	}

	prep := buildRefs(lines, req.Refs)
	if len(prep.refs) == 0 {
		return nil, fmt.Errorf("bookmark not saved: %s", strings.Join(prep.problems, "; "))
	}

	book := bookmark{
		Path:     server.RelPath(abs),
		Refs:     prep.refs,
		At:       nowStamp(),
		Verified: nowStamp(),
	}
	book.Branch, book.SHA, book.Dirty = gitSnapshot(server.ProjectRoot)

	err = saveStore(storeFile, book)
	if err != nil {
		return nil, err
	}

	server.EnsureGitignore(server.ProjectRoot)

	var parts []string

	for _, r := range prep.refs {
		start, end := sectionBounds(lines, findHeadings(lines, r.Heading)[0])
		parts = append(parts, fmt.Sprintf("%s→L%d-%d", r.Label, start+1, end+1))
	}

	line := fmt.Sprintf("Bookmarked %s: %s", book.Path, strings.Join(parts, ", "))
	if len(prep.notes) > 0 {
		line += "\n" + strings.Join(prep.notes, "; ")
	}

	return textResult(line), nil
}

// prepared is the outcome of resolving raw ref specs: the refs that will be
// stored, notes about what was adjusted, and problems for refs that failed.
type prepared struct {
	refs     []ref
	notes    []string
	problems []string
}

// buildRefs resolves raw specs without refusing when a deterministic choice
// exists: auto labels for bare specs, first-wins for duplicates, first of
// repeated headings, truncation over rejection. Only unresolvable refs fail.
func buildRefs(lines []string, specs []string) prepared {
	var p prepared

	labels := map[string]bool{}
	headings := map[string]bool{}

	var (
		truncated     int
		droppedLabels int
		droppedHeads  int
		ciMatched     int
		repeated      int
		emptySections int
		overCap       int
	)

	for _, spec := range specs {
		label, heading, hasEq := strings.Cut(spec, "=")
		label, heading = strings.TrimSpace(label), strings.TrimSpace(heading)

		if !hasEq {
			heading = label
			label = ""
		}

		if heading == "" {
			p.problems = append(p.problems, fmt.Sprintf("%q: want \"label=heading\"", spec))

			continue
		}

		if label == "" {
			label = autoLabel(heading)
		}

		if runes := []rune(label); len(runes) > labelCap {
			label = strings.TrimRight(string(runes[:labelCap]), "-")
			truncated++

			if label == "" {
				label = autoLabel(heading)
			}
		}

		key := strings.ToLower(label)
		if labels[key] {
			droppedLabels++

			continue
		}

		idx, actual, matches, ci := resolveHeading(lines, heading)
		if idx < 0 {
			msg := fmt.Sprintf("%s: heading %q not found", label, heading)
			if sug := suggestHeadings(lines, heading, 2); len(sug) > 0 {
				msg += fmt.Sprintf(" (nearest: %s)", strings.Join(sug, ", "))
			}

			p.problems = append(p.problems, msg)

			continue
		}

		headKey := strings.ToLower(headingText(actual))
		if headings[headKey] {
			droppedHeads++

			continue
		}

		if len(p.refs) >= maxRefs {
			overCap++

			continue
		}

		if ci {
			ciMatched++
		}

		if matches > 1 {
			repeated++
		}

		mark := sectionMark(lines, idx)
		if mark == "" {
			emptySections++
		}

		labels[key] = true
		headings[headKey] = true

		p.refs = append(p.refs, ref{Label: label, Heading: actual, Mark: mark})
	}

	if truncated > 0 {
		p.notes = append(p.notes, fmt.Sprintf("truncated %d label(s) to %d chars", truncated, labelCap))
	}

	if droppedLabels > 0 {
		p.notes = append(p.notes, fmt.Sprintf("dropped %d duplicate label(s)", droppedLabels))
	}

	if droppedHeads > 0 {
		p.notes = append(p.notes, fmt.Sprintf("dropped %d duplicate heading(s)", droppedHeads))
	}

	if overCap > 0 {
		p.notes = append(p.notes, fmt.Sprintf("kept first %d refs, dropped %d", maxRefs, overCap))
	}

	if ciMatched > 0 {
		p.notes = append(p.notes, fmt.Sprintf("%d matched case-insensitively", ciMatched))
	}

	if repeated > 0 {
		p.notes = append(p.notes, fmt.Sprintf("%d used the first of repeated headings", repeated))
	}

	if emptySections > 0 {
		p.notes = append(p.notes, fmt.Sprintf("%d empty section(s): mark arms on first content", emptySections))
	}

	p.notes = append(p.notes, p.problems...)

	return p
}

// read resolves the stored refs against the current doc, repairs what moved,
// evicts what is gone, and reports freshness.
func read(storeFile string) (*server.ToolCallResult, error) {
	book, ok := loadStore(storeFile)
	if !ok {
		return textResult("No bookmark yet. set: path + refs \"label=heading\" (max 4)."), nil
	}

	abs := server.ResolvePath(book.Path)

	err := server.CheckBounds(abs)
	if err != nil {
		return cleared(storeFile, fmt.Sprintf("Bookmark cleared: doc %s is outside the project root.", book.Path))
	}

	err = server.CheckBanned(abs)
	if err != nil {
		return cleared(storeFile, fmt.Sprintf("Bookmark cleared: doc %s is a protected path.", book.Path))
	}

	lines, err := readDoc(abs)
	if err != nil {
		return cleared(storeFile, fmt.Sprintf("Bookmark cleared: doc %s is gone or unreadable.", book.Path))
	}

	prev := parseStamp(book.Verified)
	if prev.IsZero() {
		prev = parseStamp(book.At)
	}

	repairs := map[string]string{}

	var (
		kept    []ref
		spans   []span
		evicted []string
	)

	for _, r := range book.Refs {
		matches := findHeadings(lines, r.Heading)

		var idx int

		switch len(matches) {
		case 1:
			idx = matches[0]
		case 0:
			// Heading gone: repair via the unique mark, or evict.
			markLine := findMark(lines, r.Mark)
			if markLine < 0 {
				evicted = append(evicted, r.Label+" (heading and mark gone)")

				continue
			}

			kept = append(kept, r)

			start, end := sectionBounds(lines, markLine)
			spans = append(spans, span{label: r.Label, start: start, end: end})
			repairs[r.Label] = fmt.Sprintf("(repaired via mark → L%d-%d)", start+1, end+1)

			continue
		default:
			// Duplicate headings: the mark is the only tiebreaker.
			idx = uniqueMarkMatch(lines, matches, r.Mark)
			if idx < 0 {
				evicted = append(evicted, fmt.Sprintf("%s (%d identical headings; mark ambiguous)",
					r.Label, len(matches)))

				continue
			}
		}

		r.Mark = sectionMark(lines, idx) // silent refresh; empty disarms the mark
		kept = append(kept, r)

		start, end := sectionBounds(lines, idx)
		spans = append(spans, span{label: r.Label, start: start, end: end})
	}

	if len(kept) == 0 {
		return cleared(storeFile, "Bookmark cleared: no refs left ("+strings.Join(evicted, "; ")+").")
	}

	now := time.Now().UTC()
	book.Refs = kept
	book.Verified = now.Format(time.RFC3339Nano)

	err = saveStore(storeFile, book)
	if err != nil {
		return nil, err
	}

	return textResult(renderRead(book, abs, prev, lines, spans, repairs, evicted)), nil
}

// renderRead builds the verdict block: header, ref ranges, ranked evidence, evictions.
func renderRead(book bookmark, abs string, prev time.Time, lines []string,
	spans []span, repairs map[string]string, evicted []string,
) string {
	ageOld := time.Since(prev)

	nowBranch, nowSHA, nowDirty := gitSnapshot(server.ProjectRoot)
	branchChanged := nowBranch != "" && book.Branch != "" && nowBranch != book.Branch

	var (
		commits   []string
		changed   []string
		rewritten bool
	)

	if nowSHA != "" && book.SHA != "" && nowSHA != book.SHA {
		if isAncestor(server.ProjectRoot, book.SHA) {
			commits = commitsSince(server.ProjectRoot, book.SHA)
			changed = changedFiles(server.ProjectRoot, book.SHA)
		} else {
			rewritten = true
		}
	}

	scope := scopeMatches(server.ProjectRoot, lines, spans, nowDirty, changed)

	hits := countMentioning(commits, keywords(book))

	boxesDone, boxesTotal := countBoxes(lines, spans)

	edited := ""
	if fi, err := os.Stat(abs); err == nil && !prev.IsZero() && fi.ModTime().After(prev) {
		edited = ageString(time.Since(fi.ModTime()))
	}

	dAdded, dRemoved := dirtyDiff(book.Dirty, nowDirty)

	verdict := "IDLE"

	switch {
	case len(scope) > 0 || hits > 0 || edited != "":
		verdict = "PROGRESS"
	case len(commits) > 0 || dAdded+dRemoved > 0 || branchChanged || rewritten:
		verdict = "DRIFT-OUT"
	}

	var evidence []string

	if branchChanged {
		evidence = append(evidence, fmt.Sprintf("branch %s→%s", book.Branch, nowBranch))
	}

	if rewritten {
		evidence = append(evidence, "history rewritten since anchor")
	}

	if len(scope) > 0 {
		evidence = append(evidence, fmt.Sprintf("%d in scope: %s", len(scope), joinCap(scope, 3)))
	}

	if len(commits) > 0 {
		line := fmt.Sprintf("%s since %s", commitCount(len(commits)), shortSHA(book.SHA))
		if hits > 0 {
			line += fmt.Sprintf(" (%d mention the work)", hits)
		}

		evidence = append(evidence, line)
	} else if dAdded+dRemoved > 0 {
		evidence = append(evidence, fmt.Sprintf("working tree +%d/-%d files", dAdded, dRemoved))
	}

	if boxesTotal > 0 {
		evidence = append(evidence, fmt.Sprintf("boxes %d/%d done", boxesDone, boxesTotal))
	}

	if edited != "" {
		evidence = append(evidence, "doc edited "+edited+" ago")
	}

	if len(evidence) == 0 {
		evidence = append(evidence, "nothing moved in "+ageString(ageOld))
	}

	if len(evidence) > evidenceCap {
		evidence = evidence[:evidenceCap]
	}

	head := fmt.Sprintf("bookmark %s — %s", book.Path, refCount(len(book.Refs)))
	if nowBranch != "" && nowSHA != "" {
		head += fmt.Sprintf(", %s@%s", nowBranch, shortSHA(nowSHA))
	}

	head += ", verified just now"

	var buf strings.Builder

	buf.WriteString(head)
	buf.WriteByte('\n')

	for _, sp := range spans {
		suffix := ""
		if note, ok := repairs[sp.label]; ok {
			suffix = " " + note
		}

		fmt.Fprintf(&buf, "  %s → L%d-%d%s\n", sp.label, sp.start+1, sp.end+1, suffix)
	}

	fmt.Fprintf(&buf, "verdict: %s — %s\n", verdict, strings.Join(evidence, " · "))

	if len(evicted) > 0 {
		fmt.Fprintf(&buf, "evicted: %s\n", strings.Join(evicted, "; "))
	}

	return strings.TrimRight(buf.String(), "\n")
}

// clear removes the bookmark file; the doc is never touched.
func clear(storeFile string) (*server.ToolCallResult, error) {
	err := os.Remove(storeFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("clear bookmark: %w", err)
	}

	return textResult("Bookmark cleared. Doc untouched."), nil
}

// cleared removes the store and reports why.
func cleared(storeFile, msg string) (*server.ToolCallResult, error) {
	_ = os.Remove(storeFile)

	return textResult(msg), nil
}

// findHeadings returns every line index matching the heading text
// (leading '#'s ignored on both sides).
func findHeadings(lines []string, heading string) []int {
	want := headingText(heading)
	if want == "" {
		return nil
	}

	var idx []int

	for i, l := range lines {
		if headingText(l) == want {
			idx = append(idx, i)
		}
	}

	return idx
}

// resolveHeading finds the heading: exact match first, then case-insensitive.
// matches is the count in the chosen tier; ci flags a case-insensitive match.
func resolveHeading(lines []string, heading string) (idx int, actual string, matches int, ci bool) {
	want := headingText(heading)
	if want == "" {
		return -1, "", 0, false
	}

	exact := findHeadings(lines, want)
	if len(exact) > 0 {
		return exact[0], want, len(exact), false
	}

	var loose []int

	for i, l := range lines {
		if headingLevel(l) > 0 && strings.EqualFold(headingText(l), want) {
			loose = append(loose, i)
		}
	}

	if len(loose) > 0 {
		return loose[0], headingText(lines[loose[0]]), len(loose), true
	}

	return -1, "", 0, false
}

// suggestHeadings returns up to n headings sharing the longest common prefix
// with the query, so a failed set can name its nearest alternatives.
func suggestHeadings(lines []string, heading string, n int) []string {
	want := strings.ToLower(headingText(heading))

	type candidate struct {
		name  string
		score int
	}

	seen := map[string]bool{}

	var candidates []candidate

	for _, l := range lines {
		if headingLevel(l) == 0 {
			continue
		}

		name := headingText(l)
		if name == "" || seen[name] {
			continue
		}

		seen[name] = true

		lo := strings.ToLower(name)

		score := 0
		for score < len(lo) && score < len(want) && lo[score] == want[score] {
			score++
		}

		if score >= 2 {
			candidates = append(candidates, candidate{name: name, score: score})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	var out []string

	for _, c := range candidates {
		out = append(out, c.name)
		if len(out) == n {
			break
		}
	}

	return out
}

// autoLabel derives a label from a heading: lowercase, non-alphanumerics to '-'.
func autoLabel(heading string) string {
	s := strings.ToLower(headingText(heading))

	var b strings.Builder

	lastDash := false

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)

			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')

				lastDash = true
			}
		}
	}

	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "ref"
	}

	if runes := []rune(out); len(runes) > labelCap {
		out = strings.TrimRight(string(runes[:labelCap]), "-")
	}

	return out
}
func uniqueMarkMatch(lines []string, headings []int, mark string) int {
	found := -1

	for _, h := range headings {
		if sectionMark(lines, h) != mark {
			continue
		}

		if found >= 0 {
			return -1 // ambiguous: refuse instead of guessing
		}

		found = h
	}

	return found
}

// headingText strips leading '#'s and surrounding space.
func headingText(line string) string {
	s := strings.TrimSpace(line)
	for strings.HasPrefix(s, "#") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	}

	return s
}

// headingLevel counts leading '#'s; 0 for a plain line.
func headingLevel(line string) int {
	s := strings.TrimSpace(line)

	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}

	return n
}

// sectionEndExclusive returns the first following heading of the same or
// higher level (level 0 counts as top level).
func sectionEndExclusive(lines []string, start int) int {
	level := headingLevel(lines[start])

	for i := start + 1; i < len(lines); i++ {
		if lv := headingLevel(lines[i]); lv > 0 && lv <= level {
			return i
		}
	}

	return len(lines)
}

// sectionBounds returns the heading line and the last non-empty body line.
func sectionBounds(lines []string, start int) (int, int) {
	end := sectionEndExclusive(lines, start)

	last := start
	for i := start + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			last = i
		}
	}

	return start, last
}

// sectionMark returns the first non-empty body line, capped. Empty when the section is empty.
func sectionMark(lines []string, start int) string {
	end := sectionEndExclusive(lines, start)

	for i := start + 1; i < end; i++ {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return capMark(s)
		}
	}

	return ""
}

// findMark returns the line of the unique exact mark match, or -1 (absent or ambiguous).
func findMark(lines []string, mark string) int {
	if mark == "" {
		return -1
	}

	found := -1

	for i, l := range lines {
		if capMark(l) == mark {
			if found >= 0 {
				return -1 // ambiguous: refuse instead of guessing
			}

			found = i
		}
	}

	return found
}

// capMark trims and caps a line to markCap runes.
func capMark(line string) string {
	s := strings.TrimSpace(line)

	r := []rune(s)
	if len(r) > markCap {
		return string(r[:markCap])
	}

	return s
}

// readDoc reads the doc as lines, normalising CRLF.
func readDoc(abs string) ([]string, error) {
	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("unreadable: %w", err)
	}

	return strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), nil
}

// gitSnapshot captures branch, HEAD sha and dirty files; empty outside a repo.
func gitSnapshot(dir string) (branch, sha string, dirty []string) {
	branch = runGit(dir, "rev-parse", "--abbrev-ref", "HEAD")
	sha = runGit(dir, "rev-parse", "HEAD")

	out := runGit(dir, "status", "--porcelain")
	if out != "" {
		dirty = parsePorcelain(out)
	}

	if len(dirty) > dirtyCap {
		dirty = dirty[:dirtyCap]
	}

	return branch, sha, dirty
}

// parsePorcelain extracts changed paths from `git status --porcelain` output.
func parsePorcelain(out string) []string {
	var files []string

	for _, l := range strings.Split(out, "\n") {
		if len(l) < 4 {
			continue
		}

		p := strings.TrimSpace(l[3:])
		if _, after, ok := strings.Cut(p, " -> "); ok {
			p = after // renames: keep the new path
		}

		files = append(files, p)
	}

	return files
}

// runGit runs one git command in dir; empty string on any failure.
func runGit(dir string, gitArgs ...string) string {
	// #nosec G204 -- fixed git binary
	cmd := exec.CommandContext(context.Background(), "git", gitArgs...)
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}

// isAncestor reports whether sha is an ancestor of HEAD.
func isAncestor(dir, sha string) bool {
	// #nosec G204 -- fixed git binary; sha comes from the stored bookmark
	cmd := exec.CommandContext(context.Background(), "git", "merge-base", "--is-ancestor", sha, "HEAD")
	cmd.Dir = dir

	return cmd.Run() == nil
}

// commitsSince returns "sha subject" lines for the range sha..HEAD, capped.
func commitsSince(dir, sha string) []string {
	out := runGit(dir, "log", "--format=%h %s", sha+"..HEAD")
	if out == "" {
		return nil
	}

	lines := strings.Split(out, "\n")
	if len(lines) > commitCap {
		lines = lines[:commitCap]
	}

	return lines
}

// changedFiles returns the files touched in the range sha..HEAD, capped.
func changedFiles(dir, sha string) []string {
	out := runGit(dir, "log", "--format=", "--name-only", sha+"..HEAD")
	if out == "" {
		return nil
	}

	seen := map[string]bool{}

	var files []string

	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}

		seen[l] = true

		files = append(files, l)
		if len(files) >= commitCap*5 {
			break
		}
	}

	return files
}

// keywords returns label/heading words used to match commit subjects.
func keywords(book bookmark) []string {
	stop := map[string]bool{
		"next": true, "goal": true, "steps": true, "work": true, "notes": true,
		"status": true, "current": true, "todo": true, "plan": true, "done": true,
		"blockers": true, "section": true,
	}

	var words []string

	seen := map[string]bool{}

	for _, r := range book.Refs {
		for _, w := range wordRe.FindAllString(r.Label+" "+r.Heading, -1) {
			w = strings.ToLower(w)
			if stop[w] || seen[w] {
				continue
			}

			seen[w] = true

			words = append(words, w)
			if len(words) >= 12 {
				return words
			}
		}
	}

	return words
}

// countMentioning counts commit lines containing any keyword.
func countMentioning(commits, words []string) int {
	if len(words) == 0 {
		return 0
	}

	hits := 0

	for _, line := range commits {
		lower := strings.ToLower(line)

		for _, w := range words {
			if strings.Contains(lower, w) {
				hits++

				break
			}
		}
	}

	return hits
}

// scopeMatches returns changed files that the doc sections mention by path.
func scopeMatches(root string, lines []string, spans []span, groups ...[]string) []string {
	paths := inScopePaths(root, lines, spans)

	seen := map[string]bool{}

	var names []string

	for _, group := range groups {
		for _, f := range group {
			if seen[f] || !matchesDocPath(f, paths) {
				continue
			}

			seen[f] = true

			names = append(names, f)
		}
	}

	return names
}

// inScopePaths extracts project-local paths mentioned in the resolved sections.
func inScopePaths(root string, lines []string, spans []span) map[string]bool {
	paths := map[string]bool{}

	for _, sp := range spans {
		for i := sp.start; i <= sp.end && i < len(lines); i++ {
			for _, tok := range pathTokenRe.FindAllString(lines[i], -1) {
				tok = strings.Trim(tok, ".,;:()[]`\"'")
				if !strings.ContainsAny(tok, "./") || !filepath.IsLocal(tok) {
					continue
				}

				if _, err := os.Stat(filepath.Join(root, tok)); err != nil {
					continue
				}

				paths[tok] = true
				if len(paths) >= pathScanCap {
					return paths
				}
			}
		}
	}

	return paths
}

// matchesDocPath reports whether a repo path matches any doc-mentioned path.
func matchesDocPath(file string, paths map[string]bool) bool {
	if paths[file] {
		return true
	}

	base := filepath.Base(file)

	for p := range paths {
		if strings.HasSuffix(file, "/"+p) || filepath.Base(p) == base {
			return true
		}
	}

	return false
}

// dirtyDiff counts files added to and removed from the dirty set.
func dirtyDiff(before, after []string) (added, removed int) {
	beforeSet := map[string]bool{}
	for _, f := range before {
		beforeSet[f] = true
	}

	afterSet := map[string]bool{}
	for _, f := range after {
		afterSet[f] = true
	}

	for f := range afterSet {
		if !beforeSet[f] {
			added++
		}
	}

	for f := range beforeSet {
		if !afterSet[f] {
			removed++
		}
	}

	return added, removed
}

// countBoxes counts markdown checkboxes inside the resolved sections.
func countBoxes(lines []string, spans []span) (done, total int) {
	for _, sp := range spans {
		for i := sp.start; i <= sp.end && i < len(lines); i++ {
			t := strings.TrimSpace(lines[i])
			if len(t) < 5 || (t[0] != '-' && t[0] != '*') || t[1] != ' ' || t[2] != '[' || t[4] != ']' {
				continue
			}

			switch t[3] {
			case ' ':
				total++
			case 'x', 'X':
				total++
				done++
			}
		}
	}

	return done, total
}

// loadStore reads the bookmark store; ok is false when absent or empty.
func loadStore(path string) (bookmark, bool) {
	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		return bookmark{}, false
	}

	var book bookmark

	err = json.Unmarshal(data, &book)
	if err != nil || book.Path == "" {
		return bookmark{}, false
	}

	book.Refs = sanitizeRefs(book.Refs)
	if len(book.Refs) == 0 {
		return bookmark{}, false
	}

	return book, true
}

// sanitizeRefs drops malformed or duplicate refs from hand-edited stores:
// first occurrence wins, labels compared case-insensitively, headings normalised.
// Empty marks are kept: a ref may point at a section that is still empty.
func sanitizeRefs(refs []ref) []ref {
	seenLabels := map[string]bool{}
	seenHeadings := map[string]bool{}

	var out []ref

	for _, r := range refs {
		label := strings.ToLower(strings.TrimSpace(r.Label))
		heading := headingText(r.Heading)

		if label == "" || heading == "" || seenLabels[label] || seenHeadings[heading] {
			continue
		}

		seenLabels[label] = true
		seenHeadings[heading] = true

		out = append(out, ref{Label: r.Label, Heading: r.Heading, Mark: r.Mark})
	}

	return out
}

// saveStore persists atomically (temp file + rename) so a reader never sees torn JSON.
func saveStore(path string, book bookmark) error {
	err := os.MkdirAll(filepath.Dir(path), dirMode)
	if err != nil {
		return fmt.Errorf("create bookmark dir: %w", err)
	}

	data, err := json.MarshalIndent(book, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bookmark: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".bookmark-*")
	if err != nil {
		return fmt.Errorf("create temp bookmark: %w", err)
	}

	name := tmp.Name()

	defer func() { _ = os.Remove(name) }() // no-op after a successful rename

	_, err = tmp.Write(data)
	if err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write temp bookmark: %w", err)
	}

	err = tmp.Close()
	if err != nil {
		return fmt.Errorf("close temp bookmark: %w", err)
	}

	err = os.Chmod(name, fileMode)
	if err != nil {
		return fmt.Errorf("chmod temp bookmark: %w", err)
	}

	err = os.Rename(name, path)
	if err != nil {
		return fmt.Errorf("replace bookmark: %w", err)
	}

	return nil
}

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
}

func nowStamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}

	return t
}

// ageString renders a duration coarsely: 4m, 3h, 2d.
func ageString(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}

	return s
}

func commitCount(n int) string {
	if n == 1 {
		return "1 commit"
	}

	return fmt.Sprintf("%d commits", n)
}

func refCount(n int) string {
	if n == 1 {
		return "1 ref"
	}

	return fmt.Sprintf("%d refs", n)
}

// joinCap joins up to n items, noting the remainder.
func joinCap(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}

	return strings.Join(items[:n], ", ") + fmt.Sprintf(" +%d more", len(items)-n)
}
