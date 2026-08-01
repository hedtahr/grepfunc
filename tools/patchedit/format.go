package patchedit

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// preFormat runs the formatter before edits so the diff shows only intended changes.
// It returns the formatted content and whether formatting changed anything.
func preFormat(path string, content []byte, ext string) ([]byte, bool) {
	cmd := formatCommand(path, ext)
	if cmd == nil {
		return content, false
	}

	cmd.Stdin = bytes.NewReader(content)

	var out bytes.Buffer

	cmd.Stdout = &out

	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return content, false
	}

	formatted := out.Bytes()
	if len(formatted) == 0 || bytes.Equal(formatted, content) {
		return content, false
	}

	return formatted, true
}

func formatCommand(path string, ext string) *exec.Cmd {
	switch ext {
	case ".go":
		// #nosec G204 -- fixed formatter binary
		return exec.CommandContext(context.Background(), "gofmt", path)
	case ".rs":
		// #nosec G204 -- fixed formatter binary
		return exec.CommandContext(context.Background(), "rustfmt", "--edition", "2021")
	case ".js", ".ts", ".jsx", ".tsx", ".json":
		// #nosec G204 -- fixed formatter binary
		return exec.CommandContext(context.Background(), "prettier", "--stdin-filepath", path)
	case ".py":
		// #nosec G204 -- fixed formatter binary
		return exec.CommandContext(context.Background(), "ruff", "format", "--stdin-filename", path, "-")
	default:
		return nil
	}
}
