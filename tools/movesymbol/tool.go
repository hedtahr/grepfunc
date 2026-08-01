// Package movesymbol moves a named symbol between files.
package movesymbol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

const (
	typeString       = "string"
	maxSearchResults = 10
	newFileMode      = 0600
	maxFileSize      = 2 * 1024 * 1024
)

var (
	errNameRequired   = errors.New("name is required")
	errSymbolNotFound = errors.New("symbol not found")
	errInvalidRange   = errors.New("invalid line range")
	errInsertLine     = errors.New("insert line must be >= 1")
	errDstLineHasCode = errors.New("dst line has code")
)

// Tool describes the move_symbol tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "move_symbol",
	Description: "Move a named symbol (function, method, struct, interface, enum, class) from a source file to a " +
		"destination file. Removes the symbol from src and inserts it into dst at the specified line (or appends if " +
		"no line given).",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name": {Type: typeString, Description: "Name of the symbol to move. Exact match.", Items: nil},
			"src": {
				Type:        typeString,
				Description: "Source file containing the symbol. Absolute path or project-relative.",
				Items:       nil,
			},
			"dst": {
				Type:        typeString,
				Description: "Destination file. Absolute path or project-relative.",
				Items:       nil,
			},
			"line": {
				Type:        "integer",
				Description: "Line number in dst to insert before. If omitted, appends to end of dst.",
				Items:       nil,
			},
			"dry_run": {Type: "boolean", Description: "Preview changes without writing files.", Items: nil},
		},
		Required:             []string{"name", "src", "dst"},
		AdditionalProperties: false,
	},
}

type args struct {
	Name   string `json:"name"`
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Line   int    `json:"line"`
	DryRun bool   `json:"dry_run"`
}

// Handle moves a symbol from src to dst.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	req, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}

	err = checkPaths(req.Src, req.Dst)
	if err != nil {
		return nil, err
	}

	pat, _ := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(req.Name) + `\b`)

	match, err := findSymbol(req.Src, req.Name, pat)
	if err != nil {
		return nil, err
	}

	srcData, err := os.ReadFile(req.Src)
	if err != nil {
		return nil, fmt.Errorf("read src: %w", err)
	}

	newSrc, err := removeLines(srcData, match.Line, match.EndLine)
	if err != nil {
		return nil, fmt.Errorf("remove from src: %w", err)
	}

	dstData, err := readDst(req.Dst)
	if err != nil {
		return nil, err
	}

	body := strings.TrimRight(match.Body, "\n") + "\n"

	insertLine, err := resolveInsertLine(toLines(dstData), req.Line)
	if err != nil {
		return nil, err
	}

	newDst, err := insertAt(dstData, body, insertLine)
	if err != nil {
		return nil, fmt.Errorf("insert into dst: %w", err)
	}

	return buildResult(req, match, newSrc, newDst, insertLine)
}

func parseArgs(raw json.RawMessage) (args, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return args{}, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Name == "" {
		return args{}, errNameRequired
	}

	req.Src = server.ResolvePath(req.Src)
	req.Dst = server.ResolvePath(req.Dst)

	return req, nil
}

func checkPaths(src, dst string) error {
	err := server.CheckBounds(src)
	if err != nil {
		return fmt.Errorf("check bounds %s: %w", src, err)
	}

	err = server.CheckBounds(dst)
	if err != nil {
		return fmt.Errorf("check bounds %s: %w", dst, err)
	}

	err = server.CheckBanned(src)
	if err != nil {
		return fmt.Errorf("check banned %s: %w", src, err)
	}

	err = server.CheckBanned(dst)
	if err != nil {
		return fmt.Errorf("check banned %s: %w", dst, err)
	}

	return nil
}

func findSymbol(path, name string, pat *regexp.Regexp) (*grepfunc.FuncMatch, error) {
	combined := func(line []byte) bool { return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line) }

	results, err := grepfunc.Search(path, "*", pat, maxSearchResults, combined)
	if err != nil {
		return nil, fmt.Errorf("search src: %w", err)
	}

	for i, r := range results {
		if strings.EqualFold(r.Name, name) {
			return &results[i], nil
		}
	}

	return nil, fmt.Errorf("%w: %q in %s", errSymbolNotFound, name, server.RelPath(path))
}

func readDst(path string) ([]byte, error) {
	// #nosec G304 -- paths bounds-checked by server
	data, err := os.ReadFile(path)
	if err != nil {
		// If creating, start empty.
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("read dst: %w", err)
	}

	return data, nil
}

func resolveInsertLine(dstLines [][]byte, requested int) (int, error) {
	if requested <= 0 {
		return len(dstLines) + 1, nil
	}

	if requested > len(dstLines) {
		return requested, nil
	}

	target := strings.TrimSpace(string(dstLines[requested-1]))
	if target != "" {
		return 0, fmt.Errorf(
			"%w %d: %q — refusing to replace. Pick a different line", errDstLineHasCode, requested, target)
	}

	return requested, nil
}

func buildResult(req args, match *grepfunc.FuncMatch, newSrc, newDst []byte, insertLine int) (
	*server.ToolCallResult, error,
) {
	var buf strings.Builder

	srcRel := server.RelPath(req.Src)
	dstRel := server.RelPath(req.Dst)
	fmt.Fprintf(&buf, "move_symbol %q:\n", req.Name)
	fmt.Fprintf(&buf, "  src: %s (remove L%d–L%d, %d lines)\n", srcRel, match.Line, match.EndLine, match.Lines)
	fmt.Fprintf(&buf, "  dst: %s (insert at L%d)\n", dstRel, insertLine)
	fmt.Fprintf(&buf, "  kind: %s\n", symbolKind(match.Body))

	if req.DryRun {
		buf.WriteString("\n[Dry run — no files changed]\n")
		buf.WriteString("\n--- src after removal ---\n")
		buf.Write(newSrc)
		buf.WriteString("\n--- dst after insert ---\n")
		buf.Write(newDst)
	} else {
		err := writeFilePreserveMode(req.Src, newSrc)
		if err != nil {
			return nil, fmt.Errorf("write src: %w", err)
		}

		err = writeFilePreserveMode(req.Dst, newDst)
		if err != nil {
			return nil, fmt.Errorf("write dst: %w", err)
		}

		buf.WriteString("\n[Done]\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		IsError: false,
	}, nil
}

func writeFilePreserveMode(path string, data []byte) error {
	mode := fs.FileMode(newFileMode)

	info, statErr := os.Stat(path)
	if statErr == nil {
		mode = info.Mode()
	}

	err := os.WriteFile(path, data, mode)
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

func insertAt(data []byte, text string, line int) ([]byte, error) {
	lines := toLines(data)

	if line < 1 {
		return nil, errInsertLine
	}

	var buf strings.Builder

	for i := range min(line-1, len(lines)) {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}

	if line <= len(lines)+1 {
		buf.WriteString(text)

		if !strings.HasSuffix(text, "\n") {
			buf.WriteByte('\n')
		}
	}

	for i := line - 1; i < len(lines); i++ {
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
