package patchedit

import (
	"encoding/json"

	"github.com/hedtahr/grepfunc/server"
)

const (
	contentTypeText = "text"

	jsonTypeString  = "string"
	jsonTypeInteger = "integer"
	jsonTypeBoolean = "boolean"
	jsonTypeArray   = "array"
	jsonTypeObject  = "object"

	propPath  = "path"
	propEdits = "edits"
)

// Tool is the MCP tool definition for patch_file.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name: "patch_file",
	Description: "Edit a file by replacing old_text with new_text (fuzzy matching) or inserting text at a line. " +
		"Runs formatter BEFORE the edit when applicable. Prefer edit mode for surgical changes.",
	InputSchema: server.InputSchema{
		Type:                 jsonTypeObject,
		AdditionalProperties: false,
		Properties: map[string]server.Property{
			propPath: {Type: jsonTypeString, Items: nil, Description: "File to edit. Absolute path or project-relative."},
			propEdits: {Type: jsonTypeArray, Description: "Array of edit operations (find-and-replace). " +
				"Each item: {old_text, new_text, index?, replace_all?}.",
				Items: &server.Property{Type: jsonTypeObject, Items: nil,
					Description: "An edit with old_text, new_text, optional index, " +
						"and optional replace_all (replaces all occurrences instead of erroring on AMBIGUOUS_MATCH)."}},
			"inserts": {Type: jsonTypeArray,
				Description: "Array of insert operations. Each inserts text before a specific line.",
				Items: &server.Property{Type: jsonTypeObject, Items: nil,
					Description: "{line (1-based), text, index?}."}},
			"dry_run": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, preview changes without writing the file."},
			"fail_fast": {Type: jsonTypeBoolean, Items: nil,
				Description: "All-or-nothing: if any edit fails to match, write nothing."},

			"diff_context": {Type: jsonTypeInteger, Items: nil,
				Description: "Diff hunk context lines. Default 3, max 10."},
			"create_if_missing": {Type: jsonTypeBoolean, Items: nil,
				Description: "Create an empty file first if path does not exist."},
			"skip_validate": {Type: jsonTypeBoolean, Items: nil,
				Description: "Skip post-write validation (go vet / python ast)."},
			"append_text": {Type: jsonTypeString, Items: nil,
				Description: "Text to append at end of file (auto-newline if missing)."},
			"no_diff": {Type: jsonTypeBoolean, Items: nil,
				Description: "Omit the diff block; show only the summary line."},
			"echo_lines": {Type: jsonTypeInteger, Items: nil,
				Description: "Context lines around first edit point in result echo. Default 3; 0 disables."},
			"terse": {Type: jsonTypeBoolean, Items: nil,
				Description: "Minimal output: '[OK] N/N edits applied' or '[FAIL] errors'."},
			"insert_file": {Type: jsonTypeString, Items: nil,
				Description: "File whose contents are inserted at insert_line."},
			"insert_line": {Type: jsonTypeInteger, Items: nil,
				Description: "Line number to insert insert_file contents before. Default 1."},
		},
		Required: []string{propPath},
	},
}

// EditOp is a single find-and-replace edit operation.
type EditOp struct {
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	Index      int    `json:"index,omitempty"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// InsertOp inserts text before a specific line.
type InsertOp struct {
	Line  int    `json:"line"`
	Text  string `json:"text"`
	Index int    `json:"index,omitempty"`
}

// EditFileArgs is the parsed JSON arguments for patch_file.
type EditFileArgs struct {
	Path     string     `json:"path"`
	Edits    []EditOp   `json:"edits"`
	Inserts  []InsertOp `json:"inserts"`
	DryRun   bool       `json:"dry_run"`
	FailFast bool       `json:"fail_fast"`

	DiffContext     int    `json:"diff_context"`
	CreateIfMissing bool   `json:"create_if_missing"`
	SkipValidate    bool   `json:"skip_validate"`
	AppendText      string `json:"append_text"`
	NoDiff          bool   `json:"no_diff"`
	EchoLines       int    `json:"echo_lines"`
	Terse           bool   `json:"terse"`
	InsertFile      string `json:"insert_file"`
	InsertLine      int    `json:"insert_line"`
}

// MatchLoc describes one match region in the target file.
type MatchLoc struct {
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Offset    int    `json:"offset"`
	EndOffset int    `json:"end_offset"`
	Strategy  string `json:"strategy"`
}

type editResult struct {
	Index        int
	Success      bool
	Matches      []MatchLoc
	Error        string
	OldText      string
	NewText      string
	LinesChanged int
}

// Handle processes a patch_file request.
func Handle(args json.RawMessage) (*server.ToolCallResult, error) {
	return handleEditFile(args)
}
