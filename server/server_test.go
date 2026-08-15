package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testProjectRoot = "/tmp/prj"

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

	err := CheckBounds("/home/user/myproject/src/main.go")
	if err != nil {
		t.Errorf("path inside root should be allowed: %v", err)
	}

	err = CheckBounds("/home/user/myproject")
	if err != nil {
		t.Errorf("root itself should be allowed: %v", err)
	}

	for _, path := range []string{
		"/etc/passwd",
		"/home/user/otherproject/main.go",
		"/home/user/myproject/../otherproject/main.go",
	} {
		err := CheckBounds(path)
		if err == nil {
			t.Errorf("path outside root should be denied: %s", path)
		}
	}
}

func TestRootsListRoundTrip(t *testing.T) {
	srv := New("test", "0")

	// Simulate client responding to roots/list with a known root.
	const testRoot = "/tmp/testproject"

	go func() {
		var respCh chan rawResponse

		// Wait for the pending entry to appear, then send the response.
		for {
			srv.pending.Range(func(_, v any) bool {
				if chanVal, ok := v.(chan rawResponse); ok {
					respCh = chanVal
				}

				return false
			})

			// tiny spin — only in test.
			if respCh != nil {
				break
			}
		}

		result, err := json.Marshal(map[string]any{
			"roots": []map[string]string{{"uri": "file://" + testRoot}},
		})
		if err != nil {
			t.Errorf("marshal: %v", err)

			return
		}

		respCh <- rawResponse{Result: result, Err: nil}
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
	ProjectRoot = testProjectRoot
	projectRootLocked = true

	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked; lastDir = origLast }()

	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")

	err := os.MkdirAll(sub, 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(sub, "a.go"), []byte("package x"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(tmp, "root.go"), []byte("package x"), 0600)
	if err != nil {
		t.Fatal(err)
	}

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
	err := CheckBanned("/etc/passwd")
	if err != nil {
		t.Errorf("unexpected ban for passwd: %v", err)
	}

	banErr := CheckBanned("/home/user/.env")
	if banErr == nil {
		t.Fatal("expected error for .env, got nil")
	}

	if !strings.Contains(banErr.Error(), "access denied") {
		t.Errorf("error should say 'access denied': %v", banErr)
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

	err := os.MkdirAll(filepath.Join(markerDir, "src", "pkg"), 0700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(markerDir, "go.mod"), []byte("module t"), 0600)
	if err != nil {
		t.Fatal(err)
	}

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
	filePath := filepath.Join(tmp, "x.go")

	// Unlocked: adopts the file's dir WITHOUT locking.
	ProjectRoot = testProjectRoot
	projectRootLocked = false

	got := ResolvePath(filePath)
	if got != filePath {
		t.Fatalf("ResolvePath = %q, want %q", got, filePath)
	}

	if ProjectRoot != tmp {
		t.Errorf("ProjectRoot = %q, want %q (dir of path, no marker)", ProjectRoot, tmp)
	}

	if projectRootLocked {
		t.Error("projectRootLocked should stay false when no marker found")
	}

	// Locked: no re-orientation at all.
	ProjectRoot = testProjectRoot
	projectRootLocked = true

	got = ResolvePath(filePath)
	if got != filePath {
		t.Fatalf("ResolvePath (locked) = %q, want %q", got, filePath)
	}

	if ProjectRoot != testProjectRoot {
		t.Errorf("ProjectRoot changed while locked: %q", ProjectRoot)
	}
}

func TestResolvePathReorientWithMarker(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked

	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked }()

	proj := t.TempDir()

	// #nosec G304 -- proj is a t.TempDir test fixture
	err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module t"), 0600)
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(proj, "main.go")

	ProjectRoot = testProjectRoot
	projectRootLocked = true

	got := ResolvePath(filePath)
	if got != filePath {
		t.Fatalf("ResolvePath = %q, want %q", got, filePath)
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

	// #nosec G304 -- logPath is a t.TempDir test fixture
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

	// #nosec G304 -- logPath is a t.TempDir test fixture
	data, _ = os.ReadFile(logPath)
	if strings.Contains(string(data), "three") {
		t.Errorf("log written with env unset: %q", data)
	}
}

func TestPendingRootApplied(t *testing.T) {
	origRoot := ProjectRoot
	origLocked := projectRootLocked

	defer func() { ProjectRoot = origRoot; projectRootLocked = origLocked }()

	ProjectRoot = testProjectRoot
	projectRootLocked = false
	newRoot := t.TempDir()
	pendingRoot.Store(newRoot)

	s := New("test", "0")

	resp := s.handle(Request{JSONRPC: jsonrpcVersion, ID: 1, Method: "tools/list", Params: nil})
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

	// Wait for the async persist/autoDiscover goroutines so TempDir cleanup is race-free.
	memFile := filepath.Join(newRoot, ".llm", "memory.json")
	deadline := time.Now().Add(2 * time.Second)

	for {
		_, err := os.Stat(memFile)
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("async persistence did not write %s", memFile)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct {
		in     string
		capLen int
		want   string
	}{
		{"func Foo() {", 0, "func Foo() {"},
		{"func Foo() {\n\tbody\n}", 0, "func Foo() {"},
		{"  func Foo() {\n\tbody\n}", 0, "func Foo() {"},
		{"func Foo() {", 120, "func Foo() {"},
		{"abcdefghijklmnopqrstuvwxyz", 5, "abcde..."},
		{"abcdefghijklmnopqrstuvwxyz", 120, "abcdefghijklmnopqrstuvwxyz"},
	}
	for _, c := range cases {
		if got := FirstLine(c.in, c.capLen); got != c.want {
			t.Errorf("FirstLine(%q, %d) = %q, want %q", c.in, c.capLen, got, c.want)
		}
	}
}

func TestBudgetHint(t *testing.T) {
	plain := BudgetHint(500, "")
	if !strings.Contains(plain, "token_budget=500") || strings.Contains(plain, ". ") {
		t.Errorf("BudgetHint(500, \"\") = %q, want plain notice", plain)
	}

	withTip := BudgetHint(500, "Reduce scope.")
	if !strings.Contains(withTip, "Reduce scope.") {
		t.Errorf("BudgetHint(500, tip) = %q, want tip included", withTip)
	}
}

func TestBudgetResultTruncates(t *testing.T) {
	res := &ToolCallResult{Content: []ToolCallContent{{Type: "text", Text: strings.Repeat("x", 100)}}}
	res = BudgetResult(res, 20)
	got := res.Content[0].Text
	if len(got) > 100 {
		t.Fatalf("BudgetResult did not shorten output")
	}

	if !strings.Contains(got, "Output trimmed to fit token_budget=20") {
		t.Errorf("BudgetResult missing notice, got %q", got)
	}
}
