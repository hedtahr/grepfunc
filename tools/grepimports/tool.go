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

var Tool = server.Tool{
	Name:        "grep_imports",
	Description: "Find all files importing a given package/module, or list what a specific file imports. Handles Go, Python, JS/TS, and Rust import syntax. Returns structured file→symbols output. Replaces the grep_context('import.*module') pattern with one call.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"module":     {Type: "string", Description: "Module/package name or path substring to find. E.g. 'grepfunc' matches any import path containing 'grepfunc'. Omit when using 'file' to list all imports."},
			"path":       {Type: "string", Description: "MUST be absolute path to directory to search."},
			"include":    {Type: "string", Description: "Glob filter (e.g. '**/*.go'). Auto-detects source files when omitted."},
			"file":       {Type: "string", Description: "MUST be absolute path to file. If set, list ALL imports in this specific file (ignores module/path)."},
			"compact":    {Type: "boolean", Description: "Terse output. Default false."},
			"body":       {Type: "boolean", Description: "If true, show matched import lines inline (code block per file). Saves a follow-up grep_context call."},
			"names_only": {Type: "boolean", Description: "If true, return only file:import_path — no code blocks. Cheapest mode."},
		},
		Required: []string{},
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

// package-level compiled regexes
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

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a struct {
		Module    string `json:"module"`
		Path      string `json:"path"`
		Include   string `json:"include"`
		File      string `json:"file"`
		Compact   bool   `json:"compact"`
		Body      bool   `json:"body"`
		NamesOnly bool   `json:"names_only"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Module == "" && a.File == "" {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: "either 'module' or 'file' is required"}},
		}, nil
	}

	// File mode: list all imports in one file
	if a.File != "" {
		a.File = server.ResolvePath(a.File)
		server.SetLastPath(a.File)
		data, err := os.ReadFile(a.File)
		if err != nil {
			return nil, fmt.Errorf("read failed: %v", err)
		}
		ext := strings.ToLower(filepath.Ext(a.File))
		entries := parseImports(data, ext, "")
		if a.NamesOnly {
			var buf strings.Builder
			rel := server.RelPath(a.File)
			for _, e := range entries {
				fmt.Fprintf(&buf, "%s:%s\n", rel, e.path)
			}
			return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
		}
		return renderFileImports(server.RelPath(a.File), entries, a.Compact), nil
	}

	// Search mode
	if a.Path == "" {
		a.Path = server.ProjectRoot
	} else {
		a.Path = server.ResolvePath(a.Path)
	}
	if a.Include == "" {
		a.Include = "*"
	}

	var results []fileImports
	_ = filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" ||
				base == ".idea" || base == "__pycache__" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(a.Path, path)
		if !grepfunc.MatchGlob(a.Include, rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if grepfunc.IsBinaryExt(ext) {
			return nil
		}
		if a.Include == "*" && grepfunc.IsNonSourceExt(ext) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		entries := parseImports(data, ext, a.Module)
		if len(entries) > 0 {
			results = append(results, fileImports{relPath: server.RelPath(path), imports: entries})
		}
		return nil
	})

	if a.NamesOnly {
		var buf strings.Builder
		fmt.Fprintf(&buf, "%d file(s) importing %q:\n", len(results), a.Module)
		for _, f := range results {
			for _, e := range f.imports {
				fmt.Fprintf(&buf, "%s:%s\n", f.relPath, e.path)
			}
		}
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}, nil
	}
	return renderSearchResults(a.Module, results, a.Compact, a.Body), nil
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
	for i, line := range lines {
		if goGroupStart.MatchString(line) {
			inGroup = true
			continue
		}
		if inGroup && strings.TrimSpace(line) == ")" {
			inGroup = false
			continue
		}
		if inGroup {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := goGroupLine.FindStringSubmatch(line); m != nil {
				pkg := m[2]
				if filter == "" || strings.Contains(pkg, filter) {
					entries = append(entries, importEntry{alias: m[1], path: pkg, line: i + 1})
				}
			}
			continue
		}
		if m := goSingleImport.FindStringSubmatch(line); m != nil {
			pkg := m[2]
			if filter == "" || strings.Contains(pkg, filter) {
				entries = append(entries, importEntry{alias: m[1], path: pkg, line: i + 1})
			}
		}
	}
	return entries
}

func parsePyImports(data []byte, filter string) []importEntry {
	var entries []importEntry
	lines := strings.Split(string(data), "\n")

	inMultiline := false
	var mlMod string
	var mlSyms []string
	var mlLine int

	for i, line := range lines {
		if inMultiline {
			trimmed := strings.TrimSpace(line)
			if strings.Contains(trimmed, ")") {
				trimmed = trimmed[:strings.Index(trimmed, ")")]
				inMultiline = false
			}
			for _, s := range strings.Split(trimmed, ",") {
				s = strings.TrimSpace(s)
				if before, _, found := strings.Cut(s, " as "); found {
					s = strings.TrimSpace(before)
				}
				if s != "" && s != "*" {
					mlSyms = append(mlSyms, s)
				}
			}
			if !inMultiline {
				entries = append(entries, importEntry{path: mlMod, line: mlLine, symbols: mlSyms})
				mlSyms = nil
			}
			continue
		}

		if m := pyFromImport.FindStringSubmatch(line); m != nil {
			mod := m[1]
			if filter != "" && !strings.Contains(mod, filter) {
				continue
			}
			raw := strings.TrimSpace(m[2])
			if strings.HasSuffix(strings.TrimRight(raw, " \t"), "(") && !strings.Contains(raw, ")") {
				inMultiline = true
				mlMod = mod
				mlLine = i + 1
				mlSyms = nil
				after := raw[strings.LastIndex(raw, "(")+1:]
				after = strings.TrimSpace(after)
				for _, s := range strings.Split(after, ",") {
					s = strings.TrimSpace(s)
					if before, _, found := strings.Cut(s, " as "); found {
						s = strings.TrimSpace(before)
					}
					if s != "" && s != "*" {
						mlSyms = append(mlSyms, s)
					}
				}
				continue
			}
			var syms []string
			raw = strings.Trim(raw, "()")
			for _, s := range strings.Split(raw, ",") {
				s = strings.TrimSpace(s)
				if before, _, found := strings.Cut(s, " as "); found {
					s = strings.TrimSpace(before)
				}
				if s != "" && s != "*" {
					syms = append(syms, s)
				}
			}
			if raw == "*" {
				syms = []string{"*"}
			}
			entries = append(entries, importEntry{path: mod, line: i + 1, symbols: syms})
			continue
		}
		if m := pyImport.FindStringSubmatch(line); m != nil {
			for _, mod := range strings.Split(m[1], ",") {
				mod = strings.TrimSpace(mod)
				if before, _, found := strings.Cut(mod, " as "); found {
					mod = strings.TrimSpace(before)
				}
				if filter == "" || strings.Contains(mod, filter) {
					entries = append(entries, importEntry{path: mod, line: i + 1})
				}
			}
		}
	}
	return entries
}

func parseJSImports(data []byte, filter string) []importEntry {
	var entries []importEntry
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if m := jsFromImport.FindStringSubmatch(line); m != nil {
			mod := m[2]
			if filter == "" || strings.Contains(mod, filter) {
				specifiers := m[1]
				var syms []string
				if bm := jsBraces.FindStringSubmatch(specifiers); bm != nil {
					for _, s := range strings.Split(bm[1], ",") {
						s = strings.TrimSpace(s)
						if before, _, found := strings.Cut(s, " as "); found {
							s = strings.TrimSpace(before)
						}
						if s != "" {
							syms = append(syms, s)
						}
					}
				} else {
					s := strings.TrimSpace(specifiers)
					if s != "" && s != "type" {
						syms = []string{s}
					}
				}
				entries = append(entries, importEntry{path: mod, line: i + 1, symbols: syms})
			}
			continue
		}

		if m := jsSideEffect.FindStringSubmatch(line); m != nil {
			mod := m[1]
			if filter == "" || strings.Contains(mod, filter) {
				entries = append(entries, importEntry{path: mod, line: i + 1})
			}
			continue
		}

		for _, m := range jsRequire.FindAllStringSubmatch(line, -1) {
			mod := m[1]
			if filter == "" || strings.Contains(mod, filter) {
				entries = append(entries, importEntry{path: mod, line: i + 1})
			}
		}
	}
	return entries
}

func parseRustImports(data []byte, filter string) []importEntry {
	var entries []importEntry
	for i, line := range strings.Split(string(data), "\n") {
		if m := rustCrate.FindStringSubmatch(line); m != nil {
			name := m[1]
			if filter == "" || strings.Contains(name, filter) {
				entries = append(entries, importEntry{path: name, line: i + 1})
			}
			continue
		}
		if m := rustUse.FindStringSubmatch(line); m != nil {
			raw := m[1]
			if filter == "" || strings.Contains(raw, filter) {
				if idx := strings.Index(raw, "::{"); idx >= 0 {
					base := raw[:idx]
					inner := strings.Trim(raw[idx+3:], "}")
					var syms []string
					for _, s := range strings.Split(inner, ",") {
						s = strings.TrimSpace(s)
						if s != "" {
							syms = append(syms, s)
						}
					}
					entries = append(entries, importEntry{path: base, line: i + 1, symbols: syms})
				} else {
					// split last :: segment as the symbol
					if idx := strings.LastIndex(raw, "::"); idx >= 0 {
						entries = append(entries, importEntry{
							path:    raw[:idx],
							line:    i + 1,
							symbols: []string{raw[idx+2:]},
						})
					} else {
						entries = append(entries, importEntry{path: raw, line: i + 1})
					}
				}
			}
		}
	}
	return entries
}

func renderFileImports(relPath string, entries []importEntry, compact bool) *server.ToolCallResult {
	var buf strings.Builder
	if compact {
		fmt.Fprintf(&buf, "%d imports in %s:\n", len(entries), relPath)
	} else {
		fmt.Fprintf(&buf, "%d import(s) in %s:\n\n", len(entries), relPath)
	}
	for _, e := range entries {
		switch {
		case len(e.symbols) > 0:
			fmt.Fprintf(&buf, "  L%-4d %s → {%s}\n", e.line, e.path, strings.Join(e.symbols, ", "))
		case e.alias != "":
			fmt.Fprintf(&buf, "  L%-4d %s (as %s)\n", e.line, e.path, e.alias)
		default:
			fmt.Fprintf(&buf, "  L%-4d %s\n", e.line, e.path)
		}
	}
	if len(entries) == 0 {
		buf.WriteString("  (no imports found)\n")
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}
}

func renderSearchResults(module string, results []fileImports, compact bool, body bool) *server.ToolCallResult {
	var buf strings.Builder
	if len(results) == 0 {
		fmt.Fprintf(&buf, "No files import %q.\n", module)
		return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}
	}
	if compact {
		fmt.Fprintf(&buf, "%d files import %q:\n", len(results), module)
	} else {
		fmt.Fprintf(&buf, "%d file(s) importing %q:\n\n", len(results), module)
	}
	for _, f := range results {
		var allSyms []string
		var aliases []string
		for _, e := range f.imports {
			allSyms = append(allSyms, e.symbols...)
			if e.alias != "" {
				aliases = append(aliases, e.alias)
			}
		}
		lineTag := ""
		if len(f.imports) == 1 {
			lineTag = fmt.Sprintf(":L%d", f.imports[0].line)
		} else if len(f.imports) > 1 {
			parts := make([]string, len(f.imports))
			for i, e := range f.imports {
				parts[i] = fmt.Sprintf("L%d", e.line)
			}
			lineTag = " (" + strings.Join(parts, ",") + ")"
		}
		switch {
		case len(allSyms) > 0:
			fmt.Fprintf(&buf, "- %s%s → {%s}\n", f.relPath, lineTag, strings.Join(allSyms, ", "))
		case len(aliases) > 0:
			fmt.Fprintf(&buf, "- %s%s (as %s)\n", f.relPath, lineTag, strings.Join(aliases, ", "))
		default:
			fmt.Fprintf(&buf, "- %s%s\n", f.relPath, lineTag)
		}
		if body {
			// Re-read file to show matched import lines
			absPath := filepath.Join(server.ProjectRoot, f.relPath)
			if data, err := os.ReadFile(absPath); err == nil {
				lines := strings.Split(string(data), "\n")
				ext := strings.TrimPrefix(filepath.Ext(f.relPath), ".")
				if ext == "" {
					ext = "go"
				}
				fmt.Fprintf(&buf, "```%s\n", ext)
				for _, e := range f.imports {
					if e.line > 0 && e.line <= len(lines) {
						fmt.Fprintf(&buf, "%s\n", lines[e.line-1])
					}
				}
				fmt.Fprintf(&buf, "```")
				if compact {
					buf.WriteByte('\n')
				} else {
					buf.WriteString("\n\n")
				}
			}
		}
	}
	return &server.ToolCallResult{Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}}}
}
