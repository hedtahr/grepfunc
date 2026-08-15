// Package util provides shared helpers for the grep tools.
package util

import (
	"fmt"
	"strings"

	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

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
