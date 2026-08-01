package toolstats

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hedtahr/grepfunc/server"
)

var Tool = server.Tool{
	Name:        "tool_stats",
	Description: "Show usage telemetry across ALL projects: how often each tool was called (call count, error rate, avg duration) and which registered tools were never called. Read-only audit of model tool adoption — the feedback loop for deciding which tools to keep, merge, or drop. Recorded globally to the user cache dir (grepfunc/toolstats.log). Pass path to filter to one project.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path":    {Type: "string", Description: "Project directory to filter telemetry to. Optional — omit for a global view across all projects."},
			"compact": {Type: "boolean", Description: "Terse output: one line per tool, no header. Default false."},
		},
		Required: []string{},
	},
}

type args struct {
	Path    string `json:"path"`
	Compact bool   `json:"compact"`
}

type toolAgg struct {
	name     string
	calls    int
	errs     int
	totalDur time.Duration
	first    time.Time
	last     time.Time
}

func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	var filter string
	if a.Path != "" {
		filter = server.ResolvePath(a.Path)
	} else if server.ProjectRoot != "" && server.ProjectRoot != "." {
		filter = server.ProjectRoot
	}
	path := server.StatsLogPath()

	if _, err := os.Stat(path); err != nil {
		return &server.ToolCallResult{
			Content: []server.ToolCallContent{{Type: "text", Text: fmt.Sprintf("No telemetry yet. Tool calls are recorded to %s automatically.", path)}},
		}, nil
	}

	aggs, registered, projects, err := parseLog(path, filter)
	if err != nil {
		return nil, err
	}

	var buf strings.Builder
	totalCalls, totalErrs := 0, 0
	var first, last time.Time
	sorted := make([]*toolAgg, 0, len(aggs))
	for _, ag := range aggs {
		totalCalls += ag.calls
		totalErrs += ag.errs
		if first.IsZero() || ag.first.Before(first) {
			first = ag.first
		}
		if last.IsZero() || ag.last.After(last) {
			last = ag.last
		}
		sorted = append(sorted, ag)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].calls != sorted[j].calls {
			return sorted[i].calls > sorted[j].calls
		}
		return sorted[i].name < sorted[j].name
	})

	if filter != "" {
		fmt.Fprintf(&buf, "Project: %s\n", filter)
	} else if len(projects) > 0 {
		fmt.Fprintf(&buf, "Projects: %d\n", len(projects))
	}

	if a.Compact {
		for _, ag := range sorted {
			fmt.Fprintf(&buf, "%s: %d calls, %d err, %dms avg\n", ag.name, ag.calls, ag.errs, avgMs(ag))
		}
	} else {
		errRate := 0.0
		if totalCalls > 0 {
			errRate = 100 * float64(totalErrs) / float64(totalCalls)
		}
		fmt.Fprintf(&buf, "Tool usage — %d calls, %d errors (%.1f%%)\n", totalCalls, totalErrs, errRate)
		if !first.IsZero() {
			fmt.Fprintf(&buf, "Range: %s → %s\n", first.Format(time.RFC3339), last.Format(time.RFC3339))
		}
		fmt.Fprintf(&buf, "\n%-20s %6s %6s %6s %9s\n", "tool", "calls", "errors", "err%", "avg(ms)")
		for _, ag := range sorted {
			e := 0.0
			if ag.calls > 0 {
				e = 100 * float64(ag.errs) / float64(ag.calls)
			}
			fmt.Fprintf(&buf, "%-20s %6d %6d %5.1f%% %9d\n", ag.name, ag.calls, ag.errs, e, avgMs(ag))
		}
	}

	var uncalled []string
	for _, r := range registered {
		if _, ok := aggs[r]; !ok {
			uncalled = append(uncalled, r)
		}
	}
	sort.Strings(uncalled)
	if len(uncalled) > 0 {
		if a.Compact {
			fmt.Fprintf(&buf, "\nnever: %s\n", strings.Join(uncalled, ", "))
		} else {
			fmt.Fprintf(&buf, "\nNever called (%d): %s\n", len(uncalled), strings.Join(uncalled, ", "))
		}
	}

	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: "text", Text: buf.String()}},
	}, nil
}

func avgMs(a *toolAgg) int {
	if a.calls == 0 {
		return 0
	}
	return int(a.totalDur.Milliseconds() / int64(a.calls))
}

func parseLog(path, filter string) (map[string]*toolAgg, []string, map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, err
	}
	defer f.Close()

	aggs := map[string]*toolAgg{}
	projects := map[string]int{}
	var registered []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# registered: "); ok {
			registered = strings.Split(rest, ",")
			continue
		}
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 5 {
			continue
		}
		if !projMatch(parts[1], filter) {
			continue
		}
		ts, err := time.Parse(time.RFC3339, parts[0])
		if err != nil {
			continue
		}
		projects[parts[1]]++
		tool := parts[2]
		isErr := parts[3] == "err"
		durMs, _ := strconv.Atoi(parts[4])

		ag := aggs[tool]
		if ag == nil {
			ag = &toolAgg{name: tool, first: ts, last: ts}
			aggs[tool] = ag
		}
		ag.calls++
		if isErr {
			ag.errs++
		}
		ag.totalDur += time.Duration(durMs) * time.Millisecond
		if ts.Before(ag.first) {
			ag.first = ts
		}
		if ts.After(ag.last) {
			ag.last = ts
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, nil, err
	}
	return aggs, registered, projects, nil
}

func projMatch(proj, filter string) bool {
	if filter == "" {
		return true
	}
	if proj == filter {
		return true
	}
	return strings.HasPrefix(proj, filter+string(filepath.Separator))
}
