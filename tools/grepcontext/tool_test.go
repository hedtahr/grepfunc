package grepcontext

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

func TestMatchWindowsBuildsContext(t *testing.T) {
	data := []byte("l1\nl2\nhit here\nl4\nl5\nl6\nl7\n")
	pattern := regexp.MustCompile("hit")

	windows := matchWindows(data, "a.go", args{ContextLines: 1}, pattern, 10)
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}

	win := windows[0]
	if win.matchLine != 3 {
		t.Errorf("matchLine = %d, want 3", win.matchLine)
	}

	if win.start != 1 || win.end != 3 {
		t.Errorf("window = %d–%d, want 1–3", win.start, win.end)
	}

	if got := strings.Join(win.lines, "|"); got != "l2|hit here|l4" {
		t.Errorf("lines = %q, want %q", got, "l2|hit here|l4")
	}
}

// Hits inside the previous window must not produce a second window.
func TestMatchWindowsDeduplicatesOverlap(t *testing.T) {
	data := []byte("hit\nx\nhit\n")
	pattern := regexp.MustCompile("hit")

	windows := matchWindows(data, "a.go", args{ContextLines: 2}, pattern, 10)
	if len(windows) != 1 {
		t.Fatalf("overlapping hits should share one window, got %d", len(windows))
	}

	if len(windows[0].lines) != 3 {
		t.Errorf("window should cover all three lines, got %q", windows[0].lines)
	}
}

func TestMatchWindowsScopes(t *testing.T) {
	data := []byte("package p\n\nfunc handle() {\n\tmarker\n}\n")
	pattern := regexp.MustCompile("marker")

	windows := matchWindows(data, "a.go", args{ContextLines: 1, Scope: true}, pattern, 10)
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}

	if windows[0].scope != "handle" {
		t.Errorf("scope = %q, want %q", windows[0].scope, "handle")
	}

	plain := matchWindows(data, "a.go", args{ContextLines: 1}, pattern, 10)
	if plain[0].scope != "" {
		t.Errorf("scope should be empty without scope=true, got %q", plain[0].scope)
	}
}

// No hit must stay cheap: no line splitting, no allocations.
func TestMatchWindowsNoHitIsAllocationFree(t *testing.T) {
	data := benchData(200, 10)
	pattern := regexp.MustCompile("no_such_marker")

	allocs := testing.AllocsPerRun(10, func() {
		if windows := matchWindows(data, "a.go", args{ContextLines: 3}, pattern, 20); windows != nil {
			t.Fatalf("expected no windows")
		}
	})

	if allocs > 0 {
		t.Errorf("no-hit scan allocated %v times per run, want 0", allocs)
	}
}

// CRLF files: anchors must work and rendered lines must not carry \r.
func TestMatchWindowsNormalisesCRLF(t *testing.T) {
	data := []byte("a\r\nfoo match\r\nb\r\n")

	pattern, err := grepfunc.CompilePattern("foo match$", false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	windows := matchWindows(data, "a.go", args{ContextLines: 1}, pattern, 10)
	if len(windows) != 1 {
		t.Fatalf("$ anchor should match before \\r\\n, got %d windows", len(windows))
	}

	for _, line := range windows[0].lines {
		if strings.Contains(line, "\r") {
			t.Errorf("rendered line still has a carriage return: %q", line)
		}
	}
}
