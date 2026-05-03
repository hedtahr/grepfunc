package filestats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
)

var Tool = server.Tool{
	Name:        "file_stats",
	Description: "Instant project overview: file counts, line counts, and extension breakdown per directory. Zero content overhead — reads only file sizes and line counts. Saves the list_directory + file_symbols chain at session start. Mode 'top' walks the project counting symbols (funcs+types) per file and returns the top N files by symbol count.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "MUST be absolute path to root directory to analyze."},
			"include": {Type: "string", Description: "Glob filter. Defaults to all source files (\"*\")."},
			"depth":   {Type: "integer", Description: "Max directory depth to group by. Default 2, max 5. Dirs deeper than this are folded into their parent at depth."},
			"compact": {Type: "boolean", Description: "Terse output: one line per dir, no header. Default false."},
			"mode":    {Type: "string", Description: "'stats' (default) or 'top'. Top mode returns the files with the most symbols (funcs+types)."},
			"top_n":   {Type: "integer", Description: "Number of top files to return in 'top' mode. Default 10, max 30."},
		},
		Required: []string{},
	},
}

type args struct {
	Path    string `json:"path"`
	Include string `json:"include"`
	Depth   int    `json:"depth"`
	Compact bool   `json:"compact"`
	Mode    string `json:"mode"`
	TopN    int    `json:"top_n"`
}

type dirStats struct {
	files     int
	lines     int
	extCounts map[string]int
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	".idea": true, "__pycache__": true,
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}

	if a.Path == "" {
		a.Path = server.ProjectRoot
	} else {
		a.Path = server.ResolvePath(a.Path)
	}
	if a.Depth <= 0 {
		a.Depth = 2
	}
	a.Depth = min(a.Depth, 5)
	if a.Include == "" {
		a.Include = "*"
	}

	if a.Mode == "top" {
		return handleTop(a)
	}

	useGlob := a.Include != "*"

	stats := map[string]*dirStats{}
	totalFiles, totalLines := 0, 0

	err := filepath.WalkDir(a.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || server.IsBannedPath(path) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if grepfunc.IsBinaryExt(ext) {
			return nil
		}

		rel, _ := filepath.Rel(a.Path, path)
		rel = filepath.ToSlash(rel)

		if useGlob {
			if !grepfunc.MatchGlob(a.Include, rel) {
				return nil
			}
		} else if grepfunc.IsNonSourceExt(ext) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := bytes.Count(data, []byte{'\n'})

		dirRel, _ := filepath.Rel(a.Path, filepath.Dir(path))
		dirRel = filepath.ToSlash(dirRel)
		key := clampDir(dirRel, a.Depth)

		s := stats[key]
		if s == nil {
			s = &dirStats{extCounts: map[string]int{}}
			stats[key] = s
		}
		s.files++
		s.lines += lines
		extKey := ext
		if extKey == "" {
			extKey = "(none)"
		}
		s.extCounts[extKey]++
		totalFiles++
		totalLines += lines
		return nil
	})
	if err != nil {
		return nil, err
	}

	dirs := make([]string, 0, len(stats))
	for k := range stats {
		dirs = append(dirs, k)
	}
	sort.Strings(dirs)

	compact := a.Compact
	var buf strings.Builder
	if compact {
		fmt.Fprintf(&buf, "%s %d files %d lines", server.RelPath(a.Path), totalFiles, totalLines)
	} else {
		fmt.Fprintf(&buf, "File stats for %s (%d files, %d total lines)\n\n", server.RelPath(a.Path), totalFiles, totalLines)
	}

	for _, dir := range dirs {
		s := stats[dir]
		if compact {
			fmt.Fprintf(&buf, "\n%-30s %d files %d lines", dirLabel(dir), s.files, s.lines)
		} else {
			fmt.Fprintf(&buf, "%-30s %6d files %6d lines   %s\n", dirLabel(dir), s.files, s.lines, formatExts(s.extCounts))
		}
	}
	if compact {
		buf.WriteByte('\n')
	} else {
		fmt.Fprintf(&buf, "%-30s %6d files %6d lines\n", "TOTAL", totalFiles, totalLines)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func clampDir(rel string, depth int) string {
	if rel == "." || rel == "" {
		return "."
	}
	parts := strings.Split(rel, "/")
	if len(parts) <= depth {
		return rel
	}
	return strings.Join(parts[:depth], "/")
}

func dirLabel(dir string) string {
	if dir == "." {
		return "."
	}
	return dir + "/"
}

type extCount struct {
	ext   string
	count int
}

type fileSymCount struct {
	file       string
	funcs      int
	types      int
	totalLines int
}

func handleTop(a args) (*server.ToolCallResult, error) {
	topN := a.TopN
	if topN <= 0 {
		topN = 10
	}
	topN = min(topN, 30)

	pattern, err := grepfunc.CompilePattern(".", false)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %v", err)
	}

	funcs, types, err := grepfunc.SearchBoth(a.Path, a.Include, pattern, 50000)
	if err != nil {
		return nil, err
	}

	counts := map[string]*fileSymCount{}
	for _, m := range funcs {
		c := counts[m.File]
		if c == nil {
			c = &fileSymCount{file: m.File}
			counts[m.File] = c
		}
		c.funcs++
	}
	for _, m := range types {
		c := counts[m.File]
		if c == nil {
			c = &fileSymCount{file: m.File}
			counts[m.File] = c
		}
		c.types++
	}

	// Count lines for files with symbols
	for _, c := range counts {
		data, err := os.ReadFile(c.file)
		if err == nil {
			c.totalLines = bytes.Count(data, []byte{'\n'})
		}
	}

	sorted := make([]*fileSymCount, 0, len(counts))
	for _, c := range counts {
		sorted = append(sorted, c)
	}
	sort.Slice(sorted, func(i, j int) bool {
		ti := sorted[i].funcs + sorted[i].types
		tj := sorted[j].funcs + sorted[j].types
		if ti != tj {
			return ti > tj
		}
		return sorted[i].totalLines > sorted[j].totalLines
	})

	end := min(topN, len(sorted))
	sorted = sorted[:end]

	var buf strings.Builder
	fmt.Fprintf(&buf, "Top %d files by symbol count in %s\n\n", end, server.RelPath(a.Path))
	fmt.Fprintf(&buf, "| File | Funcs | Types | Total Lines |\n")
	fmt.Fprintf(&buf, "|------|-------|-------|-------------|\n")
	for _, c := range sorted {
		rel := server.RelPath(c.file)
		fmt.Fprintf(&buf, "| %s | %d | %d | %d |\n", rel, c.funcs, c.types, c.totalLines)
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func formatExts(m map[string]int) string {
	counts := make([]extCount, 0, len(m))
	for ext, n := range m {
		counts = append(counts, extCount{ext, n})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].count != counts[j].count {
			return counts[i].count > counts[j].count
		}
		return counts[i].ext < counts[j].ext
	})
	show := min(3, len(counts))
	parts := make([]string, 0, show+1)
	for i := range show {
		parts = append(parts, fmt.Sprintf("%s(%d)", counts[i].ext, counts[i].count))
	}
	if len(counts) > 3 {
		parts = append(parts, fmt.Sprintf("+%d more", len(counts)-3))
	}
	return strings.Join(parts, " ")
}
