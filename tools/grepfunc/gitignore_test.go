package grepfunc

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestLoadGitignoreIgnores(t *testing.T) {
	dir := t.TempDir()

	// #nosec G304 -- t.TempDir test fixture
	err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(
		"# comment\n\n"+
			"ignored_dir/\n"+
			"*.log\n"+
			"build/**/gen/\n"+
			"/anchored.txt\n"+
			"!keep.log\n"+
			"nested/path/\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	gi := loadGitignore(dir)
	if gi == nil {
		t.Fatal("expected rules, got nil")
	}

	cases := []struct {
		rel     string
		isDir   bool
		ignored bool
	}{
		{"ignored_dir", true, true},
		{"src/ignored_dir", true, true},
		// File under an ignored dir: the walker prunes the dir before ever
		// visiting the file, so the direct rule call legitimately matches only
		// the dir entry.
		{"src/ignored_dir/file.go", false, false},
		{"main.go", false, false},
		{"debug.log", false, true},
		{"src/debug.log", false, true},
		{"keep.log", false, false}, // negated
		{"src/keep.log", false, false},
		{"build/gen", true, true},
		{"build/x/gen", true, true},
		{"build/genfile.go", false, false}, // 'gen/' only, not 'gen' file
		{"anchored.txt", false, true},
		{"src/anchored.txt", false, false}, // anchored to root only
		{"nested/path", true, true},
	}

	for _, c := range cases {
		got := gi.ignores(c.rel, c.isDir)
		if got != c.ignored {
			t.Errorf("ignores(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.ignored)
		}
	}
}

func TestLoadGitignoreMissing(t *testing.T) {
	if gi := loadGitignore(t.TempDir()); gi != nil {
		t.Errorf("expected nil for missing .gitignore, got %v", gi)
	}
}

func TestSearchSkipsGitignoredDirs(t *testing.T) {
	dir := t.TempDir()

	// #nosec G304 -- t.TempDir test fixture
	err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("vendor/\n*.min.js\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "vendor", "dep"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "src"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	files := []string{
		"src/app.go",
		"vendor/dep/dep.go",
		"vendor.min.js",
		"src/snippet.min.js",
	}

	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("package t\nfunc Hit() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	re := regexp.MustCompile(`func Hit`)
	results, err := Search(dir, "*", re, 100, IsFuncSig)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (only src/app.go)", len(results))
	}

	if results[0].File != filepath.Join(dir, "src", "app.go") {
		t.Errorf("unexpected match file: %s", results[0].File)
	}
}
