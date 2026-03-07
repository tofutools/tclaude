package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type StatusCallbackParams struct {
	Status string `pos:"true" help:"New status (working, idle, awaiting_permission, awaiting_input)"`
}

// Valid status values for callbacks
const (
	StatusWorking            = "working"
	StatusIdle               = "idle"
	StatusAwaitingPermission = "awaiting_permission"
	StatusAwaitingInput      = "awaiting_input"
)

func StatusCallbackCmd() *cobra.Command {
	cmd := boa.CmdT[StatusCallbackParams]{
		Use:         "status-callback <status>",
		Short:       "Update session status (called by Claude hooks)",
		Long:        "Internal command called by Claude Code hooks to update session status. Not intended for direct use.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *StatusCallbackParams, cmd *cobra.Command, args []string) {
			if err := runStatusCallback(params); err != nil {
				// Silent failure - don't disrupt Claude's flow
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Hidden = true // Hide from help since it's for hooks
	return cmd
}

// HookInput represents the JSON input from Claude Code hooks
type HookInput struct {
	SessionID        string `json:"session_id"`
	Cwd              string `json:"cwd"`
	HookEventName    string `json:"hook_event_name"`
	NotificationType string `json:"notification_type,omitempty"`
}

func runStatusCallback(params *StatusCallbackParams) error {
	// Validate status first
	switch params.Status {
	case StatusWorking, StatusIdle, StatusAwaitingPermission, StatusAwaitingInput:
		// Valid
	default:
		return fmt.Errorf("invalid status: %s", params.Status)
	}

	// Read hook input from stdin - we need this for auto-registration
	var hookInput HookInput
	stdinData, err := io.ReadAll(os.Stdin)
	if err == nil && len(stdinData) > 0 {
		json.NewDecoder(bytes.NewReader(stdinData)).Decode(&hookInput)
	}

	// Debug logging - useful for troubleshooting hook issues
	// Log is stored in ~/.tofu/claude-sessions/debug.log
	_ = EnsureSessionsDir()
	debugFile, _ := os.OpenFile(DebugLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if debugFile != nil {
		fmt.Fprintf(debugFile, "--- %s ---\n", time.Now().Format(time.RFC3339))
		fmt.Fprintf(debugFile, "Status: %s\n", params.Status)
		fmt.Fprintf(debugFile, "Stdin: %s\n", string(stdinData))
		debugFile.Close()
	}


	// Get tofu session ID from environment
	tofuSessionID := os.Getenv("TOFU_SESSION_ID")

	var state *SessionState

	if tofuSessionID == "" {
		// Session wasn't started via tofu - try to auto-register
		if hookInput.SessionID == "" {
			// No session ID from hook input, can't register
			return nil
		}

		// Check if we already have a session for this Claude conversation
		state = findSessionByConvID(hookInput.SessionID)
		if state == nil {
			// Create a new auto-registered session
			state = autoRegisterSession(hookInput)
			if state == nil {
				return nil
			}
		}
	} else {
		// Load existing session state
		state, err = LoadSessionState(tofuSessionID)
		if err != nil {
			return fmt.Errorf("failed to load session state: %w", err)
		}
	}

	// Update status
	state.Status = params.Status
	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes on resume)
	if hookInput.SessionID != "" && state.ConvID != hookInput.SessionID {
		state.ConvID = hookInput.SessionID
	}

	// Update PID if the current one is stale (session was resumed with new process)
	// This is important for detecting when the session actually exits
	if state.PID > 0 && !IsProcessAlive(state.PID) {
		if newPID := FindClaudePID(); newPID > 0 {
			state.PID = newPID
		}
	} else if state.PID == 0 {
		// Session had no PID, try to find one
		if newPID := FindClaudePID(); newPID > 0 {
			state.PID = newPID
		}
	}

	// Add detail if available
	if hookInput.HookEventName != "" {
		state.StatusDetail = hookInput.HookEventName
	}

	// Save updated state
	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	return nil
}

// findSessionByConvID searches for an existing session with the given Claude conversation ID
func findSessionByConvID(convID string) *SessionState {
	states, err := ListSessionStates()
	if err != nil {
		return nil
	}
	for _, state := range states {
		if state.ConvID == convID {
			return state
		}
	}
	return nil
}

// autoRegisterSession creates a new session state for a Claude session
// that wasn't started via tofu
func autoRegisterSession(hookInput HookInput) *SessionState {
	// Find Claude's PID by walking up the process tree
	claudePID := FindClaudePID()
	if claudePID == 0 {
		// Can't find Claude process - can't track this session
		return nil
	}

	// Check if we're inside tmux
	tmuxSession := GetCurrentTmuxSession()

	// Generate a session ID
	// Use first 8 chars of Claude's session ID for recognizability
	sessionID := hookInput.SessionID
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}

	// Determine cwd
	cwd := hookInput.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         claudePID,
		Cwd:         cwd,
		ConvID:      hookInput.SessionID,
		Status:      StatusWorking,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	// Ensure sessions directory exists
	if err := EnsureSessionsDir(); err != nil {
		return nil
	}

	// Check for ID collision (unlikely but possible)
	existingPath := SessionStatePath(sessionID)
	if _, err := os.Stat(existingPath); err == nil {
		// ID already exists - check if it's the same conversation
		existing, err := LoadSessionState(sessionID)
		if err == nil && existing.ConvID == hookInput.SessionID {
			// Same conversation, return existing
			return existing
		}
		// Different conversation - append disambiguator
		for i := 1; i < 100; i++ {
			newID := fmt.Sprintf("%s-%d", sessionID, i)
			if _, err := os.Stat(SessionStatePath(newID)); os.IsNotExist(err) {
				state.ID = newID
				break
			}
		}
	}

	// Save the new session
	if err := SaveSessionState(state); err != nil {
		return nil
	}

	// Write a marker file so we know this was auto-registered
	// This could be useful for UI differentiation later
	markerPath := filepath.Join(SessionsDir(), state.ID+".auto")
	os.WriteFile(markerPath, []byte("auto-registered"), 0644)

	return state
}
