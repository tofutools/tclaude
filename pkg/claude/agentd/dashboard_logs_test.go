package agentd

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLog writes the given lines (joined with newlines, trailing
// newline) to a fresh temp log file and returns its path.
func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}
	return path
}

// jsonLine builds one slog-style JSON log line.
func jsonLine(ts, level, msg string, kv ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"time":%q,"level":%q,"msg":%q`, ts, level, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, `,%q:%q`, kv[i], kv[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

func TestParseLogLine(t *testing.T) {
	// A well-formed JSON record splits into its slog fields with the
	// remaining attributes collected under Fields.
	e := parseLogLine(jsonLine("2026-07-01T12:00:00.000Z", "INFO", "hello", "conv", "abc", "n", "1"))
	if e.Level != "INFO" || e.Msg != "hello" || e.Time != "2026-07-01T12:00:00.000Z" {
		t.Fatalf("unexpected core fields: %+v", e)
	}
	if e.Fields["conv"] != "abc" || e.Fields["n"] != "1" {
		t.Fatalf("unexpected fields: %+v", e.Fields)
	}
	if e.Raw != "" {
		t.Fatalf("JSON line should not set Raw, got %q", e.Raw)
	}

	// A non-JSON line survives verbatim as a raw entry (nothing dropped).
	raw := parseLogLine("this is not json")
	if raw.Msg != "this is not json" || raw.Raw != "this is not json" || raw.Level != "" {
		t.Fatalf("raw line not preserved: %+v", raw)
	}
}

func TestBuildLogsResponse_NewestFirstAndCount(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "first"),
		jsonLine("2026-07-01T12:00:01.000Z", "INFO", "second"),
		jsonLine("2026-07-01T12:00:02.000Z", "INFO", "third"),
	)
	resp := buildLogsResponse(path, false, logFilter{}, "all", 1, 100)
	if resp.Total != 3 || resp.TotalUnfiltered != 3 {
		t.Fatalf("counts = %d/%d, want 3/3", resp.Total, resp.TotalUnfiltered)
	}
	if len(resp.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(resp.Entries))
	}
	// Newest first: the last-written line leads.
	if resp.Entries[0].Msg != "third" || resp.Entries[2].Msg != "first" {
		t.Fatalf("ordering wrong: %q … %q", resp.Entries[0].Msg, resp.Entries[2].Msg)
	}
}

func TestBuildLogsResponseKeysSurviveFullPageTailRollover(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same"),
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same"),
	)
	before := buildLogsResponse(path, false, logFilter{}, "all", 1, 2)
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same"),
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same"),
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same"),
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := buildLogsResponse(path, false, logFilter{}, "all", 1, 2)
	if before.Entries[0].Key != after.Entries[1].Key {
		t.Fatalf("existing newest duplicate key changed across full-page rollover: %q -> %q", before.Entries[0].Key, after.Entries[1].Key)
	}
	if after.Entries[0].Key == after.Entries[1].Key {
		t.Fatalf("duplicate rows share key %q", after.Entries[0].Key)
	}
}

func TestGatherLogRecordKeysSurviveTailCapBoundaryAdvance(t *testing.T) {
	line := jsonLine("2026-07-01T12:00:00.000Z", "INFO", "same")
	path := writeLog(t, line, line, line)
	// Start one byte before the oldest retained line. readLogTail drops that
	// preceding newline, leaving exactly the newest two complete records.
	maxBytes := int64(2*(len(line)+1) + 1)
	before, _, _ := gatherLogRecords(path, false, maxBytes)
	if err := os.WriteFile(path, []byte(strings.Repeat(line+"\n", 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _ := gatherLogRecords(path, false, maxBytes)
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("record counts = %d then %d, want 2 then 2", len(before), len(after))
	}
	if before[1].key != after[0].key {
		t.Fatalf("surviving record key changed as cap advanced: %q -> %q", before[1].key, after[0].key)
	}
}

func TestGatherLogRecordKeysSurviveRotationAndNewActiveFile(t *testing.T) {
	oldLine := jsonLine("2026-07-01T12:00:00.000Z", "INFO", "old physical file")
	path := writeLog(t, oldLine)
	before, _, _ := gatherLogRecords(path, false, maxLogReadBytes)
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	newLine := jsonLine("2026-07-01T12:00:01.000Z", "INFO", "replacement active file")
	if err := os.WriteFile(path, []byte(newLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _ := gatherLogRecords(path, true, maxLogReadBytes)
	if len(before) != 1 || len(after) != 2 {
		t.Fatalf("record counts = %d then %d, want 1 then 2", len(before), len(after))
	}
	if before[0].key != after[0].key {
		t.Fatalf("rotated physical file key changed: %q -> %q", before[0].key, after[0].key)
	}
	if after[0].key == after[1].key {
		t.Fatalf("replacement active file reused rotated key %q", after[0].key)
	}
	oldInfo, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	_, _, _ = gatherLogRecords(path, false, maxLogReadBytes)
	logFileIdentityRegistry.Lock()
	defer logFileIdentityRegistry.Unlock()
	for _, entry := range logFileIdentityRegistry.entries {
		if os.SameFile(oldInfo, entry.info) {
			t.Fatal("deleted rotated file identity remained retained after the next scan")
		}
	}
}

func TestGatherLogRecordsSkipsSamePhysicalFileAtTwoPaths(t *testing.T) {
	line := jsonLine("2026-07-01T12:00:00.000Z", "INFO", "one physical file")
	path := writeLog(t, line)
	if err := os.Link(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	records, sources, _ := gatherLogRecords(path, true, maxLogReadBytes)
	if len(records) != 1 || len(sources) != 1 {
		t.Fatalf("same physical file emitted records/sources twice: records=%d sources=%d", len(records), len(sources))
	}
}

func TestBuildLogsResponse_LevelMinFilter(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "DEBUG", "d"),
		jsonLine("2026-07-01T12:00:01.000Z", "INFO", "i"),
		jsonLine("2026-07-01T12:00:02.000Z", "WARN", "w"),
		jsonLine("2026-07-01T12:00:03.000Z", "ERROR", "e"),
		"a bare non-json line", // raw: kept regardless of level filter
	)
	resp := buildLogsResponse(path, false, logFilter{minLevel: slog.LevelWarn, hasLevel: true}, "warn", 1, 100)
	// warn + error + the raw line = 3
	if resp.Total != 3 {
		t.Fatalf("Total = %d, want 3 (warn, error, raw)", resp.Total)
	}
	got := map[string]bool{}
	for _, e := range resp.Entries {
		got[e.Msg] = true
	}
	if got["d"] || got["i"] {
		t.Fatalf("debug/info leaked past min-level warn: %+v", resp.Entries)
	}
	if !got["w"] || !got["e"] || !got["a bare non-json line"] {
		t.Fatalf("expected warn+error+raw kept: %+v", resp.Entries)
	}
	if resp.TotalUnfiltered != 5 {
		t.Fatalf("TotalUnfiltered = %d, want 5", resp.TotalUnfiltered)
	}
}

func TestBuildLogsResponse_HideRaw(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "structured"),
		"a bare non-json line",
		"time=2026-07-01T12:00:02Z level=INFO msg=legacy-text-format",
	)
	// Default: raw lines are kept.
	kept := buildLogsResponse(path, false, logFilter{}, "all", 1, 100)
	if kept.Total != 3 {
		t.Fatalf("default Total = %d, want 3 (raw kept)", kept.Total)
	}
	// hideRaw: only the structured JSON line survives.
	hidden := buildLogsResponse(path, false, logFilter{hideRaw: true}, "all", 1, 100)
	if hidden.Total != 1 || hidden.Entries[0].Msg != "structured" {
		t.Fatalf("hideRaw should leave only the JSON line, got %+v", msgs(hidden.Entries))
	}
	// TotalUnfiltered still counts every line read, filter or not.
	if hidden.TotalUnfiltered != 3 {
		t.Fatalf("TotalUnfiltered = %d, want 3", hidden.TotalUnfiltered)
	}
	// hideRaw + search still matches raw content that is NOT hidden... i.e.
	// hideRaw wins: a raw line is dropped even if it matches the search.
	both := buildLogsResponse(path, false, logFilter{hideRaw: true, search: "legacy-text"}, "all", 1, 100)
	if both.Total != 0 {
		t.Fatalf("hideRaw must drop raw lines even when they match search, got %+v", msgs(both.Entries))
	}
}

func TestBuildLogsResponse_Search(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "spawning agent", "conv", "deadbeef"),
		jsonLine("2026-07-01T12:00:01.000Z", "INFO", "reaping session"),
	)
	// Match on a structured field value, not just the message.
	resp := buildLogsResponse(path, false, logFilter{search: "deadbeef"}, "all", 1, 100)
	if resp.Total != 1 || resp.Entries[0].Msg != "spawning agent" {
		t.Fatalf("search on field failed: %+v", resp)
	}
	// Match on the message text.
	resp = buildLogsResponse(path, false, logFilter{search: "reaping"}, "all", 1, 100)
	if resp.Total != 1 || resp.Entries[0].Msg != "reaping session" {
		t.Fatalf("search on msg failed: %+v", resp)
	}
}

func TestBuildLogsResponse_TimeRange(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "old"),
		jsonLine("2026-07-01T13:00:00.000Z", "INFO", "mid"),
		jsonLine("2026-07-01T14:00:00.000Z", "INFO", "new"),
	)
	from, _ := time.Parse(time.RFC3339, "2026-07-01T12:30:00Z")
	resp := buildLogsResponse(path, false, logFilter{from: from, hasFrom: true}, "all", 1, 100)
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2 (mid, new)", resp.Total)
	}
	for _, e := range resp.Entries {
		if e.Msg == "old" {
			t.Fatalf("entry before `from` leaked: %+v", resp.Entries)
		}
	}
}

func TestBuildLogsResponse_Pagination(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, jsonLine(
			fmt.Sprintf("2026-07-01T12:00:%02d.000Z", i), "INFO", fmt.Sprintf("m%d", i)))
	}
	path := writeLog(t, lines...)
	// Page 1 of size 3 → newest three: m9, m8, m7.
	p1 := buildLogsResponse(path, false, logFilter{}, "all", 1, 3)
	if len(p1.Entries) != 3 || p1.Entries[0].Msg != "m9" || p1.Entries[2].Msg != "m7" {
		t.Fatalf("page 1 wrong: %+v", msgs(p1.Entries))
	}
	// Page 2 → m6, m5, m4.
	p2 := buildLogsResponse(path, false, logFilter{}, "all", 2, 3)
	if len(p2.Entries) != 3 || p2.Entries[0].Msg != "m6" || p2.Entries[2].Msg != "m4" {
		t.Fatalf("page 2 wrong: %+v", msgs(p2.Entries))
	}
	// A page past the end is clamped to the last page (not empty).
	pLast := buildLogsResponse(path, false, logFilter{}, "all", 99, 3)
	if pLast.Page != 4 || len(pLast.Entries) != 1 || pLast.Entries[0].Msg != "m0" {
		t.Fatalf("clamped last page wrong: page=%d %+v", pLast.Page, msgs(pLast.Entries))
	}
}

func msgs(es []logEntryView) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Msg
	}
	return out
}

func TestGatherLogLines_TailCapDropsPartialLine(t *testing.T) {
	path := writeLog(t,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "first-line-is-long-enough-to-be-cut"),
		jsonLine("2026-07-01T12:00:01.000Z", "INFO", "second"),
		jsonLine("2026-07-01T12:00:02.000Z", "INFO", "third"),
	)
	// Cap the read 10 bytes short of the whole file so the window opens
	// mid-first-line (line 1 is far longer than 10 bytes). The partial
	// leading line must be dropped and truncated set, while the later
	// complete lines survive.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	lines, _, truncated := gatherLogLines(path, false, info.Size()-10)
	if !truncated {
		t.Fatal("expected truncated=true when the byte cap cuts the file")
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "first-line-is-long-enough-to-be-cut") {
		t.Fatalf("partial leading line should have been dropped, got: %v", lines)
	}
	if !strings.Contains(joined, "third") {
		t.Fatalf("newest line should survive the tail read, got: %v", lines)
	}
}

func TestGatherLogLines_IncludeRotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	// Chronological: .2 (oldest) → .1 → active (newest).
	mustWrite(t, path+".2", jsonLine("2026-07-01T10:00:00.000Z", "INFO", "oldest")+"\n")
	mustWrite(t, path+".1", jsonLine("2026-07-01T11:00:00.000Z", "INFO", "older")+"\n")
	mustWrite(t, path, jsonLine("2026-07-01T12:00:00.000Z", "INFO", "newest")+"\n")

	// Active only: just the newest line, and one source (the active log).
	only, onlySrc, _ := gatherLogLines(path, false, maxLogReadBytes)
	if len(only) != 1 || !strings.Contains(only[0], "newest") {
		t.Fatalf("active-only should read 1 line, got %v", only)
	}
	if len(onlySrc) != 1 || onlySrc[0].Rotated || onlySrc[0].Lines != 1 {
		t.Fatalf("active-only sources wrong: %+v", onlySrc)
	}

	// With rotated: chronological oldest → newest.
	all, allSrc, _ := gatherLogLines(path, true, maxLogReadBytes)
	if len(all) != 3 {
		t.Fatalf("include_rotated should read 3 lines, got %d: %v", len(all), all)
	}
	if !strings.Contains(all[0], "oldest") || !strings.Contains(all[2], "newest") {
		t.Fatalf("rotated files not stitched chronologically: %v", all)
	}
	// Sources are reported active-first, then rotated siblings; each read
	// exactly one line here.
	if len(allSrc) != 3 {
		t.Fatalf("include_rotated should report 3 sources, got %+v", allSrc)
	}
	if allSrc[0].Rotated || allSrc[0].Name != "output.log" {
		t.Fatalf("first source should be the active log, got %+v", allSrc[0])
	}
	if !allSrc[1].Rotated || allSrc[1].Name != "output.log.1" ||
		!allSrc[2].Rotated || allSrc[2].Name != "output.log.2" {
		t.Fatalf("rotated sources out of order: %+v", allSrc)
	}
	for _, s := range allSrc {
		if s.Lines != 1 {
			t.Fatalf("each source should read 1 line, got %+v", s)
		}
	}
}

func TestGatherLogLines_TruncatingReadStopsRotatedWalk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	mustWrite(t, path,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "active-old-line-long-enough-to-be-cut")+"\n"+
			jsonLine("2026-07-01T12:00:01.000Z", "INFO", "active-newest")+"\n")
	mustWrite(t, path+".1", jsonLine("2026-07-01T11:00:00.000Z", "INFO", "rotated-should-not-appear")+"\n")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Cap 10 bytes short of the active file so its read truncates. Because
	// the cap is reached, the walk must NOT descend into the rotated
	// sibling and read a mid-record sliver off it (LOW-1 regression).
	lines, sources, truncated := gatherLogLines(path, true, info.Size()-10)
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	// The truncating active read must stop the walk before the rotated
	// sibling, so only the active log is reported as a source.
	if len(sources) != 1 || sources[0].Rotated {
		t.Fatalf("truncating read should report only the active source, got %+v", sources)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "rotated-should-not-appear") {
		t.Fatalf("rotated sliver leaked after a truncating active read: %v", lines)
	}
	if !strings.Contains(joined, "active-newest") {
		t.Fatalf("newest active line should survive: %v", lines)
	}
}

func TestBuildLogsResponse_SourcesAndRotatedAvailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	mustWrite(t, path+".2", jsonLine("2026-07-01T10:00:00.000Z", "INFO", "oldest")+"\n")
	mustWrite(t, path+".1", jsonLine("2026-07-01T11:00:00.000Z", "INFO", "older")+"\n")
	mustWrite(t, path,
		jsonLine("2026-07-01T12:00:00.000Z", "INFO", "newA")+"\n"+
			jsonLine("2026-07-01T12:00:01.000Z", "INFO", "newB")+"\n")

	// Rotated OFF: only the active log is a source, but the response still
	// advertises that two rotated siblings exist so the tab can offer them.
	off := buildLogsResponse(path, false, logFilter{}, "all", 1, 100)
	if off.IncludeRotated {
		t.Fatalf("IncludeRotated should be false, got %+v", off)
	}
	if off.RotatedAvailable != 2 {
		t.Fatalf("RotatedAvailable = %d, want 2", off.RotatedAvailable)
	}
	if len(off.Sources) != 1 || off.Sources[0].Rotated || off.Sources[0].Name != "output.log" {
		t.Fatalf("rotated-off sources wrong: %+v", off.Sources)
	}
	if off.Sources[0].Lines != 2 {
		t.Fatalf("active source should report 2 lines, got %+v", off.Sources[0])
	}

	// Rotated ON: all three files are reported, active first.
	on := buildLogsResponse(path, true, logFilter{}, "all", 1, 100)
	if !on.IncludeRotated || on.RotatedAvailable != 2 {
		t.Fatalf("rotated-on flags wrong: include=%v avail=%d", on.IncludeRotated, on.RotatedAvailable)
	}
	if len(on.Sources) != 3 {
		t.Fatalf("rotated-on should report 3 sources, got %+v", on.Sources)
	}
	names := []string{on.Sources[0].Name, on.Sources[1].Name, on.Sources[2].Name}
	want := []string{"output.log", "output.log.1", "output.log.2"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("source %d = %q, want %q (%+v)", i, names[i], want[i], on.Sources)
		}
	}
	if on.Sources[0].Rotated || !on.Sources[1].Rotated || !on.Sources[2].Rotated {
		t.Fatalf("rotated flags wrong across sources: %+v", on.Sources)
	}
}

func TestCountRotatedLogFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	mustWrite(t, path, "active\n")

	if n := countRotatedLogFiles(path); n != 0 {
		t.Fatalf("no rotated siblings should count 0, got %d", n)
	}
	mustWrite(t, path+".1", "one\n")
	mustWrite(t, path+".2", "two\n")
	if n := countRotatedLogFiles(path); n != 2 {
		t.Fatalf("two contiguous siblings should count 2, got %d", n)
	}
	// Slots are contiguous: a gap stops the count (a stray .5 is ignored).
	mustWrite(t, path+".5", "five\n")
	if n := countRotatedLogFiles(path); n != 2 {
		t.Fatalf("a non-contiguous sibling must not be counted, got %d", n)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseLogTimeParam(t *testing.T) {
	// Unix millis (what the "since" preset sends).
	if tm, ok := parseLogTimeParam("1751371200000"); !ok || tm.UnixMilli() != 1751371200000 {
		t.Fatalf("millis parse failed: %v %v", tm, ok)
	}
	// RFC3339 must NOT be mis-read as an integer off its leading digits.
	tm, ok := parseLogTimeParam("2026-07-01T12:00:00Z")
	if !ok || tm.Year() != 2026 || tm.Month() != time.July {
		t.Fatalf("RFC3339 parse failed: %v %v", tm, ok)
	}
	if _, ok := parseLogTimeParam(""); ok {
		t.Fatal("empty should not parse")
	}
	if _, ok := parseLogTimeParam("garbage"); ok {
		t.Fatal("garbage should not parse")
	}
}
