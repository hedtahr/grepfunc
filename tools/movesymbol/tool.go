package movesymbol

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "move_symbol",
	Description: "Move a named symbol (function, method, struct, interface, enum, class) from a source file to a destination file. Removes the symbol from src and inserts it into dst at the specified line (or appends if no line given).",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":    {Type: "string", Description: "Name of the symbol to move. Exact match."},
			"src":     {Type: "string", Description: "Source file containing the symbol. Absolute path or project-relative."},
			"dst":     {Type: "string", Description: "Destination file. Absolute path or project-relative."},
			"line":    {Type: "integer", Description: "Line number in dst to insert before. If omitted, appends to end of dst. If the target line has non-whitespace code, the operation is refused."},
			"dry_run": {Type: "boolean", Description: "Preview changes without writing files."},
		},
		Required: []string{"name", "src", "dst"},
	},
}

type args struct {
	Name   string `json:"name"`
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Line   int    `json:"line"`
	DryRun bool   `json:"dry_run"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	a.Src = server.ResolvePath(a.Src)
	a.Dst = server.ResolvePath(a.Dst)
	if err := server.CheckBounds(a.Src); err != nil {
		return nil, err
	}
	if err := server.CheckBounds(a.Dst); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Src); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Dst); err != nil {
		return nil, err
	}

	// Find symbol in src using exact name match.
	pat, _ := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(a.Name) + `\b`)
	combined := func(line []byte) bool { return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line) }
	results, err := grepfunc.Search(a.Src, "*", pat, 10, combined)
	if err != nil {
		return nil, fmt.Errorf("search src: %v", err)
	}

	var match *grepfunc.FuncMatch
	for i, r := range results {
		if strings.EqualFold(r.Name, a.Name) {
			match = &results[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("symbol %q not found in %s", a.Name, server.RelPath(a.Src))
	}

	// Read src, remove symbol lines.
	srcData, err := os.ReadFile(a.Src)
	if err != nil {
		return nil, fmt.Errorf("read src: %v", err)
	}
	newSrc, err := removeLines(srcData, match.Line, match.EndLine)
	if err != nil {
		return nil, fmt.Errorf("remove from src: %v", err)
	}

	// Read dst.
	dstData, err := os.ReadFile(a.Dst)
	if err != nil {
		// If creating, start empty.
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read dst: %v", err)
		}
		dstData = nil
	}

	// Determine insert position in dst.
	body := strings.TrimRight(match.Body, "\n") + "\n"
	dstLines := toLines(dstData)
	insertLine := a.Line

	if insertLine > 0 {
		// Check the target line for existing code.
		if insertLine <= len(dstLines) {
			target := strings.TrimSpace(string(dstLines[insertLine-1]))
			if target != "" {
				return nil, fmt.Errorf("dst line %d has code: %q — refusing to replace. Pick a different line.", insertLine, target)
			}
		}
	} else {
		insertLine = len(dstLines) + 1
	}

	newDst, err := insertAt(dstData, body, insertLine)
	if err != nil {
		return nil, fmt.Errorf("insert into dst: %v", err)
	}

	var buf strings.Builder
	srcRel := server.RelPath(a.Src)
	dstRel := server.RelPath(a.Dst)
	fmt.Fprintf(&buf, "move_symbol %q:\n", a.Name)
	fmt.Fprintf(&buf, "  src: %s (remove L%d–L%d, %d lines)\n", srcRel, match.Line, match.EndLine, match.Lines)
	fmt.Fprintf(&buf, "  dst: %s (insert at L%d)\n", dstRel, insertLine)
	fmt.Fprintf(&buf, "  kind: %s\n", symbolKind(match.Body))

	if a.DryRun {
		buf.WriteString("\n[Dry run — no files changed]\n")
		buf.WriteString("\n--- src after removal ---\n")
		buf.WriteString(string(newSrc))
		buf.WriteString("\n--- dst after insert ---\n")
		buf.WriteString(string(newDst))
	} else {
		if err := os.WriteFile(a.Src, newSrc, 0644); err != nil {
			return nil, fmt.Errorf("write src: %v", err)
		}
		if err := os.WriteFile(a.Dst, newDst, 0644); err != nil {
			return nil, fmt.Errorf("write dst: %v", err)
		}
		buf.WriteString("\n[Done]\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
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
		return nil, fmt.Errorf("invalid range L%d–L%d (file has %d lines)", start, end, len(lines))
	}
	var buf strings.Builder
	// Lines before start (0-indexed)
	for i := 0; i < start-1; i++ {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}
	// Lines after end
	for i := end; i < len(lines); i++ {
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}
	return []byte(buf.String()), nil
}

func insertAt(data []byte, text string, line int) ([]byte, error) {
	lines := toLines(data)
	if line < 1 {
		return nil, fmt.Errorf("insert line must be >= 1")
	}
	var buf strings.Builder
	for i := 0; i < min(line-1, len(lines)); i++ {
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
