package server

import (
	"os"
	"path/filepath"
	"strings"
)

// EnsureGitignore adds a .llm/ entry to the project .gitignore so local
// state stays local. Shared by the memory and bookmark stores.
func EnsureGitignore(root string) {
	gitignorePath := filepath.Join(root, ".gitignore")

	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		data = nil
	}

	if strings.Contains(string(data), ".llm") {
		return
	}

	// #nosec G304 -- paths bounds-checked by server
	file, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePermPrivate)
	if err != nil {
		return
	}

	defer func() { _ = file.Close() }()

	if len(data) > 0 && data[len(data)-1] != '\n' {
		_, _ = file.WriteString("\n")
	}

	_, _ = file.WriteString(".llm/\n")
}
