package grepfunc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMergePatterns(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		patterns []string
		want     []string
	}{
		{"single", "alpha", nil, []string{"alpha"}},
		{"list", "", []string{"a", "b"}, []string{"a", "b"}},
		{"singular first", "a", []string{"b"}, []string{"a", "b"}},
		{"dedupe and empties", "a", []string{"b", "", "a", "c"}, []string{"a", "b", "c"}},
		{"nothing", "", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MergePatterns(tt.pattern, tt.patterns); !slices.Equal(got, tt.want) {
				t.Errorf("MergePatterns(%q, %q) = %q, want %q", tt.pattern, tt.patterns, got, tt.want)
			}
		})
	}
}

// One pattern must compile to exactly what CompilePattern produces.
func TestCompilePatternsSingleEqualsCompilePattern(t *testing.T) {
	got, err := CompilePatterns([]string{"foo.bar"}, false)
	if err != nil {
		t.Fatal(err)
	}

	want, err := CompilePattern("foo.bar", false)
	if err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Errorf("single pattern compiled to %q, want %q", got.String(), want.String())
	}
}

func TestCompilePatternsUnion(t *testing.T) {
	// case_sensitive so the inline (?i) of the last branch is what makes GAMMA match.
	re, err := CompilePatterns([]string{"alpha", "beta", "(?i)gamma"}, true)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		text string
		want bool
	}{
		{"alpha", true},
		{"ALPHA", false},
		{"beta", true},
		{"GAMMA", true}, // inline flag stays scoped to its own branch
		{"gamma", true},
		{"delta", false},
	}

	for _, tt := range tests {
		if got := re.MatchString(tt.text); got != tt.want {
			t.Errorf("MatchString(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestCompilePatternsDedupesAndGroups(t *testing.T) {
	re, err := CompilePatterns([]string{"a", "a", "", "b"}, true)
	if err != nil {
		t.Fatal(err)
	}

	if want := `(?m)(?:a)|(?:b)`; re.String() != want {
		t.Errorf("union = %q, want %q", re.String(), want)
	}
}

func TestCompilePatternsErrors(t *testing.T) {
	_, err := CompilePatterns([]string{"", ""}, true)
	if !errors.Is(err, errNoPatterns) {
		t.Errorf("empty set error = %v, want errNoPatterns", err)
	}

	many := make([]string, maxUnionPatterns+1)
	for i := range many {
		many[i] = string(rune('a' + i))
	}

	if _, err := CompilePatterns(many, true); err == nil {
		t.Error("expected too-many-patterns error")
	}

	_, err = CompilePatterns([]string{"good", "bad("}, true)
	if err == nil || !strings.Contains(err.Error(), "bad(") {
		t.Errorf("invalid part error = %v, want it to name %q", err, "bad(")
	}
}

func TestPatternLabel(t *testing.T) {
	if got := PatternLabel([]string{"a", "b"}); got != "a | b" {
		t.Errorf("PatternLabel = %q, want %q", got, "a | b")
	}

	if got := PatternLabel(nil); got != "" {
		t.Errorf("PatternLabel(nil) = %q, want empty", got)
	}
}

// The patterns array must union with pattern and match in one walk.
func TestHandlePatternsUnion(t *testing.T) {
	dir := t.TempDir()
	code := "package p\n\nfunc alpha() {\n\treturn\n}\n\n" +
		"func beta() {\n\treturn\n}\n\nfunc gamma() {\n\treturn\n}\n"

	err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		schemaPatterns: []string{"alpha", "gamma"},
		schemaPath:     dir,
		schemaInclude:  globGo,
		"names_only":   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	for _, want := range []string{"alpha", "gamma", `"alpha | gamma"`} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "beta") {
		t.Errorf("output must not contain beta:\n%s", text)
	}
}

func TestHandleRequiresAPattern(t *testing.T) {
	raw, err := json.Marshal(map[string]any{schemaPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Handle(raw)
	if !errors.Is(err, errPatternRequired) {
		t.Errorf("error = %v, want errPatternRequired", err)
	}
}
