package patchedit

import (
	"strings"
	"testing"
)

func TestExactMatch(t *testing.T) {
	content := []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n")
	tests := []struct {
		name     string
		oldText  string
		wantLen  int
		wantLine int
	}{
		{"single line", "func main() {", 1, 3},
		{"with tab", "\tfmt.Println(\"hello\")", 1, 4},
		{"not found", "nonexistent", 0, 0},
		{"empty", "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locs := exactMatch(content, tt.oldText, buildLineTable(content))
			if len(locs) != tt.wantLen {
				t.Fatalf("got %d matches, want %d", len(locs), tt.wantLen)
			}
			if tt.wantLen > 0 && locs[0].LineStart != tt.wantLine {
				t.Errorf("line start = %d, want %d", locs[0].LineStart, tt.wantLine)
			}
			if tt.wantLen > 0 && locs[0].Strategy != "exact" {
				t.Errorf("strategy = %s, want exact", locs[0].Strategy)
			}
		})
	}
}

func TestExactMatchMultiple(t *testing.T) {
	content := []byte("return nil, false\nreturn nil, false\nreturn nil, true\nreturn nil, false\n")
	locs := exactMatch(content, "return nil, false", buildLineTable(content))
	if len(locs) != 3 {
		t.Fatalf("got %d matches, want 3", len(locs))
	}
	if locs[0].LineStart != 1 || locs[1].LineStart != 2 || locs[2].LineStart != 4 {
		t.Errorf("wrong line numbers: %v", locs)
	}
}

func TestFindAllMatches(t *testing.T) {
	content := []byte("package main\n\nvar wg sync.WaitGroup\nvar stopCh chan struct{}\n")
	locs := findAllMatches(content, "var wg sync.WaitGroup")
	if len(locs) != 1 || locs[0].Strategy != "exact" {
		t.Fatalf("exact match failed: %v", locs)
	}
	contentTab := []byte("\tcounter uint64\n\titems   map[string]*Item\n")
	locs = wsFuzzyMatch(contentTab, "  counter uint64", buildLineTable(contentTab))
	if len(locs) != 1 || locs[0].Strategy != "whitespace_fuzzy" {
		t.Fatalf("whitespace fuzzy match failed: %v", locs)
	}
	contentLong := []byte("func NewDataStore(cfg StoreConfig) *DataStore {\n\treturn &DataStore{}\n}\n")
	locs = findAllMatches(contentLong, "func NewDataStore(cfg StoreConfig) *DataStore { // constructor")
	if len(locs) != 1 || locs[0].Strategy != "line_fuzzy" {
		t.Fatalf("line fuzzy match failed (bidirectional contains): %v", locs)
	}
}

func TestLineFuzzyMatch(t *testing.T) {
	content := []byte("func (s *Server) Start() error {\n\tif err := s.initDB(); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n")
	locs := lineFuzzyMatch(content, "if err := s.initDB(); err != nil {\n\t\treturn err")
	if len(locs) != 1 || locs[0].Strategy != "line_fuzzy" {
		t.Fatalf("line fuzzy match failed: %v", locs)
	}
	if locs[0].LineStart != 2 || locs[0].LineEnd != 3 {
		t.Errorf("line range = %d-%d, want 2-3", locs[0].LineStart, locs[0].LineEnd)
	}
}

func TestOffsetToLines(t *testing.T) {
	content := []byte("line1\nline2\nline3\n")
	lt := buildLineTable(content)
	ls, le := lineOffsetsToLines(lt, 0, 5)
	if ls != 1 || le != 2 {
		t.Errorf("lineOffsetsToLines(0,5) = (%d,%d), want (1,2)", ls, le)
	}
	ls, le = lineOffsetsToLines(lt, 6, 11)
	if ls != 2 || le != 3 {
		t.Errorf("lineOffsetsToLines(6,11) = (%d,%d), want (2,3)", ls, le)
	}
}

func TestFindNearest(t *testing.T) {
	content := []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n")
	n := findNearest(content, "func main(")
	if n.Line != 3 {
		t.Errorf("nearest line = %d, want 3", n.Line)
	}
	if n.Preview == "" {
		t.Error("nearest preview is empty")
	}
}

func TestAmbiguousMatch(t *testing.T) {
	content := []byte("return nil, false\n// some code\nreturn nil, false\n// more code\nreturn nil, false\n")
	locs := findAllMatches(content, "return nil, false")
	if len(locs) != 3 {
		t.Fatalf("ambiguous match: got %d, want 3", len(locs))
	}
}

func TestWsFuzzyEndOffset(t *testing.T) {
	file := []byte("func Foo() error {\n\treturn nil\n}\n\nfunc Bar() {\n")
	oldText := "func Foo() error {\n    return nil\n}"
	locs := wsFuzzyMatch(file, oldText, buildLineTable(file))
	if len(locs) != 1 {
		t.Fatalf("got %d matches", len(locs))
	}
	loc := locs[0]
	matched := string(file[loc.Offset:loc.EndOffset])
	t.Logf("matched region: %q", matched)
	t.Logf("Offset=%d EndOffset=%d", loc.Offset, loc.EndOffset)
	if loc.EndOffset != loc.Offset+len(matched) {
		t.Errorf("EndOffset inconsistent: %d != %d+%d", loc.EndOffset, loc.Offset, len(matched))
	}
	// Verify matched region starts with the right content (not shifted by 1)
	if !strings.HasPrefix(matched, "func Foo") {
		t.Errorf("matched region should start with 'func Foo', got %q", matched[:min(len(matched), 20)])
	}
	if int(loc.EndOffset) >= len(file) || file[loc.EndOffset] != '\n' {
		t.Errorf("byte after EndOffset should be newline, got %q", file[loc.EndOffset:])
	}
	newText := "func Foo() error {\n    // changed\n    return nil\n}"
	var out []byte
	out = append(out, file[:loc.Offset]...)
	out = append(out, []byte(newText)...)
	out = append(out, file[loc.EndOffset:]...)
	result := string(out)
	if strings.Contains(result, "}}") {
		t.Errorf("replacement produced double }}: %q", result)
	}
	if !strings.Contains(result, "func Bar") {
		t.Error("func Bar missing after replacement")
	}
}

func TestWsFuzzyTrailingSpace(t *testing.T) {
	// Regression: TrimSpace was dropping origForNorm from front instead of back,
	// shifting all offsets by 1 and corrupting files.
	file := []byte("  func foo() {\n    return nil\n  }\n")
	oldText := "  func foo() {\n    return nil\n  }"
	locs := wsFuzzyMatch(file, oldText, buildLineTable(file))
	if len(locs) != 1 {
		t.Fatalf("got %d matches, want 1", len(locs))
	}
	loc := locs[0]
	matched := string(file[loc.Offset:loc.EndOffset])
	t.Logf("matched region: %q", matched)
	t.Logf("Offset=%d EndOffset=%d", loc.Offset, loc.EndOffset)
	// Critical: matched region must start with the actual content, not shifted
	if !strings.HasPrefix(strings.TrimSpace(matched), "func foo()") {
		t.Errorf("matched region corrupt, got %q", matched)
	}
	// Apply replacement and verify no leftovers
	newText := "// replaced\n"
	var out []byte
	out = append(out, file[:loc.Offset]...)
	out = append(out, []byte(newText)...)
	out = append(out, file[loc.EndOffset:]...)
	result := string(out)
	if strings.Contains(result, "return nil") {
		t.Errorf("old text remnants in output: %q", result)
	}
	if !strings.Contains(result, "// replaced") {
		t.Errorf("new text not found: %q", result)
	}
}
