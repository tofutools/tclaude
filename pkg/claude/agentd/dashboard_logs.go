package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// dashboard_logs.go serves the dashboard's Logs tab — a read-only viewer
// over tclaude's own log file (~/.tclaude/output.log), now written as
// JSON lines (see common.SetupLogging). Like the Audit and Messages
// tabs, filtering and pagination happen server-side so the tab stays
// responsive; the client only ever holds one page. Fetched on tab
// activation, the ⟳ refresh button, and — when the operator opts in —
// a slow tail-follow poll ("stream", default off). Never on the 2s
// snapshot tick.
//
// The log file is bounded by size-based rotation (default 10 MiB — see
// config.ResolvedLogRotation), so parsing it per request is cheap. As a
// backstop against a rotation-disabled (unbounded) log, only the newest
// maxLogReadBytes are ever read+parsed; older lines are dropped and the
// response's `truncated` flag says so.

const (
	// defaultLogPageSize is what the dashboard requests when the operator
	// hasn't picked one; maxLogPageSize caps a hand-crafted query so it
	// can't ask the daemon to materialise an unbounded page.
	defaultLogPageSize = 100
	maxLogPageSize     = 1000

	// maxLogReadBytes caps how much of the log tail agentd parses per
	// request. The default rotation cap is 10 MiB; this leaves generous
	// headroom while keeping the work bounded even if rotation is off.
	maxLogReadBytes int64 = 64 * common.MB

	// maxLogRotatedFiles bounds how many rotated siblings (output.log.1,
	// .2, …) the "rotated" toggle scans, so a stray pile of old files
	// can't unbound the walk. Comfortably above any realistic keep count.
	maxLogRotatedFiles = 50
)

// logEntryView is the JSON shape one log line takes on the wire. A line
// that parses as a JSON object is split into its slog fields (time /
// level / msg) with any remaining attributes collected under Fields; a
// line that is not valid JSON (e.g. a pre-cutover text-format record in
// a rotated file, or a stray stdout write) is returned verbatim in Raw
// with Msg mirroring it, so nothing is ever silently dropped.
type logEntryView struct {
	Time   string         `json:"time,omitempty"`
	Level  string         `json:"level,omitempty"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
	Raw    string         `json:"raw,omitempty"`
}

// logsResponse is the Logs tab payload: one newest-first page of entries
// plus the pager state and a couple of status hints (the source path,
// and whether the byte cap dropped older lines).
type logsResponse struct {
	Entries         []logEntryView `json:"entries"`
	Page            int            `json:"page"`
	PageSize        int            `json:"page_size"`
	Total           int            `json:"total"`            // rows matching the filters
	TotalUnfiltered int            `json:"total_unfiltered"` // all rows read
	Level           string         `json:"level"`            // normalized min-level echo ("all" when unset)
	Truncated       bool           `json:"truncated"`        // the byte cap dropped older lines
	Path            string         `json:"path"`             // the active log file
}

// logFilter is the parsed, validated query for one logs request.
type logFilter struct {
	minLevel slog.Level
	hasLevel bool
	search   string // already lowercased
	from, to time.Time
	hasFrom  bool
	hasTo    bool
	hideRaw  bool // drop non-JSON (raw) lines entirely
}

// match reports whether an entry passes the filter.
//
// Raw (non-JSON) lines — legacy pre-cutover text records, panics, stray
// stdout writes — are KEPT by default and pass the level/time filters
// (they have no parseable level or timestamp to compare against), so a
// crash dump is never silently hidden. The operator can opt into dropping
// them with hideRaw when the transition noise gets in the way.
func (f logFilter) match(e logEntryView) bool {
	if e.Raw != "" {
		return !f.hideRaw && (f.search == "" || strings.Contains(logEntryHaystack(e), f.search))
	}
	if f.hasLevel && e.Level != "" {
		if common.ParseLogLevel(e.Level) < f.minLevel {
			return false
		}
	}
	if f.hasFrom || f.hasTo {
		if t, ok := parseLogEntryTime(e.Time); ok {
			if f.hasFrom && t.Before(f.from) {
				return false
			}
			if f.hasTo && t.After(f.to) {
				return false
			}
		}
	}
	if f.search != "" && !strings.Contains(logEntryHaystack(e), f.search) {
		return false
	}
	return true
}

// logEntryHaystack builds the lowercased text a search matches against:
// the message, level, timestamp, any structured fields, and the raw line
// for non-JSON entries.
func logEntryHaystack(e logEntryView) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(e.Msg))
	if e.Level != "" {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(e.Level))
	}
	if e.Time != "" {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(e.Time))
	}
	if len(e.Fields) > 0 {
		if fb, err := json.Marshal(e.Fields); err == nil {
			b.WriteByte(' ')
			b.Write(bytes.ToLower(fb))
		}
	}
	if e.Raw != "" {
		b.WriteByte(' ')
		b.WriteString(strings.ToLower(e.Raw))
	}
	return b.String()
}

// parseLogEntryTime parses a log line's timestamp. slog's JSONHandler
// writes RFC3339 with milliseconds; RFC3339 (which accepts an optional
// fractional second) handles it, with RFC3339Nano as a lenient fallback.
func parseLogEntryTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// parseLogLine turns one raw line into a logEntryView. A JSON object is
// unpacked into its slog fields; anything else is returned as a raw
// entry (Msg mirrors Raw) so unparseable content stays visible.
func parseLogLine(line string) logEntryView {
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil || m == nil {
		return logEntryView{Msg: line, Raw: line}
	}
	var e logEntryView
	if v, ok := m["time"].(string); ok {
		e.Time = v
		delete(m, "time")
	}
	if v, ok := m["level"].(string); ok {
		e.Level = v
		delete(m, "level")
	}
	if v, ok := m["msg"].(string); ok {
		e.Msg = v
		delete(m, "msg")
	}
	if len(m) > 0 {
		e.Fields = m
	}
	return e
}

// readLogTail reads up to maxBytes from the END of the file at path. When
// the file is larger, it seeks to the tail and drops the leading partial
// line (the seek lands mid-record), returning truncated=true.
func readLogTail(path string, maxBytes int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size <= maxBytes {
		b, err := io.ReadAll(f)
		return b, false, err
	}
	if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
		return nil, false, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	return b, true, nil
}

// gatherLogLines returns the log lines in chronological order (oldest
// first), reading only the newest maxBytes total. The active log is read
// first; when includeRotated is set, its rotated siblings (.1, .2, …)
// are also read, newest to oldest, until the byte budget is exhausted.
func gatherLogLines(path string, includeRotated bool, maxBytes int64) (lines []string, truncated bool) {
	// Newest-first file order: the active log, then each rotated sibling.
	files := []string{path}
	if includeRotated {
		for n := 1; n <= maxLogRotatedFiles; n++ {
			rp := fmt.Sprintf("%s.%d", path, n)
			if _, err := os.Stat(rp); err != nil {
				break // rotated slots are contiguous; stop at the first gap
			}
			files = append(files, rp)
		}
	}

	budget := maxBytes
	for _, f := range files {
		if budget <= 0 {
			truncated = true
			break
		}
		data, tr, err := readLogTail(f, budget)
		if err != nil {
			continue // missing / unreadable file — skip it
		}
		if tr {
			truncated = true
		}
		budget -= int64(len(data))
		fileLines := splitNonEmptyLines(data)
		// We iterate newest → oldest, so each file we visit is older than
		// everything gathered so far: prepend to stay chronological.
		lines = append(fileLines, lines...)
		// A truncating read means the byte cap is reached — the dropped
		// leading partial line leaves budget just above zero, so stop here
		// rather than reading tiny mid-record slivers off older siblings.
		if tr {
			break
		}
	}
	return lines, truncated
}

// splitNonEmptyLines splits on '\n' and drops blank lines.
func splitNonEmptyLines(data []byte) []string {
	raw := strings.Split(string(data), "\n")
	out := make([]string, 0, len(raw))
	for _, ln := range raw {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// buildLogsResponse is the tab's core: read the log, parse + filter every
// line, then return the requested newest-first page. Split out from the
// HTTP handler so it is unit-testable against a temp log file without a
// home dir or a live server.
func buildLogsResponse(path string, includeRotated bool, filter logFilter, normLevel string, page, pageSize int) logsResponse {
	lines, truncated := gatherLogLines(path, includeRotated, maxLogReadBytes)
	totalUnfiltered := len(lines)

	filtered := make([]logEntryView, 0, len(lines))
	for _, ln := range lines {
		e := parseLogLine(ln)
		if filter.match(e) {
			filtered = append(filtered, e)
		}
	}
	total := len(filtered)

	// Newest first for display. The lines came in chronological (file)
	// order — reverse rather than string-sorting timestamps, which is not
	// a reliable order.
	slices.Reverse(filtered)

	servedPage, offset := clampOffset(page, pageSize, total)
	end := min(offset+pageSize, total)
	entries := []logEntryView{}
	if offset < total {
		entries = filtered[offset:end]
	}

	return logsResponse{
		Entries:         entries,
		Page:            servedPage,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: totalUnfiltered,
		Level:           normLevel,
		Truncated:       truncated,
		Path:            path,
	}
}

// parseLogTimeParam accepts a time bound as either unix milliseconds
// (what the dashboard's "since" preset sends) or an RFC3339 string (so a
// hand-crafted query / future date picker works too).
func parseLogTimeParam(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// Pure integer → unix millis (the "since" preset). strconv.ParseInt
	// requires the whole string be numeric, so an RFC3339 stamp (which
	// starts with the year's digits) falls through to the parsers below
	// instead of being mis-read as a millis value.
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func handleDashboardLogs(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	filter := logFilter{search: strings.ToLower(strings.TrimSpace(q.Get("q")))}

	// Level is a MINIMUM severity: "warn" shows warn + error. "all" (or
	// blank / unrecognised) applies no level filter.
	normLevel := "all"
	switch strings.ToLower(strings.TrimSpace(q.Get("level"))) {
	case "debug":
		filter.minLevel, filter.hasLevel, normLevel = slog.LevelDebug, true, "debug"
	case "info":
		filter.minLevel, filter.hasLevel, normLevel = slog.LevelInfo, true, "info"
	case "warn", "warning":
		filter.minLevel, filter.hasLevel, normLevel = slog.LevelWarn, true, "warn"
	case "error":
		filter.minLevel, filter.hasLevel, normLevel = slog.LevelError, true, "error"
	}

	if t, ok := parseLogTimeParam(q.Get("from")); ok {
		filter.from, filter.hasFrom = t, true
	}
	if t, ok := parseLogTimeParam(q.Get("to")); ok {
		filter.to, filter.hasTo = t, true
	}

	includeRotated := q.Get("include_rotated") == "1" || q.Get("include_rotated") == "true"
	filter.hideRaw = q.Get("hide_raw") == "1" || q.Get("hide_raw") == "true"

	page := max(atoiOr(q.Get("page"), 1), 1)
	pageSize := atoiOr(q.Get("page_size"), defaultLogPageSize)
	if pageSize < 1 {
		pageSize = defaultLogPageSize
	}
	pageSize = min(pageSize, maxLogPageSize)

	path := common.OutputLogPath()
	writeJSON(w, http.StatusOK, buildLogsResponse(path, includeRotated, filter, normLevel, page, pageSize))
}
