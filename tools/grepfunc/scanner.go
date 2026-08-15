// Package grepfunc scans source trees for function and type blocks.
package grepfunc

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// CompilePattern wraps a user pattern into a regex.
func CompilePattern(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex %q: %w", pattern, err)
	}

	return re, nil
}

// Search walks the directory tree, finds matching files, and extracts blocks.
// sigFn detects whether a line starts a code block (function, struct, class, etc).
// Traversal is parallelized over a small bounded worker pool (ripgrep-style:
// Amdahl's law makes 4-8 workers optimal; more mostly adds contention).
// Results are sorted by (file, line) so output stays deterministic.
func Search(root, glob string, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool) ([]FuncMatch, error) {
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
				funcs, err := searchFile(root, j.path, glob, j.entry, pattern, limit, sigFn)
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
func searchFile(root, path, glob string, entry fs.DirEntry, pattern *regexp.Regexp,
	remaining int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	if !entry.Type().IsRegular() || server.IsBannedPath(path) {
		return nil, nil
	}

	rel, _ := filepath.Rel(root, path)

	if !MatchGlob(glob, rel) {
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
	// When glob is default "*", skip known non-source files
	if glob == "*" && IsNonSourceExt(ext) {
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

	return extractBlocksData(path, data, pattern, remaining, sigFn)
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
func IsBinaryExt(ext string) bool {
	switch ext {
	case ".exe", ".dll", ".so", ".dylib", ".a", ".o", ".obj",
		".class", ".jar", ".war", ".ear",
		".zip", ".tar", ".gz", ".bz2", ".7z", ".rar",
		".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".svg",
		".mp3", ".mp4", ".wav", ".avi", ".mov", ".mkv",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".ttf", ".otf", ".woff", ".woff2",
		".bin", ".dat", ".db", ".sqlite", ".sqlite3",
		".lock", ".sum":
		return true
	}

	return false
}

// IsNonSourceExt skips known non-code file types when no include glob given.
// Files with no extension (e.g. Makefile, Dockerfile) are kept.
func IsNonSourceExt(ext string) bool {
	if ext == "" {
		return false
	}

	switch ext {
	case ".go", ".rs", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".py", ".pyi", ".pyx",
		".java", ".kt", ".kts", ".scala", ".groovy",
		".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".hh",
		".cs", ".fs", ".vb",
		".rb", ".php", ".pl", ".pm",
		".swift", ".m", ".mm",
		".lua", ".zig", ".nim", ".odin",
		".sh", ".bash", ".zsh", ".fish",
		".sql", ".graphql", ".proto",
		".hs", ".ml", ".clj", ".cljs", ".edn", ".ex", ".exs",
		".erl", ".hrl",
		".vue", ".svelte",
		".tf", ".hcl",
		".r", ".R",
		".dart":
		return false
	}

	return true
}

// MatchGlob matches a file path against a glob pattern with ** support.
// ** matches zero or more directory components.
func MatchGlob(pattern, path string) bool {
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

	if part == "**" {
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

	return extractBlocksData(filePath, data, pattern, limit, sigFn)
}

// extractBlocksData scans an already-read file's contents for blocks matching
// pattern, avoiding a second read when the caller sniffed the file first.
func extractBlocksData(filePath string, data []byte, pattern *regexp.Regexp, limit int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	lines := toLines(data)
	if len(lines) == 0 {
		return nil, nil
	}

	if isPythonExt(strings.ToLower(filepath.Ext(filePath))) {
		return extractBlocksIndent(lines, pattern, limit, sigFn)
	}

	boundaries := mapBlockBoundaries(lines, sigFn)

	// Find lines matching the pattern
	var results []FuncMatch

	seen := make(map[int]bool) // dedup by func start line

	for lineIdx, line := range lines {
		if !pattern.Match(line) {
			continue
		}

		fnStart, ok := boundaries[lineIdx]
		if !ok || seen[fnStart] {
			continue
		}
		// Belt-and-suspenders: fnStart must actually be a valid signature
		if !sigFn(lines[fnStart]) {
			continue
		}

		seen[fnStart] = true

		results = append(results, *buildFuncMatch(lines, fnStart))
		if len(results) >= limit {
			break
		}
	}

	return results, nil
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
	startLine int // signature line index
	bodyDepth int // brace depth just before opening brace (-1 if not yet found)
}

// MapBlockBoundaries maps every line index in a file to the start line of the innermost
// function/type/struct/class block that contains it, using sigFn to detect block signatures.
func MapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) map[int]int {
	return mapBlockBoundaries(lines, sigFn)
}

// line index → inner-most block start line index.
func mapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) map[int]int {
	boundaries := make(map[int]int)
	stack := make([]fnEntry, 0, initialStackCap)
	depth := 0

	for lineIdx, line := range lines {
		depthBefore := depth
		opens, closes := braceDelta(line)
		depth = depthBefore + opens - closes

		// Detect new block signature (function, struct, class, etc)
		if sigFn(line) {
			if opens > 0 {
				// Signature + opening brace on same line: bodyDepth = depth before signature
				stack = append(stack, fnEntry{startLine: lineIdx, bodyDepth: depthBefore})
			} else {
				// Signature without brace; bodyDepth set when brace found
				stack = append(stack, fnEntry{startLine: lineIdx, bodyDepth: -1})
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

// closeEndedBlocks pops functions whose body closed at line i, mapping their lines.
func closeEndedBlocks(stack []fnEntry, i, depth int, boundaries map[int]int) []fnEntry {
	for len(stack) > 0 && stack[len(stack)-1].bodyDepth >= 0 && depth == stack[len(stack)-1].bodyDepth {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		// Map only unclaimed lines (inner functions keep their mapping)
		for j := top.startLine; j <= i; j++ {
			if _, exists := boundaries[j]; !exists {
				boundaries[j] = top.startLine
			}
		}
	}

	return stack
}

// closeUnterminated maps functions left open at EOF to the last line.
func closeUnterminated(stack []fnEntry, lineCount int, boundaries map[int]int) {
	for _, v := range slices.Backward(stack) {
		if v.bodyDepth >= 0 {
			for j := v.startLine; j < lineCount; j++ {
				if _, exists := boundaries[j]; !exists {
					boundaries[j] = v.startLine
				}
			}
		}
	}
}

// EnclosingSymbol returns the name of the function/type that encloses lineIdx (0-based).
func EnclosingSymbol(lines [][]byte, lineIdx int, boundaries map[int]int) string {
	start, ok := boundaries[lineIdx]
	if !ok {
		return ""
	}

	return extractFuncName(string(lines[start]))
}

func buildFuncMatch(lines [][]byte, fnStart int) *FuncMatch {
	depth := 0
	started := false

	fnEnd := len(lines) - 1

	for lineIdx := fnStart; lineIdx < len(lines); lineIdx++ {
		opens, closes := braceDelta(lines[lineIdx])
		if opens > 0 {
			started = true
		}

		depth += opens - closes
		if started && depth == 0 {
			fnEnd = lineIdx

			break
		}
	}

	name := extractFuncName(string(lines[fnStart]))

	var body bytes.Buffer

	for lineIdx := fnStart; lineIdx <= fnEnd; lineIdx++ {
		if lineIdx > fnStart {
			body.WriteByte('\n')
		}

		body.Write(lines[lineIdx])
	}

	return &FuncMatch{
		Line:    fnStart + 1,
		EndLine: fnEnd + 1,
		Name:    name,
		Lines:   fnEnd - fnStart + 1,
		Body:    body.String(),
		File:    "",
		Kind:    "",
	}
}

// braceDelta counts { and } in a line, skipping strings and comments.
// Uses a simple state machine DFA.
func braceDelta(line []byte) (int, int) {
	opens, closes := 0, 0
	state := byte(stateCode)

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
			return opens, closes
		}
	}

	return opens, closes
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
	if !strings.Contains(sig, "(") {
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
			return strings.TrimRight(parts[0], " \t\r\n{")
		}

		return sig
	}

	before = strings.TrimSpace(before)
	if before == "" {
		return "<anonymous>"
	}

	// Handle generics: Name<T>( → Name
	if gtIdx := strings.Index(before, "<"); gtIdx >= 0 {
		before = strings.TrimSpace(before[:gtIdx])
	}

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
	sigFn func([]byte) bool) ([]FuncMatch, error) {
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
		body := joinBlock(lines, lineIdx, fnEnd)

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
