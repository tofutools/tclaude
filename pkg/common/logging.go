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

// SetupLogging configures slog to write to ~/.tclaude/output.log (file only, not stderr).
func SetupLogging(level slog.Level) {
	handler := logFileHandler(level)
	if handler == nil {
		return
	}
	slog.SetDefault(slog.New(handler))
}

// SetupLoggingWithStderr configures slog to write to both ~/.tclaude/output.log and stderr.
// Stderr output uses \r\n line endings for compatibility with raw terminal mode.
func SetupLoggingWithStderr(level slog.Level) {
	stderrHandler := slog.NewTextHandler(crlfWriter{w: os.Stderr}, &slog.HandlerOptions{Level: level})
	fileHandler := logFileHandler(level)
	if fileHandler == nil {
		slog.SetDefault(slog.New(stderrHandler))
		return
	}
	slog.SetDefault(slog.New(multiHandler{handlers: []slog.Handler{fileHandler, stderrHandler}}))
}

func logFileHandler(level slog.Level) slog.Handler {
	logPath := OutputLogPath()
	if logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil
	}
	return slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: level})
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
