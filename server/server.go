// Package server implements a stdio MCP server speaking JSON-RPC 2.0.
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	jsonrpcVersion        = "2.0"
	scannerInitialBuf     = 1024 * 1024
	defaultRequestTimeout = 3 * time.Second
	filePermPrivate       = 0o600
	dirPermPrivate        = 0o700
)

var (
	errClientResponse  = errors.New("client error response")
	errRequestTimeout  = errors.New("request timeout")
	errProtectedSecret = errors.New("access denied: protected secret file")
	errOutsideProject  = errors.New("access denied: path outside project root")
)

// Server is a stdio MCP server implementing JSON-RPC 2.0.
type Server struct {
	name        string
	version     string
	tools       []ToolEntry
	projectRoot string
	stats       *toolStats

	writeMu        sync.Mutex
	pending        sync.Map // string(id) → chan rawResponse
	nextReqID      atomic.Int64
	clientHasRoots bool
}

type rawResponse struct {
	Result json.RawMessage
	Err    *RPCError
}

// Package-level cross-tool state.
//
// WRITE CONTRACT: every write to ProjectRoot, LastPath, lastDir, and
// projectRootLocked happens either (a) before Run starts (main.go) or
// (b) synchronously on the Run goroutine — handlers are invoked inline from
// processLine, so ResolvePath/SetLastPath/applyCwdOverride all run there.
// Goroutines spawned by Run (requestRoots, persistProjectRoot, autoDiscover,
// writeCachedRoot) must never touch these directly: requestRoots stages its
// result in pendingRoot (atomic) for the Run loop to apply via
// applyPendingRootIfAny, and the persist goroutines only write files.
//
// ProjectRoot returns the project root discovered during initialize, or "." if unknown.
var ProjectRoot = "." //nolint:gochecknoglobals // deliberate cross-tool server state

// LastPath is the last file path operated on. find_related defaults to it.
var LastPath = "" //nolint:gochecknoglobals // deliberate cross-tool server state
var lastDir = ""  //nolint:gochecknoglobals // deliberate cross-tool server state

// SetLastPath records the last operated-on file path for relative resolution.
func SetLastPath(p string) {
	LastPath = p

	if p != "" {
		lastDir = filepath.Dir(p)
	}
}

var initLogOnce sync.Once //nolint:gochecknoglobals // deliberate cross-tool server state

// logInitf writes debug diagnostics to the path in GREPFUNC_INIT_LOG (if set).
// No-op by default: the per-call payload log is opt-in only.
func logInitf(format string, args ...any) {
	logPath := os.Getenv("GREPFUNC_INIT_LOG")
	if logPath == "" {
		return
	}

	initLogOnce.Do(func() {
		// #nosec G703 -- path comes from the user-set GREPFUNC_INIT_LOG env var
		_ = os.WriteFile(logPath, nil, filePermPrivate)
	})

	// #nosec G304,G703 -- path comes from the user-set GREPFUNC_INIT_LOG env var
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePermPrivate)
	if err != nil {
		return
	}

	_, _ = fmt.Fprintf(logFile, format+"\n", args...)
	_ = logFile.Close()
}

// New creates a Server with the given name and version.
func New(name, version string) *Server {
	return &Server{
		name:           name,
		version:        version,
		stats:          newToolStats(),
		tools:          nil,
		projectRoot:    "",
		writeMu:        sync.Mutex{},
		pending:        sync.Map{},
		nextReqID:      atomic.Int64{},
		clientHasRoots: false,
	}
}

// Register adds a tool and its handler to the server's tool list.
func (s *Server) Register(tool Tool, handler ToolHandler) {
	s.tools = append(s.tools, ToolEntry{Tool: tool, Handler: handler})
}

// Run serves JSON-RPC messages from stdin until EOF. Messages are read as
// whole lines with no length cap, so oversized tool outputs cannot crash the
// server. Returns a non-nil error on any read failure (not on EOF).
func (s *Server) Run() error {
	reader := bufio.NewReaderSize(os.Stdin, scannerInitialBuf)

	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.processLine(line)
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("read stdin: %w", err)
		}
	}
}

// processLine handles one JSON-RPC message: client responses are routed to
// the pending channel, requests are dispatched to handle.
func (s *Server) processLine(line []byte) {
	// Peek to detect client responses vs requests.
	var env struct {
		ID     any             `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}

	err := json.Unmarshal(line, &env)
	if err != nil {
		s.sendError(nil, -32700, "Parse error", err.Error())

		return
	}
	// Route client response to pending channel (server-initiated request).
	if env.Method == "" {
		if chVal, ok := s.pending.Load(fmt.Sprint(env.ID)); ok {
			if ch, ok := chVal.(chan rawResponse); ok {
				ch <- rawResponse{Result: env.Result, Err: env.Error}
			}
		}

		return
	}

	var req Request

	err = json.Unmarshal(line, &req)
	if err != nil {
		s.sendError(nil, -32700, "Parse error", err.Error())

		return
	}

	resp := s.handle(req)
	if resp != nil {
		s.writeJSON(resp)
	}
}

// handle routes a JSON-RPC request to the matching case handler.
func (s *Server) handle(req Request) *Response {
	s.applyPendingRootIfAny()

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)

	case "notifications/initialized":
		if !projectRootLocked && s.clientHasRoots {
			go s.requestRoots()
		}

		return nil

	case "notifications/roots/list_changed":
		go s.requestRoots()

		return nil

	case "tools/list":
		return s.handleToolsList(req)

	case "tools/call":
		return s.handleToolsCall(req)

	default:
		return &Response{
			JSONRPC: jsonrpcVersion,
			ID:      req.ID,
			Result:  nil,
			Error:   &RPCError{Code: -32601, Message: "Method not found: " + req.Method, Data: nil},
		}
	}
}

// applyPendingRootIfAny applies a root queued by roots/list on the Run goroutine.
// Keeping all ProjectRoot writes on the Run goroutine makes them race-free.
func (s *Server) applyPendingRootIfAny() {
	queuedRoot, ok := pendingRoot.Load().(string)
	if !ok || queuedRoot == "" {
		return
	}

	s.applyPendingRoot(queuedRoot)
}

// handleToolsList returns the list of registered tools.
func (s *Server) handleToolsList(req Request) *Response {
	tools := make([]Tool, len(s.tools))

	for i, te := range s.tools {
		tools[i] = te.Tool
	}

	return &Response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: map[string]any{"tools": tools}, Error: nil}
}

// applyPendingRoot applies a root queued by roots/list on the Run goroutine.
func (s *Server) applyPendingRoot(queuedRoot string) {
	pendingRoot.Store("")

	if queuedRoot != ProjectRoot || !projectRootLocked {
		s.projectRoot = queuedRoot
		ProjectRoot = queuedRoot
		projectRootLocked = true

		fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot=%s (roots/list, locked)\n", ProjectRoot)

		go persistProjectRoot(queuedRoot)
		go autoDiscover(queuedRoot)
	}
}

// handleInitialize processes the MCP initialize handshake and pins ProjectRoot.
func (s *Server) handleInitialize(req Request) *Response {
	var initParams struct {
		RootPath string `json:"rootPath"`
		RootURI  string `json:"rootUri"`
		Roots    []struct {
			URI string `json:"uri"`
		} `json:"roots"`
		Capabilities struct {
			Roots *json.RawMessage `json:"roots"`
		} `json:"capabilities"`
	}

	err := json.Unmarshal(req.Params, &initParams)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] json.Unmarshal initialize: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[mcp] initialize: rootPath=%q rootUri=%q roots=%d\n",
		initParams.RootPath, initParams.RootURI, len(initParams.Roots))
	logInitf("raw params: %s\nrootPath=%q\nrootUri=%q\nroots=%d",
		string(req.Params), initParams.RootPath, initParams.RootURI, len(initParams.Roots))

	s.clientHasRoots = initParams.Capabilities.Roots != nil

	var firstRootURI string
	if len(initParams.Roots) > 0 {
		firstRootURI = initParams.Roots[0].URI
	}

	root, editorProvided, cwdAtStart, exePath := discoverRoot(initParams.RootPath, initParams.RootURI, firstRootURI)

	s.finalizeRoot(root, editorProvided, cwdAtStart, exePath)

	return &Response{
		JSONRPC: jsonrpcVersion,
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]string{"name": s.name, "version": s.version},
			"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
		},
		Error: nil,
	}
}

// finalizeRoot pins the resolved root, persists it, and logs the outcome.
func (s *Server) finalizeRoot(root string, editorProvided bool, cwdAtStart, exePath string) {
	// Normalize away symlinks (macOS /Users → /private/Users).
	if root != "" {
		resolved, err := filepath.EvalSymlinks(root)
		if err == nil {
			root = resolved
		}
	}

	s.projectRoot = root
	ProjectRoot = root
	// Persist for next auto-start.
	if root != "" && root != "/" {
		go writeCachedRoot(root)
	}
	// Append resolved root + existence check to init log.
	{
		_, statErr := os.Stat(root) // #nosec G703 -- paths are bounds-checked via CheckBounds/CheckBanned
		logInitf("ProjectRoot=%q exists=%v editorProvided=%v clientHasRoots=%v\ncwd=%q exe=%q",
			root, statErr == nil, editorProvided, s.clientHasRoots, cwdAtStart, exePath)
	}

	switch {
	case editorProvided:
		projectRootLocked = true

		fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot=%s (locked from initialize)\n", ProjectRoot)

	case root == "":
		fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot unset — waiting for cwd from first tool call\n")

	default:
		fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot=%s (cwd fallback, not locked)\n", ProjectRoot)
	}

	if root != "" {
		go persistProjectRoot(root)
		go autoDiscover(root)
	}
}

// discoverRoot derives the project root from initialize params, env, cwd, or cache.
func discoverRoot(rootPath, rootURI, firstRootURI string) (string, bool, string, string) {
	root := rootPath
	if root == "" {
		root = rootURI
	}

	if root == "" && firstRootURI != "" {
		root = firstRootURI
	}

	root = strings.TrimPrefix(root, "file://")
	editorProvided := root != ""

	cwdAtStart, _ := os.Getwd()
	exePath, _ := os.Executable()

	if root != "" {
		return root, editorProvided, cwdAtStart, exePath
	}

	if r := os.Getenv("PROJECT_ROOT"); r != "" {
		return r, true, cwdAtStart, exePath
	}

	return discoverRootFromFallbacks(cwdAtStart, exePath), false, cwdAtStart, exePath
}

// discoverRootFromFallbacks walks up from the executable dir, then falls back to the cached root.
func discoverRootFromFallbacks(cwdAtStart, exePath string) string {
	root := cwdAtStart
	// If cwd is empty or filesystem root, walk up from executable dir.
	if root == "" || root == "/" {
		if exePath != "" {
			root = FindProjectRoot(filepath.Dir(exePath))
		}
	}
	// Last resort: read cached root from previous session.
	if root == "" || root == "/" {
		root = readCachedRoot()
	}

	return root
}

// handleToolsCall dispatches a tool invocation to its registered handler.
func (s *Server) handleToolsCall(req Request) *Response {
	var params CallToolParams

	err := json.Unmarshal(req.Params, &params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] json.Unmarshal tools/call: %v\n", err)
	}

	args := params.Arguments
	if len(args) == 0 {
		args = params.Args
	}
	// Log first tool call to init log so we can see what Zed sends.
	logInitf("[call] tool=%s args=%s", params.Name, string(args))
	// Allow per-call cwd override only until a real project root exists.
	// Ignore it afterwards to prevent mid-session re-rooting to a stale value.
	s.applyCwdOverride(args)

	start := time.Now()

	for _, te := range s.tools {
		if te.Tool.Name == params.Name {
			result, err := te.Handler(args)
			isErr := err != nil || (result != nil && result.IsError)
			s.stats.record(s.tools, params.Name, isErr, time.Since(start), len(args), resultBytes(result, err))

			if err != nil {
				return &Response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: &ToolCallResult{
					Content: []ToolCallContent{{Type: "text", Text: err.Error()}},
					IsError: true,
				}, Error: nil}
			}

			return &Response{JSONRPC: jsonrpcVersion, ID: req.ID, Result: result, Error: nil}
		}
	}

	msg := "unknown tool: " + params.Name
	s.stats.record(s.tools, params.Name, true, time.Since(start), len(args), len(msg))

	return &Response{
		JSONRPC: jsonrpcVersion,
		ID:      req.ID,
		Result: &ToolCallResult{
			Content: []ToolCallContent{{Type: "text", Text: msg}},
			IsError: true,
		},
		Error: nil,
	}
}

// resultBytes approximates the output size sent back to the client.
func resultBytes(result *ToolCallResult, err error) int {
	if result == nil {
		if err != nil {
			return len(err.Error())
		}

		return 0
	}

	n := 0
	for _, c := range result.Content {
		n += len(c.Text)
	}

	return n
}

// applyCwdOverride re-orients ProjectRoot from a per-call cwd until a real root exists.
func (s *Server) applyCwdOverride(args json.RawMessage) {
	var cwdExtract struct {
		CWD string `json:"cwd"`
	}

	_ = json.Unmarshal(args, &cwdExtract)

	if cwdExtract.CWD == "" || projectRootLocked || (ProjectRoot != "" && ProjectRoot != ".") {
		return
	}

	resolved, err := filepath.EvalSymlinks(cwdExtract.CWD)
	if err == nil {
		cwdExtract.CWD = resolved
	}

	info, err := os.Stat(cwdExtract.CWD)
	if err != nil || !info.IsDir() {
		return
	}

	s.switchProjectRoot(cwdExtract.CWD)
}

// switchProjectRoot sets the active root, persisting when it was previously unset.
func (s *Server) switchProjectRoot(newRoot string) {
	if newRoot == ProjectRoot {
		ProjectRoot = newRoot
		s.projectRoot = newRoot

		return
	}

	wasEmpty := ProjectRoot == ""
	fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot switch: %q → %q\n", ProjectRoot, newRoot)
	logInitf("[switch] %q → %q", ProjectRoot, newRoot)

	ProjectRoot = newRoot

	s.projectRoot = newRoot

	if wasEmpty {
		go persistProjectRoot(newRoot)
		go autoDiscover(newRoot)
	}
}

func (s *Server) writeJSON(v any) {
	out, err := json.Marshal(v)
	if err != nil {
		return
	}

	s.writeMu.Lock()
	_, _ = fmt.Fprintln(os.Stdout, string(out))
	s.writeMu.Unlock()
}

func (s *Server) sendError(id any, code int, message string, data any) {
	s.writeJSON(Response{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Result:  nil,
		Error:   &RPCError{Code: code, Message: message, Data: data},
	})
}

// sendToClient sends a server-initiated JSON-RPC request and awaits the response.
func (s *Server) sendToClient(method string, params any) (json.RawMessage, error) {
	reqID := fmt.Sprintf("__srv__%d", s.nextReqID.Add(1))
	respCh := make(chan rawResponse, 1)

	s.pending.Store(reqID, respCh)
	defer s.pending.Delete(reqID)

	s.writeJSON(map[string]any{"jsonrpc": jsonrpcVersion, "id": reqID, "method": method, "params": params})

	select {
	case r := <-respCh:
		if r.Err != nil {
			return nil, fmt.Errorf("%w: %s error %d: %s", errClientResponse, method, r.Err.Code, r.Err.Message)
		}

		return r.Result, nil

	case <-time.After(requestTimeout()):
		return nil, fmt.Errorf("%s: %w", method, errRequestTimeout)
	}
}

// requestTimeout returns the timeout for server-initiated request round-trips.
// Override with GREPFUNC_REQUEST_TIMEOUT (Go duration, e.g. "10s").
func requestTimeout() time.Duration {
	if v := os.Getenv("GREPFUNC_REQUEST_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}

	return defaultRequestTimeout
}

// requestRoots requests the list of roots from the client and updates ProjectRoot.
func (s *Server) requestRoots() {
	result, err := s.sendToClient("roots/list", map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] roots/list: %v\n", err)

		return
	}

	var resp struct {
		Roots []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"roots"`
	}

	err = json.Unmarshal(result, &resp)
	if err != nil || len(resp.Roots) == 0 {
		fmt.Fprintf(os.Stderr, "[mcp] roots/list: no roots\n")

		return
	}

	root := strings.TrimPrefix(resp.Roots[0].URI, "file://")

	resolved, err := filepath.EvalSymlinks(root)
	if err == nil {
		root = resolved
	}

	pendingRoot.Store(root)
	fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot=%s queued from roots/list\n", root)
}

// IsBannedPath reports whether p is a protected secret file.
func IsBannedPath(p string) bool {
	base := filepath.Base(p)
	if strings.HasPrefix(base, ".env") || strings.HasPrefix(base, "credentials.") || strings.HasPrefix(base, "secrets.") {
		return true
	}

	switch base {
	case ".netrc", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", "credentials":
		return true
	}

	switch strings.ToLower(filepath.Ext(base)) {
	case ".pem", ".key", ".p12", ".pfx":
		return true
	}

	return false
}

// CheckBanned returns an error if p is a banned path.
func CheckBanned(path string) error {
	if IsBannedPath(path) {
		return fmt.Errorf("%w: %q", errProtectedSecret, filepath.Base(path))
	}

	return nil
}

// CheckBounds returns an error if p is outside ProjectRoot.
// No-op when root is not yet locked (unknown project root).
func CheckBounds(path string) error {
	if !projectRootLocked || ProjectRoot == "" {
		return nil
	}

	clean := filepath.Clean(path) + string(filepath.Separator)

	root := filepath.Clean(ProjectRoot) + string(filepath.Separator)
	if !strings.HasPrefix(clean, root) {
		return fmt.Errorf("%w: %q", errOutsideProject, path)
	}

	return nil
}

var projectRootLocked bool //nolint:gochecknoglobals // deliberate cross-tool server state

// pendingRoot queues a root from roots/list to be applied by the Run loop,
// keeping all ProjectRoot writes on a single goroutine (no data race).
// Value is a string; "" means none.
var pendingRoot atomic.Value //nolint:gochecknoglobals // deliberate cross-tool server state

// LockProjectRoot prevents further re-orientation of ProjectRoot.
func LockProjectRoot() {
	projectRootLocked = true
}

// ResolvePath resolves a tool path argument relative to the project root.
// Accepts: absolute paths, paths relative to project root, and paths prefixed
// with the project root's base name (Zed convention: "myproject/src/foo.go").
func ResolvePath(path string) string {
	if path == "" || path == "." {
		return ProjectRoot
	}

	if filepath.IsAbs(path) {
		lastDir = filepath.Dir(path)
		reorientRoot(path)

		return path
	}
	// Try last-known directory from previous absolute path.
	if lastDir != "" {
		candidate := filepath.Join(lastDir, path)

		_, err := os.Stat(candidate)
		if err == nil {
			return candidate
		}
	}
	// Strip project-root basename prefix if present.
	// e.g. ProjectRoot=/Users/x/myproject, p=myproject/src/foo.go → /Users/x/myproject/src/foo.go
	if base := filepath.Base(ProjectRoot); base != "" && base != "." {
		if path == base {
			return ProjectRoot
		}

		if rest, ok := strings.CutPrefix(path, base+"/"); ok {
			return filepath.Join(ProjectRoot, rest)
		}
	}

	return filepath.Join(ProjectRoot, path)
}

// FirstLine returns the first line of s, trimmed. When capLen > 0 the line is
// truncated to capLen chars with a trailing "...". A capLen of 0 keeps the
// full line. Single source of truth for signature previews across tools.
func FirstLine(s string, capLen int) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		s = strings.TrimSpace(before)
	}

	if capLen > 0 && len(s) > capLen {
		return s[:capLen] + "..."
	}

	return s
}

// TruncateToBudget trims s to budget bytes, cutting only at line boundaries so
// each kept line stays complete (head-preserving). Falls back to a hard slice
// when no newline falls within budget.
func TruncateToBudget(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}

	cut := strings.LastIndexByte(s[:budget], '\n')
	if cut < 0 {
		return s[:budget]
	}

	return s[:cut]
}

// BudgetHint returns the standard trimmed-output notice appended after budget truncation.
func BudgetHint(budget int, tip string) string {
	if tip != "" {
		return fmt.Sprintf("\n[Output trimmed to fit token_budget=%d. %s]\n", budget, tip)
	}

	return fmt.Sprintf("\n[Output trimmed to fit token_budget=%d.]\n", budget)
}

// BudgetResult trims the first text content of a result to budget chars, noting the trim.
func BudgetResult(result *ToolCallResult, budget int) *ToolCallResult {
	if result == nil || budget <= 0 || len(result.Content) == 0 {
		return result
	}

	text := result.Content[0].Text
	if len(text) <= budget {
		return result
	}

	result.Content[0].Text = TruncateToBudget(text, budget) + BudgetHint(budget, "")

	return result
}

// reorientRoot re-pins ProjectRoot when an absolute path lies outside it.
func reorientRoot(path string) {
	cleanRoot := filepath.Clean(ProjectRoot)
	underRoot := filepath.Clean(path) + string(filepath.Separator)

	if ProjectRoot != "" && strings.HasPrefix(underRoot, cleanRoot+string(filepath.Separator)) {
		projectRootLocked = true

		return
	}

	if root := FindProjectRoot(filepath.Dir(path)); root != "" {
		ProjectRoot = root
		projectRootLocked = true

		return
	}

	if !projectRootLocked {
		ProjectRoot = filepath.Dir(path)
	}
}

// RelPath strips the project root prefix from an absolute path for display.
func RelPath(abs string) string {
	if ProjectRoot == "." || ProjectRoot == "" {
		return abs
	}

	root := ProjectRoot
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}

	if rel, ok := strings.CutPrefix(abs, root); ok {
		return rel
	}
	// fallback: resolve symlinks (macOS /Users → /private/Users)
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		if rel, ok := strings.CutPrefix(resolved, root); ok {
			return rel
		}
	}

	return abs
}

// persistProjectRoot writes server.project_root into the project's memory store.
// Called async on initialize so the AI sees the resolved root on memory() recall.
func persistProjectRoot(root string) {
	mem := readMemStore(root)

	filtered := mem.Entries[:0]

	for _, e := range mem.Entries {
		if e.Key != "server.project_root" {
			filtered = append(filtered, e)
		}
	}
	// Prepend so it's first on recall
	filtered = append([]memEntry{{
		Key:   "server.project_root",
		Value: root,
		At:    time.Now().UTC().Format(time.RFC3339),
	}}, filtered...)

	writeMemStore(root, filtered)
}

// cachedRootFile returns the path to the persisted last-known project root.
func cachedRootFile() string {
	home, _ := os.UserHomeDir()

	return filepath.Join(home, ".config", "grepfunc", "last_root")
}

func writeCachedRoot(root string) {
	cachedFile := cachedRootFile()
	_ = os.MkdirAll(filepath.Dir(cachedFile), dirPermPrivate)
	_ = os.WriteFile(cachedFile, []byte(root), filePermPrivate)
}

func readCachedRoot() string {
	data, err := os.ReadFile(cachedRootFile())
	if err != nil {
		return ""
	}

	root := strings.TrimSpace(string(data))

	_, err = os.Stat(root) // #nosec G703 -- paths are bounds-checked via CheckBounds/CheckBanned
	if err != nil {
		return ""
	}

	return root
}

// FindProjectRoot walks up from dir looking for a project marker file.
// Returns "" when no marker is found.
func FindProjectRoot(dir string) string {
	markers := []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml",
		"setup.py", "Gemfile", "pom.xml", "build.gradle"}

	for {
		for _, marker := range markers {
			_, err := os.Stat(filepath.Join(dir, marker))
			if err == nil {
				return dir
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}

		dir = parent
	}
}

// autoDiscover scans the project root and persists facts the AI needs every session.
// Runs async on initialize so the AI doesn't have to re-discover them.
func autoDiscover(root string) {
	facts := make(map[string]string)

	detectLanguage(root, facts)

	// Available tools
	for _, tool := range []string{"gofmt", "go", "prettier", "rustfmt", "ruff", "python3"} {
		_, err := exec.LookPath(tool)
		if err == nil {
			facts["server.tool."+tool] = "available"
		}
	}

	if len(facts) == 0 {
		return
	}

	mem := readMemStore(root)

	// Remove stale server.* entries before writing (dedup with persistProjectRoot)
	filtered := mem.Entries[:0]

	for _, e := range mem.Entries {
		if !strings.HasPrefix(e.Key, "server.") {
			filtered = append(filtered, e)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range facts {
		filtered = append([]memEntry{{Key: k, Value: v, At: now}}, filtered...)
	}

	writeMemStore(root, filtered)
}

// memEntry is one key/value record in the project's memory store.
type memEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	At    string `json:"at"`
}

// memStore is the on-disk .llm/memory.json layout shared by server writers.
type memStore struct {
	Entries []memEntry `json:"entries"`
}

// memPathOf returns the memory store path for a project root.
func memPathOf(root string) string {
	return filepath.Join(root, ".llm", "memory.json")
}

// readMemStore loads the project's memory store, tolerating missing/corrupt files.
func readMemStore(root string) memStore {
	// #nosec G304 -- memPathOf is derived from the locked project root
	data, _ := os.ReadFile(memPathOf(root))

	var mem memStore

	_ = json.Unmarshal(data, &mem)

	return mem
}

// writeMemStore persists entries to the project's memory store.
func writeMemStore(root string, entries []memEntry) {
	out, err := json.MarshalIndent(memStore{Entries: entries}, "", "  ")
	if err != nil {
		return
	}

	memPath := memPathOf(root)
	_ = os.MkdirAll(filepath.Dir(memPath), dirPermPrivate)

	// #nosec G304 -- memPath is derived from the locked project root
	_ = os.WriteFile(memPath, out, filePermPrivate)
}

// detectLanguage records language facts from project marker files.
func detectLanguage(root string, facts map[string]string) {
	// #nosec G304 -- root is the server's project root
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err == nil {
		facts["server.lang"] = "go"

		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "go "); ok {
				facts["server.go_version"] = strings.TrimSpace(v)

				break
			}
		}

		return
	}

	// #nosec G703 -- paths are bounds-checked via CheckBounds/CheckBanned
	_, err = os.Stat(filepath.Join(root, "package.json"))
	if err == nil {
		facts["server.lang"] = "js/ts"

		return
	}

	_, err = os.Stat(filepath.Join(root, "Cargo.toml"))
	if err == nil {
		facts["server.lang"] = "rust"

		return
	}

	_, err = os.Stat(filepath.Join(root, "pyproject.toml"))
	if err == nil {
		facts["server.lang"] = "python"
	}
}
