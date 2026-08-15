// Package grepdead finds declared symbols with no references outside their file.
package grepdead

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

// Schema keys used in the tool definition.
const schemaString = "string"

// kindAll is the default kind filter that checks both funcs and types.
const kindAll = "all"

// searchMultiplier scales the symbol cap to leave headroom for the dedup pass.
const searchMultiplier = 2

// refChunkSize batches name lookups to keep generated regexes small.
const refChunkSize = 30

// Tool defines the grep_dead MCP tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_dead",
	Description: "Dead code: declared symbols with zero references outside their file.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        schemaString,
				Items:       nil,
				Description: "Root directory. Defaults to project root.",
			},
			"include": {
				Type:        schemaString,
				Items:       nil,
				Description: "Glob filter for declaration/reference search.",
			},
			"kind": {
				Type:        schemaString,
				Items:       nil,
				Description: "Symbol kind: 'func', 'type', or 'all' (default).",
			},
			"min_lines": {
				Type:        "integer",
				Items:       nil,
				Description: "Skip symbols with bodies shorter than N lines. Default 2.",
			},
			"max_symbols": {
				Type:        "integer",
				Items:       nil,
				Description: "Max symbols to check. Default 200.",
			},
			"compact": {
				Type:        "boolean",
				Items:       nil,
				Description: "Terse output.",
			},
		},
		AdditionalProperties: false,
		Required:             []string{},
	},
}

type args struct {
	Path       string `json:"path"`
	Include    string `json:"include"`
	Kind       string `json:"kind"`
	MinLines   int    `json:"min_lines"`
	MaxSymbols int    `json:"max_symbols"`
	Compact    bool   `json:"compact"`
}

type deadSym struct {
	file string
	line int
	name string
	kind string
}

type symWithKind struct {
	m    grepfunc.FuncMatch
	kind string
}

type declInfo struct {
	file string
	line int
	kind string
}

// Handle processes a grep_dead tool call.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var arg args

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	arg.Path = server.ResolvePath(arg.Path)
	if arg.Kind == "" {
		arg.Kind = kindAll
	}

	if arg.MinLines <= 0 {
		arg.MinLines = 2
	}

	if arg.MaxSymbols <= 0 {
		arg.MaxSymbols = 200
	}

	include := arg.Include
	if include == "" {
		include = "*"
	}

	rawCandidates := collectCandidates(arg, include)

	nameToDecl, nameToInfo, refCount := indexCandidates(rawCandidates)
	checked := len(nameToDecl)

	var dead []deadSym
	if checked > 0 {
		dead = checkReferences(arg, include, nameToDecl, refCount, nameToInfo)
	}

	if len(dead) == 0 {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{
				Type: "text",
				Text: fmt.Sprintf("No dead symbols found among %d checked.\n", checked),
			}},
			IsError: false,
		}, nil
	}

	return renderReport(arg, rawCandidates, dead, checked), nil
}

// renderReport builds the final dead-symbol listing.
func renderReport(arg args, rawCandidates []symWithKind, dead []deadSym, checked int) *server.ToolCallResult {
	var buf strings.Builder

	writeDeadHeader(&buf, arg, len(dead), checked)

	buf.WriteString("```\n")

	for _, sym := range dead {
		rel := server.RelPath(sym.file)

		kind := sym.kind
		if kind == "" {
			kind = "?"
		}

		fmt.Fprintf(&buf, "%s:%d [%s] %s\n", rel, sym.line, kind, sym.name)
	}

	buf.WriteString("```\n")

	if len(rawCandidates) == arg.MaxSymbols {
		fmt.Fprintf(&buf, "\n(capped at %d symbols — increase max_symbols for full scan)\n", arg.MaxSymbols)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
		IsError: false,
	}
}

// collectCandidates finds all func/type candidates, dedups them, and applies the cap.
func collectCandidates(arg args, include string) []symWithKind {
	matchAll := regexp.MustCompile(`(?s).`)

	var rawCandidates []symWithKind

	if arg.Kind == kindAll || arg.Kind == "func" {
		funcs, _ := grepfunc.Search(arg.Path, include, matchAll, arg.MaxSymbols*searchMultiplier, grepfunc.IsFuncSig)
		for _, f := range funcs {
			rawCandidates = append(rawCandidates, symWithKind{m: f, kind: "func"})
		}
	}

	if arg.Kind == kindAll || arg.Kind == "type" {
		types, _ := grepfunc.Search(arg.Path, include, matchAll, arg.MaxSymbols*searchMultiplier, grepfunc.IsStructSig)
		for _, t := range types {
			rawCandidates = append(rawCandidates, symWithKind{m: t, kind: "type"})
		}
	}

	// Dedup by file+line, then filter by min_lines and apply the cap
	return dedupCandidates(rawCandidates, arg.MinLines, arg.MaxSymbols)
}

// dedupCandidates removes duplicate declarations and applies the cap.
func dedupCandidates(rawCandidates []symWithKind, minLines, maxSymbols int) []symWithKind {
	seen := map[string]bool{}

	unique := rawCandidates[:0]

	for _, cand := range rawCandidates {
		k := fmt.Sprintf("%s:%d", cand.m.File, cand.m.Line)
		if !seen[k] {
			seen[k] = true

			unique = append(unique, cand)
		}
	}

	filtered := unique[:0]

	for _, cand := range unique {
		if cand.m.Lines >= minLines {
			filtered = append(filtered, cand)
		}
	}

	if len(filtered) > maxSymbols {
		filtered = filtered[:maxSymbols]
	}

	return filtered
}

// indexCandidates maps candidate names to their declaration file and ref counts.
func indexCandidates(rawCandidates []symWithKind) (map[string]string, map[string]declInfo, map[string]int) {
	nameToDecl := make(map[string]string, len(rawCandidates))
	nameToInfo := make(map[string]declInfo, len(rawCandidates))
	refCount := make(map[string]int, len(rawCandidates))

	for _, cand := range rawCandidates {
		if cand.m.Name == "" || cand.m.Name == "<fn>" {
			continue
		}

		nameToDecl[cand.m.Name] = cand.m.File
		nameToInfo[cand.m.Name] = declInfo{file: cand.m.File, line: cand.m.Line, kind: cand.kind}
		refCount[cand.m.Name] = 0
	}

	return nameToDecl, nameToInfo, refCount
}

// checkReferences counts cross-file references for each candidate name.
func checkReferences(arg args, include string, nameToDecl map[string]string,
	refCount map[string]int, nameToInfo map[string]declInfo) []deadSym {
	declRe := regexp.MustCompile(`(?i)(?:(?:func|type|var|const|let|class|def|struct|interface|enum)\s+|` +
		`func\s+\([^)]+\)\s+)\w+\b`)

	names := make([]string, 0, len(nameToDecl))
	for name := range nameToDecl {
		names = append(names, name)
	}

	sort.Strings(names)

	for i := 0; i < len(names); i += refChunkSize {
		end := min(i+refChunkSize, len(names))
		chunk := names[i:end]

		parts := make([]string, len(chunk))
		for j, name := range chunk {
			parts[j] = regexp.QuoteMeta(name)
		}

		chunkRe := regexp.MustCompile(`(?i)\b(` + strings.Join(parts, "|") + `)\b`)

		scanChunk(arg.Path, include, chunkRe, declRe, nameToDecl, refCount)
	}

	var dead []deadSym

	for name, count := range refCount {
		if count == 0 {
			info := nameToInfo[name]
			dead = append(dead, deadSym{file: info.file, line: info.line, name: name, kind: info.kind})
		}
	}

	sort.Slice(dead, func(i, j int) bool {
		if dead[i].file != dead[j].file {
			return dead[i].file < dead[j].file
		}

		return dead[i].line < dead[j].line
	})

	return dead
}

// scanChunk walks the tree counting references for one chunk of names.
func scanChunk(root, include string, chunkRe, declRe *regexp.Regexp,
	nameToDecl map[string]string, refCount map[string]int) {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if isSkippableDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		err = scanChunkFile(root, path, include, entry, chunkRe, declRe, nameToDecl, refCount)
		if err != nil {
			return err
		}

		return nil
	})
	_ = err
}

// scanChunkFile counts references in one file during the walk.
func scanChunkFile(root, path, include string, entry fs.DirEntry, chunkRe, declRe *regexp.Regexp,
	nameToDecl map[string]string, refCount map[string]int) error {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil
	}

	rel, _ := filepath.Rel(root, path)
	if !grepfunc.MatchGlob(include, rel) {
		return nil
	}

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Size() > 2*1024*1024 {
		return nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if grepfunc.IsBinaryExt(ext) {
		return nil
	}

	if include == "*" && grepfunc.IsNonSourceExt(ext) {
		return nil
	}

	data, err := os.ReadFile(path) // #nosec G122,G304 -- paths bounds-checked by server
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	countRefs(data, chunkRe, declRe, path, nameToDecl, refCount)

	return nil
}

// isSkippableDir reports whether a directory should be excluded from searches.
func isSkippableDir(base string) bool {
	return base == ".git" || base == "node_modules" || base == "vendor" || base == ".idea" ||
		base == "__pycache__" || strings.HasPrefix(base, ".")
}

// countRefs tallies references to candidate names in one file's content.
func countRefs(data []byte, chunkRe, declRe *regexp.Regexp, path string,
	nameToDecl map[string]string, refCount map[string]int) {
	for line := range strings.SplitSeq(string(data), "\n") {
		if !chunkRe.MatchString(line) {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}

		if declRe.MatchString(line) {
			continue
		}

		for _, m := range chunkRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if declFile, ok := nameToDecl[name]; ok && path != declFile {
				refCount[name]++
			}
		}
	}
}

// writeDeadHeader writes the summary line of the dead-symbol report.
func writeDeadHeader(buf *strings.Builder, arg args, deadCount, checked int) {
	if arg.Compact {
		fmt.Fprintf(buf, "%d potentially dead symbols (of %d checked):\n", deadCount, checked)
	} else {
		fmt.Fprintf(buf, "%d potentially dead symbols (of %d checked):\n\n", deadCount, checked)
	}
}
