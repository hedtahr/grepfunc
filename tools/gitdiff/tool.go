// Package gitdiff shows line-level git diff output.
package gitdiff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hedtahr/grepfunc/server"
)

const (
	typeText = "text"

	defaultContext  = 3
	maxContextLines = 10
)

var errGitDiff = errors.New("git diff failed")

// Tool describes the git_diff tool.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "git_diff",
	Description: "Show git diff output for a file or all changes. Shows actual line-level changes, avoiding the " +
		"need to read whole files. Use staged=true for staged changes, base='HEAD~1' to compare commits.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        "string",
				Description: "Specific file to diff. Absolute path or project-relative. Omit for all changed files.",
				Items:       nil,
			},
			"staged": {
				Type:        "boolean",
				Description: "Show staged (--cached) changes. Default false.",
				Items:       nil,
			},
			"context_lines": {
				Type:        "integer",
				Description: "Lines of context around changes. Default 3, max 10.",
				Items:       nil,
			},
			"stat_only": {
				Type:        "boolean",
				Description: "Show only --stat summary (no line diff).",
				Items:       nil,
			},
			"base": {
				Type:        "string",
				Description: "Base commit or branch to diff against, e.g. 'HEAD~1', 'main'. Default: working tree diff.",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
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
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}
}

// Handle renders the git diff for the requested scope.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var req args

	err := json.Unmarshal(raw, &req)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	ctx := req.ContextLines
	if ctx <= 0 {
		ctx = defaultContext
	}

	ctx = min(ctx, maxContextLines)

	gitArgs, err := resolveGitArgs(req, ctx)
	if err != nil {
		return nil, err
	}

	gitRoot, ok := findGitRoot(server.ProjectRoot)
	if !ok {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{
				Type: typeText,
				Text: "no git repository found (checked " + server.ProjectRoot + " and parents)",
			}},
			IsError: false,
		}, nil
	}

	out, err := runGit(gitRoot, gitArgs)
	if err != nil {
		return nil, err
	}

	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		msg := "No changes"
		if req.Staged {
			msg = "No staged changes"
		}

		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: typeText, Text: msg}},
			IsError: false,
		}, nil
	}

	if req.StatOnly {
		text = "```\n" + text + "\n```"
	} else {
		text = "```diff\n" + text + "\n```"
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}, nil
}

func resolveGitArgs(req args, ctx int) ([]string, error) {
	gitArgs := []string{"diff"}

	if req.Base != "" {
		gitArgs = append(gitArgs, req.Base)
	} else if req.Staged {
		gitArgs = append(gitArgs, "--cached")
	}

	if req.StatOnly {
		gitArgs = append(gitArgs, "--stat")
	} else {
		gitArgs = append(gitArgs, "-U"+strconv.Itoa(ctx))
	}

	if req.Path != "" {
		resolved := server.ResolvePath(req.Path)

		err := server.CheckBounds(resolved)
		if err != nil {
			return nil, fmt.Errorf("check bounds: %w", err)
		}

		err = server.CheckBanned(resolved)
		if err != nil {
			return nil, fmt.Errorf("check banned: %w", err)
		}

		gitArgs = append(gitArgs, "--", resolved)
	}

	return gitArgs, nil
}

func runGit(gitRoot string, gitArgs []string) ([]byte, error) {
	// #nosec G204 -- fixed git binary
	cmd := exec.CommandContext(context.Background(), "git", gitArgs...)
	cmd.Dir = gitRoot

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("%w: %s", errGitDiff, strings.TrimSpace(string(exitErr.Stderr)))
		}

		if len(out) == 0 {
			return nil, fmt.Errorf("git diff failed: %w", err)
		}
	}

	return out, nil
}
