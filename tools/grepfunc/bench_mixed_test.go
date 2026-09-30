package grepfunc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The unrestricted "*" glob is what tool callers get by default, and it is the
// case the extension filters exist for: a noise file must cost a name check, not
// a stat and a read. This tree is half noise (.png/.md/.json) by file count.
func BenchmarkSearchDefaultGlobMixedTree(b *testing.B) {
	dir := b.TempDir()
	benchTree(b, dir, 60, 20)

	noise := []byte(strings.Repeat("noise\n", 512))

	for i := range 60 {
		sub := filepath.Join(dir, fmt.Sprintf("pkg%d", i%8))

		if err := os.MkdirAll(sub, 0o755); err != nil {
			b.Fatal(err)
		}

		for _, name := range []string{"img%d.png", "doc%d.md", "data%d.json"} {
			if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf(name, i)), noise, 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}

	ageTree(b, dir)

	pattern := regexp.MustCompile(`marker hit`)

	if _, err := Search(dir, "*", pattern, 50, IsFuncSig); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()

	for b.Loop() {
		results, err := Search(dir, "*", pattern, 50, IsFuncSig)
		if err != nil {
			b.Fatal(err)
		}

		if len(results) == 0 {
			b.Fatal("no results")
		}
	}
}
