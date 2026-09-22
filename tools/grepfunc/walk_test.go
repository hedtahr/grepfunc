package grepfunc

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
)

func TestWalkDirSkipsGitignoredAndDotDirs(t *testing.T) {
	dir := t.TempDir()

	writeTree(t, dir, map[string]string{
		".gitignore":        "build/\n*.log\n",
		"keep.go":           "package t\n",
		"src/keep.go":       "package t\n",
		"src/skip.log":      "noise\n",
		"build/out.go":      "package t\n",
		".hidden/x.go":      "package t\n",
		"node_modules/x.go": "package t\n",
	})

	got := walkFiles(t, dir)

	// .gitignore itself is a file, and only dot-DIRECTORIES are pruned.
	want := []string{".gitignore", "keep.go", "src/keep.go"}
	if !slices.Equal(got, want) {
		t.Errorf("visited %v, want %v", got, want)
	}
}

// A dot-directory named as the root is still searched: the prune applies to
// what a walk finds inside itself, not to the directory the caller asked for.
func TestWalkDirSearchesExplicitDotRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".hidden")

	err := os.MkdirAll(dir, 0700)
	if err != nil {
		t.Fatal(err)
	}

	writeTree(t, dir, map[string]string{"x.go": "package t\n"})

	if got := walkFiles(t, dir); !slices.Equal(got, []string{"x.go"}) {
		t.Errorf("visited %v, want [x.go]", got)
	}
}

// walkFiles returns the root-relative (slash-normalised) files a WalkDir visits.
func walkFiles(t *testing.T, root string) []string {
	t.Helper()

	var got []string

	err := WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		got = append(got, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	sort.Strings(got)

	return got
}

// writeTree creates files and their parent directories under root; keys are
// slash-separated paths.
func writeTree(t *testing.T, root string, tree map[string]string) {
	t.Helper()

	for name, content := range tree {
		path := filepath.Join(root, filepath.FromSlash(name))

		err := os.MkdirAll(filepath.Dir(path), 0700)
		if err != nil {
			t.Fatal(err)
		}

		// #nosec G304 -- t.TempDir test fixture
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
