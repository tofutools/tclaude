// Package notify provides OS notifications for session state transitions.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

// IsEnabled returns whether notifications are enabled.
func IsEnabled() bool {
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return cfg.Notifications != nil && cfg.Notifications.Enabled
}

// OnStateTransition is called when a session changes state.
// It checks cooldown via the database and sends a notification if appropriate.
// convTitle is optional - pass empty string if not available.
func OnStateTransition(sessionID, from, to, cwd, convTitle string) {
	cfg, err := config.Load()
	if err != nil || cfg.Notifications == nil || !cfg.Notifications.Enabled {
		return
	}

	if !cfg.Notifications.MatchesTransition(from, to) {
		return
	}

	// Check cooldown via database
	cooldown := time.Duration(cfg.Notifications.CooldownSeconds) * time.Second
	if lastNotify, found, err := db.GetNotifyTime(sessionID); err == nil && found {
		if time.Since(lastNotify) < cooldown {
			return
		}
	}

	Send(sessionID, formatStatus(to), cwd, convTitle)

	// Record notification time
	_ = db.SetNotifyTime(sessionID)
}

// formatStatus returns a human-readable status string.
func formatStatus(status string) string {
	switch status {
	case "working":
		return "Working"
	case "idle":
		return "Idle"
	case "main_agent_idle":
		return "Main agent idle, subagents running"
	case "awaiting_permission":
		return "Awaiting permission"
	case "awaiting_input":
		return "Awaiting input"
	case "exited":
		return "Exited"
	default:
		return status
	}
}

// Send actually sends the notification.
func Send(sessionID, status, cwd, convTitle string) {
	slog.Debug("sending notification",
		"sessionID", sessionID,
		"status", status,
		"cwd", cwd,
		"convTitle", convTitle,
	)

	// Build notification content
	projectName := filepath.Base(cwd)
	if projectName == "" || projectName == "." {
		projectName = "unknown"
	}

	title := fmt.Sprintf("Claude: %s", status)

	// Build body: ID | Project - conversation title
	var body string
	if convTitle != "" {
		body = fmt.Sprintf("%s | %s - %s", shortID(sessionID), projectName, convTitle)
	} else {
		body = fmt.Sprintf("%s | %s", shortID(sessionID), projectName)
	}

	var err error

	// Check for a custom notification command
	cfg, cfgErr := config.Load()
	if cfgErr == nil && cfg.Notifications != nil && len(cfg.Notifications.NotificationCommand) > 0 {
		err = runCustomCommand(cfg.Notifications.NotificationCommand, sessionID, title, body)
	} else {
		err = platformSend(sessionID, title, body)
	}

	if err != nil {
		// Final fallback to stderr
		fmt.Fprintf(os.Stderr, "[notify] %s: %s\n", title, body)
	}
}

// shortID returns a shortened session ID for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// runCustomCommand executes a custom notification command, passing notification
// data as JSON on stdin. The JSON structure is always:
//
//	{"title":"...","body":"...","sessionID":"..."}
//
// The command is specified as a slice (program + arguments); no placeholder
// substitution is performed on the arguments. The command must complete within
// 5 seconds; a warning is logged if it times out.
func runCustomCommand(cmdTemplate []string, sessionID, title, body string) error {
	if len(cmdTemplate) == 0 {
		return fmt.Errorf("empty notification command")
	}

	payload := map[string]string{
		"title":     title,
		"body":      body,
		"sessionID": sessionID,
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal notification payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := executil.CommandContext(ctx, cmdTemplate[0], cmdTemplate[1:]...)
	cmd.Stdin = io.MultiReader(bytes.NewReader(jsonData), bytes.NewReader([]byte{'\n'}))
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		slog.Warn("notification command timed out", "cmd", cmdTemplate[0], "stdout", stdout.String(), "stderr", stderr.String())
		return ctx.Err()
	}
	if err != nil {
		slog.Warn("notification command error", "err", err, "stdout", stdout.String(), "stderr", stderr.String())
	}
	return err
}
