package session

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// HooksLogPath returns the path to the hooks log file
func HooksLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tofu", "hooks.log")
}

// SetupHookLogging configures slog to write to both stderr and ~/.tofu/hooks.log
// Should be called early in hook-callback execution
func SetupHookLogging() {
	logPath := HooksLogPath()
	if logPath == "" {
		return
	}

	// Ensure ~/.tofu directory exists
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return
	}

	// Open log file for appending
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}

	// Create multi-writer for both stderr and file
	multiWriter := io.MultiWriter(os.Stderr, logFile)

	// Set up slog with text handler writing to both destinations
	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
}
