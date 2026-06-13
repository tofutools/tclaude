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
	"strings"
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
// It checks the global toggle, the per-agent/per-group filters
// (AllowedForConv) and the cooldown via the database, and sends a
// notification if appropriate. convID identifies the agent for the
// filter lookup; convTitle is optional - pass empty string if not
// available. harness is the session's coding tool ("claude", "codex",
// …) and drives the banner title attribution; pass "" for the historical
// Claude-Code default.
func OnStateTransition(sessionID, convID, from, to, cwd, convTitle, harness string) {
	// A no-op "transition" never notifies — and an explicit config rule
	// cannot opt back in. Without this guard, CC's ~60s idle timer fires
	// a Notification(idle_prompt) hook that re-stamps an already-idle
	// session (idle → idle); the wildcard {from:"*", to:"idle"} rule
	// matched that, and with the cooldown (default 5s) long expired the
	// human got a duplicate "Idle" banner about a minute after the real
	// one. Same shape for a SessionEnd hook landing after the reaper
	// already stamped the session exited (exited → exited).
	//
	// Deliberate trade-off: a genuinely NEW prompt that re-lands on the
	// same status (deny tool A, the model immediately requests tool B —
	// awaiting_permission → awaiting_permission with no hook in between)
	// is suppressed too. The banner would have been byte-identical to
	// the one already sent (Send renders the status + project, not the
	// detail), and the human was at the keyboard to deny A moments
	// earlier — accepted as noise reduction, not signal loss.
	if from == to {
		return
	}

	cfg, err := config.Load()
	if err != nil || cfg.Notifications == nil || !cfg.Notifications.Enabled {
		return
	}

	if !cfg.Notifications.MatchesTransition(from, to) {
		return
	}

	if !AllowedForConv(convID) {
		return
	}

	// Check cooldown via database
	cooldown := time.Duration(cfg.Notifications.CooldownSeconds) * time.Second
	if lastNotify, found, err := db.GetNotifyTime(sessionID); err == nil && found {
		if time.Since(lastNotify) < cooldown {
			return
		}
	}

	sendWithHarness(sessionID, formatStatus(to), cwd, convTitle, harness)

	// Record notification time
	_ = db.SetNotifyTime(sessionID)
}

// AllowedForConv evaluates the per-agent / per-group notification
// filters for an agent (conv-id). The decision ladder:
//
//  1. A per-agent pref (agent_notify_prefs) wins outright: 'off'
//     silences the agent, 'on' forces notifications even when a
//     containing group is muted.
//  2. Otherwise the agent inherits from its groups: if ANY active
//     (non-archived) group containing it has notify_enabled = false,
//     the agent is silenced — muting a group reliably silences its
//     members.
//  3. No pref, no muted group (including "not an agent at all") →
//     allowed.
//
// The global config.notifications.enabled master switch sits ABOVE
// this and is checked by the caller (OnStateTransition) — a per-agent
// 'on' does not override a globally-off config. Fails open: a DB error
// or an empty convID never suppresses a notification, so filtering
// degrades to the historical notify-everything behaviour.
func AllowedForConv(convID string) bool {
	if convID == "" {
		return true
	}
	mode, err := db.GetConvNotifyPref(convID)
	if err != nil {
		// Fail open right here: falling through to the group check
		// could return false on a muted group even though the human
		// may have set a (now unreadable) 'on' override.
		return true
	}
	switch mode {
	case db.NotifyPrefOff:
		return false
	case db.NotifyPrefOn:
		return true
	}
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return true
	}
	for _, g := range groups {
		if !g.IsArchived() && !g.NotifyEnabled {
			return false
		}
	}
	return true
}

// harnessLabel maps a session's harness id to the label used in the
// notification banner title. The empty/unknown harness defaults to
// "Claude" so Claude Code notifications — and harness-neutral callers
// like the task runner and rate-limit warnings, which go through Send —
// read exactly as before.
func harnessLabel(harness string) string {
	switch harness {
	case "codex": // session.SessionState.Harness for OpenAI Codex CLI
		return "Codex"
	default:
		return "Claude"
	}
}

// notificationTitle composes the banner title from the harness label and
// the human-readable status, truncated to the platform title budget.
func notificationTitle(harness, status string) string {
	return truncate(fmt.Sprintf("%s: %s", harnessLabel(harness), status), notifyTitleMaxLen)
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
	case "error":
		return "Error"
	case "exited":
		return "Exited"
	default:
		return status
	}
}

// Send actually sends the notification, attributing the banner to Claude
// Code (the historical default). Callers that know the session's harness
// reach the harness-aware path through OnStateTransition instead.
func Send(sessionID, status, cwd, convTitle string) {
	sendWithHarness(sessionID, status, cwd, convTitle, "")
}

// sendWithHarness builds and dispatches a notification, attributing the
// banner title to the session's harness ("Codex: …" vs "Claude: …").
func sendWithHarness(sessionID, status, cwd, convTitle, harness string) {
	slog.Debug("sending notification",
		"sessionID", sessionID,
		"status", status,
		"cwd", cwd,
		"convTitle", convTitle,
		"harness", harness,
	)

	// Build notification content
	projectName := filepath.Base(cwd)
	if projectName == "" || projectName == "." {
		projectName = "unknown"
	}

	title := notificationTitle(harness, status)

	// Build body: ID | Project - conversation title
	var body string
	if convTitle != "" {
		body = fmt.Sprintf("%s | %s - %s", shortID(sessionID), projectName, convTitle)
	} else {
		body = fmt.Sprintf("%s | %s", shortID(sessionID), projectName)
	}
	body = truncate(body, notifyBodyMaxLen)

	dispatch(sessionID, title, body)
}

// dispatch delivers an already-formatted notification through the
// configured channel — a custom notification_command if one is set,
// otherwise the platform default (D-Bus / toast / terminal-notifier) —
// and falls back to stderr if that fails. sessionID drives the
// click-to-focus action. Shared by Send and SendHumanMessage so both
// honor notification_command identically.
func dispatch(sessionID, title, body string) {
	var err error
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

// SendHumanMessage raises an OS notification for a `tclaude agent
// notify-human` message — the desktop companion to the dashboard
// Messages tab, so the human sees an agent's ping off the busy terminal.
//
// It no-ops unless notifications are enabled AND the human_messages knob
// is on (see config.NotificationConfig.NotifyHumanMessages) — mirroring
// how OnStateTransition self-gates on config, so callers stay dumb.
//
// senderSessionID, when non-empty, makes the notification click-to-focus
// the sending agent's terminal — the OS-notification twin of the
// dashboard's per-message Focus button. Pass "" when the sender has no
// live session; the notification still fires, just non-clickable.
func SendHumanMessage(senderSessionID, fromTitle, group, subject, body string) {
	cfg, err := config.Load()
	if err != nil || !cfg.Notifications.NotifyHumanMessages() {
		return
	}
	title, notifBody := formatHumanMessage(fromTitle, group, subject, body)
	slog.Debug("sending human-message notification",
		"senderSessionID", senderSessionID, "from", fromTitle, "group", group)
	dispatch(senderSessionID, title, notifBody)
}

// formatHumanMessage builds the title/body of a human-message
// notification. The title carries the subject (or a "messaged you"
// attribution when there is none); the body carries the message, prefixed
// by the sender when a subject occupied the title, and suffixed by the
// group — truncated to the same caps as Send so an over-long message
// can't overflow the banner. who falls back to a generic phrase when the
// sender title is unknown.
func formatHumanMessage(fromTitle, group, subject, body string) (title, notifBody string) {
	who := strings.TrimSpace(fromTitle)
	if who == "" {
		who = "An agent"
	}

	if s := strings.TrimSpace(subject); s != "" {
		title = truncate("Claude: "+s, notifyTitleMaxLen)
	} else {
		title = truncate(fmt.Sprintf("Claude: %s messaged you", who), notifyTitleMaxLen)
	}

	var b strings.Builder
	if s := strings.TrimSpace(subject); s != "" {
		// Title already carries the subject; lead the body with the
		// sender so the human knows who, even when the subject is set.
		b.WriteString(who)
		b.WriteString(": ")
	}
	b.WriteString(strings.TrimSpace(body))
	if g := strings.TrimSpace(group); g != "" {
		b.WriteString("\n— ")
		b.WriteString(g)
	}
	notifBody = truncate(b.String(), notifyBodyMaxLen)
	return title, notifBody
}

const (
	// D-Bus spec has no hard title limit, but some implementations truncate around 120 chars.
	notifyTitleMaxLen = 120
	notifyBodyMaxLen  = 1024
)

// shortID returns a shortened session ID for display.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncate shortens s to at most maxLen runes, appending … if cut.
// Operates on runes to avoid splitting multi-byte characters.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen || maxLen <= 0 {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
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
	cmd := executil.CommandContextWithGrace(ctx, time.Second, cmdTemplate[0], cmdTemplate[1:]...)
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
