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
	dir := gitFixture(t)

	f := filepath.Join(dir, fixtureFile)

	// #nosec G304 -- test temp dir path
	if err := os.WriteFile(f, []byte("v2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Without confirm the call must refuse, show what is at stake, and change nothing.
	res, err := GitHandle(json.RawMessage(`{"mode":"restore","file":"f.txt"}`))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "REFUSED") || !strings.Contains(res.Content[0].Text, "confirm=true") {
		t.Fatalf("expected a refusal pointing at confirm=true, got: %s", res.Content[0].Text)
	}

	if !strings.Contains(res.Content[0].Text, "-v1") || !strings.Contains(res.Content[0].Text, "+v2") {
		t.Fatalf("refusal should show the diff that would be lost, got: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "v2\n" {
		t.Fatalf("refused restore must not touch the file, got %q", data)
	}

	// With confirm=true it proceeds and reports what was discarded.
	res, err = GitHandle(json.RawMessage(`{"mode":"restore","file":"f.txt","confirm":true}`))
	if err != nil {
		t.Fatalf("confirmed restore: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "Restored") || !strings.Contains(res.Content[0].Text, "Discarded") {
		t.Fatalf("confirmed restore output: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, err = os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "v1\n" {
		t.Fatalf("after restore: %q, want %q", data, "v1\n")
	}
}

// A clean file needs no confirmation: there is nothing to discard.
func TestGitRestoreCleanFile(t *testing.T) {
	gitFixture(t)

	res, err := GitHandle(json.RawMessage(`{"mode":"restore","file":"f.txt"}`))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "Nothing to restore") {
		t.Fatalf("expected nothing-to-restore, got: %s", res.Content[0].Text)
	}
}

// An untracked file has no HEAD version: say so instead of letting git fail.
func TestGitRestoreUntrackedFile(t *testing.T) {
	dir := gitFixture(t)
	untracked := filepath.Join(dir, "new.txt")

	// #nosec G304 -- test temp dir path
	if err := os.WriteFile(untracked, []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}

	res, err := GitHandle(json.RawMessage(`{"mode":"restore","file":"new.txt","confirm":true}`))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "untracked") {
		t.Fatalf("expected an untracked explanation, got: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	if _, err := os.ReadFile(untracked); err != nil {
		t.Fatalf("untracked file must survive: %v", err)
	}
}

// fixtureFile is the tracked file every restore fixture starts with.
const fixtureFile = "f.txt"

// gitFixture builds a one-commit repo holding fixtureFile and returns its dir.
func gitFixture(t *testing.T) string {
	t.Helper()

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

	f := filepath.Join(dir, fixtureFile)

	// #nosec G304 -- test temp dir path
	if err := os.WriteFile(f, []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	run("add", fixtureFile)
	run("commit", "-m", "init")

	return dir
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
