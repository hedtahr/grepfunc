package grepfunc

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// mojoSource is current Mojo (def only — fn is a parse error now) with
// docstrings that must not become symbols.
const mojoSource = `"""Module summary with a fake signature:

def not_real(x: Int) -> Int:
    return x
"""

struct Point(Copyable, Movable):
    """A 2D point."""

    var x: Float64
    var y: Float64

    def __init__(out self, x: Float64, y: Float64):
        """Construct a point."""
        self.x = x
        self.y = y

    def distance_to(self, other: Point) -> Float64:
        """Distance to another point."""
        var dx = self.x - other.x
        var dy = self.y - other.y
        return (dx * dx + dy * dy) ** 0.5

def main() raises:
    var p = Point(1.0, 2.0)
    print(p.distance_to(Point(4.0, 6.0)))
`

// Wrapped compile-time parameter lists leave the declaration line without a
// paren (functions) or without a colon (types); both must still be found.
func TestWrappedMojoSignatures(t *testing.T) {
	for _, sig := range []string{"def _stored[", "def plan_batches["} {
		if !IsFuncSig([]byte(sig)) {
			t.Errorf("IsFuncSig(%q) = false, want true", sig)
		}
	}

	for _, sig := range []string{
		"origin: Origin",
		"](mut reader: BitReader[origin], mut out: Out) -> Bool:",
		"mojo = \"*\"",
	} {
		if IsFuncSig([]byte(sig)) {
			t.Errorf("IsFuncSig(%q) = true, want false", sig)
		}
	}

	for _, sig := range []string{"struct Grid[", "trait Sized[", "pub struct Wrapped["} {
		if !IsStructSig([]byte(sig)) {
			t.Errorf("IsStructSig(%q) = false, want true", sig)
		}
	}

	if IsStructSig([]byte("struct Grid")) {
		t.Error("IsStructSig(\"struct Grid\") = true, want false")
	}
}

func TestIsIndentExt(t *testing.T) {
	tests := map[string]bool{
		".py": true, ".pyi": true, ".pyx": true, ".mojo": true, ".🔥": true,
		".go": false, ".rs": false, "": false,
	}

	for ext, want := range tests {
		if got := isIndentExt(ext); got != want {
			t.Errorf("isIndentExt(%q) = %v, want %v", ext, got, want)
		}
	}
}

func TestMojoSymbols(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "sample.mojo", mojoSource)

	funcs, types, err := SearchBoth(dir, "*.mojo", regexp.MustCompile("."), 50)
	if err != nil {
		t.Fatal(err)
	}

	names := matchNames(funcs)
	for _, want := range []string{"__init__", "distance_to", "main"} {
		if !slices.Contains(names, want) {
			t.Errorf("functions %q missing %q", names, want)
		}
	}

	if slices.Contains(names, "not_real") {
		t.Errorf("docstring example leaked into functions: %q", names)
	}

	if got := matchNames(types); !slices.Contains(got, "Point") {
		t.Errorf("types %q missing %q", got, "Point")
	}
}

// The docstring guard also covers Python, which shares the indent scanner.
func TestPythonDocstringIsNotASymbol(t *testing.T) {
	dir := t.TempDir()

	pySource := `def real() -> int:
    """Example:

    def phantom(a):
        return a
    """
    return 1
`

	writeFixture(t, dir, "sample.py", pySource)

	funcs, _, err := SearchBoth(dir, "*.py", regexp.MustCompile("."), 50)
	if err != nil {
		t.Fatal(err)
	}

	names := matchNames(funcs)
	if !slices.Contains(names, "real") {
		t.Errorf("functions %q missing %q", names, "real")
	}

	if slices.Contains(names, "phantom") {
		t.Errorf("docstring example leaked into functions: %q", names)
	}
}

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600)
	if err != nil {
		t.Fatal(err)
	}
}

func matchNames(matches []FuncMatch) []string {
	names := make([]string, 0, len(matches))

	for _, m := range matches {
		names = append(names, m.Name)
	}

	return names
}

// TestMojoCorpusRealTree scans a real Mojo tree when MOJO_CORPUS points at one,
// skipped otherwise. It is a recall check on production code: every module-level
// declaration in the tree must come back as a symbol. Docstring examples are
// indented, so a column-zero match is a declaration, not prose.
func TestMojoCorpusRealTree(t *testing.T) {
	dir := os.Getenv("MOJO_CORPUS")
	if dir == "" {
		t.Skip("MOJO_CORPUS not set")
	}

	funcs, types, err := SearchBoth(dir, "**/*.mojo", regexp.MustCompile("."), 1<<16)
	if err != nil {
		t.Fatal(err)
	}

	found := make(map[string]bool, len(funcs)+len(types))
	for _, group := range [][]FuncMatch{funcs, types} {
		for _, m := range group {
			found[filepath.Base(m.File)+":"+m.Name] = true
		}
	}

	declRe := regexp.MustCompile(`^(?:def|fn|struct|trait)\s+([A-Za-z_][A-Za-z0-9_]*)`)

	var files, missed int

	walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".mojo" {
			return nil //nolint:nilerr // best-effort corpus walk
		}

		data, readErr := os.ReadFile(path) // #nosec G304 -- the corpus the caller pointed at
		if readErr != nil {
			return nil
		}

		files++

		for line := range strings.SplitSeq(string(data), "\n") {
			match := declRe.FindStringSubmatch(line)
			if match == nil {
				continue
			}

			if !found[filepath.Base(path)+":"+match[1]] {
				missed++

				if missed <= 10 {
					t.Errorf("missed declaration %s: %s", path, match[1])
				}
			}
		}

		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	t.Logf("%d files, %d funcs, %d types, %d missed declarations", files, len(funcs), len(types), missed)

	if len(funcs) == 0 {
		t.Error("no functions found in a real Mojo tree")
	}
}
