// Package symbolat finds the symbol enclosing a line in a file.
package symbolat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var (
	errPathRequired = errors.New("path is required")
	errLineInvalid  = errors.New("line must be >= 1")
)

// Tool describes the symbol_at tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "symbol_at",
	Description: "Return the function/type enclosing a line — name, kind, start/end lines.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {Type: "string", Description: "File to look up. Absolute or project-relative.", Items: nil},
			"line": {Type: "integer", Description: "1-based line number.", Items: nil},
		},
		Required:             []string{"path", "line"},
		AdditionalProperties: false,
	},
}

type args struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

type candidate struct {
	startLine int
	endLine   int
	kind      string
}

// Handle returns the symbol enclosing the requested line.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Path == "" {
		return nil, errPathRequired
	}

	if req.Line <= 0 {
		return nil, errLineInvalid
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

	data, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %w", err)
	}

	lines := bytes.Split(data, []byte("\n"))
	lineIdx := req.Line - 1 // 0-indexed
	best := findBestMatch(lines, lineIdx)

	rel := server.RelPath(req.Path)
	if best == nil {
		text := fmt.Sprintf("line %d is not inside any symbol in %s", req.Line, rel)

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: text}},
			IsError: false,
		}, nil
	}

	sig := strings.TrimSpace(string(lines[best.startLine]))
	name := grepfunc.EnclosingSymbol(lines, lineIdx, grepfunc.MapBlockBoundaries(lines, sigFnForKind(best.kind)))

	text := fmt.Sprintf("symbol: %s\nkind: %s\nlines: %d–%d\nsig: %s\n",
		name, best.kind, best.startLine+1, best.endLine+1, sig)

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: text}},
		IsError: false,
	}, nil
}

func findBestMatch(lines [][]byte, lineIdx int) *candidate {
	var best *candidate

	for _, entry := range []struct {
		sigFn func([]byte) bool
		kind  string
	}{
		{grepfunc.IsFuncSig, "func"},
		{grepfunc.IsStructSig, "type"},
	} {
		boundaries := grepfunc.MapBlockBoundaries(lines, entry.sigFn)

		startLine, ok := boundaries.Start(lineIdx)
		if !ok {
			continue
		}

		endLine := boundaries.EndLine(startLine)

		c := &candidate{startLine: startLine, endLine: endLine, kind: entry.kind}
		// prefer innermost (largest startLine)
		if best == nil || c.startLine > best.startLine {
			best = c
		}
	}

	return best
}

func sigFnForKind(kind string) func([]byte) bool {
	if kind == "type" {
		return grepfunc.IsStructSig
	}

	return grepfunc.IsFuncSig
}
