package server

import "encoding/json"

// Request is a JSON-RPC request received from the client.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC response sent back to the client.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Tool describes a tool offered by this server.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema is the JSON schema for a tool's arguments.
type InputSchema struct {
	Type                 string              `json:"type"`
	Properties           map[string]Property `json:"properties"`
	Required             []string            `json:"required"`
	AdditionalProperties bool                `json:"additionalProperties"`
}

// Property describes one input schema property.
type Property struct {
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Items       *Property `json:"items,omitempty"`
}

// CallToolParams are the arguments of a tools/call request.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Args      json.RawMessage `json:"args"`
}

// ToolCallContent is one text item of a tool call result.
type ToolCallContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCallResult is the result payload of a tool call.
type ToolCallResult struct {
	Content []ToolCallContent `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

// ToolHandler runs a tool and returns its result or an error.
type ToolHandler func(args json.RawMessage) (*ToolCallResult, error)

// ToolEntry binds a tool description to its handler.
type ToolEntry struct {
	Tool    Tool
	Handler ToolHandler
}
