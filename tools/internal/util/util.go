// Package util provides shared helpers for the grep tools.
package util

import (
	"fmt"
	"strings"

	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const maxSigLen = 120

// FirstSigLine returns the first line of a symbol body, trimmed, max 120 chars.
func FirstSigLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")

	line = strings.TrimSpace(line)
	if len(line) > maxSigLen {
		return line[:maxSigLen] + "..."
	}

	return line
}

// FirstLine returns the first line of str. Truncates to 120 chars unless includeBody.
func FirstLine(str string, includeBody bool) string {
	if before, _, found := strings.Cut(str, "\n"); found {
		str = strings.TrimSpace(before)
	}

	if !includeBody && len(str) > maxSigLen {
		return str[:maxSigLen] + "..."
	}

	return str
}

// TrimKind strips leading Go keyword prefixes from a signature string.
func TrimKind(sig string) string {
	for _, kw := range []string{"func ", "type ", "var ", "const ", "interface ", "struct "} {
		if s, ok := strings.CutPrefix(sig, kw); ok {
			return s
		}
	}

	return sig
}

// FilterByExclude removes FuncMatch entries whose Name or Body matches excludePattern.
func FilterByExclude(matches []grepfunc.FuncMatch, excludePattern string) ([]grepfunc.FuncMatch, error) {
	pattern, err := grepfunc.CompilePattern(excludePattern, false)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	out := matches[:0]

	for _, m := range matches {
		if pattern.MatchString(m.Body) || pattern.MatchString(m.Name) {
			continue
		}

		out = append(out, m)
	}

	return out, nil
}
