package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/filefollow"
	"github.com/tofutools/tclaude/pkg/common"
)

// dashboard_logs_export.go serves the Logs tab's "export" download. It
// applies the same filter as the paged view (search / level / since /
// rotated / hide raw) but is not bounded by the page size or by the view's
// maxLogReadBytes tail cap: it streams every matching line straight from
// disk, so the operator can pull a longer history than the tab can page
// through. Memory stays bounded by one record (maxLogRecordBytes) because
// nothing is materialised — each file is scanned and filtered line by line.
//
// The output is JSON lines, oldest first, with every matching line written
// verbatim as it appears on disk (raw non-JSON lines included unless hide
// raw is set), so the file is lossless and works with jq / grep.

// openLogExportFiles opens the active log and, when includeRotated is set,
// its contiguous rotated siblings. It returns them OLDEST first, each paired
// with the size observed at open time. Opening every handle before reading
// pins the set: a rotation rename mid-export doesn't change what an open
// descriptor reads, and reading each file only up to its open-time size
// keeps a growing active log from turning the export into a tail -f. If a
// rotation lands between two opens, the same physical file shows up at two
// paths; it is kept once.
func openLogExportFiles(path string, includeRotated bool) (files []*os.File, sizes []int64) {
	paths := []string{path}
	if includeRotated {
		for n := 1; n <= maxLogRotatedFiles; n++ {
			rp := fmt.Sprintf("%s.%d", path, n)
			if _, err := os.Stat(rp); err != nil {
				break // rotated slots are contiguous; stop at the first gap
			}
			paths = append(paths, rp)
		}
	}
	var infos []os.FileInfo
	for _, p := range paths {
		f, err := os.Open(p) //nolint:gosec // tclaude's configured log path and its rotated siblings
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		dup := false
		for _, seen := range infos {
			if os.SameFile(seen, info) {
				dup = true
				break
			}
		}
		if dup {
			_ = f.Close()
			continue
		}
		infos = append(infos, info)
		files = append(files, f)
		sizes = append(sizes, info.Size())
	}
	// Collected newest → oldest; the export is chronological.
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
		sizes[i], sizes[j] = sizes[j], sizes[i]
	}
	return files, sizes
}

// abortableReader fails the next Read once stop reports an error, so a
// non-strict ScanLines (which ignores its callback's return value) returns at
// the next buffer refill instead of scanning the rest of the log set for a
// client that has already gone away.
type abortableReader struct {
	r    io.Reader
	stop func() error
}

func (a abortableReader) Read(p []byte) (int, error) {
	if err := a.stop(); err != nil {
		return 0, err
	}
	return a.r.Read(p)
}

// writeLogExport streams every line of the log set that passes filter to w,
// oldest first, and returns how many lines it wrote. It stops early when ctx
// is done or a write fails.
func writeLogExport(ctx context.Context, w io.Writer, path string, includeRotated bool, filter logFilter) (int, error) {
	files, sizes := openLogExportFiles(path, includeRotated)
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	written := 0
	var writeErr error
	stop := func() error {
		if writeErr != nil {
			return writeErr
		}
		return ctx.Err()
	}
	for i, f := range files {
		if err := stop(); err != nil {
			return written, err
		}
		_, _, err := filefollow.ScanLines(abortableReader{r: io.LimitReader(f, sizes[i]), stop: stop}, filefollow.LineConfig{MaxRecordBytes: maxLogRecordBytes}, func(line filefollow.Line) bool {
			text := strings.TrimRight(string(line.Data), "\r\n")
			if strings.TrimSpace(text) == "" {
				return true
			}
			if line.Oversized {
				text += " … [oversized log record truncated]"
			}
			if !filter.match(parseLogLine(text)) {
				return true
			}
			if writeErr != nil {
				return false
			}
			if _, err := io.WriteString(w, text+"\n"); err != nil {
				writeErr = err
				return false
			}
			written++
			return true
		}, false)
		if writeErr != nil {
			return written, writeErr
		}
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func handleDashboardLogsExport(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	filter, _, includeRotated := parseLogsQuery(r.URL.Query())
	name := "tclaude-logs-" + time.Now().Format("20060102-150405") + ".jsonl"
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Headers are committed with the first byte, so a mid-stream failure
	// can only be logged; the client sees a short file.
	if _, err := writeLogExport(r.Context(), w, common.OutputLogPath(), includeRotated, filter); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("logs export aborted", "error", err)
	}
}
