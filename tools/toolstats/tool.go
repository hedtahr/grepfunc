// Package toolstats provides the tool_stats MCP tool: usage telemetry across all projects.
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

// Tool is the tool_stats MCP tool definition.
//
//nolint:gochecknoglobals // MCP tool definition
var Tool = server.Tool{
	Name:        "tool_stats",
	Description: "Usage telemetry: call counts, error rates, avg duration/output, never-called tools.",
	InputSchema: server.InputSchema{
		Type: "object",
		Properties: map[string]server.Property{
			"path": {
				Type:        "string",
				Description: "Filter to one project directory. Omit for global.",
				Items:       nil,
			},
			"compact": {
				Type:        "boolean",
				Description: "One line per tool.",
				Items:       nil,
			},
		},
		Required:             []string{},
		AdditionalProperties: false,
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
	totalOut int
	first    time.Time
	last     time.Time
}

const (
	typeText     = "text"
	percent      = 100
	minLogFields = 5
)

// Handle serves the tool_stats MCP tool.
func Handle(raw json.RawMessage) (*server.ToolCallResult, error) {
	var input args

	err := json.Unmarshal(raw, &input)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	var filter string
	if input.Path != "" {
		filter = server.ResolvePath(input.Path)
	} else if server.ProjectRoot != "" && server.ProjectRoot != "." {
		filter = server.ProjectRoot
	}

	path := server.StatsLogPath()

	_, statErr := os.Stat(path)
	if statErr == nil {
		return renderTelemetry(path, filter, input.Compact)
	}

	if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat stats log: %w", statErr)
	}

	return textResult(fmt.Sprintf("No telemetry yet. Tool calls are recorded to %s automatically.", path)), nil
}

// renderTelemetry builds the full or compact stats report for the log file.
func renderTelemetry(path, filter string, compact bool) (*server.ToolCallResult, error) {
	aggs, registered, projects, err := parseLog(path, filter)
	if err != nil {
		return nil, err
	}

	sorted := sortAggs(aggs)
	totalCalls, totalErrs, first, last := totals(sorted)

	var buf strings.Builder

	if filter != "" {
		fmt.Fprintf(&buf, "Project: %s\n", filter)
	} else if len(projects) > 0 {
		fmt.Fprintf(&buf, "Projects: %d\n", len(projects))
	}

	renderBody(&buf, sorted, totalCalls, totalErrs, first, last, compact)

	uncalled := uncalledTools(registered, aggs, compact)
	if uncalled != "" {
		buf.WriteString(uncalled)
	}

	return textResult(buf.String()), nil
}

func textResult(text string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.ToolCallContent{{Type: typeText, Text: text}},
		IsError: false,
	}
}

// sortAggs returns aggregations ordered by call count (desc), then name.
func sortAggs(aggs map[string]*toolAgg) []*toolAgg {
	sorted := make([]*toolAgg, 0, len(aggs))
	for _, agg := range aggs {
		sorted = append(sorted, agg)
	}

	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].calls != sorted[j].calls {
			return sorted[i].calls > sorted[j].calls
		}

		return sorted[i].name < sorted[j].name
	})

	return sorted
}

// totals sums call/error counts and the observed time range across aggregations.
func totals(sorted []*toolAgg) (int, int, time.Time, time.Time) {
	var (
		totalCalls int
		totalErrs  int
		first      time.Time
		last       time.Time
	)

	for _, agg := range sorted {
		totalCalls += agg.calls
		totalErrs += agg.errs

		if first.IsZero() || agg.first.Before(first) {
			first = agg.first
		}

		if last.IsZero() || agg.last.After(last) {
			last = agg.last
		}
	}

	return totalCalls, totalErrs, first, last
}

// renderBody writes the per-tool table in compact or full form.
func renderBody(
	buf *strings.Builder, sorted []*toolAgg, totalCalls, totalErrs int, first, last time.Time, compact bool,
) {
	if compact {
		for _, agg := range sorted {
			fmt.Fprintf(buf, "%s: %d calls, %d err, %dms avg, %dB avg out\n", agg.name, agg.calls, agg.errs, avgMs(agg), avgOut(agg))
		}

		return
	}

	errRate := 0.0
	if totalCalls > 0 {
		errRate = percent * float64(totalErrs) / float64(totalCalls)
	}

	fmt.Fprintf(buf, "Tool usage — %d calls, %d errors (%.1f%%)\n", totalCalls, totalErrs, errRate)

	if !first.IsZero() {
		fmt.Fprintf(buf, "Range: %s → %s\n", first.Format(time.RFC3339), last.Format(time.RFC3339))
	}

	fmt.Fprintf(buf, "\n%-20s %6s %6s %6s %9s %10s\n",
		"tool", "calls", "errors", "err%", "avg(ms)", "avgOut(B)")

	for _, agg := range sorted {
		rate := 0.0
		if agg.calls > 0 {
			rate = percent * float64(agg.errs) / float64(agg.calls)
		}

		fmt.Fprintf(buf, "%-20s %6d %6d %5.1f%% %9d %10d\n", agg.name, agg.calls, agg.errs, rate, avgMs(agg), avgOut(agg))
	}
}

// uncalledTools lists registered tools with no recorded calls, or "" when all were called.
func uncalledTools(registered []string, aggs map[string]*toolAgg, compact bool) string {
	var uncalled []string

	for _, reg := range registered {
		if _, ok := aggs[reg]; !ok {
			uncalled = append(uncalled, reg)
		}
	}

	sort.Strings(uncalled)

	if len(uncalled) == 0 {
		return ""
	}

	var buf strings.Builder

	if compact {
		fmt.Fprintf(&buf, "\nnever: %s\n", strings.Join(uncalled, ", "))
	} else {
		fmt.Fprintf(&buf, "\nNever called (%d): %s\n", len(uncalled), strings.Join(uncalled, ", "))
	}

	return buf.String()
}

func avgMs(agg *toolAgg) int {
	if agg.calls == 0 {
		return 0
	}

	return int(agg.totalDur.Milliseconds() / int64(agg.calls))
}

func avgOut(agg *toolAgg) int {
	if agg.calls == 0 {
		return 0
	}

	return agg.totalOut / agg.calls
}

// parseLog reads the telemetry log, filtering lines to the given project when set.
func parseLog(path, filter string) (map[string]*toolAgg, []string, map[string]int, error) {
	// #nosec G304 -- paths bounds-checked by server
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open stats log: %w", err)
	}

	defer func() { _ = file.Close() }()

	aggs := map[string]*toolAgg{}
	projects := map[string]int{}

	var registered []string

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "# registered: "); ok {
			registered = strings.Split(rest, ",")

			continue
		}

		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}

		tool, isErr, durMs, outBytes, when, project, ok := parseLogLine(line, filter)
		if !ok {
			continue
		}

		projects[project]++

		agg := aggs[tool]
		if agg == nil {
			agg = &toolAgg{name: tool, first: when, last: when, calls: 0, errs: 0, totalDur: 0, totalOut: 0}
			aggs[tool] = agg
		}

		agg.observe(when, isErr, durMs, outBytes)
	}

	scanErr := scanner.Err()
	if scanErr != nil {
		return nil, nil, nil, fmt.Errorf("read stats log: %w", scanErr)
	}

	return aggs, registered, projects, nil
}

// parseLogLine parses one data line, or ok=false when malformed or filtered out.
func parseLogLine(line, filter string) (string, bool, int, int, time.Time, string, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < minLogFields {
		return "", false, 0, 0, time.Time{}, "", false
	}

	if !projMatch(parts[1], filter) {
		return "", false, 0, 0, time.Time{}, "", false
	}

	when, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return "", false, 0, 0, time.Time{}, "", false
	}

	durMs, _ := strconv.Atoi(parts[4])

	outBytes := 0
	if len(parts) > 6 {
		outBytes, _ = strconv.Atoi(parts[6])
	}

	return parts[2], parts[3] == "err", durMs, outBytes, when, parts[1], true
}

// observe folds one log line into the aggregation.
func (agg *toolAgg) observe(when time.Time, isErr bool, durMs, outBytes int) {
	agg.calls++
	if isErr {
		agg.errs++
	}

	agg.totalDur += time.Duration(durMs) * time.Millisecond
	agg.totalOut += outBytes
	if when.Before(agg.first) {
		agg.first = when
	}

	if when.After(agg.last) {
		agg.last = when
	}
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
