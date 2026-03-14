package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// SessionState represents the state of a Claude session
type SessionState struct {
	ID           string    `json:"id"`
	TmuxSession  string    `json:"tmuxSession"`
	PID          int       `json:"pid"`
	Cwd          string    `json:"cwd"`
	ConvID       string    `json:"convId,omitempty"`
	Status       string    `json:"status"`
	StatusDetail string    `json:"statusDetail,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	Attached     int       `json:"-"` // Number of attached clients (runtime only, not persisted)
}

// Status constants
const (
	StatusRunning           = "running"
	StatusWaitingInput      = "waiting_input"
	StatusWaitingPermission = "waiting_permission"
	StatusExited            = "exited"
)

// SortSessionsByKey sorts sessions by the given sort key and direction.
func SortSessionsByKey(sessions []*SessionState, key string, dir table.SortDirection) {
	if key == "" || len(sessions) < 2 {
		return
	}
	sort.Slice(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		var less bool
		switch key {
		case "id":
			less = a.ID < b.ID
		case "project":
			less = a.Cwd < b.Cwd
		case "status":
			less = statusPriority(a.Status) < statusPriority(b.Status)
		case "updated":
			less = a.Updated.Before(b.Updated)
		default:
			return false
		}
		if dir == table.SortDesc {
			return !less
		}
		return less
	})
}

// statusPriority returns sort priority for status (lower = shown first when ascending)
// Red (needs attention) = 0, Yellow (idle) = 1, Green (working) = 2, Gray (exited) = 3
func statusPriority(status string) int {
	switch status {
	case StatusAwaitingPermission, StatusAwaitingInput:
		return 0 // Red - needs attention, show first
	case StatusIdle:
		return 1 // Yellow
	case StatusWorking:
		return 2 // Green
	case StatusExited:
		return 3 // Gray
	default:
		return 0 // Unknown = needs attention
	}
}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[boa.NoParams]{
		Use:         "session",
		Short:       "Manage Claude Code sessions (tmux-based)",
		Long:        "Multiplex and manage multiple Claude Code sessions with detach/reattach support.\n\nWhen run without a subcommand, opens the interactive session viewer.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			NewCmd(),
			ListCmd(),
			AttachCmd(),
			FocusCmd(),
			GotoCmd(),
			KillCmd(),
			PruneCmd(),
			StatusCallbackCmd(),
			HookCallbackCmd(),
			NotifyListenCmd(),
		},
		RunFunc: func(_ *boa.NoParams, cmd *cobra.Command, args []string) {
			// Default to interactive watch mode
			if err := RunWatchMode(false, table.SortState{}, nil, nil); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Aliases = []string{"sessions"}
	return cmd
}

// toRow converts a SessionState to a db.SessionRow.
func toRow(s *SessionState) *db.SessionRow {
	return &db.SessionRow{
		ID:           s.ID,
		TmuxSession:  s.TmuxSession,
		PID:          s.PID,
		Cwd:          s.Cwd,
		ConvID:       s.ConvID,
		Status:       s.Status,
		StatusDetail: s.StatusDetail,
		CreatedAt:    s.Created,
		UpdatedAt:    s.Updated,
	}
}

// fromRow converts a db.SessionRow to a SessionState.
func fromRow(r *db.SessionRow) *SessionState {
	return &SessionState{
		ID:           r.ID,
		TmuxSession:  r.TmuxSession,
		PID:          r.PID,
		Cwd:          r.Cwd,
		ConvID:       r.ConvID,
		Status:       r.Status,
		StatusDetail: r.StatusDetail,
		Created:      r.CreatedAt,
		Updated:      r.UpdatedAt,
	}
}

// SaveSessionState saves session state to the database.
func SaveSessionState(state *SessionState) error {
	state.Updated = time.Now()
	return db.SaveSession(toRow(state))
}

// LoadSessionState loads session state from the database.
func LoadSessionState(id string) (*SessionState, error) {
	row, err := db.LoadSession(id)
	if err != nil {
		return nil, err
	}
	return fromRow(row), nil
}

// DeleteSessionState removes a session from the database.
func DeleteSessionState(id string) error {
	return db.DeleteSession(id)
}

// DefaultCleanupAge is the default max age for exited sessions in prune command
const DefaultCleanupAge = 7 * 24 * time.Hour // 1 week

// ListSessionStates returns all session states.
func ListSessionStates() ([]*SessionState, error) {
	rows, err := db.ListSessions()
	if err != nil {
		return nil, err
	}
	states := make([]*SessionState, len(rows))
	for i, r := range rows {
		states[i] = fromRow(r)
	}
	return states, nil
}

// CleanupOldExitedSessions removes exited session states older than maxAge.
func CleanupOldExitedSessions(maxAge time.Duration) error {
	_, err := db.CleanupOldExited(maxAge)
	return err
}

// FindSessionByConvID finds a session by Claude conversation ID using an indexed lookup.
func FindSessionByConvID(convID string) (*SessionState, error) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return fromRow(row), nil
}

// SessionExists checks whether a session with the given ID exists.
func SessionExists(id string) (bool, error) {
	return db.SessionExists(id)
}

// MaxUpdatedAt returns the most recent updated_at across all sessions.
func MaxUpdatedAt() (time.Time, error) {
	return db.MaxUpdatedAt()
}

// IsTmuxSessionAlive checks if a tmux session exists
func IsTmuxSessionAlive(sessionName string) bool {
	cmd := clcommon.TmuxCommand("has-session", "-t", sessionName)
	return cmd.Run() == nil
}

// GetTmuxSessionAttachedCount returns the number of clients attached to a tmux session
// Returns 0 if session doesn't exist or on error
func GetTmuxSessionAttachedCount(sessionName string) int {
	cmd := clcommon.TmuxCommand("display-message", "-t", sessionName, "-p", "#{session_attached}")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	count, _ := strconv.Atoi(strings.TrimSpace(string(output)))
	return count
}

// IsTmuxSessionAttached checks if a tmux session has any clients attached
func IsTmuxSessionAttached(sessionName string) bool {
	return GetTmuxSessionAttachedCount(sessionName) > 0
}

// DetachSessionClients detaches all clients from a tmux session
func DetachSessionClients(sessionName string) error {
	// Get list of clients attached to this session
	cmd := clcommon.TmuxCommand("list-clients", "-t", sessionName, "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		return err
	}

	// Detach each client
	clients := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, client := range clients {
		if client == "" {
			continue
		}
		// Detach this specific client
		_ = clcommon.TmuxCommand("detach-client", "-t", client).Run()
	}
	return nil
}

// CheckTmuxInstalled verifies tmux is available
func CheckTmuxInstalled() error {
	_, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux is required but not installed. Install it with:\n  Ubuntu/Debian: sudo apt install tmux\n  macOS: brew install tmux")
	}
	return nil
}

// GenerateSessionID creates a short unique session ID
func GenerateSessionID() string {
	// Use last 8 hex chars of unix nano time
	hex := fmt.Sprintf("%016x", time.Now().UnixNano())
	return hex[len(hex)-8:]
}

// FormatDuration formats a duration in a human-readable way
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// RefreshSessionStatus updates the session status based on actual state
func RefreshSessionStatus(state *SessionState) {
	// For tmux-backed sessions, check if tmux session is alive
	if state.TmuxSession != "" {
		if IsTmuxSessionAlive(state.TmuxSession) {
			state.Attached = GetTmuxSessionAttachedCount(state.TmuxSession)
			return
		}
		// Tmux session is dead - fall through to check PID
		state.Attached = 0
	}

	// Check if PID is alive (works for both non-tmux sessions and
	// sessions where tmux died but the process is still running)
	if state.PID > 0 {
		if !IsProcessAlive(state.PID) {
			state.Status = StatusExited
		}
		// If PID is alive, keep the current status (updated by hooks)
		return
	}

	// No tmux session and no PID - mark as exited
	state.Status = StatusExited
}

// ShortID returns the first 8 characters of an ID for display
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// ShortenPath shortens a path for display
func ShortenPath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	// Show last part of path
	parts := filepath.SplitList(path)
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if len(last) <= maxLen-3 {
			return "…" + string(filepath.Separator) + last
		}
	}
	return "…" + path[len(path)-maxLen+1:]
}

// ParsePIDFromTmux gets the PID of the main process in a tmux session
func ParsePIDFromTmux(sessionName string) int {
	cmd := clcommon.TmuxCommand("list-panes", "-t", sessionName, "-F", "#{pane_pid}")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(string(output[:len(output)-1])) // trim newline
	return pid
}

// GetSessionCompletions returns completions for session IDs
// If includeExited is true, includes exited sessions (for kill command)
func GetSessionCompletions(includeExited bool) []string {
	states, err := ListSessionStates()
	if err != nil || len(states) == 0 {
		return nil
	}

	var completions []string
	for _, state := range states {
		RefreshSessionStatus(state)
		if !includeExited && state.Status == StatusExited {
			continue
		}

		// Format: ID_status_directory
		dir := state.Cwd
		if len(dir) > 30 {
			dir = "…" + dir[len(dir)-29:]
		}
		dir = strings.ReplaceAll(dir, " ", "_")

		completion := fmt.Sprintf("%s_%s_%s", state.ID, state.Status, dir)
		completions = append(completions, completion)
	}

	return completions
}
