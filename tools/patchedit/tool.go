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
	Description: "Edit a file by replacing old_text with new_text using fuzzy matching, " +
		"or inserting text at a specific line. Reads the file fresh before every edit " +
		"(eliminates stale content errors). Runs formatter BEFORE the edit when applicable, " +
		"so the diff shows only your intended changes.",
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
					Description: "An insert with 'line' (1-based line number to insert before), " +
						"'text' to insert, and optional index for ordering."}},
			"dry_run": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, preview changes without writing the file."},
			"fail_fast": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, apply all-or-nothing: if any edit fails to match, " +
					"no edits are written to disk. Returns the full match report without modifying the file."},

			"diff_context": {Type: jsonTypeInteger, Items: nil,
				Description: "Lines of context around diff hunks. Default 3. " +
					"Use 0 for minimal diff (changed lines only). Max 10."},
			"create_if_missing": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true and path does not exist, create an empty file before applying edits. " +
					"Useful for new-file creation without switching to write mode."},
			"skip_validate": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, skip post-write validation (go vet / python ast). " +
					"Reduces latency for multi-edit sequences."},
			"append_text": {Type: jsonTypeString, Items: nil,
				Description: "Text to append to the end of the file. Applied after all edits/inserts. " +
					"A newline separator is added automatically if the file doesn't end with one."},
			"no_diff": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, omit the diff block from the response. " +
					"Shows only the edit summary line. Reduces token usage for confirmation-only workflows."},
			"echo_lines": {Type: jsonTypeInteger, Items: nil,
				Description: "Lines of context around first edit point in result echo. Default 3. Set 0 to disable " +
					"(saves ~50 tokens). Eliminates a follow-up file_head call to verify the result."},
			"terse": {Type: jsonTypeBoolean, Items: nil,
				Description: "If true, return minimal output. " +
					"Just '[OK] N/N edits applied' or '[FAIL] errors'. No diff, no per-edit table, no echo_lines."},
			"insert_file": {Type: jsonTypeString, Items: nil,
				Description: "Path to a file whose contents should be inserted. Reads the file and treats it " +
					"as an insert operation at the specified line."},
			"insert_line": {Type: jsonTypeInteger, Items: nil,
				Description: "Line number to insert the file contents before. Default 1."},
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
