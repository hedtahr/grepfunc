package gitdiff

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "git_diff",
	Description: "Show git diff output for a file or all changes. Shows actual line-level changes, avoiding the need to read whole files. Use staged=true for staged changes, base='HEAD~1' to compare commits.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":          {Type: "string", Description: "Specific file to diff. Absolute path or project-relative. Omit for all changed files."},
			"staged":        {Type: "boolean", Description: "Show staged (--cached) changes. Default false."},
			"context_lines": {Type: "integer", Description: "Lines of context around changes. Default 3, max 10."},
			"stat_only":     {Type: "boolean", Description: "Show only --stat summary (no line diff)."},
			"base":          {Type: "string", Description: "Base commit or branch to diff against, e.g. 'HEAD~1', 'main'. Default: working tree diff."},
		},
		Required: []string{},
	},
}

type args struct {
	Path         string `json:"path"`
	Staged       bool   `json:"staged"`
	ContextLines int    `json:"context_lines"`
	StatOnly     bool   `json:"stat_only"`
	Base         string `json:"base"`
}

func findGitRoot(start string) (string, bool) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}

	ctx := a.ContextLines
	if ctx <= 0 {
		ctx = 3
	}
	ctx = min(ctx, 10)

	gitArgs := []string{"diff"}

	if a.Base != "" {
		gitArgs = append(gitArgs, a.Base)
	} else if a.Staged {
		gitArgs = append(gitArgs, "--cached")
	}

	if a.StatOnly {
		gitArgs = append(gitArgs, "--stat")
	} else {
		gitArgs = append(gitArgs, "-U"+strconv.Itoa(ctx))
	}

	if a.Path != "" {
		resolved := server.ResolvePath(a.Path)
		if err := server.CheckBounds(resolved); err != nil {
			return nil, err
		}
		if err := server.CheckBanned(resolved); err != nil {
			return nil, err
		}
		gitArgs = append(gitArgs, "--", resolved)
	}

	gitRoot, ok := findGitRoot(server.ProjectRoot)
	if !ok {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: "no git repository found (checked " + server.ProjectRoot + " and parents)"}},
		}, nil
	}

	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = gitRoot

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("git diff failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("git diff failed: %v", err)
		}
	}

	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		msg := "No changes"
		if a.Staged {
			msg = "No staged changes"
		}
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: msg}},
		}, nil
	}

	if a.StatOnly {
		text = "```\n" + text + "\n```"
	} else {
		text = "```diff\n" + text + "\n```"
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: text}},
	}, nil
}
