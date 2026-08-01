package gitcontext

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestGitRestore(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	t.Cleanup(func() { server.ProjectRoot = orig })

	run := func(args ...string) {
		t.Helper()

		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")

	f := filepath.Join(dir, "f.txt")

	// #nosec G304 -- test temp dir path
	if err := os.WriteFile(f, []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	run("add", "f.txt")
	run("commit", "-m", "init")

	// #nosec G304 -- test temp dir path
	if err := os.WriteFile(f, []byte("v2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := GitHandle(json.RawMessage(`{"mode":"restore","file":"f.txt"}`))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "Restored") {
		t.Fatalf("restore output: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "v1\n" {
		t.Fatalf("after restore: %q, want %q", data, "v1\n")
	}
}

func TestGitRestoreErrors(t *testing.T) {
	dir := t.TempDir()
	orig := server.ProjectRoot

	server.ProjectRoot = dir
	t.Cleanup(func() { server.ProjectRoot = orig })

	// No git repo: restore must fail.
	_, err := GitHandle(json.RawMessage(`{"mode":"restore","file":"x.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "git restore") {
		t.Fatalf("expected git restore error outside repo, got %v", err)
	}

	// Missing file argument.
	_, err = GitHandle(json.RawMessage(`{"mode":"restore"}`))
	if err == nil || !strings.Contains(err.Error(), "file is required") {
		t.Fatalf("expected file-required error, got %v", err)
	}

	// Banned path.
	_, err = GitHandle(json.RawMessage(`{"mode":"restore","file":"` + filepath.Join(dir, ".env") + `"}`))
	if err == nil || !strings.Contains(err.Error(), "check banned") {
		t.Fatalf("expected banned-path error, got %v", err)
	}
}
