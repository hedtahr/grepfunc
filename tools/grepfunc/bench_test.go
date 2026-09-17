package grepfunc

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// benchTree writes files×funcs Go-ish functions; every tenth body has a marker.
func benchTree(tb testing.TB, dir string, files, funcs int) {
	tb.Helper()

	for f := range files {
		var buf strings.Builder

		buf.WriteString("package bench\n\n")

		for i := range funcs {
			fmt.Fprintf(&buf, "func fn%d_%d(ctx context.Context, req *Request) (*Response, error) {\n", f, i)

			for j := range 8 {
				if (i+j)%10 == 0 {
					buf.WriteString("\t// marker hit\n")
				}

				buf.WriteString("\tif req != nil {\n\t\treturn handle(req)\n\t}\n")
			}

			buf.WriteString("\treturn nil, nil\n}\n\n")
		}

		sub := filepath.Join(dir, fmt.Sprintf("pkg%d", f%8))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			tb.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%d.go", f)), []byte(buf.String()), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

func benchLines(funcs, bodyLines int) [][]byte {
	var buf strings.Builder

	buf.WriteString("package bench\n\n")

	for i := range funcs {
		fmt.Fprintf(&buf, "func fn%d(ctx context.Context, req *Request) (*Response, error) {\n", i)

		for j := range bodyLines {
			if (i+j)%10 == 0 {
				buf.WriteString("\t// marker hit\n")
			}

			buf.WriteString("\tif req != nil {\n\t\treturn handle(req)\n\t}\n")
		}

		buf.WriteString("\treturn nil, nil\n}\n\n")
	}

	return toLines([]byte(buf.String()))
}

func BenchmarkSearchTree(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 120, 30)

	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		results, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) == 0 {
			b.Fatal("no results")
		}
	}
}

// names_only callers never read Body, so the walk skips joining it.
func BenchmarkSearchNamesTree(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 120, 30)

	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		results, err := SearchNames(dir, "**/*.go", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) == 0 {
			b.Fatal("no results")
		}
	}
}

// The scan-heavy case: every file is read and scanned, nothing matches.
func BenchmarkSearchTreeNoMatch(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 120, 30)

	pattern := regexp.MustCompile(`no_such_symbol_anywhere`)

	b.ReportAllocs()

	for b.Loop() {
		results, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) != 0 {
			b.Fatal("unexpected match")
		}
	}
}

// ageTree back-dates every file so the read cache is allowed to hold it.
func ageTree(tb testing.TB, dir string) {
	tb.Helper()

	old := time.Now().Add(-time.Hour)

	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // best-effort ageing for the benchmark
		}

		return os.Chtimes(path, old, old)
	})
}

// Repeat queries over an unchanged repo: the read cache should skip every read.
func BenchmarkSearchTreeWarmCache(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 120, 30)
	ageTree(b, dir)

	pattern := regexp.MustCompile(`marker hit`)

	if _, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()

	for b.Loop() {
		results, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) == 0 {
			b.Fatal("no results")
		}
	}
}

// Full scans over an unchanged repo: the read cache is worth most here.
func BenchmarkSearchTreeNoMatchWarmCache(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 120, 30)
	ageTree(b, dir)

	pattern := regexp.MustCompile(`no_such_symbol_anywhere`)

	if _, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()

	for b.Loop() {
		results, err := Search(dir, "**/*.go", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) != 0 {
			b.Fatal("unexpected match")
		}
	}
}

// Evidence for staying on a byte loop instead of SIMD primitives: the per-call
// cost dominates on the short lines real code is made of, and even on long lines a
// single-needle vector search only beats the loop once its overhead is amortised.
// The vector variants also cannot lex (they count braces inside strings/comments).
func BenchmarkLineScanAlternatives(b *testing.B) {
	shortLines := benchLines(2000, 10)

	longLine := []byte(strings.Repeat("\tjson := map[string]any{\"k\": \"v\"}; ", 80))

	const specials = "{}`\"'/"

	open, closeB := []byte("{"), []byte("}")

	b.Run("short/loop", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			braces := 0

			for _, line := range shortLines {
				o, c := braceDelta(line)
				braces += o + c
			}

			if braces == 0 {
				b.Fatal("no braces")
			}
		}
	})

	b.Run("short/count", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			braces := 0

			for _, line := range shortLines {
				braces += bytes.Count(line, open) + bytes.Count(line, closeB)
			}

			if braces == 0 {
				b.Fatal("no braces")
			}
		}
	})

	b.Run("short/indexany", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			found := 0

			for _, line := range shortLines {
				for i := 0; i < len(line); {
					idx := bytes.IndexAny(line[i:], specials)
					if idx < 0 {
						break
					}

					found++
					i += idx + 1
				}
			}

			if found == 0 {
				b.Fatal("no specials")
			}
		}
	})

	b.Run("long/loop", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if o, c := braceDelta(longLine); o+c == 0 {
				b.Fatal("no braces")
			}
		}
	})

	b.Run("long/count", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if bytes.Count(longLine, open)+bytes.Count(longLine, closeB) == 0 {
				b.Fatal("no braces")
			}
		}
	})

	b.Run("long/indexany", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			found := 0

			for i := 0; i < len(longLine); {
				idx := bytes.IndexAny(longLine[i:], specials)
				if idx < 0 {
					break
				}

				found++
				i += idx + 1
			}

			if found == 0 {
				b.Fatal("no specials")
			}
		}
	})
}

func BenchmarkMapBlockBoundaries(b *testing.B) {
	lines := benchLines(2000, 10)

	b.ReportAllocs()

	for b.Loop() {
		_ = mapBlockBoundaries(lines, IsFuncSig)
	}
}

// benchPaths is a representative spread of root-relative paths to match.
func benchPaths() []string {
	paths := make([]string, 0, 64)

	for i := range 32 {
		paths = append(paths, fmt.Sprintf("pkg%d/sub/util_%d.go", i%8, i))
		paths = append(paths, fmt.Sprintf("pkg%d/sub/data_%d.sql", i%8, i))
	}

	return paths
}

func BenchmarkMatchGlob(b *testing.B) {
	paths := benchPaths()

	b.ReportAllocs()

	for b.Loop() {
		hits := 0

		for _, path := range paths {
			if MatchGlob("pkg*/**/*.go", path) {
				hits++
			}
		}

		if hits == 0 {
			b.Fatal("no matches")
		}
	}
}

// The shape used by walkers: compile once, match every file.
func BenchmarkCompiledGlob(b *testing.B) {
	paths := benchPaths()
	matcher := CompileGlob("pkg*/**/*.go")

	b.ReportAllocs()

	for b.Loop() {
		hits := 0

		for _, path := range paths {
			if matcher.Match(path) {
				hits++
			}
		}

		if hits == 0 {
			b.Fatal("no matches")
		}
	}
}

// benchGoData is a parseable Go file with 2000 functions.
func benchGoData() []byte {
	return bytes.Join(benchLines(2000, 10), []byte("\n"))
}

// Symbol extraction through go/parser (what .go files use).
func BenchmarkGoParserSymbols(b *testing.B) {
	data := benchGoData()
	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		results, ok := parseGoSymbols("f.go", data, pattern, 50, IsFuncSig, true)
		if !ok || len(results) == 0 {
			b.Fatal("parser produced no symbols")
		}
	}
}

// Symbol extraction through the brace scanner, on the same content.
func BenchmarkBraceScannerSymbols(b *testing.B) {
	data := benchGoData()
	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		if len(braceBlocks(data, pattern, 50, IsFuncSig, true)) == 0 {
			b.Fatal("scanner produced no symbols")
		}
	}
}
