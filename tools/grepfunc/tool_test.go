package grepfunc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// Shared glob patterns and fixture paths used across tests.
const (
	globGo        = "*.go"
	globAllGo     = "**/*.go"
	globSubGo     = "test/*.go"
	globSubAllGo  = "test/**/*.go"
	globAllTestGo = "**/*_test.go"
	fooGo         = "foo.go"
	testFooGo     = "test/foo.go"
	testSubFooGo  = "test/sub/foo.go"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestBraceDelta(t *testing.T) {
	tests := []struct {
		line          string
		opens, closes int
	}{
		{"func foo() {", 1, 0},
		{"}", 0, 1},
		{`"hello { world"`, 0, 0},        // brace in string
		{"return &Foo{Bar: 1}", 1, 1},    // struct literal: { opens, } closes
		{"// this is a { comment", 0, 0}, // line comment
		{"/* block { */", 0, 0},          // block comment
		{"'{'", 0, 0},                    // single-quoted
		{"`${name}`", 0, 0},              // backtick (template literal)
		{"  { // opening", 1, 0},
		{"a := map[string]int{}", 1, 1}, // both in same line
		{"return nil } else {", 1, 1},   // close then open
	}
	for _, tt := range tests {
		o, c := braceDelta([]byte(tt.line))
		if o != tt.opens || c != tt.closes {
			t.Errorf("braceDelta(%q) = (%d,%d), want (%d,%d)", tt.line, o, c, tt.opens, tt.closes)
		}
	}
}

func TestIsFuncSig(t *testing.T) {
	sigs := []string{
		"func Hello() {",
		"func (s *Server) Handle(ctx context.Context) error {",
		"fn process<T>(data: &[T]) -> Result<T> {",
		"pub fn new() -> Self {",
		"def hello():",
		"async def fetch():",
		"function processItems(items) {",
		"export function init() {",
		"const handle = (req, res) => {",
		"public void handle() {",
		"private async process(data: string): Promise<void> {",
		"protected static create(): Self {",
		"  someMethod(param: Type): ReturnType {",
		"constructor(props: Props) {",
	}
	for _, sig := range sigs {
		if !IsFuncSig([]byte(sig)) {
			t.Errorf("IsFuncSig(%q) = false, want true", sig)
		}
	}

	nonSigs := []string{
		"if (x > 0) {",
		"for (let i = 0; i < n; i++) {",
		"while (true) {",
		"switch (x) {",
		"catch (Exception e) {",
		"with (obj) {",
		"case Value:",
		"return x;",
		"import { foo } from 'bar';",
		"// func hello() {",
		"# comment",
		"",
		"  ",
	}
	for _, sig := range nonSigs {
		if IsFuncSig([]byte(sig)) {
			t.Errorf("IsFuncSig(%q) = true, want false", sig)
		}
	}
}

func TestExtractFuncName(t *testing.T) {
	tests := []struct {
		sig, want string
	}{
		{"func Hello() {", "Hello"},
		{"func (s *Server) Handle(ctx context.Context) error {", "Handle"},
		{"fn process<T>(data: &[T]) -> Result<T> {", "process"},
		{"pub fn new() -> Self {", "new"},
		{"def hello():", "hello"},
		{"async def fetch_data():", "fetch_data"},
		{"function processItems(items) {", "processItems"},
		{"export function init() {", "init"},
		{"const handle = (req, res) => {", "handle"},
		{"public void handle() {", "handle"},
		{"private async process(data: string): Promise<void> {", "process"},
		{"constructor(props: Props) {", "constructor"},
	}
	for _, tt := range tests {
		got := extractFuncName(tt.sig)
		if got != tt.want {
			t.Errorf("extractFuncName(%q) = %q, want %q", tt.sig, got, tt.want)
		}
	}
}

func TestMapFuncBoundaries(t *testing.T) {
	code := `package main

func foo() {
	// do stuff
	x := 1
}

func (s *Server) bar(ctx context.Context, req *Request) (*Response, error) {
	if req.Valid() {
		return s.handle(req)
	}
	return nil
}

func baz() string {
	return "hello"
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	// Lines 0-0: package main → no function
	// Lines 1: blank → no function
	// Lines 2-4: func foo → fnStart=2
	// Lines 5: blank → no function
	// Lines 6-10: func bar → fnStart=6
	// Lines 11: blank → no function
	// Lines 12-14: func baz → fnStart=12

	check := func(lineIdx, expectedFnStart int) {
		got, found := boundaries[lineIdx]
		if expectedFnStart < 0 {
			if found {
				t.Errorf("line %d should have no function, got fnStart=%d", lineIdx, got)
			}
		} else {
			if !found {
				t.Errorf("line %d should map to fnStart=%d, got nothing", lineIdx, expectedFnStart)
			} else if got != expectedFnStart {
				t.Errorf("line %d mapped to fnStart=%d, want %d", lineIdx, got, expectedFnStart)
			}
		}
	}

	check(0, -1)  // package main
	check(1, -1)  // blank
	check(2, 2)   // func foo
	check(3, 2)   // inside foo
	check(4, 2)   // inside foo
	check(5, 2)   // closing brace of foo
	check(6, -1)  // blank
	check(7, 7)   // func bar
	check(8, 7)   // inside bar
	check(9, 7)   // inside bar
	check(10, 7)  // inside bar
	check(11, 7)  // inside bar
	check(12, 7)  // closing brace of bar
	check(13, -1) // blank
	check(14, 14) // func baz
	check(15, 14) // inside baz
	check(16, 14) // closing brace of baz
}

func TestMapFuncBoundariesNested(t *testing.T) {
	code := `package main

func outer() {
	inner := func() {
		nested := func() {
			depth3()
		}
		inner2()
	}
	more()
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	// Line 0: package → none
	// Line 1: blank → none
	// Line 2: func outer → outer (fnStart=2)
	// Line 3: inner := func() → still outer, but inner starts here too (fnStart=3)
	// Line 4: nested := func() → inner (fnStart=4)
	// Line 5: depth3() → nested (fnStart=4)
	// Line 6: } → closes nested, back to inner (fnStart=3)
	// Line 7: inner2() → inner (fnStart=3)
	// Line 8: } → closes inner, back to outer (fnStart=2)
	// Line 9: more() → outer (fnStart=2)
	// Line 10: } → closes outer

	// Inner function takes precedence
	if got := boundaries[5]; got != 4 {
		t.Errorf("deepest nested line (depth3) should map to fnStart=4, got %d", got)
	}

	if got := boundaries[7]; got != 3 {
		t.Errorf("inner2 line should map to fnStart=3, got %d", got)
	}

	if got := boundaries[2]; got != 2 {
		t.Errorf("outer line should map to fnStart=2, got %d", got)
	}

	if got := boundaries[9]; got != 2 {
		t.Errorf("more() line should map to fnStart=2, got %d", got)
	}
}

func TestExtractFuncsIntegration(t *testing.T) {
	// Write a test file
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.go")

	code := `package test

// Greet says hello.
func Greet(name string) string {
	return "Hello, " + name
}

// Add adds two integers.
func Add(a, b int) int {
	if a == 0 {
		return b
	}
	return a + b
}

type Handler struct{}

func (h *Handler) Serve(req *Request) error {
	if req == nil {
		return nil
	}
	return h.process(req)
}
`

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	pat := regexp.MustCompile(`return`)

	funcs, err := extractBlocks(filePath, pat, 10, IsFuncSig)
	if err != nil {
		t.Fatal(err)
	}

	// Should find Greet, Add, and Serve (all have "return")
	if len(funcs) != 3 {
		t.Fatalf("got %d funcs, want 3", len(funcs))
	}

	if funcs[0].Name != "Greet" {
		t.Errorf("funcs[0].Name = %q, want Greet", funcs[0].Name)
	}

	if funcs[1].Name != "Add" {
		t.Errorf("funcs[1].Name = %q, want Add", funcs[1].Name)
	}

	if funcs[2].Name != "Serve" {
		t.Errorf("funcs[2].Name = %q, want Serve", funcs[2].Name)
	}
}

// ==========================================================================
// Edge case / smell tests
// ==========================================================================

func TestOneLiners(t *testing.T) {
	code := `package test

func Foo() int { return 1 }
func Bar(s string) string { return "hello " + s }
func Baz(a, b int) int { if a > b { return a } return b }
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[2]; got != 2 {
		t.Errorf("Foo start: got %d, want 2", got)
	}

	if got := boundaries[3]; got != 3 {
		t.Errorf("Bar start: got %d, want 3", got)
	}

	if got := boundaries[4]; got != 4 {
		t.Errorf("Baz start: got %d, want 4", got)
	}
}

func TestAllmanStyle(t *testing.T) {
	code := `package test

func Foo()
{
	return 1
}

func Bar(s string)
{
	return "hello " + s
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[2]; got != 2 {
		t.Errorf("Foo sig line: got %d, want 2", got)
	}

	if got := boundaries[3]; got != 2 {
		t.Errorf("Foo brace line: got %d, want 2", got)
	}

	if got := boundaries[4]; got != 2 {
		t.Errorf("Foo body line: got %d, want 2", got)
	}

	if got := boundaries[5]; got != 2 {
		t.Errorf("Foo close line: got %d, want 2", got)
	}
}

func TestMultiLineSignature(t *testing.T) {
	code := `package test

func (s *Server) HandleRequest(
	ctx context.Context,
	req *http.Request,
	opts ...Option,
) (*Response, error) {
	return s.process(ctx, req, opts)
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	for i := 2; i <= 8; i++ {
		if got := boundaries[i]; got != 2 {
			t.Errorf("line %d: got %d, want 2", i, got)
		}
	}
}

func TestGoGenerics(t *testing.T) {
	code := `package test

func Process[T any](items []T, fn func(T) T) []T {
	out := make([]T, len(items))
	for i, v := range items {
		out[i] = fn(v)
	}
	return out
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[2]; got != 2 {
		t.Errorf("generic func start: got %d, want 2", got)
	}

	if got := boundaries[7]; got != 2 {
		t.Errorf("generic func close: got %d, want 2", got)
	}
}

func TestRustStyle(t *testing.T) {
	code := `// test.rs

pub fn new() -> Self {
	Self { value: 0 }
}

fn process<T: Debug>(data: &[T]) -> Result<Vec<T>, Error> {
	data.iter().map(|x| Ok(x.clone())).collect()
}

pub(crate) fn internal(value: u64) -> Option<u64> {
	if value > 0 { Some(value) } else { None }
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[2]; got != 2 {
		t.Errorf("pub fn new: got %d, want 2", got)
	}

	if got := boundaries[6]; got != 6 {
		t.Errorf("fn process: got %d, want 6", got)
	}

	if got := boundaries[10]; got != 10 {
		t.Errorf("pub(crate) fn internal: got %d, want 10", got)
	}
}

func TestWeirdSpacing(t *testing.T) {
	code := `package test

func  Foo()  int  {
	return 1
}

  func	Bar(	x	string	)	string	{
	return "x"
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[2]; got != 2 {
		t.Errorf("extra space func: got %d, want 2", got)
	}

	if got := boundaries[6]; got != 6 {
		t.Errorf("tab-indented func: got %d, want 6", got)
	}
}

func TestNestedClosures(t *testing.T) {
	code := `function main() {
	fetch(url).then(function(data) {
		process(data, function(err, result) {
			if (err) { return console.log(err); }
			render(result, function() {
				console.log("done");
			});
		});
	});
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[0]; got != 0 {
		t.Errorf("main: got %d, want 0", got)
	}

	if got := boundaries[1]; got != 1 {
		t.Errorf("function(data): got %d, want 1", got)
	}

	if got := boundaries[2]; got != 2 {
		t.Errorf("function(err,result): got %d, want 2", got)
	}

	if got := boundaries[4]; got != 4 {
		t.Errorf("function(): got %d, want 4", got)
	}
}

func TestUnterminatedEOF(t *testing.T) {
	code := `package test

func Incomplete() {
	if true {
		return 1
	}
// missing closing brace for func
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	for i := 2; i < len(lines); i++ {
		if got := boundaries[i]; got != 2 {
			t.Errorf("line %d: got %d, want 2 (EOF fallback)", i, got)
		}
	}
}

func TestDedupSameFunc(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "dedup.go")
	code := `package test

func Process() error {
	return validate(
		input,
		opts,
	)
	}
	`

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	pat := regexp.MustCompile(`return`)

	funcs, _ := extractBlocks(filePath, pat, 10, IsFuncSig)
	if len(funcs) != 1 {
		t.Fatalf("got %d funcs, want 1 (deduped)", len(funcs))
	}

	if funcs[0].Name != "Process" {
		t.Errorf("Name = %q, want Process", funcs[0].Name)
	}
}

func TestTemplateLiteralBraces(t *testing.T) {
	code := "function greet(name) {\n" +
		"\tconst msg = `Hello ${name}, welcome!`;\n" +
		"\tconst detail = `Your score: ${scores.reduce((a,b) => a+b, 0)}`;\n" +
		"\treturn msg + \" \" + detail;\n}\n"
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[0]; got != 0 {
		t.Errorf("greet start: got %d, want 0", got)
	}

	if got := boundaries[3]; got != 0 {
		t.Errorf("greet close: got %d, want 0", got)
	}

	o, c := braceDelta([]byte("`Hello ${name} welcome`"))
	if o != 0 || c != 0 {
		t.Errorf("template literal braces should be ignored: got (%d,%d)", o, c)
	}
}

func TestRustClosureChains(t *testing.T) {
	code := `fn process() {
	let result = items.iter()
		.map(|x| {
			x * 2
		})
		.filter(|x| {
			*x > 10
		})
		.collect();
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	if got := boundaries[0]; got != 0 {
		t.Errorf("fn process: got %d, want 0", got)
	}

	if got := boundaries[2]; got != 2 {
		t.Errorf("first closure: got %d, want 2", got)
	}

	if got := boundaries[5]; got != 5 {
		t.Errorf("second closure: got %d, want 5", got)
	}
}

func TestPythonStyle(t *testing.T) {
	code := "def hello(name):\n" +
		"    return f\"Hello {name}\"\n\n" +
		"async def fetch(url):\n" +
		"    async with session.get(url) as resp:\n" +
		"        return await resp.json()\n"
	lines := toLines([]byte(code))

	if !IsFuncSig(lines[0]) {
		t.Error("IsFuncSig should detect 'def hello(name):'")
	}

	if !IsFuncSig(lines[3]) {
		t.Error("IsFuncSig should detect 'async def fetch(url):'")
	}
}

func TestCommentBlockInsideFunc(t *testing.T) {
	code := `func process() {
	/*
		multiline comment with { braces }
		that should be { ignored }
	*/
	x := map[string]int{
		"a": 1,
	}
	return x
}
`
	lines := toLines([]byte(code))
	boundaries := mapBlockBoundaries(lines, IsFuncSig)

	for i := range lines {
		if got := boundaries[i]; got != 0 {
			t.Errorf("line %d: got %d, want 0 (block comment with braces)", i, got)
		}
	}
}

func TestSearchIntegration(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.go")

	code := `package test

func Foo() int { return 1 }
func Bar() string { return "bar" }
func FooBar() int { return Foo() + 1 }
`

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	pat := regexp.MustCompile(`return`)

	results, err := Search(dir, globGo, pat, 10, IsFuncSig)
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}

	for _, res := range results {
		if res.Line < 1 {
			t.Errorf("invalid line number for %s", res.Name)
		}

		if res.Body == "" {
			t.Errorf("empty body for %s", res.Name)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		// Simple base-name patterns
		{globGo, fooGo, true},
		{globGo, "foo.rs", false},
		{"foo.*", fooGo, true},
		{"f??.go", fooGo, true},

		// Subdirectory patterns
		{globSubGo, testFooGo, true},
		{globSubGo, testSubFooGo, false},
		{"sub/*.go", testFooGo, false},

		// ** recursive patterns
		{globAllGo, fooGo, true},
		{globAllGo, testFooGo, true},
		{globAllGo, "a/b/c/foo.go", true},
		{globAllGo, "foo.rs", false},
		{globSubAllGo, testFooGo, true},
		{globSubAllGo, testSubFooGo, true},
		{globSubAllGo, "test/sub/deep/foo.go", true},
		{globSubAllGo, "other/foo.go", false},

		// Mixed patterns
		{"**/test/**", testFooGo, true},
		{globAllTestGo, "foo_test.go", true},
		{globAllTestGo, "test/foo_test.go", true},
		{globAllTestGo, "test/sub/foo_test.go", true},
		{globAllTestGo, testSubFooGo, false},
	}

	for _, tt := range tests {
		got := MatchGlob(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestGlobSearchSubdirs(t *testing.T) {
	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(dir, "cmd"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "pkg", "sub", "lib.go"),
		[]byte("package sub\nfunc Lib() int { return 42 }"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "cmd", "main.go"),
		[]byte("package main\nfunc main() { return }"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "root.go"),
		[]byte("package root\nfunc Root() string { return \"root\" }"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	pat := regexp.MustCompile(`return`)

	// **/*.go should find all 3
	results, _ := Search(dir, globAllGo, pat, 10, IsFuncSig)
	if len(results) != 3 {
		t.Fatalf("**/*.go: got %d, want 3", len(results))
	}

	// pkg/**/*.go should find only lib.go
	results, _ = Search(dir, "pkg/**/*.go", pat, 10, IsFuncSig)
	if len(results) != 1 {
		t.Fatalf("pkg/**/*.go: got %d, want 1", len(results))
	}

	if results[0].Name != "Lib" {
		t.Errorf("pkg/**/*.go: Name = %q, want Lib", results[0].Name)
	}

	// Simple *.go should find all .go at any depth (like ripgrep --include)
	results, _ = Search(dir, globGo, pat, 10, IsFuncSig)
	if len(results) != 3 {
		t.Fatalf("*.go: got %d, want 3 (all .go files at any depth)", len(results))
	}
}

func TestBodyFalseSignatureOnly(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.go")
	code := `package test

func Foo() int { return 1 }
func Bar() string { return "bar" }
`

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		schemaPattern: "return",
		schemaPath:    dir,
		schemaInclude: globGo,
		schemaBody:    false,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	if text == "" {
		t.Fatal("empty output")
	}
	// sig-only mode wraps results in a code fence for markdown rendering
	if !strings.Contains(text, "\x60\x60\x60") {
		t.Error("body=false output should contain a code fence wrapper")
	}
	// Must contain function names
	if !strings.Contains(text, "Foo") || !strings.Contains(text, "Bar") {
		t.Error("output should contain function names")
	}
}

func TestNoFalsePositiveExactMatch(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.go")
	// Exact structure matching grepfunc/tool.go's var block
	code := "package test\n" +
		"\n" +
		"import (\n" +
		"\t\"fmt\"\n" +
		")\n" +
		"\n" +
		"var Tool = Config{\n" +
		"\tName:        \"grep_func\",\n" +
		"\tDescription: \"Search for functions/methods matching a pattern and return complete " +
		"function bodies. Handles braces with string awareness.\",\n" +
		"\tInputSchema: InputSchema{\n" +
		"\t\tType: \"object\",\n" +
		"\t\tProperties: map[string]Property{\n" +
		"\t\t\t\"pattern\": {Type: \"string\", Description: \"Regex pattern with func and Handle in it.\"},\n" +
		"\t\t},\n" +
		"\t},\n" +
		"}\n" +
		"\n" +
		"func Handle(req Request) error { return nil }\n"

	err := os.WriteFile(filePath, []byte(code), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		schemaPattern: "func.*Handle",
		schemaPath:    dir,
		schemaInclude: globGo,
		schemaBody:    false,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	// Must NOT match the Description line (which contains "func" and "Handle")
	if strings.Contains(text, "Description") {
		t.Error("FALSE POSITIVE: matched Description line inside var block")
	}
	// Must match the actual Handle function
	if !strings.Contains(text, "func Handle") {
		t.Error("should match func Handle")
	}
}

func TestBodyTrueIncludesBody(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.go")

	err := os.WriteFile(filePath, []byte("package test\nfunc Foo() int { return 1 }"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		schemaPattern: "return",
		schemaPath:    dir,
		schemaInclude: globGo,
		schemaBody:    true,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(raw)
	if err != nil {
		t.Fatal(err)
	}

	text := result.Content[0].Text
	// Must contain code block markers
	if !strings.Contains(text, "\x60\x60\x60") {
		t.Error("body=true output should contain code blocks")
	}
}

func TestRenderPagedBudgetSinglePass(t *testing.T) {
	big := FuncMatch{
		File: "/p/f.go", Line: 1, EndLine: 50, Name: "bigOne", Kind: "func",
		Body: "func bigOne() {\n" + strings.Repeat("\twork()\n", 40) + "}\n",
	}

	// Tiny budget + body=true → names_only direct render, no bodies, hint present.
	out := renderPaged(args{Pattern: "big", Path: "/p", MaxResults: 15, Body: true, TokenBudget: 100}, []FuncMatch{big})
	if strings.Contains(out, "work()") {
		t.Error("bodies must not be rendered when budget is tiny")
	}

	if !strings.Contains(out, "bigOne") {
		t.Error("names_only output must include the symbol name")
	}

	if !strings.Contains(out, "Output trimmed to fit token_budget=100") {
		t.Errorf("missing budget hint in %q", out)
	}

	// No budget → full bodies rendered.
	full := renderPaged(args{Pattern: "big", Path: "/p", MaxResults: 15, Body: true}, []FuncMatch{big})
	if !strings.Contains(full, "work()") {
		t.Error("bodies must render when no budget is set")
	}
}

func TestRenderPagedBudgetSigFallback(t *testing.T) {
	// Body=false: the single-pass guard doesn't apply, so an overflowing full
	// (signature) render must fall back to names_only + truncate.
	var all []FuncMatch
	for i := range 20 {
		all = append(all, FuncMatch{
			File: "/p/f.go", Line: i + 1, EndLine: i + 1, Name: "symN", Kind: "func",
			Body: "func symN() {",
		})
	}

	out := renderPaged(args{Pattern: "symN", Path: "/p", MaxResults: 20, TokenBudget: 200}, all)
	if !strings.Contains(out, "names_only") {
		t.Errorf("fallback hint should suggest names_only, got %q", out)
	}

	if !strings.Contains(out, "Output trimmed to fit token_budget=200") {
		t.Errorf("missing budget hint in %q", out)
	}
}
