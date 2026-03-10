// Package notify provides OS notifications for session state transitions.
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// stateDir returns the directory for notification state files.
func stateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "notify-state")
}

// stateFile returns the path to a session's notification state file.
func stateFile(sessionID string) string {
	return filepath.Join(stateDir(), sessionID)
}

// OnStateTransition is called when a session changes state.
// It checks cooldown via file modification time and sends notification if appropriate.
// convTitle is optional - pass empty string if not available.
func OnStateTransition(sessionID, from, to, cwd, convTitle string) {
	cfg, err := config.Load()
	if err != nil || cfg.Notifications == nil || !cfg.Notifications.Enabled {
		return
	}

	// Check if transition matches config
	if !cfg.Notifications.MatchesTransition(from, to) {
		return
	}

	// Check cooldown via file modification time
	cooldown := time.Duration(cfg.Notifications.CooldownSeconds) * time.Second
	statePath := stateFile(sessionID)

	if info, err := os.Stat(statePath); err == nil {
		if time.Since(info.ModTime()) < cooldown {
			return
		}
	}

	// Send notification
	send(sessionID, to, cwd, convTitle)

	// Update state file (touch it)
	if err := os.MkdirAll(stateDir(), 0755); err == nil {
		// Create or update the file
		f, err := os.Create(statePath)
		if err == nil {
			f.Close()
		}
	}
}

// send actually sends the notification.
func send(sessionID, to, cwd, convTitle string) {
	// Build notification content
	projectName := filepath.Base(cwd)
	if projectName == "" || projectName == "." {
		projectName = "unknown"
	}

	statusDisplay := formatStatus(to)
	title := fmt.Sprintf("Claude: %s", statusDisplay)

	// Build body: ID | Project - conversation title
	var body string
	if convTitle != "" {
		body = fmt.Sprintf("%s | %s - %s", shortID(sessionID), projectName, convTitle)
	} else {
		body = fmt.Sprintf("%s | %s", shortID(sessionID), projectName)
	}

	var err error

	// Check for custom notification command
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

// runCustomCommand executes a custom notification command with template substitution.
// Each element in cmdTemplate may contain {sessionID}, {title}, {body} placeholders.
func runCustomCommand(cmdTemplate []string, sessionID, title, body string) error {
	if len(cmdTemplate) == 0 {
		return fmt.Errorf("empty notification command")
	}

	r := strings.NewReplacer(
		"{sessionID}", sessionID,
		"{title}", title,
		"{body}", body,
	)

	args := make([]string, len(cmdTemplate))
	for i, tmpl := range cmdTemplate {
		args[i] = r.Replace(tmpl)
	}

	return exec.Command(args[0], args[1:]...).Run()
}

// formatStatus returns a human-readable status string.
func formatStatus(status string) string {
	switch status {
	case "working":
		return "Working"
	case "idle":
		return "Idle"
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

// shortID returns a shortened session ID for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// IsEnabled returns whether notifications are enabled.
func IsEnabled() bool {
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return cfg.Notifications != nil && cfg.Notifications.Enabled
}
