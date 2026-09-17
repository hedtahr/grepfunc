package grepfunc

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"
)

// symbolKey identifies a symbol by name and line range.
type symbolKey struct {
	name  string
	line  int
	end   int
	lines int
}

func symbolKeys(matches []FuncMatch) []symbolKey {
	keys := make([]symbolKey, 0, len(matches))

	for _, m := range matches {
		keys = append(keys, symbolKey{name: m.Name, line: m.Line, end: m.EndLine, lines: m.Lines})
	}

	sort.Slice(keys, func(i, j int) bool { return keys[i].line < keys[j].line })

	return keys
}

// Divergence baseline, counted over this repo's Go files (58 files). It is zero:
// the scanner and go/parser agree exactly on the corpus. Treat it as an
// invariant, not a target — a non-zero count means one of these came back:
//   - symbols invented inside a multi-line raw string or block comment (needs the
//     cross-line lexical state carried by scanLine);
//   - a wrapped signature's continuation line taken for a signature of its own
//     (guarded by awaitingKeywordSignature);
//   - a multi-line call taken for a declaration (guarded by the dot-qualified
//     check in looksLikeFuncStart);
//   - a name that ignores generic parameters (trimTypeParams).
//
// Investigate before raising: the failure output lists every diverging symbol.
const (
	maxScannerOnlySymbols = 0
	maxParserOnlySymbols  = 0
)

// TestBraceScannerMatchesGoParser uses go/parser as an oracle. The scanner runs in
// production because it is ~4x faster and ~40x lighter (see the symbol
// benchmarks), so it must not drift further from exact parsing.
func TestBraceScannerMatchesGoParser(t *testing.T) {
	matchAll := regexp.MustCompile(`(?s).`)

	var files, scannerOnly, parserOnly int

	err := filepath.WalkDir("../..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil //nolint:nilerr // skip unreadable entries, the oracle is best-effort
		}

		data, readErr := os.ReadFile(path) // #nosec G304 -- repo-local test corpus
		if readErr != nil {
			return nil
		}

		parsed, ok := parseGoSymbols(path, data, matchAll, oracleLimit, IsFuncSig, false)
		if !ok {
			return nil // not valid Go right now (mid-edit), nothing to compare
		}

		scanned := braceBlocks(toLines(data), matchAll, oracleLimit, IsFuncSig, false)

		files++

		oracle := symbolKeys(parsed)
		fromScanner := symbolKeys(scanned)

		for _, key := range fromScanner {
			if !slices.Contains(oracle, key) {
				scannerOnly++

				t.Logf("%s: scanner-only %s L%d-%d", path, key.name, key.line, key.end)
			}
		}

		for _, key := range oracle {
			if !slices.Contains(fromScanner, key) {
				parserOnly++

				t.Logf("%s: parser-only %s L%d-%d", path, key.name, key.line, key.end)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if files == 0 {
		t.Fatal("no Go files compared")
	}

	t.Logf("compared %d files: scanner-only %d, parser-only %d", files, scannerOnly, parserOnly)

	if scannerOnly > maxScannerOnlySymbols {
		t.Errorf("scanner invents %d symbols the parser does not have (baseline %d)", scannerOnly, maxScannerOnlySymbols)
	}

	if parserOnly > maxParserOnlySymbols {
		t.Errorf("scanner misses %d symbols the parser finds (baseline %d)", parserOnly, maxParserOnlySymbols)
	}
}

const oracleLimit = 1 << 20

// The shapes the tools rely on must agree exactly, boundaries and bodies included.
// Allman style is excluded: it is not valid Go (the parser rejects `func Foo()`
// with a brace on the next line), only other languages use it.
// Closures are excluded by design: the scanner reports the innermost block that
// owns a match, go/parser reports every symbol whose text contains it.
func TestGoParserAndScannerAgreeOnFixtures(t *testing.T) {
	fixtures := map[string]string{
		"generics.go":    "package p\n\nfunc Process[T any](items []T, fn func(T) T) []T {\n\tout := make([]T, len(items))\n\treturn out\n}\n",
		"oner.go":        "package p\n\nfunc Foo() int { return 1 }\nfunc Bar() int { return 2 }\n",
		"rawstring.go":   "package p\n\nfunc Braces() string {\n\treturn `a { b } c`\n}\n",
		"method.go":      "package p\n\nfunc (s *Server) Write(v int) error {\n\treturn s.write(v)\n}\n",
		"generictype.go": "package p\n\ntype List[T any] struct {\n\titems []T\n}\n",
	}

	pattern := regexp.MustCompile(`(?s).`)

	for name, code := range fixtures {
		t.Run(name, func(t *testing.T) {
			data := []byte(code)

			parsed, ok := parseGoSymbols(name, data, pattern, oracleLimit, IsFuncSig, true)
			if !ok {
				t.Fatalf("%s: oracle could not parse the fixture", name)
			}

			var oracle []symbolKey

			for _, key := range symbolKeys(parsed) {
				if key.name != "" && key.name != "<fn>" && key.name != "func" {
					oracle = append(oracle, key)
				}
			}

			scanned := braceBlocks(toLines(data), pattern, oracleLimit, IsFuncSig, true)

			var fromScanner []symbolKey

			for _, key := range symbolKeys(scanned) {
				if key.name != "" && key.name != "<fn>" && key.name != "func" {
					fromScanner = append(fromScanner, key)
				}
			}

			if fmt.Sprint(fromScanner) != fmt.Sprint(oracle) {
				t.Errorf("scanner %s\noracle  %s", fmt.Sprint(fromScanner), fmt.Sprint(oracle))
			}
		})
	}
}
