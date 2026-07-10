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
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	Reason               string          `json:"reason,omitempty"`  // SessionEnd: clear | resume | logout | prompt_input_exit | bypass_permissions_disabled | other
	Source               string          `json:"source,omitempty"`  // SessionStart: startup | resume | clear | compact
	Trigger              string          `json:"trigger,omitempty"` // PreCompact: auto | manual
	Message              string          `json:"message,omitempty"`
	Prompt               string          `json:"prompt,omitempty"`
	Model                string          `json:"model,omitempty"`
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
// the Claude Code process is actually going away. Two reasons end the
// conversation but keep the process alive, so they are NOT exits:
//   - "clear": a /clear — a fresh SessionStart(source=clear) follows
//     immediately.
//   - "resume": an interactive /resume switching to another
//     conversation — a SessionStart(source=resume) for the new conv
//     follows immediately. (Claude Code 2.1.79 started firing
//     SessionEnd for this; treating it as an exit produced a spurious
//     "Exited" notification on every conversation switch.)
//
// Every other reason (logout, prompt_input_exit,
// bypass_permissions_disabled, other) is an exit. An empty reason is
// treated as an exit — better to over-report "exited" (the reaper /
// next hook will correct a live session) than to leave a dead one as
// "idle".
func sessionEndIsExit(reason string) bool {
	return reason != "clear" && reason != "resume"
}

// isConvTransitionStart reports whether a hook is a SessionStart that
// announces an in-process conversation transition — the only events
// allowed to carry a conv-id different from the one an env-keyed
// session row tracks. `source` names the transition: "clear" (/clear),
// "resume" (interactive /resume switch), "compact" (auto or manual
// compaction). A SessionStart with source "startup" (or none) and a
// mismatched conv-id is a different claude PROCESS booting in this
// session's pane env — a foreign event, not a transition.
//
// Known gap: a one-shot child started with `claude -p --resume <id>` /
// `--continue` also reports source=resume, so it passes as a
// transition and can still drive the conv-advance below. Conv-id
// matching cannot tell that child from the host's own /resume switch —
// discriminating would need process identity (PID/PPID), which hook
// inputs don't carry. Accepted residual: plain one-shots (`claude -p`,
// `claude mcp …`, source=startup) are the case observed in production;
// resumed one-shots inside an agent's pane are rare and deliberate.
func isConvTransitionStart(input HookCallbackInput) bool {
	if input.HookEventName != "SessionStart" {
		return false
	}
	switch input.Source {
	case "clear", "resume", "compact":
		return true
	}
	return false
}

// taskSignalPath returns the task-runner signal-file path and whether
// this hook fired under `tclaude task run`. The runner sets
// TCLAUDE_TASK_SIGNAL on every Claude it spawns (the Stop-hook signal
// channel that drives the hands-free /exit between tasks); the hook
// subprocess inherits it. The path must resolve inside CacheDir — the
// only directory we will ever write the signal into — so an
// inherited-but-bogus value can neither land a stray file nor relax the
// task-mode hook exemptions below. A set-but-out-of-bounds path returns
// ("", false); handleTaskSignal logs that case (it is the one place the
// path is consumed for a write).
//
// The bound holds even in the degenerate no-HOME case where CacheDir()
// resolves to the relative "tclaude": the producer (session.TaskSignalPath)
// builds the path from the SAME CacheDir(), so filepath.Rel still contains
// correctly — and a cross-process anchor mismatch just makes Rel error,
// failing closed.
func taskSignalPath() (string, bool) {
	signalPath := os.Getenv("TCLAUDE_TASK_SIGNAL")
	if signalPath == "" {
		return "", false
	}
	allowedDir := filepath.Clean(common.CacheDir())
	cleanPath := filepath.Clean(signalPath)
	rel, err := filepath.Rel(allowedDir, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return cleanPath, true
}

// inTaskRunnerHook reports whether this hook fired under `tclaude task
// run` (a valid TCLAUDE_TASK_SIGNAL is present — see taskSignalPath).
//
// Task mode is exempt from the foreign-process guard, the conv-advance
// identity-migration path, and instant agent-enrollment, because the
// runner is, by design, a SEQUENCE of independent Claude conversations
// under ONE env-session: one fresh conv per task in TODO.md. So the
// tracked conv-id legitimately rotates at every task boundary via a plain
// SessionStart(source=startup), which is indistinguishable — by conv-id
// alone — from a foreign one-shot the guard exists to drop. Left guarded,
// the runner's second task and everything after it lose their hooks (the
// Stop hook that signals task completion never lands) and the run wedges
// — the #284 regression Mikael hit. The conv-id-vs-env-session ambiguity
// is inherent to the guard (its own doc comment notes hook inputs carry
// no process identity); exempting only task mode keeps the guard fully in
// force for every interactive agent session, the case #284 protects.
func inTaskRunnerHook() bool {
	_, ok := taskSignalPath()
	return ok
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
// simply retried on the next hook (db.RotateAgentConv is atomic +
// idempotent: a failed attempt records no succession edge, so the
// predicate stays true; a committed one records the edge, so it flips
// false). The predicate IS the retry condition — no extra bookkeeping
// needed.
//
// On rotation causes: a `tclaude agent resume` is always a fresh
// `tclaude session` with its own TCLAUDE_SESSION_ID, so its first hook
// records the conv-id from scratch (oldConv == "" — not a rotation).
// Mid-life rotations that reach this predicate are the transition
// SessionStarts the foreign-process guard admits (source clear /
// resume / compact — see isConvTransitionStart); an interactive
// /resume switch onto a conv that already owns an identity is covered
// by the newConv-not-already-an-agent guard, and one onto a plain conv
// migrates identity along — the agent follows its operator across the
// switch.
func needsIdentityMigration(oldConv, newConv string) (bool, error) {
	oldState, err := db.AgentState(oldConv)
	if err != nil {
		return false, err
	}
	if oldState != db.AgentStateActive {
		return false, nil
	}
	newState, err := db.AgentState(newConv)
	if err != nil {
		return false, err
	}
	if newState == db.AgentStateActive {
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

// rotateAgentConv is the indirection seam test code uses to inject a
// transient rotation failure. Production code is the direct
// db.RotateAgentConv call; tests swap it via SetRotateAgentConvForTest
// (testhooks.go) to assert the retry path described on
// needsIdentityMigration above.
var rotateAgentConv = db.RotateAgentConv

// notifyOnStateTransition is the seam the hook callback notifies
// through. Production is the direct notify.OnStateTransition (config +
// cooldown + mute ladder all live inside it); tests swap it to assert
// WHEN the callback notifies versus stays silent — e.g. the task-mode
// suppression — without standing up a real notification backend.
var notifyOnStateTransition = notify.OnStateTransition

// migrateClearedIdentity advances the actor across a /clear: it links the fresh
// conv-id onto the same agent_id, moves the live pointer, records the succession
// edge and carries the display name (db.RotateAgentConv — agents-table only
// since JOH-26 PR3c, so no enrollment to retire; the predecessor is simply a
// past generation of the still-active actor), then restores the conversation
// title that /clear wiped.
//
// Returns true when the rotation committed (the caller may then record the new
// conv-id on the session row), false when it failed — in which case the caller
// leaves the session row on the old conv-id so the next hook retries (see
// needsIdentityMigration). The rotation is atomic, so a failure strands nothing:
// identity stays wholly on oldConv.
func migrateClearedIdentity(state *SessionState, newConv string) bool {
	// Freshen the old conv's conv_index from its .jsonl before the
	// rotation carries the display name. An agent's /rename of itself
	// lands as a customTitle turn in the .jsonl, and conv_index may not
	// have been re-scanned since — without this the carried name (and
	// so the /rename restore below) could miss a recent rename.
	// Best-effort: a missing file / unindexable .jsonl is a no-op.
	if state.Cwd != "" {
		if projectDir := convops.GetClaudeProjectPath(state.Cwd); projectDir != "" {
			convops.ScanAndUpsertFile(filepath.Join(projectDir, state.ConvID+".jsonl"))
		}
	}
	carriedName, err := rotateAgentConv(state.ConvID, newConv, "clear")
	if err != nil {
		slog.Error("clear-migrate: agent identity rotation failed (will retry on next hook)",
			"old_conv", state.ConvID, "new_conv", newConv, "error", err, "module", "hooks")
		return false
	}
	slog.Info("clear-migrate: agent identity advanced across /clear",
		"old_conv", state.ConvID, "new_conv", newConv, "module", "hooks")
	// /clear wiped CC's conversation title. db.RotateAgentConv already carried
	// the name onto the actor's pending_name (so the dashboard shows it at
	// once); inject /rename so the new conversation also regains a real
	// customTitle turn — durable, visible in CC's own UI, and on every other
	// surface.
	restoreClearedTitle(state.TmuxSession, carriedName)
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
// db.RotateAgentConv already carried onto the actor.
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
	target := clcommon.ExactTarget(tmuxSession) + ":0.0"
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

	// PreCompact is a gate, not a status transition: it may write a
	// {"decision":"block"} back to Claude Code to refuse an early
	// auto-compaction. Handle it on its own path (it does not flow
	// through ApplyHook's status machinery) and emit any decision to
	// the hook's stdout. Codex still reports its active model on this
	// event, so persist it first when the hook belongs to the tracked
	// main conversation; a blocked compaction has no PostCompact backstop.
	if input.HookEventName == "PreCompact" {
		sessionKey := envSessionID
		if sessionKey == "" {
			sessionKey = input.ConvID
		}
		if sessionKey != "" {
			unlock, err := acquireHookLock(sessionKey)
			if err != nil {
				return fmt.Errorf("failed to acquire PreCompact hook lock: %w", err)
			}
			defer unlock()
		}
		if state, err := LoadSessionState(envSessionID); err == nil {
			persistCodexHookModel(state, input)
		}
		return decidePreCompact(input, envSessionID, os.Stdout)
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

	// A shell row never has a ConvID, so the foreign-process guard below
	// (keyed off state.ConvID != "") can never engage for one. runNewShell
	// still exports TCLAUDE_SESSION_ID (goto/focus need it), so a headless
	// coding-harness run launched from inside the shell (`claude -p "hi"`,
	// an interactive `claude`, …) inherits it and its hooks land here,
	// against the shell's own row. Without this guard that hijacks the
	// row: the throwaway conv-id gets stamped onto it, its PID gets
	// rewritten via FindClaudePID, it gets enrolled as a dashboard agent,
	// and it flips to "exited" when the child exits — while the shell
	// itself is still alive. A shell row has no hooks of its own, ever,
	// so drop every hook unconditionally.
	if state.Harness == ShellHarnessName {
		return nil
	}

	// Foreign-process guard. An env-keyed session's hooks normally all
	// carry the conversation its row tracks. A hook carrying a DIFFERENT
	// conv-id is one of two things:
	//
	//   - an in-process conversation transition (/clear, an interactive
	//     /resume switch, compaction) — always announced by a
	//     SessionStart whose `source` names the transition; or
	//   - a FOREIGN process's event: a one-shot headless claude run
	//     (`claude -p`, `claude mcp get`, …) launched from this
	//     session's own Bash, inheriting TCLAUDE_SESSION_ID, firing
	//     hooks for its own throwaway conv against OUR row.
	//
	// Foreign events must be dropped wholesale: processing one flips the
	// live session's status (a notified "Exited" for a 2-second `claude
	// mcp get`; an idle stamp from the child's Stop that can fire a
	// context nudge mid-turn), and the conv-advance logic below would
	// read the rotation as a /clear and migrate the agent's identity
	// onto the throwaway conv — observed in production as a live agent
	// retired "superseded by <conv> (clear)" where <conv> was a plugin
	// probe's conv-id.
	//
	// A transition SessionStart records its new conv-id as pending_conv
	// BEFORE the row advances, so the migration-failure retry keeps
	// working: post-/clear hooks carry the announced conv-id and pass
	// this guard via the pending_conv match, while a foreign conv-id
	// was never announced and cannot match. PostCompact is exempt — it
	// only resets per-env-session compact state and returns before any
	// status or conv mutation, and it may legitimately arrive carrying
	// a rotated conv-id.
	//
	// `tclaude task run` is exempt too (inTaskRunnerHook): it drives a
	// sequence of independent conversations under one env-session — one
	// fresh conv per task — so its conv-id rotations are legitimate, not
	// foreign. See inTaskRunnerHook for the full rationale.
	if !inTaskRunnerHook() &&
		envSessionID != "" && state.ConvID != "" && input.ConvID != "" &&
		input.ConvID != state.ConvID &&
		input.HookEventName != "PostCompact" {
		if isConvTransitionStart(input) {
			// Announce the rotation. Persisted immediately (not via the
			// SaveSessionState at the end of this call) so a crash or
			// migration failure mid-call still leaves the announcement
			// for the retry on the next hook. If THIS write fails too,
			// the retry hooks get dropped as foreign and the rotation
			// only converges at the next transition SessionStart —
			// accepted: it takes two correlated SQLite faults in one
			// call to get there.
			if err := db.SetSessionPendingConv(state.ID, input.ConvID); err != nil {
				slog.Warn("failed to record pending conv", "error", err, "module", "hooks")
			}
		} else if pending, err := db.GetSessionPendingConv(state.ID); err != nil || pending != input.ConvID {
			if err != nil {
				slog.Warn("failed to read pending conv; dropping mismatched-conv hook", "error", err, "module", "hooks")
			} else {
				slog.Info("ignoring hook from foreign claude process",
					"event", input.HookEventName, "foreign_conv", input.ConvID,
					"tracked_conv", state.ConvID, "session_id", state.ID, "module", "hooks")
			}
			// Deliberately NOT stamping last_hook: a foreign process's
			// event is no evidence the host session itself is alive.
			return nil
		}
	}

	// A tclaude-spawned Codex pane can miss its earliest hooks while it is
	// behind a startup gate or a transient hook-install race. As soon as any
	// later hook carries the first conv-id, persist it immediately, before
	// event-specific early returns (unknown events, PostCompact, etc.) can skip
	// the normal SaveSessionState tail. This is also the exact signal the
	// pending-spawn sweeper waits on.
	if envSessionID != "" && state.ConvID == "" && input.ConvID != "" {
		if err := db.SetSessionConvID(state.ID, input.ConvID); err != nil {
			slog.Warn("failed to backfill session conv-id from hook",
				"session_id", state.ID, "conv_id", input.ConvID, "error", err, "module", "hooks")
		} else {
			state.ConvID = input.ConvID
		}
	}
	if state.Cwd == "" && input.Cwd != "" {
		state.Cwd = input.Cwd
	}

	// Capture previous status for notification
	prevStatus := state.Status

	stopped := false

	state.LastHook = time.Now()

	// ---- Sub-agent ledger (db.SubagentSet) ----
	// The hook stream is LOSSY: Claude Code fires no hooks at all on a
	// user interrupt (anthropics/claude-code#11189) and SubagentStop has
	// no documented guarantee for aborts/errors/process death — so the
	// ledger, not the event stream, is what the "🤖+N" badge trusts.
	// Sweep expired entries first (self-healing for a lost SubagentStop),
	// then apply this event's evidence: Start/Stop maintain the set, and
	// any OTHER hook carrying agent_id proves that sub-agent is alive —
	// Sight() re-adds one whose SubagentStart was lost.
	state.Subagents.Sweep(state.LastHook)
	switch {
	case input.HookEventName == "SubagentStart":
		state.Subagents = state.Subagents.Add(input.AgentID, input.AgentType, state.LastHook)
	case input.HookEventName == "SubagentStop":
		state.Subagents.Remove(input.AgentID)
	case input.AgentID != "" && input.HookEventName == "SessionEnd":
		// A sub-agent's own conversation ending is as good as a
		// SubagentStop for the ledger (the main-thread status handling
		// of this event stays in the big switch below).
		state.Subagents.Remove(input.AgentID)
	case input.AgentID != "":
		state.Subagents = state.Subagents.Sight(input.AgentID, input.AgentType, state.LastHook)
	}

	// Hooks fired from INSIDE a sub-agent (agent_id set) must not drive
	// the main thread's status machine: before this gate, a background
	// sub-agent's PreToolUse flipped an idle parent to "working" — and
	// the SubagentStop idle-fallback below only fires from
	// main_agent_idle, so the parent stayed wedged at "working" after
	// the sub-agent finished. Exceptions that DO fall through to the
	// status switch:
	//   - SubagentStart/SubagentStop — their arms below handle the main
	//     status transitions around sub-agent lifecycle;
	//   - PermissionRequest / Notification — a sub-agent's permission
	//     prompt surfaces on the parent (Claude Code parks the prompt in
	//     the parent's UI), so awaiting_permission must still be set.
	if input.AgentID != "" {
		switch input.HookEventName {
		case "SubagentStart", "SubagentStop", "PermissionRequest", "Notification", "SessionStart", "SessionEnd":
			// fall through to the status switch below
		default:
			// A sub-agent acting again while the parent shows awaiting_*
			// is exactly the evidence that the prompt (parked on the
			// parent) was answered — but the resolved state must be
			// main_agent_idle, NOT "working" via the tool arms: only
			// main_agent_idle is a state the SubagentStop settle below
			// can take back to idle, so painting "working" here wedged
			// the parent at "working: <tool>" forever once the sub-agent
			// finished (found by cold review — the gate's original
			// awaiting fall-through re-created the very wedge the gate
			// exists to fix). If the parent's own turn is genuinely in
			// flight (a foreground Task), its next main-thread hook
			// repaints "working"; both states style as busy either way.
			if state.Status == StatusAwaitingPermission || state.Status == StatusAwaitingInput {
				state.Status = StatusMainAgentIdle
				state.StatusDetail = fmt.Sprintf("%d subagents running", len(state.Subagents))
			}
			state.SubagentCount = len(state.Subagents)
			return SaveSessionState(state)
		}
	}

	// Capture before event-specific early returns such as PostCompact. The
	// helper independently verifies the conversation match because PostCompact
	// is exempt from the status machine's foreign-process guard above.
	persistCodexHookModel(state, input)

	// Update state based on hook event. This switch is tclaude's
	// cross-harness event→status map. Claude Code and Codex deliver the
	// same snake_case payload field names through the same
	// `tclaude session hook-callback` — the parse of a Codex hook payload
	// into HookCallbackInput is JOH-157's contract — so both harnesses
	// drive this switch unchanged. Codex fires only a SUBSET of these
	// events (no Notification, SessionEnd, StopFailure or
	// PostToolUseFailure), so JOH-159's two degradations are handled by
	// what the subset DOES carry:
	//   - needs-attention comes from PermissionRequest (Codex has no
	//     Notification(permission_prompt)); both land on
	//     StatusAwaitingPermission below.
	//   - exit comes from the session reaper (tmux has-session → PID
	//     liveness, RefreshSessionStatus) rather than a SessionEnd hook.
	// A subset event tclaude doesn't model (e.g. PreCompact) falls through
	// to the default arm: last_hook is stamped, status is left untouched.
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
		// Ledger already updated above; no main-status transition.

	case "SubagentStop":
		if len(state.Subagents) == 0 && state.Status == StatusMainAgentIdle {
			state.Status = StatusIdle
			state.StatusDetail = ""
			stopped = true
		}

	case "Stop":
		if len(state.Subagents) == 0 {
			state.Status = StatusIdle
			state.StatusDetail = ""
			stopped = true
		} else {
			state.Status = StatusMainAgentIdle
			state.StatusDetail = fmt.Sprintf("%d subagents running", len(state.Subagents))
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
		// the stopped branch drives the context nudge and the task-runner
		// signal — both of which would "act on" the error (typing a nudge
		// into a broken pane, or reporting a half-finished task as done).
		// Acting on an error is explicitly out of scope here. The status
		// transition and the desktop notification (notify.OnStateTransition
		// below) both fire regardless of the stopped flag.
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
		// A SessionStart carrying agent_id fired from inside a subagent
		// (subagents share the main session's conv-id, so the foreign-
		// process guard above can't catch them; agent_id is the
		// documented discriminator). It is not the main conversation
		// (re)starting — flipping a working session to idle here, or
		// clearing a recorded exit reason, would misreport the main
		// thread's state. It IS live evidence of the sub-agent though
		// (the ledger block Sighted it above), so persist the full state,
		// not just last_hook.
		if input.AgentID != "" {
			state.SubagentCount = len(state.Subagents)
			return SaveSessionState(state)
		}
		// Session started or resumed - update ConvID and set to idle.
		// A (re)starting main thread definitionally has NO sub-agents
		// running yet — this is a known-zero boundary for the ledger, and
		// the reset is what clears phantoms left by lost SubagentStops
		// (e.g. sub-agents that died with a previous process). A /clear
		// or /resume transition lands here too; a background sub-agent
		// that somehow survives one re-adds itself via Sight() on its
		// next hook.
		state.Subagents = nil
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
		// field tells a real process exit apart from a /clear or an
		// interactive /resume switch, both of which end the conversation
		// but keep the process alive and fire a fresh SessionStart
		// immediately after — so neither must mark the session exited.
		// logout / prompt_input_exit / other all mean the process is
		// going away.
		//
		// A SessionEnd carrying agent_id was fired from inside a
		// subagent (the docs call agent_id THE discriminator for
		// "subagent hook call vs main-thread call") — whatever ended
		// there, it was not the main process, so it must not flip this
		// session to exited or fire an "Exited" notification. It does
		// mean that sub-agent is gone (the ledger block Removed it
		// above), so persist the full state, not just last_hook.
		if input.AgentID != "" {
			state.SubagentCount = len(state.Subagents)
			return SaveSessionState(state)
		}
		if !sessionEndIsExit(input.Reason) {
			if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
				slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
			}
			return nil
		}
		// The process is going away — sub-agents run inside it, so none
		// can survive. Known-zero boundary, same as the reaper's
		// MarkSessionExitedIfUnchanged.
		state.Subagents = nil
		state.Status = StatusExited
		state.StatusDetail = ""
		// Record the graceful-exit reason (logout / prompt_input_exit /
		// bypass_permissions_disabled / other — clear and resume never
		// reach here, see sessionEndIsExit) so the dashboard can tell
		// this clean exit from an unexpected death — for harnesses where
		// a missing SessionEnd is itself an abnormal-death signal.
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
		// A compaction just happened — zero the pre-compaction context_pct
		// (the statusline hook will report the new, smaller figure) and the
		// nudged_pct ladder so the context nudge re-arms from scratch on the
		// next climb.
		if envSessionID != "" {
			if err := db.ResetCompact(envSessionID); err != nil {
				slog.Warn("failed to reset compact state", "error", err, "module", "hooks")
			} else {
				slog.Info("post-compact state reset", "session_id", envSessionID, "module", "hooks")
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
			// branch context-nudges into the pane, which would collide
			// with a user mid-typing.
			state.Status = StatusIdle
			state.StatusDetail = ""
		default:
			// Unknown notification type - log but don't update status.
			// One from inside a sub-agent still Sighted the ledger above,
			// so persist the full state for it; a last_hook-only write
			// would silently drop that mutation.
			if input.AgentID != "" {
				state.SubagentCount = len(state.Subagents)
				return SaveSessionState(state)
			}
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

	// subagent_count is a derived cache of the ledger — recompute after
	// every arm so no code path can drift the two apart.
	state.SubagentCount = len(state.Subagents)

	if stopped && harnessUsesSlashContextControls(state.Harness) {
		// The context nudge injects a hint that names `/reincarnate` into
		// the agent's pane. It only ran before JOH-170 because context_pct
		// stayed 0 for non-CC harnesses — nothing populated it. Now that
		// persistCodexContextTelemetry (below) DOES populate it for Codex,
		// gate the injection on the harness actually understanding those
		// commands, or a Codex pane would be typed a hint it can't act on.
		// Harness-aware nudging is future work (Codex Lifecycle).
		handleContextNudge(envSessionID)
	}

	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes). A
	// /clear rotates the conv-id; needsIdentityMigration / migrateClearedIdentity
	// handle moving the agent's identity across that rotation.
	if input.ConvID != "" && state.ConvID != input.ConvID {
		switch {
		case envSessionID == "" || state.ConvID == "" || inTaskRunnerHook():
			// Not an env-keyed rotation we can migrate identity across
			// (a non-tclaude session, or the session's first conv-id
			// record). Plain advance — the pre-/clear-fix behaviour.
			//
			// `tclaude task run` lands here too: its rotation is a fresh
			// task starting, NOT an identity move. Forcing the plain
			// advance is load-bearing, not just an optimisation — the
			// reaper (agentd enrollOnlineSession) enrolls a task session's
			// CURRENT conv each tick, so without this exemption the next
			// task boundary would see the old task conv as "an active
			// agent", fire needsIdentityMigration, and inject a stray
			// `/rename` into the running task pane via migrateClearedIdentity.
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

	// Instant agent-enrollment for a tclaude-launched session. A
	// SessionStart means the conversation just (re)booted, and by here
	// state.ConvID is the settled conv-id (after any /clear or /resume
	// identity migration above). Enrolling it now means a
	// terminal-launched session — `tclaude conv new`, a fresh
	// `tclaude session` — surfaces on the dashboard the instant it
	// starts, the same way a web-UI/CLI `tclaude agent spawn` does,
	// instead of waiting up to one reaper interval for the daemon's
	// online-enrollment sweep (which stays as the backstop, and also
	// covers sessions tclaude did not launch — see agentd
	// enrollOnlineSession).
	//
	// Gated on envSessionID (TCLAUDE_SESSION_ID): only sessions tclaude
	// launched get the instant path, so a foreign headless one-shot
	// (`claude -p`, a plugin probe) firing a SessionStart never lands an
	// enrollment row the reaper would never have created. Subagent and
	// foreign-process SessionStarts already returned early above, so they
	// cannot reach here.
	//
	// Restricted to a genuine fresh launch (source startup / none) via
	// !isConvTransitionStart: a /clear, an interactive /resume switch, or
	// a compaction also fires a SessionStart, but those are in-process
	// conversation TRANSITIONS, not a process booting — and the
	// conv-advance/migration block above already decided their
	// enrollment correctly (an agent's identity, including its
	// enrollment, migrates onto the new conv-id; a plain conversation's
	// conv-id rotation just advances the session row and stays plain).
	// Without this guard the post-/clear SessionStart(source=clear) would
	// promote a never-an-agent plain conversation to an agent on the
	// freshly rotated conv-id — the #407 regression
	// (TestClearRotation_PlainConversationNotPromotedToAgent). The reaper
	// sweep still enrolls any genuinely live transitioned conv as the
	// backstop, exactly as it does for sessions tclaude did not launch.
	//
	// EnsureAgentForConv makes this idempotent and retirement-safe: a conv
	// the rotation above already linked to an actor, or one the human
	// deliberately retired, is left untouched (a retired actor is never
	// reinstated by an ensure).
	//
	// `tclaude task run` is exempt: its per-task conversations are
	// throwaway task executions, not managed agents, so skip the INSTANT
	// path here. (The reaper's online-reconcile sweep still enrolls a task
	// session's current conv — making the roster fully task-free is a
	// separate agentd concern; the conv-advance exemption above is what
	// keeps the migration machinery from firing regardless.) The task
	// runner needs only the session row + Stop-hook signal. See
	// inTaskRunnerHook.
	//
	// Codex has one extra repair path: if the launch/start hooks were missed,
	// a later modeled hook from a non-pending spawned session still proves the
	// conv is alive and should be enrolled. Pending dashboard spawns are skipped
	// here because agentd's sweeper owns their group/name/briefing intent.
	if shouldEnrollLaunchedSessionFromHook(state, input, envSessionID) {
		if _, _, err := db.EnsureAgentForConv(state.ConvID, "session-start"); err != nil {
			slog.Warn("failed to register launched session as agent",
				"conv_id", state.ConvID, "session_id", state.ID, "error", err, "module", "hooks")
		}
	}

	// Keep the row keyed by the real harness process, not tmux's shell
	// wrapper pane PID. Spawn records #{pane_pid}; hooks run under the
	// harness, so FindClaudePID can correct wrapper-shaped rows.
	if newPID := FindClaudePID(); newPID > 0 && state.PID != newPID {
		state.PID = newPID
	}

	// Save updated state
	slog.Info("updating session", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")
	if err := SaveSessionState(state); err != nil {
		return err
	}

	persistCodexWorkspaceSnapshot(state, input)

	// Lift Codex's context-window telemetry off its rollout onto the
	// sessions row. Claude Code gets these figures from its statusbar; a
	// Codex session has no command-statusline, so the hook is where
	// context% becomes visible to the dashboard / context-info. Codex only
	// writes a token_count when the model responds, so refresh at turn
	// boundaries — Stop/SubagentStop (stopped) and resume (SessionStart) —
	// not on every PreToolUse/PostToolUse tick: that keeps the rollout read
	// (and, on the fallback, the ~/.codex/sessions walk) to ~once per turn.
	// No-op for CC (it already has the statusbar) and best-effort.
	if stopped || input.HookEventName == "SessionStart" {
		persistCodexContextTelemetry(state, input)
	}
	persistCodexVirtualCostFromHook(state, input)
	persistCodexUsageFromHook(state, input)

	// Refresh usage cache when user is likely looking at the status bar.
	// Runs synchronously — hook callbacks are separate processes so this
	// just keeps the process alive a bit longer without blocking Claude.
	// SQLite's TryClaimUsageFetch prevents concurrent API calls.
	if state.Status == StatusIdle || state.Status == StatusAwaitingPermission || state.Status == StatusAwaitingInput {
		usageapi.RefreshCache()
	}

	// Signal task runner when Stop/UserPromptSubmit fires in task mode
	// (writes/removes the signal file the auto-/exit watcher polls).
	handleTaskSignal(stopped, input)

	// In task mode the task runner owns all user-facing notifications — it
	// sends its own targeted messages ("Task failed: X", "All tasks
	// completed!", "plan ready", …) over its own notify path. Suppress the
	// generic per-hook notifications for EVERY task-mode hook, not just the
	// ones handleTaskSignal consumed (Stop / ExitPlanMode): in a task run a
	// SessionEnd "Exited" at each task's auto-/exit, plus the idle stamps as
	// each task finishes, are pure noise (reported by Mikael). A manual
	// `tclaude` /exit is deliberately NOT affected here — that is not task
	// mode, and /exit is the normal interactive/dashboard lifecycle.
	if inTaskRunnerHook() {
		return nil
	}

	// Look up conversation title for notification
	convTitle := getConvTitle(state.ConvID, state.Cwd)

	// Notify on state transition (handles cooldown internally). The
	// harness drives the banner attribution ("Codex: …" vs "Claude: …");
	// the cooldown + mute ladder inside OnStateTransition are
	// harness-agnostic.
	if input.HookEventName != "SessionStart" {
		notifyOnStateTransition(state.ID, state.ConvID, prevStatus, state.Status, state.Cwd, convTitle, state.Harness)
	}

	return nil
}

// persistCodexHookModel records Codex's active model slug when a hook belongs
// to the tracked main conversation. The conversation check is intentionally
// local to this helper: PreCompact bypasses ApplyHook, while PostCompact is
// exempt from ApplyHook's foreign-process guard so it can still reset compact
// state after a legitimate conv-id rotation. Neither exception may let a
// child/foreign Codex process overwrite the host session's model.
func persistCodexHookModel(state *SessionState, input HookCallbackInput) {
	if state == nil || state.Harness != harness.CodexName || state.ID == "" ||
		input.Model == "" || input.AgentID != "" {
		return
	}
	if state.ConvID != "" && input.ConvID != state.ConvID {
		if input.ConvID == "" {
			return
		}
		pending, err := db.GetSessionPendingConv(state.ID)
		if err != nil {
			slog.Warn("codex-model: failed to verify pending conversation",
				"session_id", state.ID, "conv_id", input.ConvID, "error", err, "module", "hooks")
			return
		}
		if pending != input.ConvID {
			return
		}
	}

	// Codex's hook `model` field is both the dashboard label and the
	// machine-facing value a successor can pass back to `codex --model`.
	if err := db.UpdateSessionModelSlug(state.ID, input.Model); err != nil {
		slog.Warn("codex-model: failed to update session model slug",
			"session_id", state.ID, "error", err, "module", "hooks")
	}
}

func shouldEnrollLaunchedSessionFromHook(state *SessionState, input HookCallbackInput, envSessionID string) bool {
	if state == nil || envSessionID == "" || state.ConvID == "" || inTaskRunnerHook() {
		return false
	}
	if input.HookEventName == "SessionStart" && !isConvTransitionStart(input) {
		return true
	}
	if state.Harness != harness.CodexName {
		return false
	}
	if input.AgentID != "" || (input.ConvID != "" && input.ConvID != state.ConvID) {
		return false
	}
	// Pending dashboard spawns carry group/name/briefing intent that only
	// agentd's pending-spawn sweeper can finish safely. Leave those for the
	// sweeper; the hook's job is to persist the conv-id and status it consumes.
	if ps, err := db.GetPendingSpawn(state.ID); err != nil {
		slog.Warn("failed to check pending spawn before hook enrollment",
			"session_id", state.ID, "error", err, "module", "hooks")
		return false
	} else if ps != nil {
		return false
	}
	switch input.HookEventName {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure",
		"PermissionRequest", "Stop", "StopFailure":
		return true
	default:
		return false
	}
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
	// taskSignalPath enforces the CacheDir bound (the same predicate
	// inTaskRunnerHook gates the hook exemptions on); warn on a
	// set-but-out-of-bounds path, since this is where the path is
	// consumed for a write.
	signalPath, ok := taskSignalPath()
	if !ok {
		if raw := os.Getenv("TCLAUDE_TASK_SIGNAL"); raw != "" {
			slog.Warn("task signal path outside allowed directory, ignoring", "path", raw, "module", "hooks")
		}
		return false
	}
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

// harnessUsesSlashContextControls reports whether a session's harness
// understands the context-management commands the stopped-hook path's
// context nudge names in the hint it types into the pane (`/reincarnate`).
// It folds to the harness's compact capability as a proxy for "understands
// context-management controls". An empty or unknown harness preserves the
// legacy Claude Code behaviour — the overwhelmingly common case, and the
// safe default since CC understands the commands.
func harnessUsesSlashContextControls(name string) bool {
	h, err := harness.Resolve(name)
	if err != nil || h == nil {
		return true
	}
	return h.SupportsCompact()
}

// persistCodexContextTelemetry lifts the latest context-window snapshot off
// a Codex session's rollout and stores it on the sessions row, mirroring
// what the statusbar's UpdateContextSnapshot does for Claude Code. It is a
// no-op for every other harness (CC already has the statusbar path) and
// best-effort throughout: a missing rollout, a session with no token_count
// event yet, or a transient read error just leaves the previous snapshot in
// place. The all-zero guard inside db.UpdateContextSnapshot keeps a
// pre-first-response read from clobbering a good snapshot.
func persistCodexContextTelemetry(state *SessionState, input HookCallbackInput) {
	if state == nil || state.Harness != harness.CodexName || state.ConvID == "" {
		return
	}

	var (
		snap      harness.ContextTelemetry
		ok        bool
		err       error
		effort    string
		effortOK  bool
		effortErr error
	)
	// Fast path: the hook payload's transcript_path is this session's
	// rollout, so read it straight — no ~/.codex/sessions walk. Guarded by
	// the rollout-filename shape so a stray/foreign path can't be parsed as
	// a rollout. Fall through to the by-id lookup when it's absent or not a
	// rollout path (older payload, unexpected shape).
	if p := input.TranscriptPath; p != "" && harness.IsCodexRolloutPath(p) {
		snap, ok, err = harness.CodexTelemetryFromRollout(p)
		effort, effortOK, effortErr = harness.CodexEffortFromRollout(p)
	} else {
		home, herr := os.UserHomeDir()
		if herr != nil {
			slog.Warn("codex-telemetry: cannot resolve home", "error", herr, "module", "hooks")
			return
		}
		snap, ok, err = harness.CodexContextTelemetry(home, state.ConvID)
		effort, effortOK, effortErr = harness.CodexEffortLevel(home, state.ConvID)
	}
	if err != nil {
		slog.Warn("codex-telemetry: failed to read rollout telemetry",
			"conv_id", state.ConvID, "error", err, "module", "hooks")
	}
	if ok {
		if err := db.UpdateContextSnapshot(state.ID, snap.Pct, snap.TokensInput, snap.TokensOutput, snap.WindowSize); err != nil {
			slog.Warn("codex-telemetry: failed to update context snapshot",
				"session_id", state.ID, "error", err, "module", "hooks")
		}
	}
	if effortErr != nil {
		slog.Warn("codex-telemetry: failed to read rollout effort",
			"session_id", state.ID, "error", effortErr, "module", "hooks")
	}
	if effortOK {
		if err := db.UpdateSessionEffort(state.ID, effort); err != nil {
			slog.Warn("codex-telemetry: failed to update session effort",
				"session_id", state.ID, "error", err, "module", "hooks")
		}
	}
}

// persistCodexUsageFromHook updates the shared Codex account-usage cache from
// the exact rollout path Codex supplied in hook stdin. Codex does not include
// the rate-limit windows directly in hook payloads; they live in the rollout's
// token_count.rate_limits block. Unlike the daemon's repair poller, this path
// does no rollout-tree discovery and fires at useful turn boundaries only.
func persistCodexUsageFromHook(state *SessionState, input HookCallbackInput) {
	if state == nil || state.Harness != harness.CodexName {
		return
	}
	switch input.HookEventName {
	case "Stop", "SubagentStop", "SessionStart", "PostCompact":
	default:
		return
	}
	p := input.TranscriptPath
	if p == "" || !harness.IsCodexRolloutPath(p) {
		return
	}
	u, err := harness.CodexUsageFromRollout(p)
	if err != nil {
		slog.Warn("codex-usage: failed to read rollout usage",
			"session_id", state.ID, "path", p, "error", err, "module", "hooks")
		return
	}
	if u == nil || u.Observed.IsZero() {
		return
	}
	data, err := json.Marshal(u)
	if err != nil {
		slog.Warn("codex-usage: failed to marshal usage snapshot",
			"session_id", state.ID, "error", err, "module", "hooks")
		return
	}
	if _, err := db.SaveCodexUsageCacheIfNewer(data, u.Observed, p); err != nil {
		slog.Warn("codex-usage: failed to persist usage snapshot",
			"session_id", state.ID, "error", err, "module", "hooks")
	}
}

// persistCodexVirtualCostFromHook lifts Codex's cumulative token usage from
// the rollout and writes the pay-per-token-equivalent estimate into
// sessions.virtual_cost_usd, the same WHAT-IF column Claude Code subscription
// sessions populate from statusline cost.total_cost_usd.
func persistCodexVirtualCostFromHook(state *SessionState, input HookCallbackInput) {
	if state == nil || state.Harness != harness.CodexName {
		return
	}
	switch input.HookEventName {
	case "Stop", "SubagentStop", "SessionStart", "PostCompact":
	default:
		return
	}
	p := input.TranscriptPath
	if p == "" || !harness.IsCodexRolloutPath(p) {
		return
	}
	cost, ok, err := harness.CodexVirtualCostFromRollout(p, input.Model)
	if err != nil {
		slog.Warn("codex-cost: failed to read rollout cost",
			"session_id", state.ID, "path", p, "error", err, "module", "hooks")
		return
	}
	if !ok {
		return
	}
	if err := db.UpdateSessionVirtualCost(state.ID, cost.CostUSD); err != nil {
		slog.Warn("codex-cost: failed to update session virtual cost",
			"session_id", state.ID, "model", cost.Model, "error", err, "module", "hooks")
	}
}

// persistCodexWorkspaceSnapshot is Codex's replacement for the Claude Code
// statusline workspace write. Codex has no command-backed statusline payload,
// but every hook carries the session cwd, so resolve git there at hook time and
// publish the same agent_workspace row the dashboard already reads. The first
// branch observed also seeds conv_index.git_branch_startup; later observations
// update only the current branch so the UI can keep showing "init" vs "now".
func persistCodexWorkspaceSnapshot(state *SessionState, input HookCallbackInput) {
	if state == nil || state.Harness != harness.CodexName || state.ConvID == "" {
		return
	}
	if input.ConvID != "" && input.ConvID != state.ConvID {
		return
	}

	cwd := input.Cwd
	if cwd == "" {
		cwd = state.Cwd
	}
	if cwd == "" {
		return
	}

	worktreeRoot, branch := GitLocationOf(cwd)
	now := time.Now()
	if err := db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID:    state.ConvID,
		Cwd:       cwd,
		Branch:    branch,
		UpdatedAt: now,
	}); err != nil {
		slog.Warn("codex-workspace: failed to upsert agent_workspace",
			"conv_id", state.ConvID, "error", err, "module", "hooks")
	}

	projectDir := worktreeRoot
	if projectDir == "" {
		projectDir = cwd
	}
	fullPath := input.TranscriptPath
	var fileMtime, fileSize int64
	if fullPath != "" {
		if info, err := os.Stat(fullPath); err == nil {
			fileMtime = info.ModTime().Unix()
			fileSize = info.Size()
			projectDir = filepath.Dir(fullPath)
		}
	}
	if err := db.UpsertConvIndexBranchSnapshot(&db.ConvIndexRow{
		ConvID:           state.ConvID,
		ProjectDir:       projectDir,
		FullPath:         fullPath,
		FileMtime:        fileMtime,
		FileSize:         fileSize,
		GitBranch:        branch,
		GitBranchStartup: branch,
		ProjectPath:      cwd,
		Harness:          harness.CodexName,
		IndexedAt:        now,
	}); err != nil {
		slog.Warn("codex-workspace: failed to upsert conv_index branch snapshot",
			"conv_id", state.ConvID, "branch", branch, "error", err, "module", "hooks")
	}
	if branch == "" {
		return
	}
	if err := db.AppendConvBranchHistoryHook(state.ConvID, branch, worktreeRoot); err != nil {
		slog.Warn("codex-workspace: failed to record branch history",
			"conv_id", state.ConvID, "branch", branch, "error", err, "module", "hooks")
	}
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

	// Never auto-register a session from its own SessionEnd: a conv we
	// have never tracked that is already ending is a one-shot headless
	// claude invocation (`claude -p`, `claude mcp get`, …) — such CLI
	// runs fire a SessionEnd(other) on exit with a fresh conv-id each
	// time. Registering it would create a row only to instantly mark it
	// exited, firing a spurious "Exited" notification per run (and the
	// per-session notify cooldown can never catch repeats, since every
	// run is a new id). The agentd plugin checker's per-minute `claude
	// mcp get` probes did exactly that.
	if input.HookEventName == "SessionEnd" {
		slog.Info("ignoring SessionEnd for untracked conversation",
			"conv_id", input.ConvID, "reason", input.Reason, "module", "hooks")
		return nil, nil
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

	// The session PK is the conversation's full UUID — never a truncation.
	// Two conversations sharing an 8-char prefix would otherwise collide on
	// the PK (SaveSession's ON CONFLICT overwrite). See JOH-248.
	sessionID := input.ConvID

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

	// Idempotency: if a row is already keyed by this conversation's full UUID,
	// reuse it rather than overwriting. Full-UUID PKs never collide across
	// different conversations, so the old 8-char-prefix -N suffixing is gone
	// (the caller reconciles conv_id/status on the returned row). See JOH-248.
	if existing, err := LoadSessionState(sessionID); err == nil && existing != nil {
		return existing
	}

	if err := SaveSessionState(state); err != nil {
		return nil
	}
	return state
}

// preCompactDecision is the JSON Claude Code reads from a PreCompact
// hook's stdout. No output (or an empty Decision) lets compaction
// proceed; Decision "block" with a Reason refuses it. See
// https://code.claude.com/docs/en/hooks ("PreCompact" — Blocks
// compaction).
type preCompactDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// decidePreCompact implements the pre-compact guard. It refuses an
// auto-compaction whose conversation has not yet reached the configured
// per-window context floor by writing {"decision":"block",...} to w
// (the hook's stdout). It fails OPEN — writes nothing, letting
// compaction proceed — whenever the guard is off, the trigger is not
// guarded, or the data needed to judge is missing. It never forces a
// compaction; it can only delay an early one.
//
// envSessionID is TCLAUDE_SESSION_ID, the key the statusline hook
// stores the context snapshot under (statusbar.UpdateContextSnapshot).
func decidePreCompact(input HookCallbackInput, envSessionID string, w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("pre-compact guard: config load failed, allowing compaction", "error", err, "module", "hooks")
		return nil
	}
	g := cfg.PreCompactGuard
	thresholds := g.ResolvedThresholds() // nil when the guard is nil/disabled
	if thresholds == nil {
		return nil // guard off → allow
	}

	// Only Claude Code's automatic compaction is guarded by default; a
	// manual /compact the human typed is honoured unless block_manual is
	// set. An unknown/empty trigger is treated as "not auto" → allow, so
	// we never block a compaction we cannot classify.
	guarded := input.Trigger == "auto" || (input.Trigger == "manual" && g.BlockManual)
	if !guarded {
		return nil
	}

	if envSessionID == "" {
		return nil // not a tclaude-launched session → no snapshot → allow
	}
	snap, err := db.GetContextSnapshot(envSessionID)
	if err != nil {
		slog.Warn("pre-compact guard: failed to read context snapshot, allowing compaction",
			"error", err, "session_id", envSessionID, "module", "hooks")
		return nil
	}
	window := snap.ContextWindowSize
	if window <= 0 || snap.ContextPct <= 0 {
		return nil // no usable usage data yet → allow
	}
	minTokens, ok := preCompactFloor(thresholds, window)
	if !ok {
		return nil // no threshold matches this window → allow
	}

	usedTokens := int64(snap.ContextPct / 100.0 * float64(window))
	if usedTokens >= minTokens {
		return nil // enough context has accrued → allow
	}

	reason := fmt.Sprintf(
		"tclaude pre-compact guard: refused %s compaction — context is ~%.0f%% (~%d of %d tokens), below the %d-token floor for this window. Let context grow (or reincarnate) before compacting; adjust pre_compact_guard in ~/.tclaude/config.json to change or disable this.",
		input.Trigger, snap.ContextPct, usedTokens, window, minTokens,
	)
	slog.Info("pre-compact guard: blocked compaction",
		"conv_id", input.ConvID,
		"session_id", envSessionID,
		"trigger", input.Trigger,
		"context_pct", snap.ContextPct,
		"window", window,
		"used_tokens", usedTokens,
		"min_tokens", minTokens,
		"module", "hooks",
	)
	if err := json.NewEncoder(w).Encode(preCompactDecision{Decision: "block", Reason: reason}); err != nil {
		return fmt.Errorf("pre-compact guard: failed to write block decision: %w", err)
	}
	return nil
}

// preCompactFloor returns the MinTokens floor to apply for a context
// window of windowSize, choosing the configured threshold whose
// window_size is the closest match by ratio. Claude Code reports a
// model's real window (≈200000 or ≈1000000); matching by nearest ratio
// rather than exact equality tolerates a reported window that differs
// slightly from the round numbers the thresholds are keyed by (e.g.
// 1048576 vs 1000000). A best match more than 2× away in either
// direction is rejected (ok=false) so a ladder listing only one window
// class never silently governs a wildly different window.
func preCompactFloor(thresholds []config.PreCompactThreshold, windowSize int64) (int64, bool) {
	var best config.PreCompactThreshold
	var bestRatio float64
	found := false
	for _, t := range thresholds {
		if t.WindowSize <= 0 {
			continue
		}
		r := float64(windowSize) / float64(t.WindowSize)
		if r < 1 {
			r = 1 / r // ratio ≥ 1 regardless of direction
		}
		if !found || r < bestRatio {
			best, bestRatio, found = t, r, true
		}
	}
	if !found || bestRatio > 2.0 {
		return 0, false
	}
	return best.MinTokens, true
}

// nextNudgeTarget computes which threshold percentile, if any, the
// context-nudge Stop-hook path should fire at given the current
// context_pct and the (min, interval) ladder. Returns 0 when no nudge
// should fire (below min, or invalid config). Caps at 90 so the agent
// gets a final "you're really running out" tap before the next gulp
// pushes it into hard-stop territory.
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
// when the agent's context crosses a configured threshold. Runs in the
// Stop-hook path, reads the stored context_pct, and delivers via tmux
// send-keys into the agent's own pane.
//
// Skips when:
//   - the feature isn't enabled in config
//   - the session id isn't known (callback running outside a tracked session)
//   - context_pct is below the configured min
//   - the same-or-higher threshold has already been fired
//     (sessions.nudged_pct; ResetCompact zeroes it so post-compact climbs re-arm)
func handleContextNudge(sessionID string) {
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

	contextPct, err := db.GetContextPct(sessionID)
	if err != nil {
		slog.Warn("context-nudge: failed to read context_pct",
			"error", err, "module", "hooks")
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
	if err := clcommon.TmuxCommand("send-keys", "-t", clcommon.ExactTarget(tmuxSession)+":", msg).Run(); err != nil {
		slog.Warn("context-nudge: send-keys failed",
			"error", err, "module", "hooks")
		return
	}
	if err := clcommon.TmuxCommand("send-keys", "-t", clcommon.ExactTarget(tmuxSession)+":", "Enter").Run(); err != nil {
		slog.Warn("context-nudge: submit failed",
			"error", err, "module", "hooks")
		return
	}
	if err := db.SetNudgedPct(sessionID, float64(target)); err != nil {
		slog.Warn("context-nudge: failed to stamp nudged_pct",
			"error", err, "module", "hooks")
	}
}
