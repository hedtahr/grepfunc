package deletesymbol

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
	Name:        "delete_symbol",
	Description: `Delete a named symbol (function, method, struct, interface, enum, class) from a file. Removes the full body including all nested scopes.`,
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"name":    {Type: "string", Description: "Name of the symbol to delete. Exact match (case-insensitive)."},
			"path":    {Type: "string", Description: "MUST be absolute path to the file containing the symbol."},
			"dry_run": {Type: "boolean", Description: "Preview the file after deletion without writing."},
		},
		Required: []string{"name", "path"},
	},
}

type args struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
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
	a.Path = server.ResolvePath(a.Path)
	if err := server.CheckBounds(a.Path); err != nil {
		return nil, err
	}
	if err := server.CheckBanned(a.Path); err != nil {
		return nil, err
	}

	pat, _ := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(a.Name) + `\b`)
	combined := func(line []byte) bool { return grepfunc.IsFuncSig(line) || grepfunc.IsStructSig(line) }
	results, err := grepfunc.Search(a.Path, "*", pat, 10, combined)
	if err != nil {
		return nil, fmt.Errorf("search: %v", err)
	}

	var match *grepfunc.FuncMatch
	for i, r := range results {
		if strings.EqualFold(r.Name, a.Name) {
			match = &results[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("symbol %q not found in %s", a.Name, server.RelPath(a.Path))
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, fmt.Errorf("read file: %v", err)
	}
	newData, err := removeLines(data, match.Line, match.EndLine)
	if err != nil {
		return nil, fmt.Errorf("remove lines: %v", err)
	}

	rel := server.RelPath(a.Path)
	var buf strings.Builder
	fmt.Fprintf(&buf, "delete_symbol %q:\n", a.Name)
	fmt.Fprintf(&buf, "  file: %s (remove L%d–L%d, %d lines)\n", rel, match.Line, match.EndLine, match.Lines)
	fmt.Fprintf(&buf, "  kind: %s\n", symbolKind(match.Body))

	if a.DryRun {
		buf.WriteString("\n[Dry run — no changes]\n")
		buf.WriteString("\n--- after deletion ---\n")
		buf.WriteString(string(newData))
	} else {
		if err := os.WriteFile(a.Path, newData, 0644); err != nil {
			return nil, fmt.Errorf("write file: %v", err)
		}
		buf.WriteString("\n[Done]\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func symbolKind(body string) string {
	first := []byte(body)
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		first = []byte(body[:nl])
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
	for i := 0; i < start-1; i++ {
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
