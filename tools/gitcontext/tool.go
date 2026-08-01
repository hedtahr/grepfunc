package gitcontext

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/gitdiff"
)

var Tool = server.Tool{
	Name:        "git_context",
	Description: "Compact git orientation: current branch, recent commits, working-tree status, and optionally diff --stat. One call instead of 3 terminal commands. Use at session start to understand where the project is.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":      {Type: "string", Description: "Project directory. Optional — defaults to the opened project root."},
			"commits":   {Type: "integer", Description: "Number of recent commits to show. Default 5."},
			"diff_stat": {Type: "boolean", Description: "Include git diff --stat (unstaged changes). Default true."},
			"compact":   {Type: "boolean", Description: "Terse output. Default false."},
		},
		Required: []string{},
	},
}

type args struct {
	Path     string `json:"path"`
	Commits  int    `json:"commits"`
	DiffStat bool   `json:"diff_stat"`
	Compact  bool   `json:"compact"`
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	dir := server.ProjectRoot
	if a.Path != "" {
		dir = server.ResolvePath(a.Path)
	}
	if a.Commits <= 0 {
		a.Commits = 5
	}

	run := func(gitArgs ...string) string {
		cmd := exec.Command("git", gitArgs...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	branch := run("rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: "Not a git repository or git not found.\n"}},
		}, nil
	}

	log := run("log", "--oneline", fmt.Sprintf("-%d", a.Commits))
	status := run("status", "--short")

	var buf strings.Builder
	if a.Compact {
		fmt.Fprintf(&buf, "branch: %s\n", branch)
	} else {
		fmt.Fprintf(&buf, "## Git Context\n\n**Branch:** %s\n", branch)
	}

	if log != "" {
		if a.Compact {
			fmt.Fprintf(&buf, "log:\n```\n%s\n```\n", log)
		} else {
			fmt.Fprintf(&buf, "\n**Recent commits:**\n```\n%s\n```\n", log)
		}
	}

	if status != "" {
		if a.Compact {
			fmt.Fprintf(&buf, "status:\n```\n%s\n```\n", status)
		} else {
			fmt.Fprintf(&buf, "\n**Status:**\n```\n%s\n```\n", status)
		}
	} else if !a.Compact {
		buf.WriteString("\n**Status:** clean\n")
	}

	diffStat := run("diff", "--stat")
	if diffStat != "" {
		if a.Compact {
			fmt.Fprintf(&buf, "diff:\n```\n%s\n```\n", diffStat)
		} else {
			fmt.Fprintf(&buf, "\n**Unstaged diff:**\n```\n%s\n```\n", diffStat)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

var GitTool = server.Tool{
	Name:        "git",
	Description: "Git operations in one tool. mode=context (default) returns branch, recent commits, working-tree status, and diff --stat; mode=diff returns line-level changes for a file or the whole tree. One call instead of 3 terminal commands.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"mode":          {Type: "string", Description: "'context' (default) or 'diff'."},
			"path":          {Type: "string", Description: "Context mode: project directory. Diff mode: specific file to diff (omit for all changed files). Optional — defaults to the opened project root."},
			"commits":       {Type: "integer", Description: "Context mode: number of recent commits to show. Default 5."},
			"diff_stat":     {Type: "boolean", Description: "Context mode: include git diff --stat. Default true."},
			"compact":       {Type: "boolean", Description: "Context mode: terse output. Default false."},
			"staged":        {Type: "boolean", Description: "Diff mode: show staged (--cached) changes. Default false."},
			"context_lines": {Type: "integer", Description: "Diff mode: lines of context around changes. Default 3, max 10."},
			"stat_only":     {Type: "boolean", Description: "Diff mode: show only --stat summary (no line diff)."},
			"base":          {Type: "string", Description: "Diff mode: base commit or branch to diff against, e.g. 'HEAD~1', 'main'. Default: working tree diff."},
		},
		Required: []string{},
	},
}

func GitHandle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	if a.Mode == "diff" {
		return gitdiff.Handle(raw)
	}
	return Handle(raw)
}