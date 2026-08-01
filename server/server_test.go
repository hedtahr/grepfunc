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
		{"/home/user/.aws/credentials.json", true},
		{"/home/user/secrets.yml", true},
		{"/home/user/secrets.json", true},
		{"/home/user/.envrc", true},
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

func TestFindProjectRoot(t *testing.T) {
	dir := t.TempDir()

	// No marker anywhere up the tree: must return "", not cwd or "/".
	if got := FindProjectRoot(dir); got != "" {
		t.Errorf("FindProjectRoot(no marker) = %q, want \"\"", got)
	}

	// Marker in parent dir is found by walking up.
	markerDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(markerDir, "src", "pkg"), 0755)
	os.WriteFile(filepath.Join(markerDir, "go.mod"), []byte("module t"), 0644)
	if got := FindProjectRoot(filepath.Join(markerDir, "src", "pkg")); got != markerDir {
		t.Errorf("FindProjectRoot(marker) = %q, want %q", got, markerDir)
	}
	if got := FindProjectRoot(markerDir); got != markerDir {
		t.Errorf("FindProjectRoot(marker dir itself) = %q, want %q", got, markerDir)
	}
}

func TestResolvePathNoMarkerReorient(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked
	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked }()

	tmp := t.TempDir()
	f := filepath.Join(tmp, "x.go")

	// Unlocked: adopts the file's dir WITHOUT locking.
	ProjectRoot = "/tmp/prj"
	projectRootLocked = false
	got := ResolvePath(f)
	if got != f {
		t.Fatalf("ResolvePath = %q, want %q", got, f)
	}
	if ProjectRoot != tmp {
		t.Errorf("ProjectRoot = %q, want %q (dir of path, no marker)", ProjectRoot, tmp)
	}
	if projectRootLocked {
		t.Error("projectRootLocked should stay false when no marker found")
	}

	// Locked: no re-orientation at all.
	ProjectRoot = "/tmp/prj"
	projectRootLocked = true
	got = ResolvePath(f)
	if got != f {
		t.Fatalf("ResolvePath (locked) = %q, want %q", got, f)
	}
	if ProjectRoot != "/tmp/prj" {
		t.Errorf("ProjectRoot changed while locked: %q", ProjectRoot)
	}
}

func TestResolvePathReorientWithMarker(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked
	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked }()

	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module t"), 0644)
	f := filepath.Join(proj, "main.go")

	ProjectRoot = "/tmp/prj"
	projectRootLocked = true
	got := ResolvePath(f)
	if got != f {
		t.Fatalf("ResolvePath = %q, want %q", got, f)
	}
	if ProjectRoot != proj {
		t.Errorf("ProjectRoot = %q, want %q (re-oriented to marker project)", ProjectRoot, proj)
	}
	if !projectRootLocked {
		t.Error("re-orientation to a marker project should lock the root")
	}
}

func TestInitLogGated(t *testing.T) {
	// No env set: nothing written.
	t.Setenv("GREPFUNC_INIT_LOG", "")
	logPath := filepath.Join(t.TempDir(), "init.log")
	t.Setenv("GREPFUNC_INIT_LOG", logPath)

	// First call truncates (fresh session), subsequent calls append.
	logInitf("one %d", 1)
	logInitf("two %d", 2)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log not written when env set: %v", err)
	}
	if !strings.Contains(string(data), "one 1") || !strings.Contains(string(data), "two 2") {
		t.Errorf("unexpected log content: %q", data)
	}

	// Gated off entirely when env is cleared.
	t.Setenv("GREPFUNC_INIT_LOG", "")
	logInitf("three")
	data, _ = os.ReadFile(logPath)
	if strings.Contains(string(data), "three") {
		t.Errorf("log written with env unset: %q", data)
	}
}

func TestPendingRootApplied(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked
	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked }()

	ProjectRoot = "/tmp/prj"
	projectRootLocked = false
	newRoot := t.TempDir()
	pendingRoot.Store(newRoot)

	s := New("test", "0")
	resp := s.handle(Request{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/list failed: %+v", resp)
	}
	if ProjectRoot != newRoot {
		t.Errorf("ProjectRoot = %q, want %q (applied from pendingRoot)", ProjectRoot, newRoot)
	}
	if !projectRootLocked {
		t.Error("applied root should be locked")
	}
	if v, _ := pendingRoot.Load().(string); v != "" {
		t.Errorf("pendingRoot not cleared, got %q", v)
	}
}
