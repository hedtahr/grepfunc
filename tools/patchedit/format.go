package patchedit

import (
	"bytes"
	"os"
	"os/exec"
)

// preFormat runs formatter BEFORE edits so diff shows only intended changes.
// Returns formatted content and whether formatting changed anything.
func preFormat(path string, content []byte, ext string) ([]byte, bool, error) {
	var cmd *exec.Cmd
	switch ext {
	case ".go":
		cmd = exec.Command("gofmt", path)
	case ".rs":
		cmd = exec.Command("rustfmt", "--edition", "2021")
	case ".js", ".ts", ".jsx", ".tsx", ".json":
		cmd = exec.Command("prettier", "--stdin-filepath", path)
	case ".py":
		cmd = exec.Command("ruff", "format", "--stdin-filename", path, "-")
	default:
		return content, false, nil
	}
	if cmd == nil {
		return content, false, nil
	}
	if _, err := exec.LookPath(cmd.Path); err != nil {
		return content, false, nil // formatter not installed
	}

	cmd.Stdin = bytes.NewReader(content)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return content, false, nil // best-effort
	}

	formatted := out.Bytes()
	if len(formatted) == 0 || bytes.Equal(formatted, content) {
		return content, false, nil
	}
	return formatted, true, nil
}
