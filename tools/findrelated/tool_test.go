package findrelated

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestHandle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "user.go"), []byte("package main"), 0644)

	raw, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "user.go")})
	result, err := Handle(raw)
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
	os.WriteFile(banned, []byte("SECRET=x\n"), 0600)

	raw, _ := json.Marshal(map[string]any{"path": banned})
	_, err := Handle(raw)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected access-denied error for .env, got %v", err)
	}
}

func TestFindTestFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "user.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "user_test.go"), []byte("package main_test"), 0644)
	os.WriteFile(filepath.Join(dir, "unrelated.go"), []byte("package main"), 0644)

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
	os.WriteFile(filepath.Join(dir, "service.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "service_mock.go"), []byte("package main"), 0644)

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
	os.WriteFile(filepath.Join(dir, "calc.ts"), []byte("export function add() {}"), 0644)
	os.WriteFile(filepath.Join(dir, "calc.spec.ts"), []byte("describe('calc')"), 0644)

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
		{"foo_test.go", "foo.go", "🧪 test"},
		{"test_foo.go", "foo.go", "🧪 test"},
		{"foo.spec.ts", "foo.ts", "🧪 test"},
		{"foo_mock.go", "foo.go", "🎭 mock/stub"},
		{"mock_foo.go", "foo.go", "🎭 mock/stub"},
		{"/other/foo.go", "/src/foo.go", "📁 other dir"},
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
		{"user", "user_test.go", true},
		{"user", "user.test.ts", true},
		{"user", "test_user.py", true},
		{"user", "userTest.java", true},
		{"user", "user.spec.ts", true},
		{"user", "user_spec.rb", true},
		// Mock patterns
		{"svc", "svc_mock.go", true},
		{"svc", "mock_svc.go", true},
		{"svc", "svcMock.go", true},
		// Same name
		{"config", "config.go", true},
		// Not related
		{"user", "account.go", false},
		{"user", "user_util.go", false},
		{"user", "user_handler.go", false},
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
	os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0644)

	got := server.FindProjectRoot(filepath.Join(dir, "src", "pkg"))
	if got != dir {
		t.Errorf("FindProjectRoot = %q, want %q", got, dir)
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct{ in, want string }{
		{"HelloWorld", "hello_world"},
		{"simple", "simple"},
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
		{"simple", "Simple"},
		{"xml_parser", "XmlParser"},
	}
	for _, tt := range tests {
		got := toCamel(tt.in)
		if got != tt.want {
			t.Errorf("toCamel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
