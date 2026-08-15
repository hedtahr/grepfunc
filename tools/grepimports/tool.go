// Package grepimports finds and renders import relationships in source files.
package grepimports

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

// Schema keys and content types shared by the tool definition and callers.
const (
	schemaString  = "string"
	schemaInteger = "integer"
	schemaBoolean = "boolean"
	schemaText    = "text"
)

// Tool defines the grep_imports MCP tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "grep_imports",
	Description: "Import relationships: which files import a module, or what one file imports. Language-aware (Go, Python, JS/TS, Rust).",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"module": {
				Type:        schemaString,
				Items:       nil,
				Description: "Module/package name or path substring. E.g. 'grepfunc'. Omit with 'file' to list all imports.",
			},
			"path": {
				Type:        schemaString,
				Items:       nil,
				Description: "Directory to search. Defaults to project root.",
			},
			"include": {
				Type:        schemaString,
				Items:       nil,
				Description: "Glob filter (e.g. '**/*.go'). Auto-detects source files.",
			},
			"file": {
				Type:        schemaString,
				Items:       nil,
				Description: "Specific file to inspect — lists ALL imports (ignores module/path).",
			},
			"compact": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Terse output.",
			},
			"body": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Show matched import lines inline (code block per file).",
			},
			"names_only": {
				Type:        schemaBoolean,
				Items:       nil,
				Description: "Only file:import_path — cheapest.",
			},
			"token_budget": {
				Type:        schemaInteger,
				Items:       nil,
				Description: "Max output chars. Overflow → line-boundary truncation.",
			},
		},
		AdditionalProperties: false,
		Required:             []string{},
	},
}

type importEntry struct {
	alias   string
	path    string
	line    int
	symbols []string
}

type fileImports struct {
	relPath string
	imports []importEntry
}

// package-level compiled regexes.
var (
	goSingleImport = regexp.MustCompile(`^\s*import\s+(?:([._\w]+)\s+)?"([^"]+)"`)
	goGroupLine    = regexp.MustCompile(`^\s*(?:([._\w]+)\s+)?"([^"]+)"`)
	goGroupStart   = regexp.MustCompile(`^\s*import\s*\(`)

	pyFromImport = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\s+(.+)`)
	pyImport     = regexp.MustCompile(`^\s*import\s+([\w., ]+)`)

	jsFromImport = regexp.MustCompile(`^\s*(?:import|export)\s+(.+?)\s+from\s+['"]([^'"]+)['"]`)
	jsSideEffect = regexp.MustCompile(`^\s*import\s+['"]([^'"]+)['"]`)
	jsRequire    = regexp.MustCompile(`require\(['"]([^'"]+)['"]\)`)
	jsBraces     = regexp.MustCompile(`\{([^}]+)\}`)

	rustUse   = regexp.MustCompile(`^\s*use\s+([\w:{}*, ]+)\s*;`)
	rustCrate = regexp.MustCompile(`^\s*extern\s+crate\s+(\w+)`)
)

// fileArgs are the parsed arguments of a grep_imports call.
type fileArgs struct {
	Module      string `json:"module"`
	Path        string `json:"path"`
	Include     string `json:"include"`
	File        string `json:"file"`
	Compact     bool   `json:"compact"`
	Body        bool   `json:"body"`
	NamesOnly   bool   `json:"names_only"`
	TokenBudget int    `json:"token_budget"`
}

// Handle processes a grep_imports tool call.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var arg fileArgs

	err := json.Unmarshal(raw, &arg)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if arg.Module == "" && arg.File == "" {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: schemaText, Text: "either 'module' or 'file' is required"}},
			IsError: false,
		}, nil
	}

	if arg.File != "" {
		result, err := handleFileMode(arg)
		return server.BudgetResult(result, arg.TokenBudget), err
	}

	result, err := handleSearchMode(arg)
	return server.BudgetResult(result, arg.TokenBudget), err
}

// handleFileMode lists all imports in a single file.
func handleFileMode(arg fileArgs) (*server.ToolCallResult, error) {
	arg.File = server.ResolvePath(arg.File)

	err := server.CheckBounds(arg.File)
	if err != nil {
		return nil, fmt.Errorf("bounds check: %w", err)
	}

	err = server.CheckBanned(arg.File)
	if err != nil {
		return nil, fmt.Errorf("banned path check: %w", err)
	}

	server.SetLastPath(arg.File)

	data, err := os.ReadFile(arg.File) // #nosec G304 -- paths bounds-checked by server
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(arg.File))
	entries := parseImports(data, ext, "")

	if arg.NamesOnly {
		var buf strings.Builder

		rel := server.RelPath(arg.File)
		for _, entry := range entries {
			fmt.Fprintf(&buf, "%s:%s\n", rel, entry.path)
		}

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
			IsError: false,
		}, nil
	}

	return renderFileImports(server.RelPath(arg.File), entries), nil
}

// handleSearchMode finds files importing the requested module.
func handleSearchMode(arg fileArgs) (*server.ToolCallResult, error) {
	if arg.Path == "" {
		arg.Path = server.ProjectRoot
	} else {
		arg.Path = server.ResolvePath(arg.Path)
	}

	if arg.Include == "" {
		arg.Include = "*"
	}

	results, err := scanImports(arg)
	if err != nil {
		return nil, err
	}

	if arg.NamesOnly {
		var buf strings.Builder

		fmt.Fprintf(&buf, "%d files importing %q:\n", len(results), arg.Module)

		for _, f := range results {
			for _, entry := range f.imports {
				fmt.Fprintf(&buf, "%s:%s\n", f.relPath, entry.path)
			}
		}

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
			IsError: false,
		}, nil
	}

	return renderSearchResults(arg.Module, results, arg.Compact, arg.Body), nil
}

// scanImports walks the search root collecting matching import entries.
func scanImports(arg fileArgs) ([]fileImports, error) {
	var results []fileImports

	err := filepath.WalkDir(arg.Path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if isSkippableDir(entry.Name()) {
				return filepath.SkipDir
			}

			return nil
		}

		entries, err := scanImportFile(arg, path, entry)
		if err != nil {
			return err
		}

		if len(entries) > 0 {
			results = append(results, fileImports{relPath: server.RelPath(path), imports: entries})
		}

		return nil
	})
	if err != nil && len(results) == 0 {
		return nil, fmt.Errorf("walk %s: %w", arg.Path, err)
	}

	return results, nil
}

// scanImportFile parses one regular file's imports during a walk.
func scanImportFile(arg fileArgs, path string, entry fs.DirEntry) ([]importEntry, error) {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil, nil
	}

	rel, _ := filepath.Rel(arg.Path, path)
	if !grepfunc.MatchGlob(arg.Include, rel) {
		return nil, nil
	}

	info, err := entry.Info()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	if info.Size() > 2*1024*1024 {
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(path))
	if grepfunc.IsBinaryExt(ext) {
		return nil, nil
	}

	if arg.Include == "*" && grepfunc.IsNonSourceExt(ext) {
		return nil, nil
	}

	data, err := os.ReadFile(path) // #nosec G122,G304 -- paths bounds-checked by server
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return parseImports(data, ext, arg.Module), nil
}

// isSkippableDir reports whether a directory should be excluded from searches.
func isSkippableDir(base string) bool {
	return base == ".git" || base == "node_modules" || base == "vendor" || base == ".idea" ||
		base == "__pycache__" || strings.HasPrefix(base, ".")
}

func parseImports(data []byte, ext, filter string) []importEntry {
	switch ext {
	case ".go":
		return parseGoImports(data, filter)
	case ".py":
		return parsePyImports(data, filter)
	case ".js", ".ts", ".jsx", ".tsx", ".mjs", ".cjs":
		return parseJSImports(data, filter)
	case ".rs":
		return parseRustImports(data, filter)
	default:
		return nil
	}
}

func parseGoImports(data []byte, filter string) []importEntry {
	var entries []importEntry

	lines := strings.Split(string(data), "\n")
	inGroup := false

	for lineIdx, line := range lines {
		if goGroupStart.MatchString(line) {
			inGroup = true

			continue
		}

		if isGoGroupEnd(line) {
			inGroup = false

			continue
		}

		if inGroup {
			if isBlankOrComment(line) {
				continue
			}

			if m := goGroupLine.FindStringSubmatch(line); m != nil {
				if entry, ok := goEntry(m, lineIdx+1, filter); ok {
					entries = append(entries, entry)
				}
			}

			continue
		}

		if entry, ok := goSingleMatch(line, lineIdx+1, filter); ok {
			entries = append(entries, entry)
		}
	}

	return entries
}

// goSingleMatch parses a single-line import statement, applying the filter.
func goSingleMatch(line string, lineNum int, filter string) (importEntry, bool) {
	m := goSingleImport.FindStringSubmatch(line)
	if m == nil {
		return importEntry{alias: "", path: "", line: 0, symbols: nil}, false
	}

	return goEntry(m, lineNum, filter)
}

// isGoGroupEnd reports whether line closes an import group.
func isGoGroupEnd(line string) bool {
	return strings.TrimSpace(line) == ")"
}

// isBlankOrComment reports whether line is empty or a comment.
func isBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)

	return t == "" || strings.HasPrefix(t, "//")
}

// goEntry builds an importEntry from a regex submatch, applying the filter.
func goEntry(match []string, line int, filter string) (importEntry, bool) {
	pkg := match[2]
	if filter != "" && !strings.Contains(pkg, filter) {
		return importEntry{alias: "", path: "", line: 0, symbols: nil}, false
	}

	return importEntry{alias: match[1], path: pkg, line: line, symbols: nil}, true
}

func parsePyImports(data []byte, filter string) []importEntry {
	var entries []importEntry

	lines := strings.Split(string(data), "\n")

	inMultiline := false

	var (
		mlMod  string
		mlSyms []string
		mlLine int
	)

	for lineIdx, line := range lines {
		if inMultiline {
			if parseMultilineLine(line, &mlSyms) {
				entries = append(entries, importEntry{alias: "", path: mlMod, line: mlLine, symbols: mlSyms})
				inMultiline = false
				mlSyms = nil
			}

			continue
		}

		if match := pyFromImport.FindStringSubmatch(line); match != nil {
			mod := match[1]
			if filter != "" && !strings.Contains(mod, filter) {
				continue
			}

			raw := strings.TrimSpace(match[2])
			if isMultilineImportStart(raw) {
				inMultiline = true
				mlMod = mod
				mlLine = lineIdx + 1
				mlSyms = nil
				parseMultilineLine(afterOpenParen(raw), &mlSyms)

				continue
			}

			raw = strings.Trim(raw, "()")
			syms := splitSyms(raw)

			if raw == "*" {
				syms = []string{"*"}
			}

			entries = append(entries, importEntry{alias: "", path: mod, line: lineIdx + 1, symbols: syms})

			continue
		}

		if m := pyImport.FindStringSubmatch(line); m != nil {
			entries = appendPyModuleImports(entries, m[1], lineIdx+1, filter)
		}
	}

	return entries
}

// splitSyms splits a comma-separated symbol list, dropping aliases and stars.
// appendPyModuleImports appends plain "import a, b" style entries.
func appendPyModuleImports(entries []importEntry, raw string, line int, filter string) []importEntry {
	for mod := range strings.SplitSeq(raw, ",") {
		mod = strings.TrimSpace(mod)
		if before, _, found := strings.Cut(mod, " as "); found {
			mod = strings.TrimSpace(before)
		}

		if filter == "" || strings.Contains(mod, filter) {
			entries = append(entries, importEntry{alias: "", path: mod, line: line, symbols: nil})
		}
	}

	return entries
}

// splitSyms splits a comma-separated symbol list, dropping aliases and stars.
func splitSyms(raw string) []string {
	var syms []string

	for sym := range strings.SplitSeq(raw, ",") {
		sym = strings.TrimSpace(sym)
		if before, _, found := strings.Cut(sym, " as "); found {
			sym = strings.TrimSpace(before)
		}

		if sym != "" && sym != "*" {
			syms = append(syms, sym)
		}
	}

	return syms
}

// parseMultilineLine appends symbols from a parenthesized import continuation line.
// It reports whether the closing paren ended the group.
func parseMultilineLine(line string, syms *[]string) bool {
	trimmed := strings.TrimSpace(line)
	closed := false

	if idx := strings.Index(trimmed, ")"); idx >= 0 {
		trimmed = trimmed[:idx]
		closed = true
	}

	for sym := range strings.SplitSeq(trimmed, ",") {
		sym = strings.TrimSpace(sym)
		if before, _, found := strings.Cut(sym, " as "); found {
			sym = strings.TrimSpace(before)
		}

		if sym != "" && sym != "*" {
			*syms = append(*syms, sym)
		}
	}

	return closed
}

// isMultilineImportStart reports whether raw opens a parenthesized import group.
func isMultilineImportStart(raw string) bool {
	return strings.HasSuffix(strings.TrimRight(raw, " \t"), "(") && !strings.Contains(raw, ")")
}

// afterOpenParen returns the text following the last "(" in raw.
func afterOpenParen(raw string) string {
	idx := strings.LastIndex(raw, "(")
	if idx < 0 {
		return ""
	}

	return raw[idx+1:]
}

func parseJSImports(data []byte, filter string) []importEntry {
	var entries []importEntry

	for lineIdx, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if match := jsFromImport.FindStringSubmatch(line); match != nil {
			mod := match[2]
			if filter == "" || strings.Contains(mod, filter) {
				entries = append(entries, importEntry{
					alias:   "",
					path:    mod,
					line:    lineIdx + 1,
					symbols: jsSyms(match[1]),
				})
			}

			continue
		}

		if m := jsSideEffect.FindStringSubmatch(line); m != nil {
			mod := m[1]
			if filter == "" || strings.Contains(mod, filter) {
				entries = append(entries, importEntry{alias: "", path: mod, line: lineIdx + 1, symbols: nil})
			}

			continue
		}

		entries = appendJSRequires(entries, line, lineIdx+1, filter)
	}

	return entries
}

// jsSyms extracts named specifiers from an import/export clause.
// appendJSRequires appends all require() imports found in line.
func appendJSRequires(entries []importEntry, line string, lineNum int, filter string) []importEntry {
	for _, m := range jsRequire.FindAllStringSubmatch(line, -1) {
		mod := m[1]
		if filter == "" || strings.Contains(mod, filter) {
			entries = append(entries, importEntry{alias: "", path: mod, line: lineNum, symbols: nil})
		}
	}

	return entries
}

// jsSyms extracts named specifiers from an import/export clause.
func jsSyms(specifiers string) []string {
	if bm := jsBraces.FindStringSubmatch(specifiers); bm != nil {
		return splitList(bm[1], false)
	}

	s := strings.TrimSpace(specifiers)
	if s != "" && s != "type" {
		return []string{s}
	}

	return nil
}

// splitList splits a comma-separated list, optionally dropping "*" entries.
func splitList(raw string, skipStar bool) []string {
	var syms []string

	for sym := range strings.SplitSeq(raw, ",") {
		sym = strings.TrimSpace(sym)
		if before, _, found := strings.Cut(sym, " as "); found {
			sym = strings.TrimSpace(before)
		}

		if sym == "" || (skipStar && sym == "*") {
			continue
		}

		syms = append(syms, sym)
	}

	return syms
}

func parseRustImports(data []byte, filter string) []importEntry {
	var entries []importEntry

	for lineIdx, line := range strings.Split(string(data), "\n") {
		if m := rustCrate.FindStringSubmatch(line); m != nil {
			name := m[1]
			if filter == "" || strings.Contains(name, filter) {
				entries = append(entries, importEntry{alias: "", path: name, line: lineIdx + 1, symbols: nil})
			}

			continue
		}

		if m := rustUse.FindStringSubmatch(line); m != nil {
			raw := m[1]
			if filter == "" || strings.Contains(raw, filter) {
				if entry, ok := rustBracedEntry(raw, lineIdx+1); ok {
					entries = append(entries, entry)
				} else {
					entries = append(entries, rustSimpleEntry(raw, lineIdx+1))
				}
			}
		}
	}

	return entries
}

// rustBracedEntry parses "base::{a, b}" style use statements.
func rustBracedEntry(raw string, line int) (importEntry, bool) {
	before, after, ok := strings.Cut(raw, "::{")
	if !ok {
		return importEntry{alias: "", path: "", line: 0, symbols: nil}, false
	}

	inner := strings.Trim(after, "}")
	syms := splitList(inner, false)

	return importEntry{alias: "", path: before, line: line, symbols: syms}, true
}

// rustSimpleEntry parses a plain path or "path::Symbol" use statement.
func rustSimpleEntry(raw string, line int) importEntry {
	if idx := strings.LastIndex(raw, "::"); idx >= 0 {
		return importEntry{
			alias:   "",
			path:    raw[:idx],
			line:    line,
			symbols: []string{raw[idx+2:]},
		}
	}

	return importEntry{alias: "", path: raw, line: line, symbols: nil}
}

func renderFileImports(relPath string, entries []importEntry) *server.ToolCallResult {
	var buf strings.Builder

	fmt.Fprintf(&buf, "%d imports in %s:\n", len(entries), relPath)

	for _, entry := range entries {
		switch {
		case len(entry.symbols) > 0:
			fmt.Fprintf(&buf, "  L%-4d %s → {%s}\n", entry.line, entry.path, strings.Join(entry.symbols, ", "))
		case entry.alias != "":
			fmt.Fprintf(&buf, "  L%-4d %s (as %s)\n", entry.line, entry.path, entry.alias)
		default:
			fmt.Fprintf(&buf, "  L%-4d %s\n", entry.line, entry.path)
		}
	}

	if len(entries) == 0 {
		buf.WriteString("  (no imports found)\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
		IsError: false,
	}
}

func renderSearchResults(module string, results []fileImports, compact, body bool) *server.ToolCallResult {
	var buf strings.Builder
	if len(results) == 0 {
		fmt.Fprintf(&buf, "No files import %q.\n", module)

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
			IsError: false,
		}
	}

	if compact {
		fmt.Fprintf(&buf, "%d files import %q:\n", len(results), module)
	} else {
		fmt.Fprintf(&buf, "%d files importing %q:\n", len(results), module)
	}

	if !body {
		buf.WriteString("```\n")
	}

	for _, file := range results {
		allSyms, aliases := collectSymbols(file.imports)
		writeFileLine(&buf, file, allSyms, aliases)

		if body {
			writeFileBody(&buf, file, compact)
		}
	}

	if !body {
		buf.WriteString("```\n")
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: schemaText, Text: buf.String()}},
		IsError: false,
	}
}

// collectSymbols gathers all imported symbols and aliases of one file.
func collectSymbols(imports []importEntry) ([]string, []string) {
	allSyms := make([]string, 0)
	aliases := make([]string, 0)

	for _, entry := range imports {
		allSyms = append(allSyms, entry.symbols...)

		if entry.alias != "" {
			aliases = append(aliases, entry.alias)
		}
	}

	return allSyms, aliases
}

// fileLineTag renders the line annotation for a file's imports.
func fileLineTag(imports []importEntry) string {
	switch len(imports) {
	case 1:
		return fmt.Sprintf(":L%d", imports[0].line)
	case 0:
		return ""
	default:
		parts := make([]string, len(imports))
		for i, entry := range imports {
			parts[i] = fmt.Sprintf("L%d", entry.line)
		}

		return " (" + strings.Join(parts, ",") + ")"
	}
}

// writeFileLine writes one result line for a file.
func writeFileLine(buf *strings.Builder, file fileImports, allSyms, aliases []string) {
	lineTag := fileLineTag(file.imports)

	switch {
	case len(allSyms) > 0:
		fmt.Fprintf(buf, "%s%s → {%s}\n", file.relPath, lineTag, strings.Join(allSyms, ", "))
	case len(aliases) > 0:
		fmt.Fprintf(buf, "%s%s (as %s)\n", file.relPath, lineTag, strings.Join(aliases, ", "))
	default:
		fmt.Fprintf(buf, "%s%s\n", file.relPath, lineTag)
	}
}

// writeFileBody re-reads the file to show the matched import lines.
func writeFileBody(buf *strings.Builder, file fileImports, compact bool) {
	absPath := filepath.Join(server.ProjectRoot, file.relPath)

	data, err := os.ReadFile(absPath) // #nosec G304 -- paths bounds-checked by server
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")

	ext := strings.TrimPrefix(filepath.Ext(file.relPath), ".")
	if ext == "" {
		ext = "go"
	}

	fmt.Fprintf(buf, "```%s\n", ext)

	for _, entry := range file.imports {
		if entry.line > 0 && entry.line <= len(lines) {
			fmt.Fprintf(buf, "%s\n", lines[entry.line-1])
		}
	}

	fmt.Fprintf(buf, "```")

	if compact {
		buf.WriteByte('\n')
	} else {
		buf.WriteString("\n")
	}
}
