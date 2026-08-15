package grepfunc

import (
	"os"
	"path"
	"regexp"
	"strings"
)

// gitignoreRules is a lightweight subset of .gitignore semantics used to
// prune directories during Search traversal (ripgrep's biggest win per the
// ignore-crate work). Only the root-level .gitignore is honored; nested
// per-directory files are not (documented limitation). Limitations vs git:
// a file cannot be re-included once a parent dir is excluded, and patterns
// apply to the tree rooted at the search root, not the repo root.
type gitignoreRules struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	negate  bool
	dirOnly bool
	re      *regexp.Regexp // anchored full-match against relPath (dirs match with trailing '/')
	baseRe  *regexp.Regexp // slash-less patterns: anchored full-match against basename
}

// loadGitignore reads root/.gitignore and compiles its rules. Returns nil
// (no filtering) when the file is missing, unreadable, or has no rules.
func loadGitignore(root string) *gitignoreRules {
	data, err := os.ReadFile(root + string(os.PathSeparator) + ".gitignore") // #nosec G304 -- root is bounds-checked by the server
	if err != nil {
		return nil
	}

	rules := &gitignoreRules{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			line = line[1:]
		}

		if line == "" {
			continue
		}

		dirOnly := strings.HasSuffix(line, "/")
		if dirOnly {
			line = strings.TrimSuffix(line, "/")
		}

		// A leading '/' anchors the pattern to the search root; drop the
		// marker itself (the anchored regex is already root-relative).
		anchored := strings.HasPrefix(line, "/")
		line = strings.TrimPrefix(line, "/")

		if line == "" {
			continue
		}

		p := ignorePattern{negate: negate, dirOnly: dirOnly}

		glob := globToRegex(line)

		if anchored || strings.Contains(line, "/") {
			p.re = regexp.MustCompile("^" + glob + "/?$")
		} else {
			p.baseRe = regexp.MustCompile("^" + glob + "/?$")
		}

		rules.patterns = append(rules.patterns, p)
	}

	if len(rules.patterns) == 0 {
		return nil
	}

	return rules
}

// globToRegex converts a gitignore glob into a regex fragment matching a
// full path component sequence. '*' matches within a component, '**' across
// components, '?' a single char, '[...]' a class ('!' normalized to '^').
func globToRegex(pat string) string {
	var b strings.Builder

	for i := 0; i < len(pat); i++ {
		c := pat[i]

		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				i++
				// '**' matches zero or more path components: consume a
				// following '/' so 'a/**/b' also matches 'a/b'.
				if i+1 < len(pat) && pat[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}

		case '?':
			b.WriteString("[^/]")

		case '[':
			j := i + 1
			if j < len(pat) && pat[j] == '!' {
				j++
			}
			if j < len(pat) && pat[j] == ']' {
				j++
			}
			for j < len(pat) && pat[j] != ']' {
				j++
			}
			if j >= len(pat) {
				b.WriteString(`\[`)
			} else {
				class := pat[i : j+1]
				if strings.HasPrefix(class, "[!") {
					class = "[^" + class[2:]
				}
				b.WriteString(class)
				i = j
			}

		case '\\':
			if i+1 < len(pat) {
				i++
				b.WriteString(regexp.QuoteMeta(string(pat[i])))
			} else {
				b.WriteString(`\\`)
			}

		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}

	return b.String()
}

// ignores reports whether rel (root-relative path) is excluded. Directories
// are passed with a trailing slash appended, per git convention. Last
// matching pattern wins (negations re-include).
func (g *gitignoreRules) ignores(rel string, isDir bool) bool {
	if g == nil {
		return false
	}

	s := rel
	if isDir {
		s += "/"
	}

	base := path.Base(rel)
	ignored := false

	for _, p := range g.patterns {
		if p.dirOnly && !isDir {
			continue
		}

		switch {
		case p.re != nil && p.re.MatchString(s):
			ignored = !p.negate
		case p.baseRe != nil && p.baseRe.MatchString(base):
			ignored = !p.negate
		}
	}

	return ignored
}
