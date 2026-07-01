package common

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// OutputLogPath returns the path to the general output log file
func OutputLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "output.log")
}

// SetupLogging configures slog to write to ~/.tclaude/output.log (file
// only, not stderr).
//
// The file is written as JSON lines (one slog.JSONHandler record per
// line) so the dashboard's Logs tab — and any other tooling — can parse
// it structurally (level, time, msg, attrs) instead of scraping text.
// slog's JSON time is fixed-width RFC3339-with-millis. A `grep msg`
// against the file still works: the message text is a plain JSON string
// value on each line.
func SetupLogging(level slog.Level) {
	rw := fileWriter()
	if rw == nil {
		// File logging unavailable (e.g. no home directory). Leave any
		// existing slog handler in place rather than blanking it.
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(rw, &slog.HandlerOptions{Level: level})))
}

// SetupLoggingWithStderr configures slog to write to both
// ~/.tclaude/output.log and stderr. The file gets JSON lines (see
// SetupLogging — machine-parseable for the Logs tab); stderr keeps the
// human-readable text format, since a person is reading the terminal.
// Stderr output uses \r\n line endings for compatibility with raw
// terminal mode.
func SetupLoggingWithStderr(level slog.Level) {
	stderrHandler := slog.NewTextHandler(crlfWriter{w: os.Stderr}, &slog.HandlerOptions{Level: level})
	rw := fileWriter()
	if rw == nil {
		slog.SetDefault(slog.New(stderrHandler))
		return
	}
	fileHandler := slog.NewJSONHandler(rw, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(multiHandler{handlers: []slog.Handler{fileHandler, stderrHandler}}))
}

// activeLogRotator is the one RotatingWriter behind tclaude's log file —
// opened lazily by the first fileWriter call and reused by every later
// one. agentd fetches it via ActiveLogRotator to drive size-based
// rotation of ~/.tclaude/output.log.
//
// SetupLogging runs more than once per process (main.go, then cobra's
// PersistentPreRun with the configured level). Reusing one writer keeps
// a single log fd for the process lifetime — not one leaked per call —
// and keeps this var pointing at that single live writer, never a
// stale one (it is set once, nil→writer, and never overwritten).
//
// It is written only during logging setup at process startup, on the
// main goroutine before any concurrency starts, so no lock is needed.
var activeLogRotator *RotatingWriter

// ActiveLogRotator returns the RotatingWriter behind tclaude's log file,
// or nil if file logging could not be set up (e.g. no home directory).
// agentd uses it to size-rotate ~/.tclaude/output.log.
func ActiveLogRotator() *RotatingWriter {
	return activeLogRotator
}

// fileWriter returns the RotatingWriter to log through, opening it on
// the first call and reusing it on every later one. It returns nil when
// the log path cannot be resolved or the file cannot be opened — in
// which case activeLogRotator is left untouched (nil on a first-call
// failure; an already-open writer is never discarded).
func fileWriter() *RotatingWriter {
	if activeLogRotator != nil {
		return activeLogRotator
	}
	logPath := OutputLogPath()
	if logPath == "" {
		return nil
	}
	rw, err := OpenRotatingWriter(logPath)
	if err != nil {
		return nil
	}
	activeLogRotator = rw
	return rw
}

// multiHandler fans out log records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return multiHandler{handlers: handlers}
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return multiHandler{handlers: handlers}
}

// crlfWriter wraps a writer and replaces \n with \r\n for raw terminal compatibility.
type crlfWriter struct {
	w io.Writer
}

func (c crlfWriter) Write(p []byte) (int, error) {
	replaced := bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))
	_, err := c.w.Write(replaced)
	return len(p), err
}

// ParseLogLevel parses a string log level into a slog.Level.
func ParseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
