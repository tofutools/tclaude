package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
)

var safeSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// HookCallbackInput represents the JSON input from any Claude Code hook
type HookCallbackInput struct {
	ConvID               string `json:"session_id"` // claude's session id, what we call conv_id
	TranscriptPath       string `json:"transcript_path"`
	Cwd                  string `json:"cwd"`
	PermissionMode       string `json:"permission_mode,omitempty"`
	HookEventName        string `json:"hook_event_name"`
	NotificationType     string `json:"notification_type,omitempty"`
	Message              string `json:"message,omitempty"`
	Prompt               string `json:"prompt,omitempty"`
	StopHookActive       bool   `json:"stop_hook_active,omitempty"`
	ToolName             string `json:"tool_name,omitempty"`
	AgentType            string `json:"agent_type,omitempty"`
	AgentID              string `json:"agent_id,omitempty"`
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
}

func HookCallbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook-callback",
		Short:  "Handle Claude Code hooks (internal)",
		Long:   "Unified callback for all Claude Code hooks. Reads hook data from stdin and updates session state accordingly.",
		Hidden: true,
		Run: func(cmd *cobra.Command, args []string) {
			if err := runHookCallback(); err != nil {
				slog.Error("hook callback failed", "error", err, "module", "hooks")
				os.Exit(1)
			}
		},
	}
	return cmd
}

func runHookCallback() error {
	// Read hook input from stdin
	stdinData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	if os.Getenv("TCLAUDE_IGNORE_HOOKS") != "" {
		return nil
	}

	envSessionID := os.Getenv("TCLAUDE_SESSION_ID")

	// Append raw JSON to <sessionId>.jsonl if record_hooks is enabled, and we are not currently replaying
	replayMode := os.Getenv("TCLAUDE_REPLAY_MODE") != ""
	if cfg, err := config.Load(); err == nil && cfg.RecordHooks && !replayMode && envSessionID != "" {
		if !safeSessionIDRe.MatchString(envSessionID) {
			slog.Warn("unsafe session ID rejected for hook recording", "session_id", envSessionID, "module", "hooks")
		} else {
			logPath := fmt.Sprintf("%s.jsonl", envSessionID)
			if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
				_ = f.Chmod(0600)
				line := bytes.TrimRight(stdinData, "\n")
				_, _ = f.Write(line)
				_, _ = f.Write([]byte("\n"))
				_ = f.Close()
			}
		}
	}

	var input HookCallbackInput
	if len(stdinData) > 0 {
		if err := json.NewDecoder(bytes.NewReader(stdinData)).Decode(&input); err != nil {
			slog.Error("failed to parse hook input", "error", err, "input_bytes", len(stdinData), "module", "hooks")
			return fmt.Errorf("failed to parse hook input: %w", err)
		}
	} else {
		return fmt.Errorf("no input received on stdin")
	}

	// Acquire a per-session exclusive lock to prevent concurrent hook callbacks
	// from racing on the read-modify-write of session state.
	sessionKey := envSessionID
	if sessionKey == "" {
		sessionKey = input.ConvID
	}
	if sessionKey != "" {
		unlock, lockErr := acquireHookLock(sessionKey)
		if lockErr != nil {
			slog.Warn("failed to acquire hook lock", "error", lockErr, "module", "hooks")
			return fmt.Errorf("failed to acquire hook lock: %w", lockErr)
		}

		defer unlock()
	}

	// Log hook event
	slog.Info("hook received",
		"event", input.HookEventName,
		"conv_id", input.ConvID,
		"notification_type", input.NotificationType,
		"tool_name", input.ToolName,
		"cwd", input.Cwd,
		"sessionId", envSessionID,
		"module", "hooks",
	)

	// Get or create session state
	state, err := getOrCreateSessionState(input)
	if err != nil || state == nil {
		return err
	}
	slog.Info("session found", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")

	// Capture previous status for notification
	prevStatus := state.Status

	stopped := false

	state.LastHook = time.Now()

	// Update state based on hook event
	switch input.HookEventName {
	case "UserPromptSubmit":
		state.Status = StatusWorking
		state.StatusDetail = "UserPromptSubmit"

	case "PreToolUse":
		// Tool is about to execute
		state.Status = StatusWorking
		state.StatusDetail = input.ToolName

	case "PostToolUse", "PostToolUseFailure":
		// Tool completed (success or failure) - back to working
		state.Status = StatusWorking
		state.StatusDetail = input.ToolName

	case "SubagentStart":
		state.SubagentCount += 1

	case "SubagentStop":
		if state.SubagentCount > 0 {
			state.SubagentCount -= 1
		}
		if state.SubagentCount == 0 && state.Status == StatusMainAgentIdle {
			state.Status = StatusIdle
			state.StatusDetail = ""
			stopped = true
		}

	case "Stop":
		if state.SubagentCount < 1 {
			state.Status = StatusIdle
			state.StatusDetail = ""
			stopped = true
		} else {
			state.Status = StatusMainAgentIdle
			state.StatusDetail = fmt.Sprintf("%d subagents running", state.SubagentCount)
		}

	case "SessionStart":
		// Session started or resumed - update ConvID and set to idle
		state.Status = StatusIdle
		state.StatusDetail = ""

	case "PermissionRequest":
		state.Status = StatusAwaitingPermission
		state.StatusDetail = input.ToolName
		if state.StatusDetail == "" {
			state.StatusDetail = "permission"
		}

	case "PostCompact":
		// Reset auto-compact state so it can trigger again next time
		if envSessionID != "" {
			if err := db.ResetCompact(envSessionID); err != nil {
				slog.Warn("failed to reset compact state", "error", err, "module", "hooks")
			} else {
				slog.Info("auto-compact state reset", "session_id", envSessionID, "module", "hooks")
			}
		}
		if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
			slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
		}
		return nil

	case "Notification":
		// Check notification type for legacy support
		switch input.NotificationType {
		case "permission_prompt":
			state.Status = StatusAwaitingPermission
			state.StatusDetail = input.Message
		case "elicitation_dialog":
			state.Status = StatusAwaitingInput
			state.StatusDetail = input.Message
		default:
			// Unknown notification type - log but don't update status
			if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
				slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
			}
			return nil
		}

	default:
		// Unknown hook event - log but don't update status
		if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
			slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
		}
		return nil
	}

	if stopped {
		// Check auto-compact threshold first — when a session's
		// context_pct has crossed BOTH the auto-compact threshold and
		// a nudge threshold, the compact takes precedence (it's the
		// actionable response; the nudge would be advice that's about
		// to be invalidated by the compact). handleContextNudge's
		// "compact_pending > 0 → skip" guard relies on
		// handleAutoCompact running first to set the flag.
		handleAutoCompact(input)
		handleContextNudge(input)
	}

	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes on resume)
	if input.ConvID != "" && state.ConvID != input.ConvID {
		slog.Info("updating conversation ID",
			"old_conv_id", state.ConvID,
			"new_conv_id", input.ConvID,
			"session_id", state.ID,
			"module", "hooks",
		)
		state.ConvID = input.ConvID
	}

	// Update PID if stale
	if state.PID > 0 && !IsProcessAlive(state.PID) {
		if newPID := FindClaudePID(); newPID > 0 {
			state.PID = newPID
		}
	} else if state.PID == 0 {
		if newPID := FindClaudePID(); newPID > 0 {
			state.PID = newPID
		}
	}

	// Save updated state
	slog.Info("updating session", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")
	if err := SaveSessionState(state); err != nil {
		return err
	}

	// Refresh usage cache when user is likely looking at the status bar.
	// Runs synchronously — hook callbacks are separate processes so this
	// just keeps the process alive a bit longer without blocking Claude.
	// SQLite's TryClaimUsageFetch prevents concurrent API calls.
	if state.Status == StatusIdle || state.Status == StatusAwaitingPermission || state.Status == StatusAwaitingInput {
		usageapi.RefreshCache()
	}

	// Signal task runner when Stop/UserPromptSubmit fires in task mode
	taskSignalWasHandled := handleTaskSignal(stopped, input)

	// In task mode, skip notifications — the task runner sends its own
	// targeted notifications (e.g. "Task failed: X", "All tasks completed!").
	if taskSignalWasHandled {
		return nil
	}

	// Look up conversation title for notification
	convTitle := getConvTitle(state.ConvID, state.Cwd)

	// Notify on state transition (handles cooldown internally)
	if input.HookEventName != "SessionStart" {
		notify.OnStateTransition(state.ID, prevStatus, state.Status, state.Cwd, convTitle)
	}

	return nil
}

// TaskSignal is the JSON structure written to the task signal file.
type TaskSignal struct {
	Report    string `json:"report"`
	SessionID string `json:"sessionId,omitempty"`
	Event     string `json:"event,omitempty"`    // hook event name (e.g. "Stop", "PermissionRequest")
	ToolName  string `json:"toolName,omitempty"` // tool name from the hook (e.g. "ExitPlanMode")
}

// handleTaskSignal writes or removes a signal file for the task runner's
// auto-continue watcher. In task mode, TCLAUDE_TASK_SIGNAL is set to a
// file path. On Stop, we write the report and session ID as JSON.
// On UserPromptSubmit, we remove the signal to cancel any pending
// auto-exit (the user is interacting).
func handleTaskSignal(isDone bool, input HookCallbackInput) bool {
	signalPath := os.Getenv("TCLAUDE_TASK_SIGNAL")
	if signalPath == "" {
		return false
	}
	// Validate that the signal path is within the expected cache directory.
	allowedDir := filepath.Clean(common.CacheDir())
	cleanPath := filepath.Clean(signalPath)
	rel, err := filepath.Rel(allowedDir, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Warn("task signal path outside allowed directory, ignoring", "path", signalPath, "module", "hooks")
		return false
	}
	signalPath = cleanPath
	if isDone {
		signal := TaskSignal{
			Report:    input.LastAssistantMessage,
			SessionID: input.ConvID,
			Event:     input.HookEventName,
		}
		if data, err := json.Marshal(signal); err == nil {
			if err := os.WriteFile(signalPath, data, 0600); err != nil {
				slog.Warn("Unable to write signal file", "err", err, "module", "hooks")
				return false
			}
			_ = os.Chmod(signalPath, 0600)
			return true
		}
	} else {
		switch input.HookEventName {
		case "PermissionRequest":
			// Signal plan-auto watcher when Claude asks to accept the plan
			if input.ToolName == "ExitPlanMode" {
				signal := TaskSignal{
					SessionID: input.ConvID,
					Event:     input.HookEventName,
					ToolName:  input.ToolName,
				}
				if data, err := json.Marshal(signal); err == nil {
					if err := os.WriteFile(signalPath, data, 0600); err != nil {
						slog.Warn("Unable to write signal file", "err", err, "module", "hooks")
						return false
					}
					_ = os.Chmod(signalPath, 0600)
					return true
				}
			}
		case "UserPromptSubmit":
			_ = os.Remove(signalPath)
		}
	}
	return false
}

// getConvTitle looks up the conversation title and prompt from Claude's session index.
// Returns formatted string like "[title]: prompt" for richer notification content.
func getConvTitle(convID, cwd string) string {
	return convindex.GetConvTitleAndPrompt(convID, cwd)
}

// getOrCreateSessionState finds existing session or creates a new one
func getOrCreateSessionState(input HookCallbackInput) (*SessionState, error) {
	envSessionID := os.Getenv("TCLAUDE_SESSION_ID")

	if envSessionID != "" {
		return LoadSessionState(envSessionID)
	}

	if input.ConvID == "" {
		return nil, nil
	}

	// Indexed lookup by conversation ID
	state, err := FindSessionByConvID(input.ConvID)
	if err != nil {
		return nil, err
	}
	if state != nil {
		return state, nil
	}

	return autoRegisterSessionFromHook(input), nil
}

// autoRegisterSessionFromHook creates a new session state for a Claude session
// that wasn't started via tclaude
func autoRegisterSessionFromHook(input HookCallbackInput) *SessionState {
	claudePID := FindClaudePID()
	if claudePID == 0 {
		return nil
	}

	tmuxSession := GetCurrentTmuxSession()

	sessionID := input.ConvID
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}

	cwd := input.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         claudePID,
		Cwd:         cwd,
		ConvID:      input.ConvID,
		Status:      StatusWorking,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	// Handle ID collision
	if exists, _ := SessionExists(sessionID); exists {
		existing, err := LoadSessionState(sessionID)
		if err == nil && existing.ConvID == input.ConvID {
			return existing
		}
		for i := 1; i < 100; i++ {
			newID := fmt.Sprintf("%s-%d", sessionID, i)
			if exists, _ := SessionExists(newID); !exists {
				state.ID = newID
				break
			}
		}
	}

	if err := SaveSessionState(state); err != nil {
		return nil
	}
	return state
}

// handleAutoCompact checks if auto-compaction should be triggered on Stop.
// Reads the config threshold and the session's stored context_pct,
// then CAS-claims compact_pending and sends /compact via tmux keys.
func handleAutoCompact(input HookCallbackInput) {
	sessionID := os.Getenv("TCLAUDE_SESSION_ID")
	if sessionID == "" {
		return
	}

	// CLI env var overrides config file
	var threshold float64
	if envVal := os.Getenv("TCLAUDE_AUTO_COMPACT"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v > 0 {
			threshold = float64(v)
		}
	}
	if threshold == 0 {
		cfg, err := config.Load()
		if err != nil || cfg.AutoCompactPercent == nil {
			return
		}
		threshold = float64(*cfg.AutoCompactPercent)
	}

	contextPct, _, err := db.GetCompactState(sessionID)
	if err != nil {
		slog.Warn("auto-compact: failed to read compact state", "error", err, "module", "hooks")
		return
	}

	if contextPct < threshold {
		return
	}

	// CAS: only one Stop hook should trigger compaction
	claimed, err := db.TryClaimCompact(sessionID)
	if err != nil {
		slog.Warn("auto-compact: failed to claim", "error", err, "module", "hooks")
		return
	}
	if !claimed {
		slog.Debug("auto-compact: already claimed", "session_id", sessionID, "module", "hooks")
		return
	}

	// Send /compact to the tmux session
	tmuxSession := GetCurrentTmuxSession()
	if tmuxSession == "" {
		slog.Warn("auto-compact: not in a tmux session", "module", "hooks")
		return
	}

	slog.Info("auto-compact: triggering /compact",
		"session_id", sessionID,
		"tmux_session", tmuxSession,
		"context_pct", contextPct,
		"threshold", threshold,
		"module", "hooks",
	)

	cmd := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, "/compact", "Enter")
	if err := cmd.Run(); err != nil {
		slog.Error("auto-compact: failed to send keys", "error", err, "module", "hooks")
	}
}

// nextNudgeTarget computes which threshold percentile, if any, the
// context-nudge Stop-hook path should fire at given the current
// context_pct and the (min, interval) ladder. Returns 0 when no nudge
// should fire (below min, or invalid config). Caps at 90 so the agent
// gets a final "you're really running out" tap before the next gulp
// pushes it into auto-compact / hard-stop territory.
//
// Examples (min=30, interval=10):
//
//	pct=25 → 0  (below min, skip)
//	pct=30 → 30
//	pct=35 → 30 (most recent crossed)
//	pct=49 → 40
//	pct=85 → 80
//	pct=92 → 90 (cap)
//
// Pure function for unit testing.
func nextNudgeTarget(pct float64, minPct, intervalPct int) int {
	if intervalPct <= 0 || minPct <= 0 || pct < float64(minPct) {
		return 0
	}
	n := int((pct - float64(minPct)) / float64(intervalPct))
	target := minPct + n*intervalPct
	if target > 90 {
		target = 90
	}
	return target
}

// formatContextNudgeMessage is the text the daemon types into the
// agent's pane via send-keys when a threshold crosses. Reads as a
// system tap-on-shoulder rather than user input so the agent picks
// up on the intent at next turn.
//
// Pure for unit testing.
func formatContextNudgeMessage(target int) string {
	return fmt.Sprintf("[system: context at %d%%. Consider /reincarnate at the next breakpoint to avoid running out of room mid-task — fresh CC inherits identity but starts with a clean window.]", target)
}

// handleContextNudge fires an opt-in "consider reincarnating" hint
// when the agent's context crosses a configured threshold. Sibling of
// handleAutoCompact: both run in the Stop-hook path, both read the
// stored context_pct, both deliver via tmux send-keys into the
// agent's own pane.
//
// Skips when:
//   - the feature isn't enabled in config
//   - the session id isn't known (callback running outside a tracked session)
//   - context_pct is below the configured min
//   - the same-or-higher threshold has already been fired
//     (sessions.nudged_pct; ResetCompact zeroes it so post-compact climbs re-arm)
//   - compact_pending is already set (the agent is about to compact
//     anyway, no point typing extra text into its pane)
func handleContextNudge(input HookCallbackInput) {
	sessionID := os.Getenv("TCLAUDE_SESSION_ID")
	if sessionID == "" {
		return
	}

	cfg, err := config.Load()
	if err != nil || cfg.Agent == nil {
		return
	}
	enabled, minPct, intervalPct := cfg.Agent.ContextNudge.Resolved()
	if !enabled {
		return
	}

	contextPct, compactPending, err := db.GetCompactState(sessionID)
	if err != nil {
		slog.Warn("context-nudge: failed to read compact state",
			"error", err, "module", "hooks")
		return
	}
	if compactPending > 0 {
		// /compact has already been claimed; the next-turn behaviour is
		// going to drop context_pct soon anyway, so suppress the nudge
		// to avoid stepping on the auto-compact path.
		return
	}

	target := nextNudgeTarget(contextPct, minPct, intervalPct)
	if target == 0 {
		return
	}

	prev, err := db.GetNudgedPct(sessionID)
	if err != nil {
		slog.Warn("context-nudge: failed to read nudged_pct",
			"error", err, "module", "hooks")
		return
	}
	if float64(target) <= prev {
		// Already nudged at this threshold (or a higher one).
		return
	}

	tmuxSession := GetCurrentTmuxSession()
	if tmuxSession == "" {
		// No tmux pane to type into. Drop the nudge but DO stamp
		// nudged_pct so a later run with tmux available doesn't
		// re-send the same threshold for the same climb.
		_ = db.SetNudgedPct(sessionID, float64(target))
		return
	}

	msg := formatContextNudgeMessage(target)
	slog.Info("context-nudge: typing hint into pane",
		"session_id", sessionID, "tmux_session", tmuxSession,
		"context_pct", contextPct, "target", target,
		"min_pct", minPct, "interval_pct", intervalPct,
		"module", "hooks")

	// Send-keys the bracketed-paste text + Enter. Same shape the
	// cron scheduler uses for solo targets. Best-effort: a failed
	// send leaves nudged_pct unchanged so we'll retry on the next
	// Stop hook.
	if err := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, msg).Run(); err != nil {
		slog.Warn("context-nudge: send-keys failed",
			"error", err, "module", "hooks")
		return
	}
	if err := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, "Enter").Run(); err != nil {
		slog.Warn("context-nudge: submit failed",
			"error", err, "module", "hooks")
		return
	}
	if err := db.SetNudgedPct(sessionID, float64(target)); err != nil {
		slog.Warn("context-nudge: failed to stamp nudged_pct",
			"error", err, "module", "hooks")
	}
}
