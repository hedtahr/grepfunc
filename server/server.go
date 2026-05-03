package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	name        string
	version     string
	tools       []ToolEntry
	projectRoot string
}

// ProjectRoot returns the project root discovered during initialize, or "." if unknown.
var ProjectRoot = "."

// LastPath is the last file path operated on. find_related defaults to it.
var LastPath = ""

func SetLastPath(p string) { LastPath = p }

// SessionCache is a cross-tool cache for expensive lookups (e.g., symbol bodies).
var (
	SessionCache   = map[string]any{}
	SessionCacheMu sync.RWMutex
)

func CacheSet(key string, val any) {
	SessionCacheMu.Lock()
	SessionCache[key] = val
	SessionCacheMu.Unlock()
}

func CacheGet(key string) (any, bool) {
	SessionCacheMu.RLock()
	v, ok := SessionCache[key]
	SessionCacheMu.RUnlock()
	return v, ok
}

func New(name, version string) *Server {
	return &Server{name: name, version: version}
}

func (s *Server) Register(tool Tool, handler ToolHandler) {
	s.tools = append(s.tools, ToolEntry{Tool: tool, Handler: handler})
}

func (s *Server) Run() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(nil, -32700, "Parse error", err.Error())
			continue
		}
		resp := s.handle(req)
		if resp != nil {
			out, _ := json.Marshal(resp)
			fmt.Println(string(out))
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scanner error: %v\n", err)
		os.Exit(1)
	}
}

func (s *Server) handle(req Request) *Response {
	switch req.Method {
	case "initialize":
		var initParams struct {
			RootPath string `json:"rootPath"`
			RootURI  string `json:"rootUri"`
			Roots    []struct {
				URI string `json:"uri"`
			} `json:"roots"`
		}
		if err := json.Unmarshal(req.Params, &initParams); err != nil {
			fmt.Fprintf(os.Stderr, "[mcp] json.Unmarshal initialize: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "[mcp] initialize: rootPath=%q rootUri=%q roots=%d\n",
			initParams.RootPath, initParams.RootURI, len(initParams.Roots))
		if os.Getenv("GREPFUNC_DEBUG") != "" {
			if f, err := os.OpenFile("/tmp/grepfunc-init.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
				fmt.Fprintf(f, "raw params: %s\nrootPath=%q\nrootUri=%q\nroots=%d\n",
					string(req.Params), initParams.RootPath, initParams.RootURI, len(initParams.Roots))
				for i, r := range initParams.Roots {
					fmt.Fprintf(f, "  roots[%d].uri=%q\n", i, r.URI)
				}
				f.Close()
			}
		}
		root := initParams.RootPath
		if root == "" {
			root = initParams.RootURI
		}
		if root == "" && len(initParams.Roots) > 0 {
			root = initParams.Roots[0].URI
		}
		root = strings.TrimPrefix(root, "file://")
		if root == "" {
			if r := os.Getenv("PROJECT_ROOT"); r != "" {
				root = r
			} else {
				root, _ = os.Getwd()
			}
		}
		// normalize away symlinks (macOS /Users → /private/Users)
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		s.projectRoot = root
		ProjectRoot = root
		fmt.Fprintf(os.Stderr, "[mcp] ProjectRoot=%s\n", ProjectRoot)
		go persistProjectRoot(root)
		go autoDiscover(root)
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo":      map[string]string{"name": s.name, "version": s.version},
				"capabilities":    map[string]any{"tools": map[string]bool{"listChanged": false}},
			},
		}

	case "notifications/initialized":
		return nil

	case "tools/list":
		tools := make([]Tool, len(s.tools))
		for i, te := range s.tools {
			tools[i] = te.Tool
		}
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}}

	case "tools/call":
		var params CallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			fmt.Fprintf(os.Stderr, "[mcp] json.Unmarshal tools/call: %v\n", err)
		}
		args := params.Arguments
		if len(args) == 0 {
			args = params.Args
		}
		// Allow per-call cwd override (Zed may not send rootPath in initialize)
		var cwdExtract struct {
			CWD string `json:"cwd"`
		}
		json.Unmarshal(args, &cwdExtract)
		if cwdExtract.CWD != "" {
			if resolved, err := filepath.EvalSymlinks(cwdExtract.CWD); err == nil {
				cwdExtract.CWD = resolved
			}
			if info, err := os.Stat(cwdExtract.CWD); err == nil && info.IsDir() {
				ProjectRoot = cwdExtract.CWD
				s.projectRoot = cwdExtract.CWD
			}
		}

		for _, te := range s.tools {
			if te.Tool.Name == params.Name {
				result, err := te.Handler(args)
				if err != nil {
					return &Response{JSONRPC: "2.0", ID: req.ID, Result: &ToolCallResult{
						Content: []ToolCallContent{{Type: "text", Text: err.Error()}},
						IsError: true,
					}}
				}
				return &Response{JSONRPC: "2.0", ID: req.ID, Result: result}
			}
		}
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Result: &ToolCallResult{
				Content: []ToolCallContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", params.Name)}},
				IsError: true,
			},
		}

	default:
		return &Response{
			JSONRPC: "2.0", ID: req.ID,
			Error: &RPCError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		}
	}
}

func (s *Server) sendError(id any, code int, message string, data any) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &RPCError{Code: code, Message: message, Data: data},
	}
	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
}

// ResolvePath resolves a tool path argument relative to the project root.
// Accepts: absolute paths, paths relative to project root, and paths prefixed
// with the project root's base name (Zed convention: "myproject/src/foo.go").
// IsBannedPath reports whether p is a protected secret file.
func IsBannedPath(p string) bool {
	base := filepath.Base(p)
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasPrefix(base, ".env_") {
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
func CheckBanned(p string) error {
	if IsBannedPath(p) {
		return fmt.Errorf("access denied: %q is a protected secret file", filepath.Base(p))
	}
	return nil
}

func ResolvePath(p string) string {
	if p == "" || p == "." {
		return ProjectRoot
	}
	if filepath.IsAbs(p) {
		// Re-orient whenever path is outside current ProjectRoot (handles project switch).
		cleanRoot := filepath.Clean(ProjectRoot)
		if !strings.HasPrefix(filepath.Clean(p)+string(filepath.Separator), cleanRoot+string(filepath.Separator)) {
			if root := FindProjectRoot(filepath.Dir(p)); root != "/" {
				ProjectRoot = root
			}
		}
		return p
	}
	// Strip project-root basename prefix if present.
	// e.g. ProjectRoot=/Users/x/myproject, p=myproject/src/foo.go → /Users/x/myproject/src/foo.go
	if base := filepath.Base(ProjectRoot); base != "" && base != "." {
		if p == base {
			return ProjectRoot
		}
		if rest, ok := strings.CutPrefix(p, base+"/"); ok {
			return filepath.Join(ProjectRoot, rest)
		}
	}
	return filepath.Join(ProjectRoot, p)
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
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if rel, ok := strings.CutPrefix(resolved, root); ok {
			return rel
		}
	}
	return abs
}

// persistProjectRoot writes server.project_root into the project's memory store.
// Called async on initialize so the AI sees the resolved root on memory() recall.
func persistProjectRoot(root string) {
	type entry struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		At    string `json:"at"`
	}
	type store struct {
		Entries []entry `json:"entries"`
	}

	memPath := filepath.Join(root, ".llm", "memory.json")
	data, _ := os.ReadFile(memPath)
	var s store
	json.Unmarshal(data, &s)

	// Remove stale server.project_root entry
	filtered := s.Entries[:0]
	for _, e := range s.Entries {
		if e.Key != "server.project_root" {
			filtered = append(filtered, e)
		}
	}
	// Prepend so it's first on recall
	s.Entries = append([]entry{{
		Key:   "server.project_root",
		Value: root,
		At:    time.Now().UTC().Format(time.RFC3339),
	}}, filtered...)

	os.MkdirAll(filepath.Dir(memPath), 0755)
	out, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(memPath, out, 0644)
}

// FindProjectRoot walks up from dir looking for a project marker file.
func FindProjectRoot(dir string) string {
	markers := []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py", "Gemfile", "pom.xml", "build.gradle"}
	for {
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// autoDiscover scans the project root and persists facts the AI needs every session.
// Runs async on initialize so the AI doesn't have to re-discover them.
func autoDiscover(root string) {
	facts := make(map[string]string)

	// Language + version from go.mod
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		facts["server.lang"] = "go"
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "go "); ok {
				facts["server.go_version"] = strings.TrimSpace(v)
				break
			}
		}
	} else if _, err := os.Stat(filepath.Join(root, "package.json")); err == nil {
		facts["server.lang"] = "js/ts"
	} else if _, err := os.Stat(filepath.Join(root, "Cargo.toml")); err == nil {
		facts["server.lang"] = "rust"
	} else if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); err == nil {
		facts["server.lang"] = "python"
	}

	// Available tools
	for _, tool := range []string{"gofmt", "go", "prettier", "rustfmt", "ruff", "python3"} {
		if _, err := exec.LookPath(tool); err == nil {
			facts["server.tool."+tool] = "available"
		}
	}

	if len(facts) == 0 {
		return
	}

	// Write to memory store (same format as memory tool)
	type entry struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		At    string `json:"at"`
	}
	type store struct {
		Entries []entry `json:"entries"`
	}

	memPath := filepath.Join(root, ".llm", "memory.json")
	data, _ := os.ReadFile(memPath)
	var s store
	json.Unmarshal(data, &s)

	// Remove stale server.* entries before writing (dedup with persistProjectRoot)
	filtered := s.Entries[:0]
	for _, e := range s.Entries {
		if !strings.HasPrefix(e.Key, "server.") {
			filtered = append(filtered, e)
		}
	}
	s.Entries = filtered

	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range facts {
		s.Entries = append([]entry{{Key: k, Value: v, At: now}}, s.Entries...)
	}

	os.MkdirAll(filepath.Dir(memPath), 0755)
	out, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(memPath, out, 0644)
}
