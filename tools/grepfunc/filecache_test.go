package grepfunc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// agedFile writes a file and back-dates it so the stability window lets it cache.
func agedFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	return path
}

func statOf(t *testing.T, path string) (int64, time.Time) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Size(), info.ModTime()
}

func TestFileCacheReusesUnchangedFile(t *testing.T) {
	dir := t.TempDir()
	path := agedFile(t, dir, "a.go", "package p\n")
	cache := newFileCache(1 << 20)

	size, mod := statOf(t, path)

	first, err := cache.read(path, size, mod)
	if err != nil {
		t.Fatal(err)
	}

	second, err := cache.read(path, size, mod)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) || string(second) != "package p\n" {
		t.Errorf("cache returned %q then %q", first, second)
	}

	if hits, misses, _ := cache.hits.Load(), cache.misses.Load(), cache.cachedBytes(); hits != 1 || misses != 1 {
		t.Errorf("hits=%d misses=%d, want 1 and 1", hits, misses)
	}
}

// A file written moments ago must always be re-read: that is the edit-then-search
// loop the cache would otherwise poison.
func TestFileCacheSkipsFreshFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.go")

	if err := os.WriteFile(path, []byte("package p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cache := newFileCache(1 << 20)
	size, mod := statOf(t, path)

	if _, err := cache.read(path, size, mod); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.read(path, size, mod); err != nil {
		t.Fatal(err)
	}

	if hits, misses := cache.hits.Load(), cache.misses.Load(); hits != 0 || misses != 2 {
		t.Errorf("hits=%d misses=%d, want 0 and 2 (fresh file is not cached)", hits, misses)
	}
}

func TestFileCacheInvalidatesOnChange(t *testing.T) {
	dir := t.TempDir()
	path := agedFile(t, dir, "b.go", "package p\n\nvar x = 1\n")
	cache := newFileCache(1 << 20)

	size, mod := statOf(t, path)

	if _, err := cache.read(path, size, mod); err != nil {
		t.Fatal(err)
	}

	agedFile(t, dir, "b.go", "package p\n\nvar y = 2\n")

	newSize, newMod := statOf(t, path)

	got, err := cache.read(path, newSize, newMod)
	if err != nil {
		t.Fatal(err)
	}

	if want := "package p\n\nvar y = 2\n"; string(got) != want {
		t.Errorf("stale content after change: %q, want %q", got, want)
	}
}

func TestFileCacheEvictsOldestAboveBudget(t *testing.T) {
	dir := t.TempDir()
	cache := newFileCache(32)

	for _, name := range []string{"one.go", "two.go", "three.go"} {
		path := agedFile(t, dir, name, "package p // padded to twenty four\n")
		size, mod := statOf(t, path)

		if _, err := cache.read(path, size, mod); err != nil {
			t.Fatal(err)
		}
	}

	if got := cache.cachedBytes(); got > 32 {
		t.Errorf("cache holds %d bytes, want <= 32", got)
	}

	first := filepath.Join(dir, "one.go")
	size, mod := statOf(t, first)

	if _, err := cache.read(first, size, mod); err != nil {
		t.Fatal(err)
	}

	if misses := cache.misses.Load(); misses != 4 {
		t.Errorf("misses=%d, want 4 (the oldest entry was evicted)", misses)
	}
}
