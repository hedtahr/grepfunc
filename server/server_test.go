package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsBannedPath(t *testing.T) {
	cases := []struct {
		path   string
		banned bool
	}{
		{"/home/user/.env", true},
		{"/home/user/.env.local", true},
		{"/home/user/.env_prod", true},
		{"/home/user/.netrc", true},
		{"/home/user/.ssh/id_rsa", true},
		{"/home/user/.ssh/id_ed25519", true},
		{"/home/user/.ssh/id_ecdsa", true},
		{"/home/user/.ssh/id_dsa", true},
		{"/home/user/.aws/credentials", true},
		{"/certs/server.pem", true},
		{"/certs/server.key", true},
		{"/certs/client.p12", true},
		{"/certs/client.pfx", true},
		{"/src/main.go", false},
		{"/config/settings.json", false},
		{"/home/user/.gitconfig", false},
		{"/home/user/envelope.go", false}, // "envelope" contains "env" but is not .env
	}
	for _, c := range cases {
		got := IsBannedPath(c.path)
		if got != c.banned {
			t.Errorf("IsBannedPath(%q) = %v, want %v", c.path, got, c.banned)
		}
	}
}

func TestCheckBounds(t *testing.T) {
	orig := ProjectRoot
	origLocked := projectRootLocked
	ProjectRoot = "/home/user/myproject"
	projectRootLocked = true
	defer func() { ProjectRoot = orig; projectRootLocked = origLocked }()

	if err := CheckBounds("/home/user/myproject/src/main.go"); err != nil {
		t.Errorf("path inside root should be allowed: %v", err)
	}
	if err := CheckBounds("/home/user/myproject"); err != nil {
		t.Errorf("root itself should be allowed: %v", err)
	}
	for _, p := range []string{
		"/etc/passwd",
		"/home/user/otherproject/main.go",
		"/home/user/myproject/../otherproject/main.go",
	} {
		if err := CheckBounds(p); err == nil {
			t.Errorf("path outside root should be denied: %s", p)
		}
	}
}

func TestRootsListRoundTrip(t *testing.T) {
	srv := New("test", "0")

	// Simulate client responding to roots/list with a known root.
	const testRoot = "/tmp/testproject"
	go func() {
		// Wait for the pending entry to appear, then send the response.
		var ch chan rawResponse
		for {
			srv.pending.Range(func(k, v any) bool {
				ch = v.(chan rawResponse)
				return false
			})
			if ch != nil {
				break
			}
			// tiny spin — only in test
		}
		result, _ := json.Marshal(map[string]any{
			"roots": []map[string]string{{"uri": "file://" + testRoot}},
		})
		ch <- rawResponse{Result: result}
	}()

	result, err := srv.sendToClient("roots/list", map[string]any{})
	if err != nil {
		t.Fatalf("sendToClient: %v", err)
	}
	if !strings.Contains(string(result), testRoot) {
		t.Errorf("expected %q in result, got %s", testRoot, result)
	}
}

func TestResolvePathLastDir(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked
	origLast := lastDir
	ProjectRoot = "/tmp/prj"
	projectRootLocked = true
	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked; lastDir = origLast }()

	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "a.go"), []byte("package x"), 0644)
	os.WriteFile(filepath.Join(tmp, "root.go"), []byte("package x"), 0644)

	// 1. Absolute path: saves lastDir, returns as-is
	got := ResolvePath(filepath.Join(sub, "a.go"))
	if got != filepath.Join(sub, "a.go") {
		t.Fatalf("abs: got %q", got)
	}
	if lastDir != sub {
		t.Fatalf("lastDir after abs: got %q, want %q", lastDir, sub)
	}

	// 2. Relative path that exists inside lastDir → concat
	got = ResolvePath("a.go")
	if got != filepath.Join(sub, "a.go") {
		t.Fatalf("relative hit: got %q", got)
	}

	// 3. Relative path NOT in lastDir → falls through to ProjectRoot
	got = ResolvePath("root.go")
	if got != filepath.Join(ProjectRoot, "root.go") {
		t.Fatalf("relative miss: got %q, want %q", got, filepath.Join(ProjectRoot, "root.go"))
	}

	// 4. Subfolder relative path not in lastDir → falls through
	got = ResolvePath("src/nope.go")
	if got != filepath.Join(ProjectRoot, "src/nope.go") {
		t.Fatalf("subfolder miss: got %q", got)
	}

	// 5. No lastDir → ProjectRoot
	lastDir = ""
	got = ResolvePath("any.go")
	if got != filepath.Join(ProjectRoot, "any.go") {
		t.Fatalf("no lastDir: got %q", got)
	}
}

func TestCheckBanned(t *testing.T) {
	if err := CheckBanned("/etc/passwd"); err != nil {
		t.Errorf("unexpected ban for passwd: %v", err)
	}
	err := CheckBanned("/home/user/.env")
	if err == nil {
		t.Fatal("expected error for .env, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should say 'access denied': %v", err)
	}
}
