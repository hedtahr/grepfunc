// Package deletesymbol removes a named symbol from a file.
package deletesymbol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const maxSearchResults = 10

var (
	errNameRequired   = errors.New("name is required")
	errSymbolNotFound = errors.New("symbol not found")
	errInvalidRange   = errors.New("invalid line range")
)

// Tool describes the delete_symbol tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "delete_symbol",
	Description: "Delete a named symbol (function, method, struct, interface, enum, class) from a file. " +
		"Removes the full body including all nested scopes.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name": {
				Type:        "string",
				Description: "Name of the symbol to delete. Exact match (case-insensitive).",
				Items:       nil,
			},
			"path": {
				Type:        "string",
				Description: "File containing the symbol. Absolute path or project-relative.",
				Items:       nil,
			},
			"dry_run": {Type: "boolean", Description: "Preview the file after deletion without writing.", Items: nil},
		},
		Required:             []string{"name", "path"},
		AdditionalProperties: false,
	},
}

type args struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run"`
}

// Handle deletes a symbol from a file.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Name == "" {
		return nil, errNameRequired
	}

	req.Path = server.ResolvePath(req.Path)

	err = server.CheckBounds(req.Path)
	if err != nil {
		return nil, fmt.Errorf("check bounds: %w", err)
	}

	err = server.CheckBanned(req.Path)
	if err != nil {
		return nil, fmt.Errorf("check banned: %w", err)
	}

	pat, _ := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(req.Name) + `\b`)

	match, err := findSymbol(req.Path, req.Name, pat)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	newData, err := removeLines(data, match.Line, match.EndLine)
	if err != nil {
		return nil, fmt.Errorf("remove lines: %w", err)
	}

	return buildResult(req, match, newData)
}

func findSymbol(path, name string, pat *regexp.Regexp) (*grepfunc.FuncMatch, error) {
	combined := func(line []byte) bool { return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line) }

	results, err := grepfunc.Search(path, "*", pat, maxSearchResults, combined)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	for i, r := range results {
		if strings.EqualFold(r.Name, name) {
			return &results[i], nil
		}
	}

	return nil, fmt.Errorf("%w: %q in %s", errSymbolNotFound, name, server.RelPath(path))
}

func buildResult(req args, match *grepfunc.FuncMatch, newData []byte) (*server.ToolCallResult, error) {
	rel := server.RelPath(req.Path)

	var buf strings.Builder

	fmt.Fprintf(&buf, "delete_symbol %q:\n", req.Name)
	fmt.Fprintf(&buf, "  file: %s (remove L%d–L%d, %d lines)\n", rel, match.Line, match.EndLine, match.Lines)
	fmt.Fprintf(&buf, "  kind: %s\n", symbolKind(match.Body))

	if req.DryRun {
		buf.WriteString("\n[Dry run — no changes]\n")
		buf.WriteString("\n--- after deletion ---\n")
		buf.Write(newData)
	} else {
		err := writeFilePreserveMode(req.Path, newData)
		if err != nil {
			return nil, fmt.Errorf("write file: %w", err)
		}

		buf.WriteString("\n[Done]\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		IsError: false,
	}, nil
}

func writeFilePreserveMode(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	err = os.WriteFile(path, data, info.Mode())
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}

func symbolKind(body string) string {
	first := []byte(body)
	if before, _, ok := strings.Cut(body, "\n"); ok {
		first = []byte(before)
	}

	if grepfunc.IsStructSig(first) {
		return "type"
	}

	return "func"
}

func removeLines(data []byte, start, end int) ([]byte, error) {
	lines := toLines(data)
	if start < 1 || end > len(lines) || start > end {
		return nil, fmt.Errorf("%w: L%d–L%d (file has %d lines)", errInvalidRange, start, end, len(lines))
	}

	var buf strings.Builder
	for i := range start - 1 {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}

	for i := end; i < len(lines); i++ {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}

	return []byte(buf.String()), nil
}

func toLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}

	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	parts := strings.Split(raw, "\n")

	result := make([][]byte, len(parts))
	for i, p := range parts {
		result[i] = []byte(p)
	}

	return result
}
