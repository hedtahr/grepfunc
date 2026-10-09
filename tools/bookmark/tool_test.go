package bookmark

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hedtahr/grepfunc/server"
)

const docName = "WORKLOG.md"

const docV1 = `# Worklog

## Goal
Ship main.go fix.

## Next steps
- [ ] set/read round trip
- [x] drift line

## Notes
Mark repair works.

## Blockers
none
`

func marshalArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// fixture creates a project dir with the doc, overrides storePath and ProjectRoot.
func fixture(t *testing.T, doc string) string {
	t.Helper()

	dir := t.TempDir()

	origRoot := server.ProjectRoot
	server.ProjectRoot = dir

	origPath := storePath
	storePath = func(_ string) (string, error) {
		return filepath.Join(dir, bookmarkDir, bookmarkFile), nil
	}

	t.Cleanup(func() {
		server.ProjectRoot = origRoot
		storePath = origPath
	})

	if doc != "" {
		writeDoc(t, dir, doc)
	} else {
		// No doc: remove any file, keep dir.
		_ = os.Remove(filepath.Join(dir, docName))
	}

	return dir
}

func writeDoc(t *testing.T, dir, content string) string {
	t.Helper()

	p := filepath.Join(dir, docName)

	err := os.WriteFile(p, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func handle(t *testing.T, args map[string]any) string {
	t.Helper()

	result, err := Handle(marshalArgs(t, args))
	if err != nil {
		t.Fatal(err)
	}

	return result.Content[0].Text
}

func setRefs(t *testing.T, refs ...string) string {
	t.Helper()

	return handle(t, map[string]any{"path": docName, "refs": refs})
}

func TestSetAndRead(t *testing.T) {
	fixture(t, docV1)

	out := setRefs(t, "goal=Goal", "next=Next steps")
	if !strings.Contains(out, "goal→L3-4") || !strings.Contains(out, "next→L6-8") {
		t.Errorf("set should report resolved ranges, got %q", out)
	}

	text := handle(t, map[string]any{})
	for _, want := range []string{"bookmark " + docName, "goal → L3-4", "next → L6-8", "boxes 1/2 done", "verdict: IDLE"} {
		if !strings.Contains(text, want) {
			t.Errorf("read missing %q, got:\n%s", want, text)
		}
	}
}

func TestReadNoBookmark(t *testing.T) {
	fixture(t, docV1)

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "No bookmark") {
		t.Errorf("empty state should explain how to set one, got %q", text)
	}
}

func TestSetRefusesMissingDoc(t *testing.T) {
	fixture(t, "")

	_, err := Handle(marshalArgs(t, map[string]any{"path": docName, "refs": []string{"goal=Goal"}}))
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("missing doc should be refused, got %v", err)
	}
}

func TestSetRefusesMissingHeading(t *testing.T) {
	fixture(t, docV1)

	_, err := Handle(marshalArgs(t, map[string]any{"path": docName, "refs": []string{"goal=Nope"}}))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing heading should be refused, got %v", err)
	}
}

func TestSetReplacesRefs(t *testing.T) {
	fixture(t, docV1)

	setRefs(t, "goal=Goal", "next=Next steps")
	setRefs(t, "goal=Goal")

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "goal → L3-4") {
		t.Errorf("goal ref should remain, got:\n%s", text)
	}

	if strings.Contains(text, "next →") {
		t.Errorf("set should replace the ref set, got:\n%s", text)
	}
}

func TestSetResolvesDuplicates(t *testing.T) {
	fixture(t, docV1)

	out := setRefs(t, "a=Goal", "b=Goal")
	if !strings.Contains(out, "dropped 1 duplicate heading") {
		t.Errorf("duplicate heading should be dropped with a note, got %q", out)
	}

	text := handle(t, map[string]any{})
	if got := strings.Count(text, " → L"); got != 1 {
		t.Errorf("store should hold one ref, got %d:\n%s", got, text)
	}

	out = setRefs(t, "Goal=Goal", "goal=Next steps")
	if !strings.Contains(out, "dropped 1 duplicate label") {
		t.Errorf("case-insensitive duplicate label should be dropped with a note, got %q", out)
	}
}

func TestSetPicksFirstOfRepeatedHeadings(t *testing.T) {
	fixture(t, "# D\n\n## Notes\nfirst\n\n## Other\nx\n\n## Notes\nsecond\n")

	out := setRefs(t, "n=Notes")
	if !strings.Contains(out, "repeated headings") {
		t.Errorf("repeated heading should resolve to the first with a note, got %q", out)
	}

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "n → L3-4") {
		t.Errorf("ref should point at the first section, got:\n%s", text)
	}
}

func TestSetAcceptsEmptySection(t *testing.T) {
	dir := fixture(t, "# D\n\n## Empty\n\n## Other\nx\n")

	out := setRefs(t, "e=Empty")
	if !strings.Contains(out, "empty section") {
		t.Errorf("empty section should be accepted with a note, got %q", out)
	}

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "e → L3-3") {
		t.Errorf("empty section should resolve to its heading line, got:\n%s", text)
	}

	// Content arrives: the mark arms itself for future repair.
	writeDoc(t, dir, "# D\n\n## Empty\ncontent now\n\n## Other\nx\n")

	text = handle(t, map[string]any{})
	if strings.Contains(text, "evicted") {
		t.Errorf("ref should survive content arriving, got:\n%s", text)
	}

	book, ok := loadStore(filepath.Join(dir, bookmarkDir, bookmarkFile))
	if !ok || book.Refs[0].Mark != "content now" {
		t.Errorf("mark should arm on first content, got %+v", book.Refs)
	}
}

func TestSetKeepsFirstFour(t *testing.T) {
	fixture(t, docV1)

	out := setRefs(t, "a=Goal", "b=Next steps", "c=Notes", "d=Blockers", "e=Worklog")
	if !strings.Contains(out, "kept first 4") {
		t.Errorf("overflow refs should be dropped with a note, got %q", out)
	}

	text := handle(t, map[string]any{})
	if got := strings.Count(text, " → L"); got != 4 {
		t.Errorf("store should hold four refs, got %d:\n%s", got, text)
	}
}

func TestSetAutoLabelAndPartialSave(t *testing.T) {
	fixture(t, docV1)

	out := setRefs(t, "Next steps")
	if !strings.Contains(out, "next-steps→L6-8") {
		t.Errorf("bare spec should auto-label, got %q", out)
	}

	// One unresolvable ref must not sink the resolvable ones.
	out = setRefs(t, "g=Goal", "x=Goalz")
	if !strings.Contains(out, "g→L3-4") || !strings.Contains(out, "not found") || !strings.Contains(out, "nearest: Goal") {
		t.Errorf("partial save should keep goal and report the miss, got %q", out)
	}
}

func TestNoStaleVerdictAfterLongPause(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "goal=Goal")

	fp := filepath.Join(dir, bookmarkDir, bookmarkFile)

	book, ok := loadStore(fp)
	if !ok {
		t.Fatal("store should load")
	}

	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	book.At, book.Verified = old.Format(time.RFC3339Nano), old.Format(time.RFC3339Nano)

	if err := saveStore(fp, book); err != nil {
		t.Fatal(err)
	}

	// Keep the doc older than the bookmark so mtime does not read as an edit.
	if err := os.Chtimes(filepath.Join(dir, docName), old, old); err != nil {
		t.Fatal(err)
	}

	text := handle(t, map[string]any{})
	if strings.Contains(text, "STALE") {
		t.Errorf("age alone must not flag staleness, got:\n%s", text)
	}

	if !strings.Contains(text, "verdict: IDLE") {
		t.Errorf("paused work with no movement should read IDLE, got:\n%s", text)
	}
}

func TestSetIsIdempotent(t *testing.T) {
	fixture(t, docV1)

	setRefs(t, "goal=Goal", "next=Next steps")
	setRefs(t, "goal=Goal", "next=Next steps")

	text := handle(t, map[string]any{})
	if got := strings.Count(text, "goal →"); got != 1 {
		t.Errorf("repeated set duplicated refs (%d occurrences):\n%s", got, text)
	}
}

func TestReadDuplicateHeadingDisambiguatedByMark(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "next=Next steps")

	// A second section with the same heading appears elsewhere.
	writeDoc(t, dir, docV1+"\n## Next steps\n- [x] something else\n")

	text := handle(t, map[string]any{})
	if strings.Contains(text, "evicted") {
		t.Errorf("unique mark should disambiguate duplicate headings, got:\n%s", text)
	}

	if !strings.Contains(text, "next → L6-8") {
		t.Errorf("ref should still resolve to the marked section, got:\n%s", text)
	}
}

func TestReadIdenticalDuplicateEvicted(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "next=Next steps")

	dup := "# Worklog\n\n## Goal\nShip main.go fix.\n\n" +
		"## Next steps\n- [ ] set/read round trip\n\n" +
		"## Next steps\n- [ ] set/read round trip\n"
	writeDoc(t, dir, dup)

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "no refs left") || !strings.Contains(text, "mark ambiguous") {
		t.Errorf("identical duplicates should be evicted, got:\n%s", text)
	}
}

func TestLoadStoreDedupesRefs(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "bookmark.json")

	data := `{"path":"X.md","refs":[` +
		`{"label":"next","heading":"Next steps","mark":"a"},` +
		`{"label":"NEXT","heading":"Other","mark":"b"},` +
		`{"label":"other","heading":"Next steps","mark":"c"},` +
		`{"label":"bad","heading":"","mark":"d"}]}`

	if err := os.WriteFile(fp, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	book, ok := loadStore(fp)
	if !ok {
		t.Fatal("store should load")
	}

	if len(book.Refs) != 1 || book.Refs[0].Label != "next" {
		t.Errorf("duplicates should collapse to the first ref, got %+v", book.Refs)
	}
}

func TestMarkRepairOnHeadingRename(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "next=Next steps")

	renamed := strings.Replace(docV1, "## Next steps", "## Following steps", 1)
	writeDoc(t, dir, renamed)

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "repaired via mark") {
		t.Errorf("renamed heading should be repaired via mark, got:\n%s", text)
	}

	if strings.Contains(text, "evicted") {
		t.Errorf("repair should not evict, got:\n%s", text)
	}
}

func TestEvictionDropsDeadRef(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "goal=Goal", "next=Next steps")

	writeDoc(t, dir, "# Worklog\n\n## Goal\nShip main.go fix.\n")

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "evicted: next (heading and mark gone)") {
		t.Errorf("dead ref should be evicted loudly, got:\n%s", text)
	}

	if !strings.Contains(text, "goal → L3-4") {
		t.Errorf("live ref should survive, got:\n%s", text)
	}

	text = handle(t, map[string]any{})
	if strings.Contains(text, "next") {
		t.Errorf("eviction should persist, got:\n%s", text)
	}
}

func TestLastRefEvictionClearsBookmark(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "goal=Goal")

	writeDoc(t, dir, "# Worklog\n")

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "no refs left") {
		t.Errorf("last eviction should clear the bookmark, got:\n%s", text)
	}

	text = handle(t, map[string]any{})
	if !strings.Contains(text, "No bookmark") {
		t.Errorf("store should be gone, got:\n%s", text)
	}
}

func TestMissingDocClearsBookmark(t *testing.T) {
	dir := fixture(t, docV1)

	setRefs(t, "goal=Goal")

	err := os.Remove(filepath.Join(dir, docName))
	if err != nil {
		t.Fatal(err)
	}

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "cleared") {
		t.Errorf("missing doc should clear, got:\n%s", text)
	}

	text = handle(t, map[string]any{})
	if !strings.Contains(text, "No bookmark") {
		t.Errorf("store should be gone after doc removal, got:\n%s", text)
	}
}

func TestClear(t *testing.T) {
	fixture(t, docV1)

	setRefs(t, "goal=Goal")

	text := handle(t, map[string]any{"clear": true})
	if !strings.Contains(text, "cleared") {
		t.Errorf("clear should confirm, got %q", text)
	}

	text = handle(t, map[string]any{})
	if !strings.Contains(text, "No bookmark") {
		t.Errorf("clear should remove the store, got:\n%s", text)
	}
}

func TestStorePath(t *testing.T) {
	path, err := storePath(".")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(path, ".llm") {
		t.Errorf("path should contain .llm: %s", path)
	}

	if !strings.HasSuffix(path, "bookmark.json") {
		t.Errorf("path should end with bookmark.json: %s", path)
	}
}

func TestGitProgressWithInScopeChange(t *testing.T) {
	dir := fixture(t, docV1)
	gitInit(t, dir)
	setRefs(t, "goal=Goal")

	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "verdict: PROGRESS") || !strings.Contains(text, "in scope") {
		t.Errorf("in-scope dirty file should read as PROGRESS, got:\n%s", text)
	}
}

func TestGitDriftOutOnUnrelatedChange(t *testing.T) {
	dir := fixture(t, docV1)
	gitInit(t, dir)
	setRefs(t, "goal=Goal")

	tools := filepath.Join(dir, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}

	err := os.WriteFile(filepath.Join(tools, "x.go"), []byte("package tools\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "verdict: DRIFT-OUT") {
		t.Errorf("unrelated dirty file should read as DRIFT-OUT, got:\n%s", text)
	}
}

func TestGitCommitCounted(t *testing.T) {
	dir := fixture(t, docV1)
	gitInit(t, dir)
	setRefs(t, "goal=Goal")

	err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("x\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitRun(t, dir, "add", "unrelated.txt")
	gitRun(t, dir, "commit", "-m", "unrelated change")

	text := handle(t, map[string]any{})
	if !strings.Contains(text, "1 commit since") {
		t.Errorf("new commit should be reported, got:\n%s", text)
	}
}

// gitInit initialises a repo in dir and commits the current tree.
func gitInit(t *testing.T, dir string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
