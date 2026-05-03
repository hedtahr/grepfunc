package server

import (
	"encoding/json"
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
