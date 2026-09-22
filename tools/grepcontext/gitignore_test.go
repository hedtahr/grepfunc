package grepcontext

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// A gitignored directory is not searched: the walk comes from grepfunc.WalkDir,
// shared with the other grep-family tools.
func TestScanWindowsSkipsGitignoredDirs(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		".gitignore":   "build/\n",
		"src/app.go":   "package t\n// hit here\n",
		"build/gen.go": "package t\n// hit here\n",
	}

	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))

		err := os.MkdirAll(filepath.Dir(path), 0700)
		if err != nil {
			t.Fatal(err)
		}

		// #nosec G304 -- t.TempDir test fixture
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	windows, _, err := scanWindows(
		args{Path: dir, Include: "*", ContextLines: 1},
		regexp.MustCompile("hit here"), dir, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1 (build/ is gitignored)", len(windows))
	}

	if got := filepath.ToSlash(windows[0].relPath); got != "src/app.go" {
		t.Errorf("searched %q, want src/app.go", got)
	}
}
