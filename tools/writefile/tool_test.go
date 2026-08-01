package writefile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hedtahr/grepfunc/server"
)

func TestHandle(t *testing.T) {
	tmp := t.TempDir()
	origRoot := server.ProjectRoot

	server.ProjectRoot = tmp
	t.Cleanup(func() { server.ProjectRoot = origRoot })

	newPath := filepath.Join(tmp, "new.txt")
	existingPath := filepath.Join(tmp, "existing.txt")
	nestedPath := filepath.Join(tmp, "sub", "nested.txt")

	// #nosec G304 -- test temp dir path
	_ = os.WriteFile(existingPath, []byte("old"), 0600)

	raw := json.RawMessage(`{"path":"` + newPath + `","content":"hello world"}`)

	res, err := Handle(raw)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !strings.Contains(res.Content[0].Text, "11 bytes") {
		t.Fatalf("create output: %s", res.Content[0].Text)
	}

	// #nosec G304 -- test temp dir path
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read created: %v", err)
	}

	if string(data) != "hello world" {
		t.Fatalf("created content: %q", data)
	}

	// Overwrite path
	raw = json.RawMessage(`{"path":"` + existingPath + `","content":"new"}`)

	_, err = Handle(raw)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	// #nosec G304 -- test temp dir path
	data, err = os.ReadFile(existingPath)
	if err != nil {
		t.Fatalf("read overwritten: %v", err)
	}

	if string(data) != "new" {
		t.Fatalf("overwritten content: %q", data)
	}

	sub := filepath.Join(tmp, "sub")

	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	raw = json.RawMessage(`{"path":"` + nestedPath + `","content":"x"}`)

	_, err = Handle(raw)
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
}

func TestHandleErrors(t *testing.T) {
	tmp := t.TempDir()
	origRoot := server.ProjectRoot

	server.ProjectRoot = tmp
	t.Cleanup(func() { server.ProjectRoot = origRoot })

	// #nosec G304 -- test temp dir path
	if err := os.Mkdir(filepath.Join(tmp, "adir"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "missing path",
			raw:  `{"content":"x"}`,
			want: "path is required",
		},
		{
			name: "directory target",
			raw:  `{"path":"` + filepath.Join(tmp, "adir") + `","content":"x"}`,
			want: "not a file",
		},
		{
			name: "missing parent",
			raw:  `{"path":"` + filepath.Join(tmp, "nosuch", "child.txt") + `","content":"x"}`,
			want: "parent directory missing",
		},
		{
			name: "banned path",
			raw:  `{"path":"` + filepath.Join(tmp, ".env") + `","content":"x"}`,
			want: "check banned",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Handle(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestHandleInvalidJSON(t *testing.T) {
	_, err := Handle(json.RawMessage(`{`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}

	var target *json.SyntaxError

	if !errors.As(err, &target) {
		t.Fatalf("expected json.SyntaxError, got %T", err)
	}
}
