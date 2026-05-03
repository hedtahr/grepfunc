package symbolat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "symbol_at",
	Description: "Given a file path and line number, return the enclosing function or type — name, kind, start/end lines. Eliminates the 'what symbol is this line inside?' round-trip.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {Type: "string", Description: "MUST be absolute path to file."},
			"line": {Type: "integer", Description: "1-based line number to look up."},
		},
		Required: []string{"path", "line"},
	},
}

type args struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if a.Line <= 0 {
		return nil, fmt.Errorf("line must be >= 1")
	}
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBounds(a.Path); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot read file: %v", err)
	}

	lines := bytes.Split(data, []byte("\n"))
	lineIdx := a.Line - 1 // 0-indexed

	type candidate struct {
		startLine int
		endLine   int
		kind      string
	}
	var best *candidate

	for _, entry := range []struct {
		sigFn func([]byte) bool
		kind  string
	}{
		{grepfunc.IsFuncSig, "func"},
		{grepfunc.IsStructSig, "type"},
	} {
		boundaries := grepfunc.MapBlockBoundaries(lines, entry.sigFn)
		startLine, ok := boundaries[lineIdx]
		if !ok {
			continue
		}
		// find end: max key mapping to this startLine
		endLine := startLine
		for k, v := range boundaries {
			if v == startLine && k > endLine {
				endLine = k
			}
		}
		c := &candidate{startLine: startLine, endLine: endLine, kind: entry.kind}
		// prefer innermost (largest startLine)
		if best == nil || c.startLine > best.startLine {
			best = c
		}
	}

	rel := server.RelPath(a.Path)
	if best == nil {
		text := fmt.Sprintf("line %d is not inside any symbol in %s", a.Line, rel)
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: text}}}, nil
	}

	sig := strings.TrimSpace(string(lines[best.startLine]))
	name := grepfunc.EnclosingSymbol(lines, lineIdx, grepfunc.MapBlockBoundaries(lines, sigFnForKind(best.kind)))

	text := fmt.Sprintf("symbol: %s\nkind: %s\nlines: %d–%d\nsig: %s\n",
		name, best.kind, best.startLine+1, best.endLine+1, sig)

	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: text}}}, nil
}

func sigFnForKind(kind string) func([]byte) bool {
	if kind == "type" {
		return grepfunc.IsStructSig
	}
	return grepfunc.IsFuncSig
}
