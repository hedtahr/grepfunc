package grepfunc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	lines := toLines(data)
	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		if len(braceBlocks(lines, pattern, 50, IsFuncSig, true)) == 0 {
			b.Fatal("scanner produced no symbols")
		}
	}
}
