package common

import (
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
	logPath := OutputLogPath()
	if logPath == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}

	handler := slog.NewTextHandler(logFile, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))
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
