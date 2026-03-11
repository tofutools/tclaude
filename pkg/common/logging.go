package common

import (
	"log/slog"
	"os"
	"path/filepath"
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
// Hook callbacks override this with their own SetupHookLogging which writes to hooks.log.
func SetupLogging() {
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
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))
}
