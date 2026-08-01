// Package gitcontext provides compact git orientation and diff tools.
package gitcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/gitdiff"
)

const (
	typeString  = "string"
	typeInteger = "integer"
	typeBoolean = "boolean"

	defaultCommitCount = 5
)

// Tool describes the git_context tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "git_context",
	Description: "Compact git orientation: current branch, recent commits, working-tree status, and optionally " +
		"diff --stat. One call instead of 3 terminal commands. Use at session start to understand where the project is.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        typeString,
				Description: "Project directory. Optional — defaults to the opened project root.",
				Items:       nil,
			},
			"commits": {
				Type:        typeInteger,
				Description: "Number of recent commits to show. Default 5.",
				Items:       nil,
			},
			"diff_stat": {
				Type:        typeBoolean,
				Description: "Include git diff --stat (unstaged changes). Default true.",
				Items:       nil,
			},
			"compact": {
				Type:        typeBoolean,
				Description: "Terse output. Default false.",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

type args struct {
	Path     string `json:"path"`
	Commits  int    `json:"commits"`
	DiffStat bool   `json:"diff_stat"`
	Compact  bool   `json:"compact"`
}

// Handle renders compact git context.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	dir := server.ProjectRoot
	if req.Path != "" {
		dir = server.ResolvePath(req.Path)
	}

	if req.Commits <= 0 {
		req.Commits = defaultCommitCount
	}

	run := func(gitArgs ...string) string {
		// #nosec G204 -- fixed git binary
		cmd := exec.CommandContext(context.Background(), "git", gitArgs...)
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
			IsError: false,
		}, nil
	}

	log := run("log", "--oneline", fmt.Sprintf("-%d", req.Commits))
	status := run("status", "--short")
	diffStat := run("diff", "--stat")

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: renderContext(req, branch, log, status, diffStat)}},
		IsError: false,
	}, nil
}

func renderContext(req args, branch, log, status, diffStat string) string {
	var buf strings.Builder
	if req.Compact {
		fmt.Fprintf(&buf, "branch: %s\n", branch)
	} else {
		fmt.Fprintf(&buf, "## Git Context\n\n**Branch:** %s\n", branch)
	}

	if log != "" {
		if req.Compact {
			fmt.Fprintf(&buf, "log:\n```\n%s\n```\n", log)
		} else {
			fmt.Fprintf(&buf, "\n**Recent commits:**\n```\n%s\n```\n", log)
		}
	}

	if status != "" {
		if req.Compact {
			fmt.Fprintf(&buf, "status:\n```\n%s\n```\n", status)
		} else {
			fmt.Fprintf(&buf, "\n**Status:**\n```\n%s\n```\n", status)
		}
	} else if !req.Compact {
		buf.WriteString("\n**Status:** clean\n")
	}

	if diffStat != "" {
		if req.Compact {
			fmt.Fprintf(&buf, "diff:\n```\n%s\n```\n", diffStat)
		} else {
			fmt.Fprintf(&buf, "\n**Unstaged diff:**\n```\n%s\n```\n", diffStat)
		}
	}

	return buf.String()
}

// GitTool is the combined git tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var GitTool = server.Tool{
	Name: "git",
	Description: "Git operations in one tool. mode=context (default) returns branch, recent commits, " +
		"working-tree status, and diff --stat; mode=diff returns line-level changes for a file or the whole tree. " +
		"One call instead of 3 terminal commands.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"mode": {
				Type:        typeString,
				Description: "'context' (default) or 'diff'.",
				Items:       nil,
			},
			"path": {
				Type: typeString,
				Description: "Context mode: project directory. Diff mode: specific file to diff " +
					"(omit for all changed files). Optional — defaults to the opened project root.",
				Items: nil,
			},
			"commits": {
				Type:        typeInteger,
				Description: "Context mode: number of recent commits to show. Default 5.",
				Items:       nil,
			},
			"diff_stat": {
				Type:        typeBoolean,
				Description: "Context mode: include git diff --stat. Default true.",
				Items:       nil,
			},
			"compact": {
				Type:        typeBoolean,
				Description: "Context mode: terse output. Default false.",
				Items:       nil,
			},
			"staged": {
				Type:        typeBoolean,
				Description: "Diff mode: show staged (--cached) changes. Default false.",
				Items:       nil,
			},
			"context_lines": {
				Type:        typeInteger,
				Description: "Diff mode: lines of context around changes. Default 3, max 10.",
				Items:       nil,
			},
			"stat_only": {
				Type:        typeBoolean,
				Description: "Diff mode: show only --stat summary (no line diff).",
				Items:       nil,
			},
			"base": {
				Type:        typeString,
				Description: "Diff mode: base commit or branch to diff against, e.g. 'HEAD~1', 'main'. Default: working tree diff.",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
	},
}

// GitHandle routes to the diff or context handler.
func GitHandle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req struct {
		Mode string `json:"mode"`
	}

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if req.Mode == "diff" {
		res, err := gitdiff.Handle(raw)
		if err != nil {
			return nil, fmt.Errorf("git diff: %w", err)
		}

		return res, nil
	}

	return Handle(raw)
}
