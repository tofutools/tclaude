package agentd

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLogExport_FilterAcrossRotatedOldestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.log")
	mustWrite(t, path+".2", jsonLine("2026-07-01T10:00:00.000Z", "WARN", "oldest boom")+"\n"+
		jsonLine("2026-07-01T10:00:01.000Z", "INFO", "quiet boom")+"\n")
	mustWrite(t, path+".1", "legacy text boom line\n\n")
	mustWrite(t, path, jsonLine("2026-07-01T12:00:00.000Z", "ERROR", "newest boom", "k", "v")+"\n"+
		jsonLine("2026-07-01T12:00:01.000Z", "ERROR", "unrelated")+"\n")

	filter, _, rotated := parseLogsQuery(url.Values{"q": {"BOOM"}, "level": {"warn"}, "include_rotated": {"1"}})
	var b strings.Builder
	n, err := writeLogExport(&b, path, rotated, filter)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := jsonLine("2026-07-01T10:00:00.000Z", "WARN", "oldest boom") + "\n" +
		"legacy text boom line\n" +
		jsonLine("2026-07-01T12:00:00.000Z", "ERROR", "newest boom", "k", "v") + "\n"
	if b.String() != want || n != 3 {
		t.Fatalf("export = %d lines\n%s\nwant\n%s", n, b.String(), want)
	}

	// Without the rotated toggle only the active log is exported; hide raw
	// drops the legacy line.
	filter, _, rotated = parseLogsQuery(url.Values{"hide_raw": {"1"}})
	b.Reset()
	if n, _ := writeLogExport(&b, path, rotated, filter); n != 2 || strings.Contains(b.String(), "oldest") {
		t.Fatalf("active-only export = %d lines: %s", n, b.String())
	}
}

// The export is the escape hatch from the view's tail cap: it is not bounded
// by maxLogReadBytes, and it stops at the size the file had when opened.
func TestWriteLogExport_NotBoundByViewPage(t *testing.T) {
	var lines []string
	for range maxLogPageSize + 5 {
		lines = append(lines, jsonLine("2026-07-01T10:00:00.000Z", "INFO", "x"))
	}
	path := writeLog(t, lines...)
	var b strings.Builder
	n, err := writeLogExport(&b, path, false, logFilter{})
	if err != nil || n != maxLogPageSize+5 {
		t.Fatalf("export = %d lines, err %v; want %d", n, err, maxLogPageSize+5)
	}
}
