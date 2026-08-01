package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// toolStats records per-call usage telemetry to the user cache dir, global across all projects.
// Append-only, one line per tool call: ts \t project \t tool \t ok|err \t durMs \t argBytes.
type toolStats struct {
	mu      sync.Mutex
	written map[string]bool // stats-file paths already initialized with header
}

func newToolStats() *toolStats {
	return &toolStats{mu: sync.Mutex{}, written: map[string]bool{}}
}

func (ts *toolStats) record(tools []ToolEntry, tool string, isErr bool, dur time.Duration, argBytes int) {
	dir := statsDir()
	path := filepath.Join(dir, "toolstats.log")

	ts.mu.Lock()
	defer ts.mu.Unlock()

	err := os.MkdirAll(dir, dirPermPrivate)
	if err != nil {
		return
	}

	// #nosec G304 -- path is under the user cache dir
	logFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePermPrivate)
	if err != nil {
		return
	}

	defer func() { _ = logFile.Close() }()

	if !ts.written[path] {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Tool.Name)
		}

		sort.Strings(names)
		_, _ = fmt.Fprintf(logFile, "# toolstats v2\n# registered: %s\n", strings.Join(names, ","))

		ts.written[path] = true
	}

	status := "ok"
	if isErr {
		status = "err"
	}

	project := ProjectRoot
	if project == "" {
		project = "."
	}

	_, _ = fmt.Fprintf(logFile, "%s\t%s\t%s\t%s\t%d\t%d\n",
		time.Now().UTC().Format(time.RFC3339), project, tool, status, dur.Milliseconds(), argBytes)
}

func statsDir() string {
	dir, err := os.UserCacheDir()
	if err == nil {
		return filepath.Join(dir, "grepfunc")
	}

	return filepath.Join(os.TempDir(), "grepfunc-cache")
}

// StatsLogPath returns the global telemetry log path (user cache dir, shared by all projects).
func StatsLogPath() string {
	return filepath.Join(statsDir(), "toolstats.log")
}
