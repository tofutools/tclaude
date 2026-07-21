package session

import (
	"crypto/rand"
	"encoding/hex"
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
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// SessionState represents the state of a Claude session
type SessionState struct {
	ID           string `json:"id"`
	TmuxSession  string `json:"tmuxSession"`
	PID          int    `json:"pid"`
	Cwd          string `json:"cwd"`
	ConvID       string `json:"convId,omitempty"`
	Status       string `json:"status"`
	StatusDetail string `json:"statusDetail,omitempty"`
	// SubagentCount is a derived cache of Subagents (recomputed by the
	// hook callback on every state-changing hook). Kept for read surfaces
	// that only need the raw figure; TTL-aware readers should use
	// Subagents.LiveCount instead.
	SubagentCount int `json:"subagentCount"`
	// Subagents is the ledger of sub-agents believed to be running under
	// this session, keyed by agent_id — see db.SubagentSet for the full
	// self-healing story (why not a bare counter).
	Subagents db.SubagentSet `json:"subagents,omitempty"`
	// BgShells is the ledger of background shell commands (Claude Code
	// `Bash` with run_in_background) believed to be running under this
	// session, keyed by backgroundTaskId — see db.BgShellSet for the
	// self-healing story (hooks announce a launch but never an exit, so
	// process liveness is the authoritative reconcile).
	BgShells db.BgShellSet `json:"bgShells,omitempty"`
	Created  time.Time     `json:"created"`
	Updated   time.Time      `json:"updated"`
	LastHook  time.Time      `json:"lastHook"`
	Attached  int            `json:"-"` // Number of attached clients (runtime only, not persisted)
	// Harness is the coding tool this session belongs to ("claude",
	// "codex"). Carried through toRow/fromRow so the hook callback's
	// load→mutate→save round-trip preserves a non-claude tag instead of
	// blanking it (which db.SaveSession would coalesce back to "claude").
	// Empty on a fresh state → coalesced to "claude" at the DB layer.
	Harness string `json:"harness,omitempty"`
	// SandboxMode is the launch-time OS-sandbox mode the session was
	// spawned under (Codex's --sandbox: read-only / workspace-write /
	// danger-full-access), or "" for a harness with no launch sandbox flag
	// (Claude Code). Set once at spawn by `session new`; carried through
	// toRow/fromRow so the hook callback's load→mutate→save round-trip
	// preserves it. Unlike Harness, "" is a genuine value (no sandbox) and
	// is stored verbatim. The dashboard renders it as a per-agent badge.
	SandboxMode      string                  `json:"sandboxMode,omitempty"`
	EffectiveSandbox *sandboxpolicy.Snapshot `json:"effectiveSandbox,omitempty"`
	ResumeProvenance string                  `json:"resumeProvenance,omitempty"`
	// ApprovalPolicy and ApprovalAutoReview preserve the resolved launch-time
	// approval posture across hook load/mutate/save cycles. They are recorded
	// for authorization and re-applied when the conversation is resumed.
	ApprovalPolicy     string `json:"approvalPolicy,omitempty"`
	ApprovalAutoReview bool   `json:"approvalAutoReview,omitempty"`
	// AskUserQuestionTimeout is the resolved Claude Code AskUserQuestion
	// idle-timeout (inherit|never|60s|5m|10m) the session was spawned under.
	// Set once at spawn by `session new` and carried through toRow/fromRow so
	// the hook callback's load→mutate→save round-trip preserves it; a relaunch
	// (resume / clone / reincarnate) reads it back to keep a per-agent timeout
	// across the handoff. "" for a pre-column row or a non-Claude harness.
	AskUserQuestionTimeout string `json:"askUserQuestionTimeout,omitempty"`
}

// Status constants
const (
	// StatusRunning marks a plain shell session (runNewShell): it has no
	// hook to report a finer-grained status, so it stays "running" for as
	// long as its tmux session / PID is alive and flips straight to
	// StatusExited when that ends (see RefreshSessionStatus).
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
	case StatusAwaitingPermission, StatusAwaitingInput, StatusError:
		return 0 // Red - needs attention, show first
	case StatusIdle:
		return 1 // Yellow
	case StatusMainAgentIdle:
		return 2 // Green
	case StatusWorking, StatusRunning:
		// StatusRunning is a plain shell session, alive and nothing to report
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
			WatchCmd(),
			AttachCmd(),
			FocusCmd(),
			GotoCmd(),
			KillCmd(),
			PruneCmd(),
			StatusCallbackCmd(),
			HookCallbackCmd(),
			ReplayCmd(),
			NotifyListenCmd(),
			codexProfileCleanupCmd(),
			exitCallbackCmd(),
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
		ID:                     s.ID,
		TmuxSession:            s.TmuxSession,
		PID:                    s.PID,
		Cwd:                    s.Cwd,
		ConvID:                 s.ConvID,
		Status:                 s.Status,
		StatusDetail:           s.StatusDetail,
		SubagentCount:          s.SubagentCount,
		SubagentsJSON:          s.Subagents.Encode(),
		BgShellsJSON:           s.BgShells.Encode(),
		CreatedAt:              s.Created,
		UpdatedAt:              s.Updated,
		LastHook:               s.LastHook,
		Harness:                s.Harness,
		SandboxMode:            s.SandboxMode,
		EffectiveSandbox:       s.EffectiveSandbox,
		ResumeProvenance:       s.ResumeProvenance,
		ApprovalPolicy:         s.ApprovalPolicy,
		ApprovalAutoReview:     s.ApprovalAutoReview,
		AskUserQuestionTimeout: s.AskUserQuestionTimeout,
	}
}

// fromRow converts a db.SessionRow to a SessionState.
func fromRow(r *db.SessionRow) *SessionState {
	return &SessionState{
		ID:                     r.ID,
		TmuxSession:            r.TmuxSession,
		PID:                    r.PID,
		Cwd:                    r.Cwd,
		ConvID:                 r.ConvID,
		Status:                 r.Status,
		StatusDetail:           r.StatusDetail,
		SubagentCount:          r.SubagentCount,
		Subagents:              db.ParseSubagentSet(r.SubagentsJSON),
		BgShells:               db.ParseBgShellSet(r.BgShellsJSON),
		Created:                r.CreatedAt,
		Updated:                r.UpdatedAt,
		LastHook:               r.LastHook,
		Harness:                r.Harness,
		SandboxMode:            r.SandboxMode,
		EffectiveSandbox:       r.EffectiveSandbox,
		ResumeProvenance:       r.ResumeProvenance,
		ApprovalPolicy:         r.ApprovalPolicy,
		ApprovalAutoReview:     r.ApprovalAutoReview,
		AskUserQuestionTimeout: r.AskUserQuestionTimeout,
	}
}

// SaveSessionState saves session state to the database.
func SaveSessionState(state *SessionState) error {
	state.Updated = time.Now()
	return db.SaveSession(toRow(state))
}

// SaveSessionStateForLaunch is the one launch-owned session write. Supplying
// the fresh generation makes row reuse atomically clear predecessor callback
// and lifecycle-intent authority in the same UPSERT that establishes the new
// launch. Generic hook saves never carry these write-only fields.
func SaveSessionStateForLaunch(state *SessionState, generation, gateState string) error {
	state.Updated = time.Now()
	row := toRow(state)
	row.ExitLaunchGeneration = generation
	row.ExitLaunchGateState = gateState
	return db.SaveSession(row)
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

// IsTmuxSessionAlive checks if a tmux session exists. The probe is
// exact-name (see clcommon.ExactTarget): a bare -t would prefix-match a
// live "-N" namesake and report a dead name as alive, which upstream turns
// into wrong-session attaches and kills.
func IsTmuxSessionAlive(sessionName string) bool {
	cmd := clcommon.TmuxCommand("has-session", "-t", clcommon.ExactTarget(sessionName))
	if cmd.Run() != nil {
		return false
	}
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t",
		clcommon.ExactTarget(sessionName)+":0.0", "#{pane_dead}").Output()
	return err != nil || strings.TrimSpace(string(out)) != "1"
}

// LiveTmuxSessions returns the set of session names currently alive
// on the tclaude tmux server, in one subprocess call. Snapshot-shaped
// callers (dashboard poll, group/peer list handlers) fetch this once
// at the top of an HTTP request and then test individual liveness via
// map lookup, replacing O(N) `has-session` fan-out with one `ls`.
// Thin wrapper over clcommon.Default.ListSessions so callers needn't
// depend on the clcommon boundary directly.
func LiveTmuxSessions() (map[string]struct{}, error) {
	return clcommon.Default.ListSessions()
}

// GetTmuxSessionAttachedCount returns the number of clients attached to a tmux session
// Returns 0 if session doesn't exist or on error
func GetTmuxSessionAttachedCount(sessionName string) int {
	cmd := clcommon.TmuxCommand("display-message", "-t", clcommon.ExactTarget(sessionName)+":", "-p", "#{session_attached}")
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

// DetachSessionClients detaches all clients from a tmux session and
// returns how many were detached. The tmux session — and the process
// running inside it — keeps running untouched; only the attached
// clients (the terminal windows) go away. A count of 0 means the
// session had no window open: a clean no-op.
func DetachSessionClients(sessionName string) (int, error) {
	// Get list of clients attached to this session
	cmd := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(sessionName), "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	// Detach each client
	detached := 0
	clients := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, client := range clients {
		if client == "" {
			continue
		}
		// Detach this specific client
		_ = clcommon.TmuxCommand("detach-client", "-t", client).Run()
		detached++
	}
	return detached, nil
}

// CheckTmuxInstalled verifies tmux is available
func CheckTmuxInstalled() error {
	_, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux is required but not installed. Install it with:\n  Ubuntu/Debian: sudo apt install tmux\n  macOS: brew install tmux")
	}
	return nil
}

// GenerateSessionID creates a unique synthetic session id, used as the
// session row's primary key when the conversation UUID isn't known at spawn
// time (a fresh, non-resumed session). 64 bits of crypto entropy — never a
// truncation of a longer id; only the tmux name / on-screen rendering are
// shortened (JOH-248). Falls back to nanosecond time if the system RNG fails.
func GenerateSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// UniqueTmuxSessionName keeps the short tmux name unique among live tmux
// sessions. tmux requires unique session names; two resumed conversations
// can share an 8-char prefix, and dir-style names (see TmuxNameBase) share
// a base whenever two sessions launch from the same directory — a taken
// base falls back to a -N suffix. "Short if possible": the bare base is
// used whenever it is free. (Best-effort: a racing creator between this
// check and `tmux new-session` just makes that spawn fail with a
// duplicate-name error — no corruption.)
func UniqueTmuxSessionName(base string) string {
	if base == "" || !IsTmuxSessionAlive(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !IsTmuxSessionAlive(candidate) {
			return candidate
		}
	}
	return base
}

// sessionHandle is the short, human-facing identifier for a session — its
// tmux name when set (a label stays whole; a conv-derived name is already the
// 8-char prefix), else the full id. Used for display and the attach hint; it
// resolves back to the session via findSession (JOH-248).
func sessionHandle(s *SessionState) string {
	if s.TmuxSession != "" {
		return s.TmuxSession
	}
	return s.ID
}

// liveSessionOwningID returns an existing, *live* session row already keyed by
// the given PK, or nil. Used to guard a label/synthetic-PK launch: unlike a
// resumed conv UUID (a stable identity that is fine to refresh), a label is a
// fresh identity each launch and can collide with a different live session.
// The tmux name is disambiguated separately, but the PK is not — so without
// this check SaveSessionState's ON CONFLICT(id) would silently overwrite (and
// orphan) the live session that already owns the label, reintroducing the very
// conflation JOH-248 removes. A row owned only by a *dead* session returns nil:
// recreating over an exited row is fine. See JOH-248.
func liveSessionOwningID(sessionID string) *SessionState {
	existing, err := LoadSessionState(sessionID)
	if err != nil || existing == nil {
		return nil
	}
	if IsTmuxSessionAlive(existing.TmuxSession) {
		return existing
	}
	return nil
}

// liveOwnerConflict runs the PK-collision guard both launch paths share
// (runNew / runNewShell). It returns the byte-identical "choose a different
// --label" error when a reused --label collides with a *live* session's PK.
// When a non-label PK collides it returns that owner (nil error) so the caller
// can craft its context-specific message (a resumed conversation vs a plain
// shell), and returns (nil, nil) when the PK is free. See JOH-248/JOH-332.
func liveOwnerConflict(sessionID, label string) (*SessionState, error) {
	owner := liveSessionOwningID(sessionID)
	if owner == nil {
		return nil, nil
	}
	if label != "" {
		return nil, fmt.Errorf("a live session already uses label %q (tmux %q); choose a different --label", sessionID, owner.TmuxSession)
	}
	return owner, nil
}

// LiveSessionForConv returns an existing, *live* session row for the given
// conversation id, or nil. It keys on conv_id — the conversation's stable
// identity — so it finds a live session regardless of that session's PK shape:
// a full-UUID resume PK, a fresh spawn's random synthetic PK, or a
// pre-de-truncation convID[:8] PK. A PK-keyed LoadSessionState lookup misses
// the latter two (their PK is not the conv UUID), which is how a manual resume
// of an already-live conversation slipped past the launch guards and ran a
// second `claude --resume` on the same .jsonl (interleaved appends → conv-file
// corruption). All three resume paths (session new -r, conv resume, the watch
// TUI) guard on this. See JOH-332.
//
// It probes ALL rows for the conv, not just the freshest: a conv can carry
// several rows (a stale spawn / old-PK row plus the live one), and a dead
// row's updated_at can be bumped above a live-but-idle row's frozen one (the
// reaper's MarkSessionExitedIfUnchanged, or a stale-handle attach), so a
// "most-recent row only" probe could miss the live session and wrongly allow a
// relaunch. Mirrors the all-rows liveness check in isConvOnline /
// pickAliveSession. Best-effort: the guard reads here and the new row is
// written later, so two truly-concurrent resumes can still race (as before) —
// tmux's unique-name rejection is the backstop for that window.
func LiveSessionForConv(convID string) *SessionState {
	if convID == "" {
		return nil
	}
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil
	}
	for _, row := range rows {
		if row != nil && IsTmuxSessionAlive(row.TmuxSession) {
			return fromRow(row)
		}
	}
	return nil
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

// MarkStateExited flips an in-memory state to exited and clears both
// activity ledgers — sub-agents run inside the (now dead) harness
// process and background shells are its children, so neither can
// survive it. Mirrors what the hook callback's SessionEnd arm and the
// reaper's MarkSessionExitedIfUnchanged do; the caller persists via
// SaveSessionState when needed.
func MarkStateExited(state *SessionState) {
	state.Status = StatusExited
	state.Subagents = nil
	state.SubagentCount = 0
	state.BgShells = nil
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
			MarkStateExited(state)
		}
		// If PID is alive, keep the current status (updated by hooks)
		return
	}

	// No tmux session and no PID - mark as exited
	MarkStateExited(state)
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
	cmd := clcommon.TmuxCommand("list-panes", "-t", clcommon.ExactTarget(sessionName)+":", "-F", "#{pane_pid}")
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

		completion := fmt.Sprintf("%s_%s_%s", sessionHandle(state), state.Status, dir)
		completions = append(completions, completion)
	}

	return completions
}

// TaskSignalPath returns a per-project path to the task signal file,
// allowing concurrent task runners in different projects.
func TaskSignalPath(cwd string) string {
	return filepath.Join(common.CacheDir(), "task-signal-"+convops.PathToProjectDir(cwd))
}
