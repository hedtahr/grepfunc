package util

import (
	"strings"

	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

// FirstSigLine returns the first line of a symbol body, trimmed, max 120 chars.
func FirstSigLine(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSpace(line)
	if len(line) > 120 {
		return line[:120] + "..."
	}
	return line
}

// FirstLine returns the first line of s. Truncates to 120 chars unless includeBody.
func FirstLine(s string, includeBody bool) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		s = strings.TrimSpace(before)
	}
	if !includeBody && len(s) > 120 {
		return s[:120] + "..."
	}
	return s
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
	re, err := grepfunc.CompilePattern(excludePattern, false)
	if err != nil {
		return nil, err
	}
	out := matches[:0]
	for _, m := range matches {
		if re.MatchString(m.Body) || re.MatchString(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
