package grepfunc

import (
	"io/fs"
	"path/filepath"
)

// WalkDir walks root like filepath.WalkDir, but prunes the directories no tool
// wants (.git, node_modules, vendor, .idea, __pycache__, any dot-dir) and
// anything excluded by the root .gitignore, so every tool searches the same
// file set. fn is called only for entries that survive the prunes; walk errors
// are passed through unchanged. root itself is never pruned, so a dot-directory
// can still be searched when it is named explicitly.
func WalkDir(root string, fn fs.WalkDirFunc) error {
	gi := loadGitignore(root)

	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fn(path, entry, err)
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr == nil && rel != "." {
			if entry.IsDir() {
				if isSkippableDir(entry.Name()) || gi.ignores(rel, true) {
					return filepath.SkipDir
				}
			} else if gi.ignores(rel, false) {
				return nil
			}
		}

		return fn(path, entry, nil)
	})
}
