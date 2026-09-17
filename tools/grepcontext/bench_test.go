package grepcontext

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// benchData builds a Go-ish file with funcs×bodyLines lines, marking 1 in 10 bodies.
func benchData(funcs, bodyLines int) []byte {
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

	return []byte(buf.String())
}

func BenchmarkMatchWindows(b *testing.B) {
	data := benchData(2000, 10)
	arg := args{ContextLines: 3, MaxResults: 20}
	pattern := regexp.MustCompile(`marker hit`)

	b.ReportAllocs()

	for b.Loop() {
		if len(matchWindows(data, "pkg/f.go", arg, pattern, 20)) == 0 {
			b.Fatal("no windows")
		}
	}
}

// The common case: a file with no match must not be parsed line by line.
func BenchmarkMatchWindowsNoHit(b *testing.B) {
	data := benchData(2000, 10)
	arg := args{ContextLines: 3, MaxResults: 20}
	pattern := regexp.MustCompile(`no_such_marker`)

	b.ReportAllocs()

	for b.Loop() {
		_ = matchWindows(data, "pkg/f.go", arg, pattern, 20)
	}
}
