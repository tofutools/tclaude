package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
)

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
			// Set up logging to both stderr and ~/.tclaude/hooks.log
			SetupHookLogging()

			if err := runHookCallback(); err != nil {
				slog.Error("hook callback failed", "error", err)
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

	var input HookCallbackInput
	if len(stdinData) > 0 {
		if err := json.NewDecoder(bytes.NewReader(stdinData)).Decode(&input); err != nil {
			slog.Error("failed to parse hook input", "error", err, "raw_input", string(stdinData))
			return fmt.Errorf("failed to parse hook input: %w", err)
		}
	} else {
		return fmt.Errorf("no input received on stdin")
	}

	// Log hook event
	slog.Info("hook received",
		"event", input.HookEventName,
		"conv_id", input.ConvID,
		"notification_type", input.NotificationType,
		"tool_name", input.ToolName,
		"cwd", input.Cwd,
	)

	// Determine status based on hook event
	var newStatus string
	var statusDetail string

	switch input.HookEventName {
	case "UserPromptSubmit":
		newStatus = StatusWorking
		statusDetail = "UserPromptSubmit"

	case "PreToolUse":
		// Tool is about to execute
		newStatus = StatusWorking
		statusDetail = input.ToolName

	case "PostToolUse", "PostToolUseFailure":
		// Tool completed (success or failure) - back to working
		newStatus = StatusWorking
		statusDetail = input.ToolName

	case "SubagentStart", "SubagentStop":
		// Just log, don't update status (can fire after Stop and overwrite idle)
		return nil

	case "Stop":
		newStatus = StatusIdle
		statusDetail = ""

	case "SessionStart":
		// Session started or resumed - update ConvID and set to idle
		newStatus = StatusIdle
		statusDetail = ""

	case "PermissionRequest":
		newStatus = StatusAwaitingPermission
		statusDetail = input.ToolName
		if statusDetail == "" {
			statusDetail = "permission"
		}

	case "Notification":
		// Check notification type for legacy support
		switch input.NotificationType {
		case "permission_prompt":
			newStatus = StatusAwaitingPermission
			statusDetail = input.Message
		case "elicitation_dialog":
			newStatus = StatusAwaitingInput
			statusDetail = input.Message
		default:
			// Unknown notification type - log but don't update status
			return nil
		}

	default:
		// Unknown hook event - log but don't update status
		return nil
	}

	// Get or create session state
	state, err := getOrCreateSessionState(input)
	if err != nil || state == nil {
		return err
	}
	slog.Info("session found", "session_id", state.ID, "status", state.Status)

	// Capture previous status for notification
	prevStatus := state.Status

	// Update status
	state.Status = newStatus
	state.StatusDetail = statusDetail
	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes on resume)
	if input.ConvID != "" && state.ConvID != input.ConvID {
		slog.Info("updating conversation ID",
			"old_conv_id", state.ConvID,
			"new_conv_id", input.ConvID,
			"session_id", state.ID,
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
	if err := SaveSessionState(state); err != nil {
		return err
	}

	// Refresh usage cache when user is likely looking at the status bar.
	// Runs synchronously — hook callbacks are separate processes so this
	// just keeps the process alive a bit longer without blocking Claude.
	// Lock only protects the usage API call (SQLite handles session concurrency).
	if newStatus == StatusIdle || newStatus == StatusAwaitingPermission || newStatus == StatusAwaitingInput {
		func() {
			defer common.AcquireHookLock()()
			usageapi.RefreshCache()
		}()
	}

	// Signal task runner when Stop/UserPromptSubmit fires in task mode
	handleTaskSignal(input)

	// In task mode, skip idle notifications — the task runner sends its own
	// targeted notifications (e.g. "Task failed: X", "All tasks completed!").
	// Other notifications (permission requests, user questions) still fire.
	if newStatus == StatusIdle && os.Getenv("TCLAUDE_TASK_SIGNAL") != "" {
		return nil
	}

	// Look up conversation title for notification
	convTitle := getConvTitle(state.ConvID, state.Cwd)

	// Notify on state transition (handles cooldown internally)
	notify.OnStateTransition(state.ID, prevStatus, newStatus, state.Cwd, convTitle)

	return nil
}

// handleTaskSignal writes or removes a signal file for the task runner's
// auto-continue watcher. In task mode, TCLAUDE_TASK_SIGNAL is set to a
// file path. On Stop, we write the last assistant message (used as the
// task report). On UserPromptSubmit, we remove the signal to cancel any
// pending auto-exit (the user is interacting).
func handleTaskSignal(input HookCallbackInput) {
	signalPath := os.Getenv("TCLAUDE_TASK_SIGNAL")
	if signalPath == "" {
		return
	}
	switch input.HookEventName {
	case "Stop":
		os.WriteFile(signalPath, []byte(input.LastAssistantMessage), 0644)
		if input.ConvID != "" {
			os.WriteFile(signalPath+".session-id", []byte(input.ConvID), 0644)
		}
	case "UserPromptSubmit":
		os.Remove(signalPath)
	}
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
