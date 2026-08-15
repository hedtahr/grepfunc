// Package writefile creates or fully overwrites a file.
package writefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

const newFileMode = os.FileMode(0644)

var errPathRequired = errors.New("path is required")

// Tool describes the write_file tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "write_file",
	Description: "Create or fully overwrite a file. Prefer patch_file for surgical edits.",
	InputSchema: server.InputSchema{
		Type:                 "object",
		AdditionalProperties: false,
		Properties: map[string]server.Property{
			"path": {
				Type:        "string",
				Description: "File to create or overwrite. Absolute or project-relative.",
				Items:       nil,
			},
			"content": {
				Type:        "string",
				Description: "Full new file contents.",
				Items:       nil,
			},
		},
		Required: []string{"path", "content"},
	},
}

type args struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Handle writes full contents to path, preserving the mode of existing files.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Path == "" {
		return nil, errPathRequired
	}

	path := server.ResolvePath(req.Path)

	err = server.CheckBounds(path)
	if err != nil {
		return nil, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(path)
	if err != nil {
		return nil, fmt.Errorf("check banned: %w", err)
	}

	mode := newFileMode

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil && info.IsDir():
		return nil, fmt.Errorf("path is a directory, not a file: %q", path)
	case statErr == nil:
		mode = info.Mode()
	}

	parent := filepath.Dir(path)

	parentInfo, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("parent directory missing: %w", err)
	}

	if !parentInfo.IsDir() {
		return nil, fmt.Errorf("parent path is not a directory: %q", parent)
	}

	err = os.WriteFile(path, []byte(req.Content), mode)
	if err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	server.SetLastPath(path)

	var buf strings.Builder

	fmt.Fprintf(&buf, "Wrote %s (%d bytes, mode %s)\n", server.RelPath(path), len(req.Content), mode)

	if len(req.Content) == 0 {
		buf.WriteString("Note: file is now empty — consider delete_path instead.\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		IsError: false,
	}, nil
}
