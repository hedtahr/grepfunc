package grepfunc

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

// CompilePattern wraps a user pattern into a regex.
func CompilePattern(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

// Search walks the directory tree, finds matching files, and extracts blocks.
// sigFn detects whether a line starts a code block (function, struct, class, etc).
func Search(root, glob string, pattern *regexp.Regexp, max int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	var results []FuncMatch
	var walkErr error

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			walkErr = err
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" || base == ".idea" || base == "__pycache__" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() || server.IsBannedPath(path) {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		matched := MatchGlob(glob, rel)
		if !matched {
			return nil
		}

		// Skip binary/large files
		info, err := d.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if IsBinaryExt(ext) {
			return nil
		}
		// When glob is default "*", skip known non-source files
		if glob == "*" && IsNonSourceExt(ext) {
			return nil
		}

		remaining := max - len(results)
		if remaining <= 0 {
			return filepath.SkipAll
		}

		funcs, err := extractBlocks(path, pattern, remaining, sigFn)
		if err != nil {
			return nil // skip files that can't be read
		}
		for i := range funcs {
			funcs[i].File = path
		}
		results = append(results, funcs...)
		return nil
	})

	if walkErr != nil && len(results) == 0 {
		return nil, walkErr
	}
	return results, err
}

// SearchBoth finds funcs and types in a single walk.
func SearchBoth(root, glob string, pattern *regexp.Regexp, max int) ([]FuncMatch, []FuncMatch, error) {
	combined := func(line []byte) bool {
		return IsFuncSig(line) || IsStructSig(line)
	}
	all, err := Search(root, glob, pattern, max*2, combined)
	if err != nil {
		return nil, nil, err
	}
	var funcs, types []FuncMatch
	for _, m := range all {
		firstLine := []byte(m.Body)
		if nl := bytes.IndexByte(firstLine, '\n'); nl >= 0 {
			firstLine = firstLine[:nl]
		}
		if IsFuncSig(firstLine) {
			funcs = append(funcs, m)
		} else {
			types = append(types, m)
		}
	}
	return funcs, types, nil
}

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

func splitPath(p string) []string {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

func matchParts(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}

	p := pat[0]

	if p == "**" {
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

	matched, _ := filepath.Match(p, path[0])
	if !matched {
		return false
	}
	return matchParts(pat[1:], path[1:])
}

// extractBlocks returns all blocks (functions, structs, etc) in a file matching pattern.
func extractBlocks(filePath string, pattern *regexp.Regexp, max int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	lines := toLines(data)
	if len(lines) == 0 {
		return nil, nil
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".py" || ext == ".pyi" || ext == ".pyx" {
		return extractBlocksIndent(lines, pattern, max, sigFn)
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

		fm, err := buildFuncMatch(lines, fnStart)
		if err != nil {
			continue
		}
		results = append(results, *fm)
		if len(results) >= max {
			break
		}
	}
	return results, nil
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

// mapBlockBoundaries identifies all block definitions and returns a map from
// MapBlockBoundaries maps every line index in a file to the start line of the innermost
// function/type/struct/class block that contains it, using sigFn to detect block signatures.
func MapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) map[int]int {
	return mapBlockBoundaries(lines, sigFn)
}

// line index → inner-most block start line index.
func mapBlockBoundaries(lines [][]byte, sigFn func([]byte) bool) map[int]int {
	boundaries := make(map[int]int)
	stack := make([]fnEntry, 0, 16)
	depth := 0

	for i, line := range lines {
		depthBefore := depth
		opens, closes := braceDelta(line)
		depth = depthBefore + opens - closes

		// Detect new block signature (function, struct, class, etc)
		if sigFn(line) {
			if opens > 0 {
				// Signature + opening brace on same line: bodyDepth = depth before signature
				stack = append(stack, fnEntry{startLine: i, bodyDepth: depthBefore})
			} else {
				// Signature without brace; bodyDepth set when brace found
				stack = append(stack, fnEntry{startLine: i, bodyDepth: -1})
			}
		}

		// If top of stack is waiting for its opening brace
		if len(stack) > 0 && stack[len(stack)-1].bodyDepth < 0 && opens > 0 {
			stack[len(stack)-1].bodyDepth = depthBefore
		}

		// Check if any functions ended (depth returned to bodyDepth)
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
	}

	// Unterminated functions at EOF: close them at last line
	for k := len(stack) - 1; k >= 0; k-- {
		if stack[k].bodyDepth >= 0 {
			for j := stack[k].startLine; j < len(lines); j++ {
				if _, exists := boundaries[j]; !exists {
					boundaries[j] = stack[k].startLine
				}
			}
		}
	}

	return boundaries
}

// EnclosingSymbol returns the name of the function/type that encloses lineIdx (0-based).
func EnclosingSymbol(lines [][]byte, lineIdx int, boundaries map[int]int) string {
	start, ok := boundaries[lineIdx]
	if !ok {
		return ""
	}
	return extractFuncName(string(lines[start]))
}

func buildFuncMatch(lines [][]byte, fnStart int) (*FuncMatch, error) {
	depth := 0
	started := false
	fnEnd := len(lines) - 1
	for j := fnStart; j < len(lines); j++ {
		opens, closes := braceDelta(lines[j])
		if opens > 0 {
			started = true
		}
		depth += opens - closes
		if started && depth == 0 {
			fnEnd = j
			break
		}
	}
	name := extractFuncName(string(lines[fnStart]))
	var body bytes.Buffer
	for j := fnStart; j <= fnEnd; j++ {
		if j > fnStart {
			body.WriteByte('\n')
		}
		body.Write(lines[j])
	}
	return &FuncMatch{
		Line:    fnStart + 1,
		EndLine: fnEnd + 1,
		Name:    name,
		Lines:   fnEnd - fnStart + 1,
		Body:    body.String(),
	}, nil
}

// braceDelta counts { and } in a line, skipping strings and comments.
// Uses a simple state machine DFA.
func braceDelta(line []byte) (opens, closes int) {
	state := byte(0) // 0=code, 1=double-str, 2=single-str, 3=backtick-str, 4=line-comment, 5=block-comment
	// Note: line-comment can only start within code context

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch state {
		case 0: // code
			switch c {
			case '{':
				opens++
			case '}':
				closes++
			case '"':
				state = 1
			case '\'':
				state = 2
			case '`':
				state = 3
			case '/':
				if i+1 < len(line) {
					next := line[i+1]
					if next == '/' {
						return // rest of line is comment
					}
					if next == '*' {
						state = 5
						i++ // skip *
					}
				}
			}
		case 1: // double-quoted string
			switch c {
			case '\\':
				i++ // skip next char
			case '"':
				state = 0
			}
		case 2: // single-quoted string
			switch c {
			case '\\':
				i++
			case '\'':
				state = 0
			}
		case 3: // backtick string
			if c == '`' {
				state = 0
			}
		case 5: // block comment
			if c == '*' && i+1 < len(line) && line[i+1] == '/' {
				state = 0
				i++ // skip /
			}
		}
	}
	return
}

func insideQuote(s string, idx int) bool {
	dq := 0
	for i := 0; i < idx; i++ {
		if s[i] == '\\' && i+1 < idx {
			i++
			continue
		}
		if s[i] == '"' {
			dq++
		}
	}
	return dq%2 == 1
}

// IsFuncSig checks if a line looks like a function/method definition.
// IsStructSig detects struct/class/interface/enum/type definition signatures.
// Matches: Go, Rust, TS/JS, Java, C/C++, C#, Python, Kotlin, Swift, etc.
func IsStructSig(line []byte) bool {
	s := strings.TrimSpace(string(line))
	if s == "" || s[0] == '#' || s[0] == '/' || s[0] == '*' {
		return false
	}
	if !strings.ContainsAny(s, "{:") {
		return false
	}

	// Go: type Name struct {, type Name interface {
	if strings.HasPrefix(s, "type ") && (strings.Contains(s, " struct ") || strings.Contains(s, " struct{") ||
		strings.Contains(s, " interface ") || strings.Contains(s, " interface{")) {
		return true
	}

	// Rust: struct Name {, enum Name {, trait Name {, impl Name {, impl Trait for Name {
	if strings.HasPrefix(s, "struct ") || strings.HasPrefix(s, "enum ") ||
		strings.HasPrefix(s, "trait ") || strings.HasPrefix(s, "impl ") ||
		strings.HasPrefix(s, "pub struct ") || strings.HasPrefix(s, "pub enum ") ||
		strings.HasPrefix(s, "pub trait ") || strings.HasPrefix(s, "pub impl ") {
		return true
	}

	// Python: class Name: or class Name(Base):
	if strings.HasPrefix(s, "class ") && strings.HasSuffix(s, ":") {
		return true
	}

	// JS/TS: class Name {, interface Name {, type Name = {
	if strings.HasPrefix(s, "class ") || strings.HasPrefix(s, "interface ") ||
		(strings.HasPrefix(s, "type ") && strings.Contains(s, "=")) ||
		strings.HasPrefix(s, "export class ") || strings.HasPrefix(s, "export interface ") ||
		strings.HasPrefix(s, "export type ") || strings.HasPrefix(s, "export default class ") ||
		strings.HasPrefix(s, "abstract class ") {
		return true
	}

	// Java/C#/Kotlin/Swift: class/interface/enum/record/data class
	modifiers := []string{"public ", "private ", "protected ", "internal ", "static ", "abstract ",
		"sealed ", "final ", "open ", "data ", "override ", "export "}
	for _, mod := range modifiers {
		s = strings.TrimPrefix(s, mod)
	}

	if strings.HasPrefix(s, "class ") || strings.HasPrefix(s, "interface ") ||
		strings.HasPrefix(s, "enum ") || strings.HasPrefix(s, "enum class ") ||
		strings.HasPrefix(s, "record ") || strings.HasPrefix(s, "object ") ||
		strings.HasPrefix(s, "struct ") || strings.HasPrefix(s, "protocol ") ||
		strings.HasPrefix(s, "extension ") || strings.HasPrefix(s, "actor ") {
		return true
	}

	return false
}
func IsFuncSig(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	// Skip obvious non-signatures
	if trimmed[0] == '#' || trimmed[0] == '/' || trimmed[0] == '*' {
		return false
	}

	s := string(trimmed)

	// Reject multi-line signature continuations and body lines
	if s[0] == ')' || s[0] == ',' || s[0] == ']' || s[0] == '{' || s[0] == '}' {
		return false
	}

	// Must contain opening paren (function parameter list)
	if !strings.Contains(s, "(") {
		return false
	}

	// Go: func (optional receiver) Name(
	if strings.HasPrefix(s, "func ") || strings.HasPrefix(s, "func(") {
		return true
	}

	// Rust: fn name<...>( or pub fn name(
	if strings.HasPrefix(s, "fn ") || strings.HasPrefix(s, "pub fn ") ||
		strings.HasPrefix(s, "pub(crate) fn ") || strings.HasPrefix(s, "pub(super) fn ") {
		return true
	}

	// Python: def name( or async def name(
	if strings.HasPrefix(s, "def ") || strings.HasPrefix(s, "async def ") {
		return true
	}

	// JavaScript/TypeScript: function keyword followed by optional space then (
	// Avoid matching "function(" inside string literals (e.g. format strings)
	if strings.HasPrefix(s, "function ") || strings.HasPrefix(s, "function(") {
		return true
	}
	for _, pat := range []string{" function(", " function ("} {
		idx := strings.Index(s, pat)
		if idx >= 0 && !insideQuote(s, idx) {
			return true
		}
	}

	// JS/TS arrow functions assigned to name: const/let/var name = (...) => {
	if (strings.HasPrefix(s, "const ") || strings.HasPrefix(s, "let ") || strings.HasPrefix(s, "var ")) &&
		(strings.Contains(s, "=>") || (strings.Contains(s, "= (") && strings.Contains(s, "{"))) {
		return true
	}

	// Strip visibility/lifetime modifiers unconditionally
	s = strings.TrimPrefix(s, "public ")
	s = strings.TrimPrefix(s, "private ")
	s = strings.TrimPrefix(s, "protected ")
	s = strings.TrimPrefix(s, "static ")
	s = strings.TrimPrefix(s, "async ")
	s = strings.TrimPrefix(s, "virtual ")
	s = strings.TrimPrefix(s, "override ")
	s = strings.TrimPrefix(s, "export ")
	s = strings.TrimPrefix(s, "abstract ")

	return looksLikeFuncStart(s)
}

// looksLikeFuncStart checks if remaining string after modifiers looks like a function.
func looksLikeFuncStart(s string) bool {
	// Constructor pattern: constructor( or className(
	if strings.HasPrefix(s, "constructor(") || strings.HasPrefix(s, "constructor ") {
		return true
	}
	// Pattern: identifier (possibly with generics) followed by (
	// Must NOT be control flow keywords
	keywords := []string{"if ", "for ", "while ", "switch ", "catch ", "else ", "with ", "try(", "case "}
	for _, kw := range keywords {
		if strings.HasPrefix(s, kw) {
			return false
		}
	}
	// Reject assignment statements that don't assign a function value.
	// e.g. reject: s.Entries = append(...) but keep: inner := func() {
	if (strings.Contains(s, " = ") || strings.Contains(s, " := ")) &&
		!strings.Contains(s, " func(") && !strings.Contains(s, " func ") &&
		!strings.Contains(s, "=>") {
		return false
	}
	// Has a word followed by ( not preceded by these keywords
	// Quick check: ends with { or has ( followed eventually by ) then { or :
	return (strings.HasSuffix(strings.TrimSpace(s), "{") ||
		strings.HasSuffix(strings.TrimSpace(s), ":")) &&
		strings.Contains(s, "(") &&
		!strings.HasPrefix(s, "if ") &&
		!strings.HasPrefix(s, "for ") &&
		!strings.HasPrefix(s, "while ")
}

// extractFuncName pulls the function/method name from a signature line.
func extractFuncName(sig string) string {
	sig = strings.TrimSpace(sig)

	// Strip known prefixes unconditionally
	prefixes := []string{"export default function ", "pub(crate) fn ", "pub(super) fn ",
		"export function ", "pub fn ", "async def ", "function ",
		"protected ", "override ", "abstract ", "private ", "virtual ",
		"static ", "async ", "export ", "const ", "func ", "def ",
		"let ", "var ", "fn ", "type "}
	for _, p := range prefixes {
		sig = strings.TrimPrefix(sig, p)
	}

	// Go receiver: (r *Type) Name → strip receiver
	if strings.HasPrefix(sig, "(") {
		idx := strings.Index(sig, ")")
		if idx >= 0 {
			sig = strings.TrimSpace(sig[idx+1:])
		}
	}

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

	// Arrow function: name = (...) => { or name: (...) => {
	if before, _, found := strings.Cut(sig, "="); found {
		before = strings.TrimSpace(before)
		parts = strings.Fields(before)
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

	return "<fn>"
}

// extractBlocksIndent handles Python and other indent-based languages.
func extractBlocksIndent(lines [][]byte, pattern *regexp.Regexp, max int, sigFn func([]byte) bool) ([]FuncMatch, error) {
	var results []FuncMatch
	seen := make(map[int]bool)

	for i, line := range lines {
		if !sigFn(line) {
			continue
		}
		if seen[i] {
			continue
		}
		sigIndent := countLeadingSpaces(line)
		fnEnd := i
		for j := i + 1; j < len(lines); j++ {
			s := bytes.TrimRight(lines[j], " \t\r")
			if len(s) == 0 {
				fnEnd = j
				continue
			}
			trimmed := bytes.TrimLeft(lines[j], " \t")
			if len(trimmed) > 0 && trimmed[0] == '#' {
				fnEnd = j
				continue
			}
			if countLeadingSpaces(lines[j]) <= sigIndent {
				break
			}
			fnEnd = j
		}
		matched := false
		for j := i; j <= fnEnd; j++ {
			if pattern.Match(lines[j]) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		seen[i] = true
		name := extractFuncName(string(lines[i]))
		var body bytes.Buffer
		for j := i; j <= fnEnd; j++ {
			if j > i {
				body.WriteByte('\n')
			}
			body.Write(lines[j])
		}
		results = append(results, FuncMatch{
			Line:    i + 1,
			EndLine: fnEnd + 1,
			Name:    name,
			Lines:   fnEnd - i + 1,
			Body:    body.String(),
		})
		if len(results) >= max {
			break
		}
	}
	return results, nil
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
