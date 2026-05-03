package gitcontext

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "git_context",
	Description: "Compact git orientation: current branch, recent commits, working-tree status, and optionally diff --stat. One call instead of 3 terminal commands. Use at session start to understand where the project is.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":      {Type: "string", Description: "Project directory. Defaults to project root."},
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
			fmt.Fprintf(&buf, "log:\n%s\n", indent(log, "  "))
		} else {
			fmt.Fprintf(&buf, "\n**Recent commits:**\n```\n%s\n```\n", log)
		}
	}

	if status != "" {
		if a.Compact {
			fmt.Fprintf(&buf, "status:\n%s\n", indent(status, "  "))
		} else {
			fmt.Fprintf(&buf, "\n**Status:**\n```\n%s\n```\n", status)
		}
	} else if !a.Compact {
		buf.WriteString("\n**Status:** clean\n")
	}

	diffStat := run("diff", "--stat")
	if diffStat != "" {
		if a.Compact {
			fmt.Fprintf(&buf, "diff:\n%s\n", indent(diffStat, "  "))
		} else {
			fmt.Fprintf(&buf, "\n**Unstaged diff:**\n```\n%s\n```\n", diffStat)
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
