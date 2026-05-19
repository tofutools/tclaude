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
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/common"
)

var safeSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// HookCallbackInput represents the JSON input from any Claude Code hook
type HookCallbackInput struct {
	ConvID               string          `json:"session_id"` // claude's session id, what we call conv_id
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	PermissionMode       string          `json:"permission_mode,omitempty"`
	HookEventName        string          `json:"hook_event_name"`
	NotificationType     string          `json:"notification_type,omitempty"`
	Reason               string          `json:"reason,omitempty"` // SessionEnd: clear | logout | prompt_input_exit | other
	Message              string          `json:"message,omitempty"`
	Prompt               string          `json:"prompt,omitempty"`
	StopHookActive       bool            `json:"stop_hook_active,omitempty"`
	ToolName             string          `json:"tool_name,omitempty"`
	ToolInput            json.RawMessage `json:"tool_input,omitempty"`
	AgentType            string          `json:"agent_type,omitempty"`
	AgentID              string          `json:"agent_id,omitempty"`
	LastAssistantMessage string          `json:"last_assistant_message,omitempty"`
	// StopFailure: error_type is one of rate_limit, authentication_failed,
	// oauth_org_not_allowed, billing_error, invalid_request, server_error,
	// max_output_tokens, unknown; error_message is the human-readable string.
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
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

// sessionEndIsExit reports whether a SessionEnd hook's `reason` means
// the Claude Code process is actually going away. A /clear ends the
// conversation but keeps the process alive (a fresh SessionStart
// follows immediately), so it is NOT an exit. Every other reason
// (logout, prompt_input_exit, other) is. An empty reason is treated as
// an exit — better to over-report "exited" (the reaper / next hook
// will correct a live session) than to leave a dead one as "idle".
func sessionEndIsExit(reason string) bool {
	return reason != "clear"
}

// needsIdentityMigration reports whether a conv-id rotation on an
// env-keyed session is a /clear whose agent identity still has to be
// migrated old → new.
//
// Returns (true, nil) when oldConv is a live agent, newConv is not
// already an agent of its own, and no succession edge has been recorded
// for oldConv yet. Returns (false, nil) when one of those checks has
// concrete evidence migration is unnecessary (oldConv not an active
// agent, newConv already an agent, succession edge already in place).
// Returns (false, err) when a DB read failed — the caller must NOT
// advance the session row's conv-id in that case: a transient SQLite
// fault here followed by an advance would skip the migration entirely
// and strand identity, defeating the retry below.
//
// The (true, nil) conditions hold for the post-/clear SessionStart AND
// for every later hook until the migration succeeds — so a migration
// that fails on the SessionStart hook (a transient SQLite error) is
// simply retried on the next hook (db.MigrateAgentIdentity is atomic +
// idempotent: a failed attempt records no succession edge, so the
// predicate stays true; a committed one records the edge, so it flips
// false). The predicate IS the retry condition — no extra bookkeeping
// needed.
//
// Why this tells a /clear apart from a /resume without keying on the
// hook source: an env-keyed tclaude session's conv-id only ever rotates
// mid-life via /clear. A resume is always a fresh `tclaude session`
// with its own TCLAUDE_SESSION_ID, so its first hook records the
// conv-id from scratch (oldConv == "" — not a rotation). The
// newConv-not-already-an-agent guard is belt-and-braces against a
// future in-session conversation switch landing on a conv that already
// owns an identity.
func needsIdentityMigration(oldConv, newConv string) (bool, error) {
	oldEnr, err := db.EnrollmentState(oldConv)
	if err != nil {
		return false, err
	}
	if oldEnr != db.EnrollmentActive {
		return false, nil
	}
	newEnr, err := db.EnrollmentState(newConv)
	if err != nil {
		return false, err
	}
	if newEnr == db.EnrollmentActive {
		return false, nil
	}
	succ, err := db.GetConvSuccessor(oldConv)
	if err != nil {
		return false, err
	}
	if succ != "" {
		return false, nil
	}
	return true, nil
}

// migrateAgentIdentity is the indirection seam test code uses to inject
// a transient migration failure. Production code is the direct
// db.MigrateAgentIdentity call; tests swap it via
// SetMigrateAgentIdentityForTest (testhooks_test.go) to assert the retry
// path described on needsIdentityMigration above.
var migrateAgentIdentity = db.MigrateAgentIdentity

// migrateClearedIdentity migrates agent identity — group memberships,
// ownerships, permission overrides, cron refs, the succession edge and
// the display name — from a /clear'd conv-id onto the fresh one
// (db.MigrateAgentIdentity, which also retires the old enrollment so
// it lands on the retired-agents roster, reactivatable later for
// knowledge pings), then restores the conversation title that /clear
// wiped.
//
// Returns true when the migration committed (the caller may then record
// the new conv-id on the session row), false when it failed — in which
// case the caller leaves the session row on the old conv-id so the next
// hook retries (see needsIdentityMigration). The migration is atomic,
// so a failure strands nothing: identity stays wholly on oldConv.
func migrateClearedIdentity(state *SessionState, newConv string) bool {
	// Freshen the old conv's conv_index from its .jsonl before the
	// migration carries the display name. An agent's /rename of itself
	// lands as a customTitle turn in the .jsonl, and conv_index may not
	// have been re-scanned since — without this the carried name (and
	// so the /rename restore below) could miss a recent rename.
	// Best-effort: a missing file / unindexable .jsonl is a no-op.
	if state.Cwd != "" {
		if projectDir := convops.GetClaudeProjectPath(state.Cwd); projectDir != "" {
			convops.ScanAndUpsertFile(filepath.Join(projectDir, state.ConvID+".jsonl"))
		}
	}
	mig, err := migrateAgentIdentity(state.ConvID, newConv, "clear", "system:clear")
	if err != nil {
		slog.Error("clear-migrate: agent identity migration failed (will retry on next hook)",
			"old_conv", state.ConvID, "new_conv", newConv, "error", err, "module", "hooks")
		return false
	}
	slog.Info("clear-migrate: agent identity migrated across /clear",
		"old_conv", state.ConvID, "new_conv", newConv,
		"migrated", mig.Items, "module", "hooks")
	// /clear wiped CC's conversation title. db.MigrateAgentIdentity
	// already carried the name onto pending_name (so the dashboard shows
	// it at once); inject /rename so the new conversation also regains a
	// real customTitle turn — durable, visible in CC's own UI, and on
	// every other surface.
	restoreClearedTitle(state.TmuxSession, mig.CarriedName)
	return true
}

// clearInjectAliveTimeout caps how long restoreClearedTitle polls for
// the agent's tmux pane to be alive before giving up on the /rename
// injection. The pane was alive a moment ago (CC just fired this hook
// from within it), so the poll usually returns immediately — the
// timeout matters only in pathological cases (pane killed during
// /clear). Declared `var` so flow tests can shrink via
// SetClearInjectTimingsForTest.
var clearInjectAliveTimeout = 5 * time.Second

// clearInjectReadyDelay is how long we sleep after the pane is alive
// before injecting any keys. CC's input box may need a moment to
// settle after a /clear redrew the screen; without this, keystrokes
// can land mid-render. Same `var` rationale as the timeout above —
// flow tests shrink it.
var clearInjectReadyDelay = 1 * time.Second

// restoreClearedTitle injects `/rename <title>` into the agent's tmux
// pane so a /clear'd conversation regains its name. Best-effort: an
// empty tmux session, an empty title, a title that fails the strict
// rename charset gate, a dead pane, or a send-keys failure all just
// fall through to the pending_name dashboard fallback that
// db.MigrateAgentIdentity already set.
//
// Replicates injectTextAndSubmit's shape from
// pkg/claude/agentd/handlers.go (text → 500 ms gap → Enter → 500 ms
// gap → Enter) so CC's bracketed-paste mode can't coalesce the
// trailing Enter into a paste-newline — the foot-gun reincarnate's
// handoff nudge originally tripped on. We can't import the agentd
// helper directly from session (would cycle), and the cold reviewer
// explicitly asked us to replicate the shape rather than reinvent.
//
// Charset gate is isValidRenameTitle — the strict gate documented at
// pkg/claude/agentd/handlers.go's runRenameOrchestration as "a hard
// security gate against keystroke injection ... not bypassable". The
// carried name comes from conv_index.custom_title (parsed verbatim
// from .jsonl files) or pending_name (stored even when invalid by
// lifecycle.go) — neither is pre-checked by the strict gate, so the
// gate runs here.
func restoreClearedTitle(tmuxSession, title string) {
	if tmuxSession == "" || title == "" {
		return
	}
	if !isValidRenameTitle(title) {
		slog.Warn("clear-migrate: carried title rejected by rename charset gate; relying on pending_name",
			"title", title, "module", "hooks")
		return
	}
	// Wait until the pane is reported alive, then sleep readyDelay so
	// CC's TUI has settled after the /clear. Mirrors reincarnate's
	// waitForConvAlive pattern. Polling is belt-and-suspenders: a
	// /clear keeps the same process and pane alive, so this typically
	// returns immediately.
	if !waitClearInjectPaneReady(tmuxSession) {
		slog.Warn("clear-migrate: tmux pane never became ready for /rename injection; relying on pending_name",
			"tmux", tmuxSession, "module", "hooks")
		return
	}
	target := tmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "/rename "+title).Run(); err != nil {
		slog.Warn("clear-migrate: /rename injection failed; relying on pending_name",
			"error", err, "module", "hooks")
		return
	}
	time.Sleep(500 * time.Millisecond)
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("clear-migrate: /rename submit failed; relying on pending_name",
			"error", err, "module", "hooks")
		return
	}
	// Belt-and-suspenders second Enter (no-op if the first already
	// submitted) — same pattern as agentd.injectTextAndSubmit. The 500
	// ms gap before it keeps the second Enter from being coalesced
	// into the same bracketed-paste window as the text.
	time.Sleep(500 * time.Millisecond)
	_ = clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run()
}

// waitClearInjectPaneReady polls IsTmuxSessionAlive on tmuxSession
// until it reports alive or the alive-timeout elapses, then sleeps
// the ready-delay so CC's TUI settles. Returns true on a settled
// pane, false on timeout.
func waitClearInjectPaneReady(tmuxSession string) bool {
	deadline := time.Now().Add(clearInjectAliveTimeout)
	for time.Now().Before(deadline) {
		if IsTmuxSessionAlive(tmuxSession) {
			time.Sleep(clearInjectReadyDelay)
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// isValidRenameTitle mirrors the daemon-side gate in
// pkg/claude/agentd/handlers.go. Kept in sync deliberately: agentd is
// the authoritative gate for cross-agent renames, but the /clear
// title-restore injection happens from inside the hook callback (a
// separate subprocess that can't import the daemon package without
// cycling), and we want the SAME strict charset to govern keystrokes
// before send-keys hits the pty — anything else would re-open the
// injection sink the daemon path closed. The agentd unit test
// TestIsValidRenameTitle is the authoritative spec; this mirror must
// stay aligned.
func isValidRenameTitle(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	if strings.Contains(t, "  ") {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		case r == '[' || r == ']' || r == '{' || r == '}':
		case r == '(' || r == ')':
		case r == ' ':
		default:
			return false
		}
	}
	return true
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

	return ApplyHook(input, envSessionID)
}

// ApplyHook applies a single parsed Claude Code hook event to session
// state. It is the body of the hook callback split out from the
// stdin/env/record-hooks IO in runHookCallback, so the hook logic can
// be driven programmatically — by the flow-test simulator's /clear
// behaviour, and by hook_callback_test.go — without poking os.Stdin or
// the process environment.
//
// envSessionID is the TCLAUDE_SESSION_ID of the calling session ("" for
// a session not launched by tclaude); it is the stable key that lets a
// conv-id rotation (/clear, /resume) be tracked across the rotation.
func ApplyHook(input HookCallbackInput, envSessionID string) error {
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
	state, err := getOrCreateSessionState(input, envSessionID)
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
		// Track where the agent is building: a file-editing tool just
		// ran, so the file's directory is the most-relevant "working
		// dir" — distinct from input.Cwd, which is the launch dir. We
		// also resolve that dir's git worktree root + branch here, so
		// read surfaces report the agent's *current* branch (correct
		// when it hops between sub-repos) rather than the launch dir's.
		// Recorded per conv-id; the daemon's read paths use it back.
		// Best-effort: a failed UpsertAgentWorkdir just leaves the
		// previous value in place.
		if dir, ok := WorkDirFromToolUse(input.ToolName, input.ToolInput, input.Cwd); ok {
			worktreeRoot, branch := GitLocationOf(dir)
			if err := db.UpsertAgentWorkdir(input.ConvID, dir, worktreeRoot, branch); err != nil {
				slog.Warn("failed to record agent workdir", "error", err, "module", "hooks")
			}
			// Append the branch to the conv's history. This catches a
			// branch in a worktree the launch-dir .jsonl never names —
			// Claude Code stamps only the launch repo's branch onto each
			// turn, so the .jsonl re-scan alone would miss it. An empty
			// branch (edit outside a git repo) is a silent no-op.
			if err := db.AppendConvBranchHistoryHook(input.ConvID, branch, worktreeRoot); err != nil {
				slog.Warn("failed to record branch history", "error", err, "module", "hooks")
			}
		}

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

	case "StopFailure":
		// The turn ended because of an API/auth/billing error rather
		// than completing normally (CC fires StopFailure instead of
		// Stop). Mark the agent "error" with error_type as the detail
		// so the dashboard can surface it (e.g. "error: rate_limit").
		//
		// This status is TRANSIENT, not sticky: every other hook case
		// here sets state.Status unconditionally, so the next normal
		// event (UserPromptSubmit, a tool event, a later Stop) clears
		// it back to working/idle. A retried agent leaves the error
		// state on its own — nothing else has to reset it.
		//
		// Deliberately NOT setting stopped=true (unlike the Stop case):
		// the stopped branch drives auto-compact, the context nudge and
		// the task-runner signal — all of which would "act on" the
		// error (typing /compact or a nudge into a broken pane, or
		// reporting a half-finished task as done). Acting on an error
		// is explicitly out of scope here. The status transition and
		// the desktop notification (notify.OnStateTransition below)
		// both fire regardless of the stopped flag.
		state.Status = StatusError
		state.StatusDetail = input.ErrorType
		if state.StatusDetail == "" {
			state.StatusDetail = "unknown"
		}
		slog.Warn("agent turn ended in error",
			"conv_id", input.ConvID,
			"error_type", input.ErrorType,
			"error_message", input.ErrorMessage,
			"module", "hooks",
		)

	case "SessionStart":
		// Session started or resumed - update ConvID and set to idle
		state.Status = StatusIdle
		state.StatusDetail = ""
		// The conversation is alive again — drop any exit_reason a
		// previous exit (or the reaper) recorded. Cleared conv-wide, not
		// just for this row: a conv can own several session rows and the
		// dashboard reads exit_reason off whichever is most recent, so a
		// stale reason left on a sibling row could later be misread as a
		// crash.
		if state.ConvID != "" {
			if err := db.ClearSessionExitReasonByConv(state.ConvID); err != nil {
				slog.Warn("failed to clear exit reason", "error", err, "module", "hooks")
			}
		}

	case "SessionEnd":
		// Claude Code is shutting down this conversation. The `reason`
		// field tells a real process exit apart from a /clear, which
		// ends the conversation but keeps the process alive and fires a
		// fresh SessionStart immediately after — so /clear must NOT mark
		// the session exited. logout / prompt_input_exit / other all
		// mean the process is going away.
		if !sessionEndIsExit(input.Reason) {
			if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
				slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
			}
			return nil
		}
		state.Status = StatusExited
		state.StatusDetail = ""
		// Record the graceful-exit reason (logout / prompt_input_exit /
		// resume / bypass_permissions_disabled / other) so the dashboard
		// can tell this clean exit from an unexpected death — for which
		// no SessionEnd fires and the session reaper stamps 'unexpected'.
		if err := db.SetSessionExitReason(state.ID, input.Reason); err != nil {
			slog.Warn("failed to record exit reason", "error", err, "module", "hooks")
		}

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
		case "idle_prompt":
			// CC has been idle and is waiting for user input. This is
			// our only signal back to idle after the user cancels an
			// in-flight turn with Escape: Stop does NOT fire on
			// interrupt (anthropics/claude-code#11189, closed as
			// not-planned), so without this case the agent stays stuck
			// at e.g. "working: UserPromptSubmit". CC's idle detection
			// runs on its own ~60s timer, so recovery is delayed, not
			// instant. Deliberately NOT setting stopped=true — that
			// branch types /compact and context-nudges into the pane,
			// which would collide with a user mid-typing.
			state.Status = StatusIdle
			state.StatusDetail = ""
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
		handleAutoCompact(input, envSessionID)
		handleContextNudge(input, envSessionID)
	}

	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes). A
	// /clear rotates the conv-id; needsIdentityMigration / migrateClearedIdentity
	// handle moving the agent's identity across that rotation.
	if input.ConvID != "" && state.ConvID != input.ConvID {
		switch {
		case envSessionID == "" || state.ConvID == "":
			// Not an env-keyed rotation we can migrate identity across
			// (a non-tclaude session, or the session's first conv-id
			// record). Plain advance — the pre-/clear-fix behaviour.
			slog.Info("updating conversation ID",
				"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
				"session_id", state.ID, "module", "hooks")
			state.ConvID = input.ConvID
		default:
			shouldMigrate, predErr := needsIdentityMigration(state.ConvID, input.ConvID)
			switch {
			case predErr != nil:
				// A transient DB error trying to decide. Do NOT advance:
				// advancing on an "I don't know" answer would skip the
				// migration if the truth was "migrate," and identity
				// would strand. The next hook re-evaluates the predicate
				// (the rotation is still visible since we left ConvID
				// alone).
				slog.Warn("clear-migrate: predicate check failed; deferring conv-id advance to the next hook",
					"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
					"session_id", state.ID, "error", predErr, "module", "hooks")
			case shouldMigrate:
				// A /clear rotated the conv-id and the old conv is an
				// agent whose identity has not moved yet. Migrate it
				// BEFORE recording the new conv-id (the migration needs
				// the old value). On a migration failure DO NOT advance
				// state.ConvID: the migration is atomic so identity is
				// still wholly on the old conv-id — keeping the session
				// row there means needsIdentityMigration still fires on
				// the next hook and the (idempotent) migration is
				// retried, rather than the conv-id silently advancing
				// to a conv whose identity never arrived (issue #192).
				if migrateClearedIdentity(state, input.ConvID) {
					slog.Info("updating conversation ID after /clear",
						"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
						"session_id", state.ID, "module", "hooks")
					state.ConvID = input.ConvID
				} else {
					slog.Warn("clear-migrate: deferring conv-id advance until the migration succeeds",
						"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
						"session_id", state.ID, "module", "hooks")
				}
			default:
				// Predicate said no — the rotation does not need
				// identity migration (oldConv not an agent, newConv
				// already an agent, or the edge is already recorded).
				// Advance normally.
				slog.Info("updating conversation ID",
					"old_conv_id", state.ConvID, "new_conv_id", input.ConvID,
					"session_id", state.ID, "module", "hooks")
				state.ConvID = input.ConvID
			}
		}
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

// getOrCreateSessionState finds existing session or creates a new one.
// envSessionID is the caller's TCLAUDE_SESSION_ID ("" when the session
// was not launched by tclaude).
func getOrCreateSessionState(input HookCallbackInput, envSessionID string) (*SessionState, error) {
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
func handleAutoCompact(input HookCallbackInput, sessionID string) {
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
func handleContextNudge(input HookCallbackInput, sessionID string) {
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
