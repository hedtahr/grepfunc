package toolstats

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolstats.log")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseLogGlobal(t *testing.T) {
	path := writeLog(t, "# toolstats v2\n"+
		"# registered: grep_func,grep_refs,tool_stats\n"+
		"2026-08-01T10:00:00Z\t/Users/me/projA\tgrep_func\tok\t10\t50\n"+
		"2026-08-01T10:00:01Z\t/Users/me/projA\tgrep_func\terr\t20\t30\n"+
		"2026-08-01T10:00:02Z\t/Users/me/projB\tgrep_refs\tok\t5\t10\n"+
		"garbage line\n")

	aggs, registered, projects, err := parseLog(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 3 {
		t.Fatalf("registered = %v", registered)
	}
	g := aggs["grep_func"]
	if g == nil || g.calls != 2 || g.errs != 1 {
		t.Fatalf("grep_func agg = %+v", g)
	}
	if got := avgMs(g); got != 15 {
		t.Fatalf("grep_func avgMs = %d, want 15", got)
	}
	if projects["/Users/me/projA"] != 2 || projects["/Users/me/projB"] != 1 {
		t.Fatalf("projects = %v", projects)
	}
	if _, ok := aggs["garbage"]; ok {
		t.Fatal("garbage line should be skipped")
	}
	if _, ok := aggs["tool_stats"]; ok {
		t.Fatal("never-called tool should not appear in aggs")
	}
}

func TestParseLogFiltered(t *testing.T) {
	path := writeLog(t, "# registered: grep_func,grep_refs\n"+
		"2026-08-01T10:00:00Z\t/Users/me/projA\tgrep_func\tok\t10\t50\n"+
		"2026-08-01T10:00:01Z\t/Users/me/projA\tgrep_func\terr\t20\t30\n"+
		"2026-08-01T10:00:02Z\t/Users/me/projB\tgrep_refs\tok\t5\t10\n")

	aggs, _, projects, err := parseLog(path, "/Users/me/projA")
	if err != nil {
		t.Fatal(err)
	}
	if g := aggs["grep_func"]; g == nil || g.calls != 2 {
		t.Fatalf("grep_func agg = %+v", g)
	}
	if _, ok := aggs["grep_refs"]; ok {
		t.Fatal("grep_refs from projB should be filtered out")
	}
	if len(projects) != 1 || projects["/Users/me/projA"] != 2 {
		t.Fatalf("projects = %v", projects)
	}
}

func TestParseLogMissingFile(t *testing.T) {
	if _, _, _, err := parseLog(filepath.Join(t.TempDir(), "nope.log"), ""); err == nil {
		t.Fatal("expected error for missing file")
	}
}
