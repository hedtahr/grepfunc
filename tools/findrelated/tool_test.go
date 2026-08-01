package findrelated

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

const (
	userName   = "user"
	svcName    = "svc"
	simpleName = "simple"
	fooFile    = "foo.go"
)

func marshalArgs(t *testing.T, args map[string]any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestHandle(t *testing.T) {
	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "user.go"), []byte("package main"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	result, err := Handle(marshalArgs(t, map[string]any{pathKey: filepath.Join(dir, "user.go")}))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Content) == 0 {
		t.Fatal("empty content")
	}
}

func TestHandleBannedPath(t *testing.T) {
	orig := server.ProjectRoot

	server.ProjectRoot = t.TempDir()
	defer func() { server.ProjectRoot = orig }()

	banned := filepath.Join(server.ProjectRoot, ".env")

	err := os.WriteFile(banned, []byte("SECRET=x\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Handle(marshalArgs(t, map[string]any{pathKey: banned}))
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected access-denied error for .env, got %v", err)
	}
}

func TestFindTestFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "user.go", "package main")
	writeFile(t, dir, "user_test.go", "package main_test")
	writeFile(t, dir, "unrelated.go", "package main")

	related := findRelated(filepath.Join(dir, "user.go"))
	foundTest := false
	foundSibling := false

	for _, r := range related {
		switch filepath.Base(r) {
		case "user_test.go":
			foundTest = true
		case "unrelated.go":
			foundSibling = true
		}
	}

	if !foundTest {
		t.Errorf("expected user_test.go in results, got %v", related)
	}

	if !foundSibling {
		t.Errorf("expected unrelated.go (sibling) in results, got %v", related)
	}

	if len(related) != 2 {
		t.Errorf("expected 2 related files, got %d: %v", len(related), related)
	}
}

func TestFindMockFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "service.go", "package main")
	writeFile(t, dir, "service_mock.go", "package main")

	related := findRelated(filepath.Join(dir, "service.go"))
	found := false

	for _, r := range related {
		if filepath.Base(r) == "service_mock.go" {
			found = true

			break
		}
	}

	if !found {
		t.Errorf("expected service_mock.go in results, got %v", related)
	}
}

func TestFindSpecFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "calc.ts", "export function add() {}")
	writeFile(t, dir, "calc.spec.ts", "describe('calc')")

	related := findRelated(filepath.Join(dir, "calc.ts"))
	found := false

	for _, r := range related {
		if filepath.Base(r) == "calc.spec.ts" {
			found = true

			break
		}
	}

	if !found {
		t.Errorf("expected calc.spec.ts in results, got %v", related)
	}
}

func TestCategorize(t *testing.T) {
	tests := []struct {
		related, original, want string
	}{
		{"foo_test.go", fooFile, catTest},
		{"test_foo.go", fooFile, catTest},
		{"foo.spec.ts", "foo.ts", catTest},
		{"foo_mock.go", fooFile, catMock},
		{"mock_foo.go", fooFile, catMock},
		{"/other/foo.go", "/src/foo.go", catOtherDir},
		{"/src/bar.go", "/src/foo.go", "📄 sibling"},
	}
	for _, tt := range tests {
		got := categorize(tt.related, tt.original)
		if got != tt.want {
			t.Errorf("categorize(%q, %q) = %q, want %q", tt.related, tt.original, got, tt.want)
		}
	}
}

func TestIsRelatedName(t *testing.T) {
	tests := []struct {
		name, entry string
		want        bool
	}{
		// Test patterns
		{userName, "user_test.go", true},
		{userName, "user.test.ts", true},
		{userName, "test_user.py", true},
		{userName, "userTest.java", true},
		{userName, "user.spec.ts", true},
		{userName, "user_spec.rb", true},
		// Mock patterns
		{svcName, "svc_mock.go", true},
		{svcName, "mock_svc.go", true},
		{svcName, "svcMock.go", true},
		// Same name
		{"config", "config.go", true},
		// Not related
		{userName, "account.go", false},
		{userName, "user_util.go", false},
		{userName, "user_handler.go", false},
	}
	for _, tt := range tests {
		got := isRelatedName(tt.name, tt.entry)
		if got != tt.want {
			t.Errorf("isRelatedName(%q, %q) = %v, want %v", tt.name, tt.entry, got, tt.want)
		}
	}
}

func TestFindProjectRoot(t *testing.T) {
	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	got := server.FindProjectRoot(filepath.Join(dir, "src", "pkg"))
	if got != dir {
		t.Errorf("FindProjectRoot = %q, want %q", got, dir)
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"HelloWorld", "hello_world"},
		{simpleName, simpleName},
		{"HTMLElement", "h_t_m_l_element"},
	}
	for _, tt := range tests {
		got := toSnake(tt.in)
		if got != tt.want {
			t.Errorf("toSnake(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToCamel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hello_world", "HelloWorld"},
		{simpleName, "Simple"},
		{"xml_parser", "XmlParser"},
	}
	for _, tt := range tests {
		got := toCamel(tt.in)
		if got != tt.want {
			t.Errorf("toCamel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600)
	if err != nil {
		t.Fatal(err)
	}
}
