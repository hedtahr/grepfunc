// Package grepfunc scans source trees for function and type blocks.
package grepfunc

import (
	"bytes"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/hedtahr/grepfunc/server"
)

// Constants for the brace-counting state machine and search caps.
const (
	searchBothFactor = 2
	initialStackCap  = 16

	// searchWorkerCap bounds the parallel search pool. ripgrep's benchmarks
	// show 4-8 workers is the Amdahl sweet spot; more adds contention.
	searchWorkerCap = 4
	// binarySniffLen is how many leading bytes are checked for NUL bytes to
	// classify a file as binary (ripgrep's heuristic: fast, extension-agnostic).
	binarySniffLen = 64 * 1024

	stateCode         = 0 // code context
	stateDoubleQuote  = 1
	stateSingleQuote  = 2
	stateBacktick     = 3
	stateLineComment  = 4
	stateBlockComment = 5
)

// CompilePattern wraps a user pattern into a regex. (?m) is always on so ^ and $
// anchor lines, which is what matching file contents means to a caller.
func CompilePattern(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}

	re, err := regexp.Compile("(?m)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex %q: %w", pattern, err)
	}

	return re, nil
}

// SearchNames is Search without body materialisation: name, line range and size
// are populated, Body is left empty. Use it when the caller only wants names.
func SearchNames(root, glob string, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	return search(root, glob, pattern, limit, sigFn, false)
}

// Search walks the directory tree, finds matching files, and extracts blocks.
// sigFn detects whether a line starts a code block (function, struct, class, etc).
// Traversal is parallelized over a small bounded worker pool (ripgrep-style:
// Amdahl's law makes 4-8 workers optimal; more mostly adds contention).
// Results are sorted by (file, line) so output stays deterministic.
func Search(root, glob string, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	return search(root, glob, pattern, limit, sigFn, true)
}

// search is Search with an option to skip body materialisation: names-only
// callers never read Body, so joining it is wasted work and memory.
func search(root, glob string, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool, needBody bool) ([]FuncMatch, error) {
	matcher := CompileGlob(glob)
	workers := min(searchWorkerCap, max(1, runtime.GOMAXPROCS(0)))

	type fileJob struct {
		path  string
		entry fs.DirEntry
	}

	jobs := make(chan fileJob, workers*4)
	results := make(chan []FuncMatch, workers*2)
	errCh := make(chan error, workers)
	stop := make(chan struct{})

	var wg sync.WaitGroup

	for range workers {
		wg.Go(func() {
			for j := range jobs {
				funcs, err := searchFile(root, j.path, matcher, j.entry, pattern, limit, sigFn, needBody)
				if err != nil {
					errCh <- err

					return
				}

				if len(funcs) == 0 {
					continue
				}

				for i := range funcs {
					funcs[i].File = j.path
				}

				results <- funcs
			}
		})
	}

	walkDone := make(chan struct{})
	var walkErr error

	gi := loadGitignore(root)

	go func() {
		defer close(jobs)
		defer close(walkDone)

		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if walkErr == nil {
					walkErr = err
				}

				return filepath.SkipDir
			}

			if entry.IsDir() {
				base := entry.Name()
				if isSkippableDir(base) {
					return filepath.SkipDir
				}

				if rel, relErr := filepath.Rel(root, path); relErr == nil && gi.ignores(rel, true) {
					return filepath.SkipDir
				}

				return nil
			}

			if rel, relErr := filepath.Rel(root, path); relErr == nil && gi.ignores(rel, false) {
				return nil
			}

			select {
			case jobs <- fileJob{path: path, entry: entry}:
				return nil

			case <-stop:
				return filepath.SkipAll
			}
		})
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var (
		out      []FuncMatch
		firstErr error
	)
	var stopOnce sync.Once

	for funcs := range results {
		out = append(out, funcs...)

		if len(out) >= limit {
			stopOnce.Do(func() { close(stop) })
		}
	}

	// Workers send at most one error each before exiting, so a single
	// non-blocking read after results close captures the first error.
	select {
	case err := <-errCh:
		firstErr = err

	default:
	}

	<-walkDone

	if walkErr != nil && firstErr == nil {
		firstErr = walkErr
	}

	if firstErr != nil && len(out) == 0 {
		return nil, fmt.Errorf("walk %s: %w", root, firstErr)
	}

	if firstErr != nil {
		return out, fmt.Errorf("walk %s: %w", root, firstErr)
	}

	slices.SortFunc(out, func(a, b FuncMatch) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}

		if a.Line < b.Line {
			return -1
		}

		if a.Line > b.Line {
			return 1
		}

		return 0
	})

	if limit < 0 {
		limit = 0
	}

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// isSkippableDir reports whether a directory should be excluded from searches.
func isSkippableDir(base string) bool {
	return base == ".git" || base == "node_modules" || base == "vendor" || base == ".idea" ||
		base == "__pycache__" || strings.HasPrefix(base, ".")
}

// searchFile extracts matching blocks from one regular file during a walk.
func searchFile(root, path string, matcher *GlobMatcher, entry fs.DirEntry, pattern *regexp.Regexp,
	remaining int, sigFn func([]byte) bool, needBody bool) ([]FuncMatch, error) {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil, nil
	}

	rel, _ := filepath.Rel(root, path)

	if !matcher.Match(rel) {
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
	if IsBinaryExt(ext) {
		return nil, nil
	}
	// When the glob is the unrestricted default, extension filtering applies.
	if matcher.IsDefault() && IsNonSourceExt(ext) {
		return nil, nil
	}

	// #nosec G304 -- path is bounds-checked by the server
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// NUL-byte sniff (ripgrep's binary heuristic): catches binaries with
	// misleading extensions that the extension list misses.
	if bytes.IndexByte(data[:min(len(data), binarySniffLen)], 0) >= 0 {
		return nil, nil
	}

	return extractBlocksData(path, data, pattern, remaining, sigFn, needBody)
}

// SearchBoth finds funcs and types in a single walk.
func SearchBoth(root, glob string, pattern *regexp.Regexp, limit int) ([]FuncMatch, []FuncMatch, error) {
	combined := func(line []byte) bool {
		return IsFuncSig(line) || IsStructSig(line)
	}

	all, err := Search(root, glob, pattern, limit*searchBothFactor, combined)
	if err != nil {
		return nil, nil, err
	}

	var funcs, types []FuncMatch

	for _, match := range all {
		firstLine := []byte(match.Body)
		if nl := bytes.IndexByte(firstLine, '\n'); nl >= 0 {
			firstLine = firstLine[:nl]
		}

		if IsFuncSig(firstLine) {
			funcs = append(funcs, match)
		} else {
			types = append(types, match)
		}
	}

	return funcs, types, nil
}

// IsBinaryExt reports whether ext is a known binary or archive file extension.
// Text formats that are merely uninteresting (.svg, .lock, .sum) are not binary:
// IsNonSourceExt skips those by default while leaving them searchable on request.
func IsBinaryExt(ext string) bool {
	switch ext {
	case ".exe", ".dll", ".so", ".dylib", ".a", ".o", ".obj",
		".class", ".jar", ".war", ".ear",
		".zip", ".tar", ".gz", ".bz2", ".7z", ".rar",
		".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico",
		".mp3", ".mp4", ".wav", ".avi", ".mov", ".mkv",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".ttf", ".otf", ".woff", ".woff2",
		".bin", ".dat", ".db", ".sqlite", ".sqlite3":
		return true
	}

	return false
}

// IsNonSourceExt reports whether ext is a known non-source format — docs, data,
// config, lock files — that searches skip unless an include glob asks for them.
// It is deliberately a denylist: an allowlist silently hides every language the
// tool has not heard of (a .plsql file used to look empty). Files with no
// extension (Makefile, Dockerfile, LICENSE) are searched.
func IsNonSourceExt(ext string) bool {
	if ext == "" {
		return false
	}

	switch ext {
	case ".md", ".markdown", ".mdx", ".rst", ".adoc", ".asciidoc",
		".txt", ".org", ".texi",
		".json", ".jsonc", ".json5", ".ipynb",
		".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".properties", ".env",
		".csv", ".tsv", ".ndjson",
		".lock", ".sum",
		".log", ".map", ".snap":
		return true
	}

	return false
}

// MatchGlob matches a file path against a glob pattern with ** support.
// ** matches zero or more directory components; {a,b} sets are expanded.
func MatchGlob(pattern, path string) bool {
	if !strings.ContainsRune(pattern, '{') {
		return matchGlob(pattern, path)
	}

	for _, candidate := range expandBraces(pattern) {
		if matchGlob(candidate, path) {
			return true
		}
	}

	return false
}

// maxGlobExpansions caps brace expansion so pathological patterns stay cheap.
const (
	maxGlobExpansions = 64
	starStar          = "**"
)

// expandBraces expands {a,b} sets, e.g. **/*.{go,sql} → **/*.go, **/*.sql.
// A group without a top-level comma stays literal, so matchGlob keeps seeing it.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}

	shut := matchingBrace(pattern, open)
	if shut < 0 {
		return []string{pattern}
	}

	alts := splitAlternates(pattern[open+1 : shut])
	if len(alts) < 2 {
		return []string{pattern}
	}

	var expanded []string

	for _, alt := range alts {
		expanded = append(expanded, expandBraces(pattern[:open]+alt+pattern[shut+1:])...)
		if len(expanded) >= maxGlobExpansions {
			return expanded[:maxGlobExpansions]
		}
	}

	return expanded
}

func matchingBrace(pattern string, open int) int {
	depth := 0

	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// splitAlternates splits on commas that are not inside a nested brace group.
func splitAlternates(group string) []string {
	var alts []string

	depth, start := 0, 0

	for i := 0; i < len(group); i++ {
		switch group[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				alts = append(alts, group[start:i])

				start = i + 1
			}
		}
	}

	return append(alts, group[start:])
}

func matchGlob(pattern, path string) bool {
	// Fast path: no path separators in pattern → match against base name only
	if !strings.ContainsAny(pattern, "/\\") {
		matched, _ := filepath.Match(pattern, filepath.Base(path))

		return matched
	}

	// Split pattern and path into components
	patParts := splitPath(pattern)
	pathParts := splitPath(path)

	return matchParts(patParts, pathParts)
}

// GlobMatcher matches root-relative paths against one compiled glob. Compile it
// once per search: Match walks the path in place, so a per-file call allocates
// nothing, where MatchGlob re-splits pattern and path every time.
type GlobMatcher struct {
	pattern string
	alts    []globAlt
}

type globAlt struct {
	parts []globPart
	base  bool // pattern has no separator: match the base name only
}

// globPart is one compiled path component of a glob.
type globPart struct {
	text  string
	plain bool // no ** and no [] \ escapes: match without filepath.Match
	spans bool // "**" spans zero or more components
}

// CompileGlob precompiles pattern (brace sets included) for repeated matching.
func CompileGlob(pattern string) *GlobMatcher {
	candidates := expandBraces(pattern)

	matcher := &GlobMatcher{pattern: pattern, alts: make([]globAlt, 0, len(candidates))}

	for _, candidate := range candidates {
		norm := strings.TrimPrefix(filepath.ToSlash(candidate), "./")
		texts := splitPath(norm)
		parts := make([]globPart, len(texts))

		for i, text := range texts {
			parts[i] = globPart{
				text:  text,
				plain: !strings.ContainsAny(text, "[\\"),
				spans: text == starStar,
			}
		}

		matcher.alts = append(matcher.alts, globAlt{
			parts: parts, base: !strings.ContainsRune(norm, '/'),
		})
	}

	return matcher
}

// IsDefault reports whether the matcher came from the unrestricted "*" pattern,
// which is when callers apply their source-extension filtering.
func (m *GlobMatcher) IsDefault() bool {
	return m != nil && m.pattern == "*"
}

// Match reports whether path (root-relative) matches any compiled alternative.
func (m *GlobMatcher) Match(path string) bool {
	if m == nil {
		return false
	}

	for i := range m.alts {
		if m.alts[i].match(path) {
			return true
		}
	}

	return false
}

func (a *globAlt) match(path string) bool {
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")

	if a.base {
		name := path
		if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
			name = path[idx+1:]
		}

		return matchComponent(a.parts[0], name)
	}

	if path == "" || path == "." {
		return len(a.parts) == 0
	}

	return matchPartsFrom(a.parts, path, 0)
}

// matchPartsFrom matches pattern parts against the path's components starting at
// byte offset ofs, without splitting the path.
func matchPartsFrom(parts []globPart, path string, ofs int) bool {
	if len(parts) == 0 {
		return ofs >= len(path)
	}

	if parts[0].spans { // ** spans zero or more components
		if matchPartsFrom(parts[1:], path, ofs) {
			return true
		}

		for ofs < len(path) {
			ofs = nextComponent(path, ofs)
			if matchPartsFrom(parts[1:], path, ofs) {
				return true
			}
		}

		return false
	}

	if ofs >= len(path) {
		return false
	}

	if !matchComponent(parts[0], path[ofs:componentEnd(path, ofs)]) {
		return false
	}

	return matchPartsFrom(parts[1:], path, nextComponent(path, ofs))
}

func matchComponent(part globPart, comp string) bool {
	if !part.plain {
		matched, _ := filepath.Match(part.text, comp)

		return matched
	}

	return matchSimple(part.text, comp)
}

// matchSimple matches a component made only of literals, '*' and '?', greedily
// backtracking on '*', the way filepath.Match would but without allocating or
// scanning the pattern twice.
func matchSimple(pattern, name string) bool {
	p, n := 0, 0
	star, mark := -1, 0

	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == name[n] || pattern[p] == '?'):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, n
			p++
		case star >= 0:
			p, mark = star+1, mark+1
			n = mark
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}

	return p == len(pattern)
}

func nextComponent(path string, ofs int) int {
	if idx := strings.IndexByte(path[ofs:], '/'); idx >= 0 {
		return ofs + idx + 1
	}

	return len(path)
}

func componentEnd(path string, ofs int) int {
	if idx := strings.IndexByte(path[ofs:], '/'); idx >= 0 {
		return ofs + idx
	}

	return len(path)
}

func splitPath(path string) []string {
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	if path == "" || path == "." {
		return nil
	}

	return strings.Split(path, "/")
}

func matchParts(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}

	part := pat[0]

	if part == starStar {
		// ** matches zero or more path components
		for i := 0; i <= len(path); i++ {
			if matchParts(pat[1:], path[i:]) {
				return true
			}
		}

		return false
	}

	if len(path) == 0 {
		return false
	}

	matched, _ := filepath.Match(part, path[0])
	if !matched {
		return false
	}

	return matchParts(pat[1:], path[1:])
}

// extractBlocks returns all blocks (functions, structs, etc) in a file matching pattern.
func extractBlocks(filePath string, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	data, err := os.ReadFile(filePath) // #nosec G304 -- paths bounds-checked by server
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}

	return extractBlocksData(filePath, data, pattern, limit, sigFn, true)
}

// extractBlocksData scans an already-read file's contents for blocks matching
// pattern, avoiding a second read when the caller sniffed the file first.
func extractBlocksData(filePath string, data []byte, pattern *regexp.Regexp, limit int,
	sigFn func([]byte) bool, needBody bool) ([]FuncMatch, error) {
	lines := toLines(data)
	if len(lines) == 0 {
		return nil, nil
	}

	// .go symbol extraction stays on the brace scanner: go/parser is 4x slower and
	// 40x more allocation-heavy (BenchmarkGoParserSymbols vs BraceScannerSymbols),
	// and TestBraceScannerMatchesGoParser shows the two agree on real Go files.
	// parseGoSymbols is kept as that oracle.
	if isPythonExt(strings.ToLower(filepath.Ext(filePath))) {
		return extractBlocksIndent(lines, pattern, limit, sigFn, needBody)
	}

	return braceBlocks(lines, pattern, limit, sigFn, needBody), nil
}

// braceBlocks is the language-agnostic scanner: brace counting plus a per-line
// match pass, with whole-block matching as the fallback for spanning patterns.
func braceBlocks(lines [][]byte, pattern *regexp.Regexp, limit int,
	sigFn func([]byte) bool, needBody bool) []FuncMatch {
	boundaries := mapBlockBoundaries(lines, sigFn)

	// Find lines matching the pattern
	var results []FuncMatch

	seen := make(map[int]bool) // dedup by func start line

	for lineIdx, line := range lines {
		if !pattern.Match(line) {
			continue
		}

		fnStart, ok := boundaries.Start(lineIdx)
		if !ok || seen[fnStart] {
			continue
		}
		// Belt-and-suspenders: fnStart must actually be a valid signature
		if !sigFn(lines[fnStart]) {
			continue
		}

		seen[fnStart] = true

		results = append(results, *buildFuncMatch(lines, fnStart, boundaries.EndLine(fnStart), needBody))
		if len(results) >= limit {
			break
		}
	}

	// A pattern that can match a line break never matches a single line, so the
	// loop above finds nothing: retry against whole blocks.
	if len(results) == 0 && canMatchNewline(pattern) {
		return matchJoinedBlocks(lines, boundaries, pattern, limit, sigFn, needBody)
	}

	return results
}

// matchJoinedBlocks matches pattern against each block's full text, so patterns
// containing a line break can match. Slower than the line loop (one join per
// block), which is why it is only used as a fallback.
func matchJoinedBlocks(lines [][]byte, boundaries BlockBoundaries, pattern *regexp.Regexp,
	limit int, sigFn func([]byte) bool, needBody bool) []FuncMatch {
	var results []FuncMatch

	for lineIdx := range lines {
		if !boundaries.IsBlockStart(lineIdx) || !sigFn(lines[lineIdx]) {
			continue
		}

		if !pattern.MatchString(joinBlock(lines, lineIdx, boundaries.EndLine(lineIdx))) {
			continue
		}

		results = append(results, *buildFuncMatch(lines, lineIdx, boundaries.EndLine(lineIdx), needBody))
		if len(results) >= limit {
			break
		}
	}

	return results
}

// canMatchNewline reports whether re can match a line break. Patterns that cannot
// are matched line by line; the rest fall back to whole-block matching. Anything
// unresolvable takes the complete (slower) path.
func canMatchNewline(re *regexp.Regexp) bool {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return true
	}

	return regexMatchesNewline(parsed)
}

func regexMatchesNewline(node *syntax.Regexp) bool {
	switch node.Op {
	case syntax.OpLiteral:
		return slices.Contains(node.Rune, '\n')
	case syntax.OpCharClass:
		for i := 0; i+1 < len(node.Rune); i += 2 {
			if node.Rune[i] <= '\n' && '\n' <= node.Rune[i+1] {
				return true
			}
		}

		return false
	case syntax.OpAnyChar:
		return true
	case syntax.OpAnyCharNotNL:
		return false
	}

	for _, sub := range node.Sub {
		if regexMatchesNewline(sub) {
			return true
		}
	}

	return false
}

// isPythonExt reports whether ext belongs to a Python source file.
func isPythonExt(ext string) bool {
	return ext == ".py" || ext == ".pyi" || ext == ".pyx"
}

// toLines splits data into lines, preserving the line content without trailing \n or \r.
func toLines(data []byte) [][]byte {
	var lines [][]byte

	start := 0

	for i, b := range data {
		if b == '\n' {
			lines = append(lines, trimCR(data[start:i]))
			start = i + 1
		}
	}

	if start < len(data) {
		lines = append(lines, trimCR(data[start:]))
	}

	return lines
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}

	return b
}

// fnEntry tracks a function being built during the scan.
type fnEntry struct {
	startLine int  // signature line index
	bodyDepth int  // brace depth just before opening brace (-1 if not yet found)
	keyword   bool // signature carried a func/def/fn keyword
}

// BlockBoundaries maps every line index to the start line of the innermost block
// containing it, or NoBlock. Slices (not maps) keep lookups O(1) and cheap: the
// map version hashed every line of every scanned file.
type BlockBoundaries struct {
	starts []int32
	ends   []int32 // set on block-start lines: that block's last line
}

// NoBlock marks a line that is not inside any block.
const NoBlock int32 = -1

// Start returns the start line of the block enclosing lineIdx.
func (b BlockBoundaries) Start(lineIdx int) (int, bool) {
	if lineIdx < 0 || lineIdx >= len(b.starts) || b.starts[lineIdx] == NoBlock {
		return 0, false
	}

	return int(b.starts[lineIdx]), true
}

// IsBlockStart reports whether lineIdx begins a block rather than falling inside one.
func (b BlockBoundaries) IsBlockStart(lineIdx int) bool {
	if lineIdx < 0 || lineIdx >= len(b.starts) {
		return false
	}

	return int(b.starts[lineIdx]) == lineIdx
}

// EndLine returns the last line index belonging to the block at startLine.
func (b BlockBoundaries) EndLine(startLine int) int {
	if startLine < 0 || startLine >= len(b.ends) || b.ends[startLine] == NoBlock {
		return startLine
	}

	return int(b.ends[startLine])
}

// Empty reports whether no lines were mapped.
func (b BlockBoundaries) Empty() bool {
	return len(b.starts) == 0
}

// MapBlockBoundaries maps every line index in a file to the start line of the innermost
// function/type/struct/class block that contains it, using sigFn to detect block signatures.
func MapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) BlockBoundaries {
	return mapBlockBoundaries(lines, sigFn)
}

// line index → inner-most block start line index.
// newBlockBoundaries allocates the per-line mapping, pre-filled with NoBlock.
func newBlockBoundaries(lineCount int) BlockBoundaries {
	starts := make([]int32, lineCount)
	ends := make([]int32, lineCount)

	for i := range starts {
		starts[i], ends[i] = NoBlock, NoBlock
	}

	return BlockBoundaries{starts: starts, ends: ends}
}

// mapBlockBoundaries returns the zero value for input too large to index with
// int32; callers cap files far below that, so it is a guard, not a limit.
func mapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) BlockBoundaries {
	if len(lines) > math.MaxInt32 {
		return BlockBoundaries{}
	}

	boundaries := newBlockBoundaries(len(lines))

	stack := make([]fnEntry, 0, initialStackCap)

	depth, state := 0, byte(stateCode)

	for lineIdx, line := range lines {
		startsInCode := state == stateCode

		depthBefore := depth
		opens, closes, nextState := scanLine(line, state)
		state = nextState
		depth = depthBefore + opens - closes

		// Detect new block signature (function, struct, class, etc). A line that
		// starts inside a string or comment is text: detecting a signature there
		// invents symbols from code samples embedded in raw strings.
		if startsInCode && sigFn(line) && !awaitingKeywordSignature(stack) {
			keyword := signatureKeyword(line)

			if opens > 0 {
				// Signature + opening brace on same line: bodyDepth = depth before signature
				stack = append(stack, fnEntry{startLine: lineIdx, bodyDepth: depthBefore, keyword: keyword})
			} else {
				// Signature without brace; bodyDepth set when brace found
				stack = append(stack, fnEntry{startLine: lineIdx, bodyDepth: -1, keyword: keyword})
			}
		}

		// If top of stack is waiting for its opening brace
		if len(stack) > 0 && stack[len(stack)-1].bodyDepth < 0 && opens > 0 {
			stack[len(stack)-1].bodyDepth = depthBefore
		}

		// Check if any functions ended (depth returned to bodyDepth)
		stack = closeEndedBlocks(stack, lineIdx, depth, boundaries)
	}

	closeUnterminated(stack, len(lines), boundaries)

	return boundaries
}

// signatureKeyword reports whether a line opens with a function keyword
// (func/fn/fun/def/function, possibly behind pub/async/export). It reads bytes so
// the scan path does not allocate for every candidate signature line.
func signatureKeyword(line []byte) bool {
	trimmed := bytes.TrimSpace(line)

	end := bytes.IndexAny(trimmed, " (")
	if end < 0 {
		end = len(trimmed)
	}

	switch string(trimmed[:end]) { //nolint:staticcheck // string(b) in a switch is folded, no allocation
	case "func", "fn", "fun", "def", "function":
		return true
	}

	fields := bytes.Fields(trimmed)
	if len(fields) > 1 {
		switch string(fields[0]) {
		case "pub", "pub(crate)", "pub(super)", "async", "export":
			return true
		}
	}

	// Mid-line " function(" ("x = function() {", "export default function() {").
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != ' ' {
			continue
		}

		rest := trimmed[i+1:]
		if len(rest) >= 9 && string(rest[:9]) == "function(" {
			return true
		}

		if len(rest) >= 10 && string(rest[:10]) == "function (" {
			return true
		}
	}

	return false
}

// awaitingKeywordSignature reports whether the innermost pending entry is a
// func/def/fn signature still looking for its opening brace. Continuation lines of
// such a signature ("\tremaining int, sigFn func([]byte) bool) error {") must not
// be taken for signatures of their own — that shadows the real line and its name.
func awaitingKeywordSignature(stack []fnEntry) bool {
	if len(stack) == 0 {
		return false
	}

	top := stack[len(stack)-1]

	return top.bodyDepth < 0 && top.keyword
}

// closeEndedBlocks pops functions whose body closed at line i, mapping their lines.
func closeEndedBlocks(stack []fnEntry, i, depth int, boundaries BlockBoundaries) []fnEntry {
	for len(stack) > 0 && stack[len(stack)-1].bodyDepth >= 0 && depth == stack[len(stack)-1].bodyDepth {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		boundaries.ends[top.startLine] = int32(i) // #nosec G115 -- guarded by len(lines) check

		// Map only unclaimed lines (inner functions keep their mapping)
		for j := top.startLine; j <= i; j++ {
			if boundaries.starts[j] == NoBlock {
				boundaries.starts[j] = int32(top.startLine) // #nosec G115 -- guarded by len(lines) check
			}
		}
	}

	return stack
}

// closeUnterminated maps functions left open at EOF to the last line.
func closeUnterminated(stack []fnEntry, lineCount int, boundaries BlockBoundaries) {
	for _, v := range slices.Backward(stack) {
		if v.bodyDepth >= 0 {
			boundaries.ends[v.startLine] = int32(lineCount - 1) // #nosec G115 -- guarded by len(lines) check

			for j := v.startLine; j < lineCount; j++ {
				if boundaries.starts[j] == NoBlock {
					boundaries.starts[j] = int32(v.startLine) // #nosec G115 -- guarded by len(lines) check
				}
			}
		}
	}
}

// EnclosingSymbol returns the name of the function/type that encloses lineIdx (0-based).
func EnclosingSymbol(lines [][]byte, lineIdx int, boundaries BlockBoundaries) string {
	start, ok := boundaries.Start(lineIdx)
	if !ok {
		return ""
	}

	return extractFuncName(string(lines[start]))
}

func buildFuncMatch(lines [][]byte, fnStart, fnEnd int, needBody bool) *FuncMatch {
	name := extractFuncName(string(lines[fnStart]))

	var body string

	if needBody {
		body = joinBlock(lines, fnStart, fnEnd)
	}

	return &FuncMatch{
		Line:    fnStart + 1,
		EndLine: fnEnd + 1,
		Name:    name,
		Lines:   fnEnd - fnStart + 1,
		Body:    body,
		File:    "",
		Kind:    "",
	}
}

// braceDelta counts { and } in a line, skipping strings and comments.
// For a whole file use scanLine, which carries state across lines: a raw string or
// block comment opened on one line continues on the next.
func braceDelta(line []byte) (int, int) {
	opens, closes, _ := scanLine(line, stateCode)

	return opens, closes
}

// scanLine counts braces in one line and returns the lexical state entering the
// next one. Multi-line constructs (backtick strings, block comments) carry over;
// quotes do not, because a newline inside them ends the construct here.
func scanLine(line []byte, state byte) (int, int, byte) {
	opens, closes := 0, 0

	for idx := 0; idx < len(line); idx++ {
		done := false

		switch state {
		case stateCode:
			opens, closes, state, idx, done = scanCodeChar(line, idx, opens, closes)
		case stateDoubleQuote:
			state, idx = scanQuotedChar(line, idx, stateDoubleQuote, '"', true)
		case stateSingleQuote:
			state, idx = scanQuotedChar(line, idx, stateSingleQuote, '\'', true)
		case stateBacktick:
			state, idx = scanQuotedChar(line, idx, stateBacktick, '`', false)
		case stateBlockComment:
			state, idx = scanBlockCommentChar(line, idx)
		}

		if done {
			break
		}
	}

	switch state {
	case stateLineComment, stateDoubleQuote, stateSingleQuote:
		state = stateCode
	}

	return opens, closes, state
}

// scanCodeChar handles one char in code context, updating brace counts and state.
func scanCodeChar(line []byte, idx, opens, closes int) (int, int, byte, int, bool) {
	switch line[idx] {
	case '{':
		opens++
	case '}':
		closes++
	case '"':
		return opens, closes, stateDoubleQuote, idx, false
	case '\'':
		return opens, closes, stateSingleQuote, idx, false
	case '`':
		return opens, closes, stateBacktick, idx, false
	case '/':
		if idx+1 < len(line) {
			switch line[idx+1] {
			case '/':
				return opens, closes, stateLineComment, idx, true
			case '*':
				return opens, closes, stateBlockComment, idx + 1, false
			}
		}
	}

	return opens, closes, stateCode, idx, false
}

// scanQuotedChar handles one char inside a quoted string, honoring escapes when enabled.
func scanQuotedChar(line []byte, idx int, state byte, quote byte, escapes bool) (byte, int) {
	c := line[idx]
	if escapes && c == '\\' && idx+1 < len(line) {
		return state, idx + 1
	}

	if c == quote {
		return stateCode, idx
	}

	return state, idx
}

// scanBlockCommentChar handles one char inside a block comment, exiting at */.
func scanBlockCommentChar(line []byte, i int) (byte, int) {
	if line[i] == '*' && i+1 < len(line) && line[i+1] == '/' {
		return stateCode, i + 1
	}

	return stateBlockComment, i
}

func insideQuote(str string, idx int) bool {
	quoteCount := 0

	for pos := 0; pos < idx; pos++ {
		if str[pos] == '\\' && pos+1 < idx {
			pos++

			continue
		}

		if str[pos] == '"' {
			quoteCount++
		}
	}

	return quoteCount%2 == 1
}

// IsStructSig detects struct/class/interface/enum/type definition signatures.
// Matches: Go, Rust, TS/JS, Java, C/C++, C#, Python, Kotlin, Swift, etc.
func IsStructSig(line []byte) bool {
	sig := strings.TrimSpace(string(line))
	if sig == "" || isCommentStart(sig) {
		return false
	}

	if !strings.ContainsAny(sig, "{:") {
		return false
	}

	if isGoTypeDecl(sig) || isRustTypeDecl(sig) || isPythonClass(sig) || isJSTypeDecl(sig) {
		return true
	}

	return isModifierTypeDecl(stripTypeModifiers(sig))
}

// isCommentStart reports whether s starts with a comment or directive marker.
func isCommentStart(s string) bool {
	return s[0] == '#' || s[0] == '/' || s[0] == '*'
}

// isGoTypeDecl matches Go declarations like "type Name struct {".
func isGoTypeDecl(s string) bool {
	return strings.HasPrefix(s, "type ") && (strings.Contains(s, " struct ") || strings.Contains(s, " struct{") ||
		strings.Contains(s, " interface ") || strings.Contains(s, " interface{"))
}

// isRustTypeDecl matches Rust struct/enum/trait/impl declarations.
func isRustTypeDecl(s string) bool {
	return strings.HasPrefix(s, "struct ") || strings.HasPrefix(s, "enum ") ||
		strings.HasPrefix(s, "trait ") || strings.HasPrefix(s, "impl ") ||
		strings.HasPrefix(s, "pub struct ") || strings.HasPrefix(s, "pub enum ") ||
		strings.HasPrefix(s, "pub trait ") || strings.HasPrefix(s, "pub impl ")
}

// isPythonClass matches "class Name:" declarations.
func isPythonClass(s string) bool {
	return strings.HasPrefix(s, "class ") && strings.HasSuffix(s, ":")
}

// isJSTypeDecl matches JS/TS class/interface/type-alias declarations.
func isJSTypeDecl(s string) bool {
	return strings.HasPrefix(s, "class ") || strings.HasPrefix(s, "interface ") ||
		(strings.HasPrefix(s, "type ") && strings.Contains(s, "=")) ||
		strings.HasPrefix(s, "export class ") || strings.HasPrefix(s, "export interface ") ||
		strings.HasPrefix(s, "export type ") || strings.HasPrefix(s, "export default class ") ||
		strings.HasPrefix(s, "abstract class ")
}

// stripTypeModifiers removes visibility/static keywords from the start of sig.
func stripTypeModifiers(sig string) string {
	modifiers := []string{"public ", "private ", "protected ", "internal ", "static ", "abstract ",
		"sealed ", "final ", "open ", "data ", "override ", "export "}
	for _, mod := range modifiers {
		sig = strings.TrimPrefix(sig, mod)
	}

	return sig
}

// isModifierTypeDecl matches class-like declarations after modifiers were stripped.
func isModifierTypeDecl(s string) bool {
	return strings.HasPrefix(s, "class ") || strings.HasPrefix(s, "interface ") ||
		strings.HasPrefix(s, "enum ") || strings.HasPrefix(s, "enum class ") ||
		strings.HasPrefix(s, "record ") || strings.HasPrefix(s, "object ") ||
		strings.HasPrefix(s, "struct ") || strings.HasPrefix(s, "protocol ") ||
		strings.HasPrefix(s, "extension ") || strings.HasPrefix(s, "actor ")
}

// IsFuncSig checks if a line looks like a function/method definition.
func IsFuncSig(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	// Skip obvious non-signatures
	if trimmed[0] == '#' || trimmed[0] == '/' || trimmed[0] == '*' {
		return false
	}

	return isFuncSigStart(string(trimmed))
}

// isFuncSigStart checks a trimmed signature line for function markers.
func isFuncSigStart(sig string) bool {
	// Reject multi-line signature continuations and body lines
	if sig[0] == ')' || sig[0] == ',' || sig[0] == ']' || sig[0] == '{' || sig[0] == '}' {
		return false
	}

	// Must contain opening paren (function parameter list)
	openParen := strings.Index(sig, "(")
	if openParen < 0 {
		return false
	}

	// A continuation line ("\tc int, d bool) error {") closes its parameter list
	// before opening the result list; a real signature opens "(" first. Without
	// this the continuation shadows the signature line it belongs to.
	if closeParen := strings.Index(sig, ")"); closeParen >= 0 && closeParen < openParen {
		return false
	}

	if isLangFuncKeyword(sig) || hasFuncKeyword(sig) || isAnonFuncAssign(sig) {
		return true
	}

	// Strip visibility/lifetime modifiers unconditionally
	return looksLikeFuncStart(stripFuncModifiers(sig))
}

// isLangFuncKeyword matches language function keywords at the start of sig.
func isLangFuncKeyword(sig string) bool {
	return strings.HasPrefix(sig, "func ") || strings.HasPrefix(sig, "func(") ||
		strings.HasPrefix(sig, "fn ") || strings.HasPrefix(sig, "pub fn ") ||
		strings.HasPrefix(sig, "pub(crate) fn ") || strings.HasPrefix(sig, "pub(super) fn ") ||
		strings.HasPrefix(sig, "fun ") ||
		strings.HasPrefix(sig, "def ") || strings.HasPrefix(sig, "async def ") ||
		strings.HasPrefix(sig, "function ") || strings.HasPrefix(sig, "function(")
}

// hasFuncKeyword finds " function(" or " function (" outside string literals.
func hasFuncKeyword(sig string) bool {
	for _, pat := range []string{" function(", " function ("} {
		idx := strings.Index(sig, pat)
		if idx >= 0 && !insideQuote(sig, idx) {
			return true
		}
	}

	return false
}

// isAnonFuncAssign matches JS/TS arrow functions assigned to a name.
func isAnonFuncAssign(sig string) bool {
	return (strings.HasPrefix(sig, "const ") || strings.HasPrefix(sig, "let ") || strings.HasPrefix(sig, "var ")) &&
		(strings.Contains(sig, "=>") || (strings.Contains(sig, "= (") && strings.Contains(sig, "{")))
}

// stripFuncModifiers removes visibility/lifetime modifiers from the start of sig.
func stripFuncModifiers(sig string) string {
	sig = strings.TrimPrefix(sig, "public ")
	sig = strings.TrimPrefix(sig, "private ")
	sig = strings.TrimPrefix(sig, "protected ")
	sig = strings.TrimPrefix(sig, "static ")
	sig = strings.TrimPrefix(sig, "async ")
	sig = strings.TrimPrefix(sig, "virtual ")
	sig = strings.TrimPrefix(sig, "override ")
	sig = strings.TrimPrefix(sig, "export ")

	return strings.TrimPrefix(sig, "abstract ")
}

// looksLikeFuncStart checks if remaining string after modifiers looks like a function.
func looksLikeFuncStart(sig string) bool {
	if isConstructorStart(sig) {
		return true
	}

	if isControlFlowStart(sig) {
		return false
	}

	if isAssignmentStart(sig) {
		return false
	}

	// Calls look like declarations when their arguments open a brace:
	// "send(Response{" and "s.writeJSON(Response{" are expressions, while
	// "void foo(", "someMethod(p: T): R {" and "render() {" are declarations.
	if idx := strings.Index(sig, "("); idx >= 0 {
		// Rust method chains take closures: ".map(|x| {".
		closureParams := strings.HasPrefix(sig[idx+1:], "|")

		// A dot-qualified callee is a call, never a declaration.
		if idx > 0 && strings.ContainsAny(sig[:idx], ".-") && !closureParams {
			return false
		}

		// A declaration closes its parameter list before the block opens.
		if !closureParams && strings.HasSuffix(sig, "{") && !strings.Contains(sig[idx:], ")") {
			return false
		}
	}

	// Quick check: ends with { or has ( followed eventually by ) then { or :
	return (strings.HasSuffix(strings.TrimSpace(sig), "{") ||
		strings.HasSuffix(strings.TrimSpace(sig), ":")) &&
		strings.Contains(sig, "(") &&
		!strings.HasPrefix(sig, "if ") &&
		!strings.HasPrefix(sig, "for ") &&
		!strings.HasPrefix(sig, "while ")
}

// isConstructorStart matches constructor( or constructor-prefixed signatures.
func isConstructorStart(s string) bool {
	return strings.HasPrefix(s, "constructor(") || strings.HasPrefix(s, "constructor ")
}

// isControlFlowStart reports whether s starts with a control-flow keyword.
func isControlFlowStart(s string) bool {
	keywords := []string{"if ", "for ", "while ", "switch ", "catch ", "else ", "with ", "try(", "case "}
	for _, kw := range keywords {
		if strings.HasPrefix(s, kw) {
			return true
		}
	}

	return false
}

// isAssignmentStart reports whether s is a plain assignment, not a function value.
func isAssignmentStart(s string) bool {
	return (strings.Contains(s, " = ") || strings.Contains(s, " := ")) &&
		!strings.Contains(s, " func(") && !strings.Contains(s, " func ") &&
		!strings.Contains(s, "=>")
}

// extractFuncName pulls the function/method name from a signature line.
func extractFuncName(sig string) string {
	sig = strings.TrimSpace(sig)
	sig = stripNamePrefixes(sig)
	sig = stripReceiver(sig)

	// Find the identifier before the first (
	before, _, found := strings.Cut(sig, "(")
	if !found {
		// No parens — take the first word (for "RemoveType struct {" after type stripping)
		parts := strings.Fields(sig)
		if len(parts) > 0 {
			return trimTypeParams(strings.TrimRight(parts[0], " \t\r\n{"))
		}

		return sig
	}

	before = strings.TrimSpace(before)
	if before == "" {
		return "<anonymous>"
	}

	// Handle generics: Name<T>( and Name[T any]( → Name
	before = trimTypeParams(before)

	// Take the last word (for "func (r *T) Name" or "type Name" or "Name<T>")
	parts := strings.Fields(before)
	if len(parts) > 0 {
		// Check if last part looks like an identifier
		last := parts[len(parts)-1]
		// Strip trailing newline/brace artifacts
		last = strings.TrimRight(last, " \t\r\n{")
		if last != "" && last != "=" && last != "=>" {
			return last
		}
	}

	if name := arrowFuncName(sig); name != "" {
		return name
	}

	return "<fn>"
}

// trimTypeParams drops a generic parameter list: Name<T any> → Name, Name[T any] → Name.
func trimTypeParams(name string) string {
	if idx := strings.IndexAny(name, "<["); idx >= 0 {
		return strings.TrimSpace(name[:idx])
	}

	return name
}

// stripNamePrefixes removes language keywords from the start of a signature.
func stripNamePrefixes(sig string) string {
	prefixes := []string{"export default function ", "pub(crate) fn ", "pub(super) fn ",
		"export function ", "pub fn ", "async def ", "function ",
		"protected ", "override ", "abstract ", "private ", "virtual ",
		"static ", "async ", "export ", "const ", "func ", "def ",
		"let ", "var ", "fn ", "type "}
	for _, p := range prefixes {
		sig = strings.TrimPrefix(sig, p)
	}

	return sig
}

// stripReceiver removes a Go receiver like "(r *Type) Name" → "Name".
func stripReceiver(sig string) string {
	if strings.HasPrefix(sig, "(") {
		if _, rest, found := strings.Cut(sig, ")"); found {
			return strings.TrimSpace(rest)
		}
	}

	return sig
}

// arrowFuncName extracts the name from arrow-function assignments like "name = (...) => {".
func arrowFuncName(sig string) string {
	if before, _, found := strings.Cut(sig, "="); found {
		before = strings.TrimSpace(before)

		parts := strings.Fields(before)
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}

	if before, _, found := strings.Cut(sig, ":"); found {
		before = strings.TrimSpace(before)
		if !strings.Contains(before, " ") {
			return before
		}
	}

	return ""
}

// extractBlocksIndent handles Python and other indent-based languages.
func extractBlocksIndent(lines [][]byte, pattern *regexp.Regexp, limit int,
	sigFn func([]byte) bool, needBody bool) ([]FuncMatch, error) {
	var results []FuncMatch

	seen := make(map[int]bool)

	for lineIdx, line := range lines {
		if !sigFn(line) {
			continue
		}

		if seen[lineIdx] {
			continue
		}

		sigIndent := countLeadingSpaces(line)
		fnEnd := findIndentBlockEnd(lines, lineIdx, sigIndent)

		if !blockMatchesPattern(lines, lineIdx, fnEnd, pattern) {
			continue
		}

		seen[lineIdx] = true
		name := extractFuncName(string(lines[lineIdx]))

		body := ""
		if needBody {
			body = joinBlock(lines, lineIdx, fnEnd)
		}

		results = append(results, FuncMatch{
			Line:    lineIdx + 1,
			EndLine: fnEnd + 1,
			Name:    name,
			Lines:   fnEnd - lineIdx + 1,
			Body:    body,
			File:    "",
			Kind:    "",
		})
		if len(results) >= limit {
			break
		}
	}

	return results, nil
}

// findIndentBlockEnd returns the last line index of the indented block starting at start.
func findIndentBlockEnd(lines [][]byte, start, sigIndent int) int {
	fnEnd := start

	for lineIdx := start + 1; lineIdx < len(lines); lineIdx++ {
		s := bytes.TrimRight(lines[lineIdx], " \t\r")
		if len(s) == 0 {
			fnEnd = lineIdx

			continue
		}

		trimmed := bytes.TrimLeft(lines[lineIdx], " \t")
		if len(trimmed) > 0 && trimmed[0] == '#' {
			fnEnd = lineIdx

			continue
		}

		if countLeadingSpaces(lines[lineIdx]) <= sigIndent {
			break
		}

		fnEnd = lineIdx
	}

	return fnEnd
}

// blockMatchesPattern reports whether any line in [start, end] matches pattern.
func blockMatchesPattern(lines [][]byte, start, end int, pattern *regexp.Regexp) bool {
	for j := start; j <= end; j++ {
		if pattern.Match(lines[j]) {
			return true
		}
	}

	return false
}

// joinBlock joins lines [start, end] into a single body string.
func joinBlock(lines [][]byte, start, end int) string {
	var body bytes.Buffer

	for j := start; j <= end; j++ {
		if j > start {
			body.WriteByte('\n')
		}

		body.Write(lines[j])
	}

	return body.String()
}

func countLeadingSpaces(line []byte) int {
	count := 0

	for _, b := range line {
		switch b {
		case ' ':
			count++
		case '\t':
			count += 4
		default:
			return count
		}
	}

	return count
}
