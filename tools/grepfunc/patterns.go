package grepfunc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// maxUnionPatterns bounds one union: alternation cost grows with the branch
// count, and no caller needs more than a handful of terms in a single lookup.
const maxUnionPatterns = 16

// errNoPatterns is returned when every supplied pattern was empty.
var errNoPatterns = errors.New("no patterns")

// MergePatterns flattens the singular pattern and a pattern list into the
// effective set: empties dropped, duplicates removed, order preserved.
func MergePatterns(pattern string, patterns []string) []string {
	return dedupePatterns(pattern, patterns)
}

// dedupePatterns drops empty and duplicate entries, preserving order.
func dedupePatterns(pattern string, patterns []string) []string {
	seen := make(map[string]struct{}, len(patterns)+1)
	out := make([]string, 0, len(patterns)+1)

	add := func(p string) {
		if p == "" {
			return
		}

		if _, dup := seen[p]; dup {
			return
		}

		seen[p] = struct{}{}
		out = append(out, p)
	}

	add(pattern)

	for _, p := range patterns {
		add(p)
	}

	return out
}

// CompilePatterns compiles patterns into one regex matching any of them. A
// single pattern compiles exactly like CompilePattern; several are wrapped in
// (?:...) and joined with |, so alternation and inline flags stay scoped to
// their own pattern.
func CompilePatterns(patterns []string, caseSensitive bool) (*regexp.Regexp, error) {
	uniq := dedupePatterns("", patterns)
	if len(uniq) == 0 {
		return nil, errNoPatterns
	}

	if len(uniq) > maxUnionPatterns {
		return nil, fmt.Errorf("too many patterns: %d (max %d)", len(uniq), maxUnionPatterns)
	}

	if len(uniq) == 1 {
		return CompilePattern(uniq[0], caseSensitive)
	}

	size := 0
	for _, p := range uniq {
		size += len(p) + 4
	}

	var union strings.Builder
	union.Grow(size)

	for i, p := range uniq {
		if i > 0 {
			union.WriteByte('|')
		}

		union.WriteString("(?:")
		union.WriteString(p)
		union.WriteByte(')')
	}

	re, err := CompilePattern(union.String(), caseSensitive)
	if err == nil {
		return re, nil
	}

	// Wrapping a valid regex in (?:...) cannot invalidate it, so a failing
	// union means one part is broken: report that part, not the joined text.
	for _, p := range uniq {
		if _, partErr := CompilePattern(p, caseSensitive); partErr != nil {
			return nil, partErr
		}
	}

	return nil, err
}

// PatternLabel renders a pattern set for result headers.
func PatternLabel(patterns []string) string {
	return strings.Join(patterns, " | ")
}
