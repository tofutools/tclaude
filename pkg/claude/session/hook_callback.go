package session

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

var safeSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var agentMessagePromptRe = regexp.MustCompile(`\[system: new agent message #([0-9]+)\b[^\]\r\n]*\]`)

// HookCallbackInput represents the JSON input from any Claude Code hook
type HookCallbackInput struct {
	ConvID         string `json:"session_id"` // claude's session id, what we call conv_id
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permission_mode,omitempty"`
	HookEventName  string `json:"hook_event_name"`
	// NativeHookEvent is internal event-stream identity used by OpenCode.
	// Claude/Codex callbacks use HookEventName directly.
	NativeHookEvent  string          `json:"-"`
	NotificationType string          `json:"notification_type,omitempty"`
	Reason           string          `json:"reason,omitempty"`  // SessionEnd: clear | resume | logout | prompt_input_exit | bypass_permissions_disabled | other
	Source           string          `json:"source,omitempty"`  // SessionStart: startup | resume | clear | compact
	Trigger          string          `json:"trigger,omitempty"` // PreCompact/PostCompact: auto | manual
	Message          string          `json:"message,omitempty"`
	Prompt           string          `json:"prompt,omitempty"`
	Model            string          `json:"model,omitempty"`
	StopHookActive   bool            `json:"stop_hook_active,omitempty"`
	ToolName         string          `json:"tool_name,omitempty"`
	ToolInput        json.RawMessage `json:"tool_input,omitempty"`
	// PayloadTrimmed is internal broker evidence, never emitted by a harness.
	// It prevents a payload matcher from turning dropped tool fields or
	// truncated prompt text into a confident clean no-match.
	PayloadTrimmed bool `json:"tclaude_payload_trimmed,omitempty"`
	// StandingOrderOrigin is internal delivery evidence. OpenCode sets it for
	// every event in a turn started by a queued standing-order message so that
	// prompt/tool automations cannot recursively trigger themselves.
	StandingOrderOrigin bool `json:"-"`
	// StandingOrderOnly asks the OpenCode projector to evaluate an observation
	// solely for standing orders, without applying a synthetic status
	// transition. It covers both exact native selectors and portable
	// boundaries (such as OpenCode's session.compacted projection).
	StandingOrderOnly bool `json:"-"`
	// ToolResponse is the structured tool RESULT a PostToolUse carries.
	// Its shape is per-tool; the only field tclaude reads today is Bash's
	// backgroundTaskId, the handle for a `run_in_background` launch (see
	// bgshell.go). Kept raw so an unknown/changed shape costs nothing.
	ToolResponse         json.RawMessage `json:"tool_response,omitempty"`
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

// sessionEndProvesExit reports whether a SessionEnd from this harness may be
// treated as evidence that the process ended. Unknown harness names keep the
// historical behavior; only a descriptor that declares its SessionEnd
// best-effort opts out.
func sessionEndProvesExit(harnessName string) bool {
	h, ok := harness.Get(harnessName)
	if !ok {
		return true
	}
	return h.SessionEndProvesExit()
}

// lateSessionStart reports whether this SessionStart arrived INSIDE a turn that
// is already running, in which case clearing the session to idle would be a
// lie.
//
// Every harness tclaude knew before Copilot announces a session before the
// first prompt of that session, so SessionStart meaning "nothing is running
// yet" was safe. GitHub Copilot CLI inverts it: the recorded event order is
// UserPromptSubmit, UserPromptTransformed, SessionStart, ... — the prompt comes
// FIRST (see pkg/claude/harness/copilotfixture/testdata/*/hooks). Applied
// unchanged, the idle reset would blank the working status the prompt just set
// and report a busy agent as free for the rest of its first turn — the exact
// signal group coordination, idle notifications and the dashboard all read.
//
// The rule is deliberately deterministic and narrow: only for a harness whose
// descriptor declares the inverted order, and only when the session is
// CURRENTLY working. An idle session still settles to idle, every other
// harness is untouched, and no timing window is involved.
//
// It does give something up, and the trade is deliberate. A SessionStart used
// to be the one event that resynced a row stuck at "working" — after a Stop
// callback the harness killed on its timeout, say, or a turn the user
// interrupted. Suppressing it means such a row now waits for the next
// completed turn to settle. The reaper does not cover that case: it acts when
// the PANE dies, not when a live pane goes quiet. Accepted because the failure
// it prevents is both worse and far more common — every first turn of every
// session reporting a busy agent as free, versus an occasional stale row after
// a dropped event.
func lateSessionStart(state *SessionState) bool {
	if state == nil || state.Status != StatusWorking {
		return false
	}
	h, ok := harness.Get(state.Harness)
	return ok && h.AnnouncesSessionAfterPrompt()
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
// The same residual has a second admission path since the
// verified-continuation fallback (isVerifiedConvContinuation): a
// resumed one-shot's transcript legitimately contains the lineage
// marker (its copied history was created under the tracked conv), so
// ANY of its hooks — not just the SessionStart(source=resume) — can
// pass the guard. Same failure class, same rarity, same acceptance.
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

// Caps for the transcript head-scan in isVerifiedConvContinuation. The
// lineage marker (the first copied user/assistant entry) sits right after the
// rotated file's preamble (custom-title / mode / file-history-snapshot
// records), so a bounded read finds it; the byte cap keeps a pathological
// preamble (huge file-history snapshots) from turning every foreign-hook drop
// into an unbounded file read. `var` so tests can shrink them.
var (
	convContinuationScanMaxBytes int64 = 8 << 20
	convContinuationScanMaxLines       = 500
)

// convLineageProbe is the per-line projection isVerifiedConvContinuation
// reads. Claude Code writes BOTH spellings on ordinary transcript entries:
// `session_id` (the conv the entry was originally created under) and
// `sessionId` (the conv whose file the entry lives in). In a file produced by
// an in-process conversation rotation (observed with the /remote-control
// bridge handoff, which copies the full history into a fresh conv file) the
// copied entries therefore carry session_id=<old conv> next to
// sessionId=<new conv> — a marker no independent one-shot (`claude -p`, a
// plugin probe) ever produces, because a one-shot's entries were all created
// under its own conv-id. Top-level fields only: conversation CONTENT that
// merely quotes the old conv-id (pasted JSONL, a log excerpt) lives inside
// nested message strings and cannot match.
type convLineageProbe struct {
	SessionID    string `json:"session_id"`
	SessionIDAlt string `json:"sessionId"`
}

// isVerifiedConvContinuation reports whether the mismatched conv-id a hook
// carries belongs to a transcript that is a verified in-process continuation
// of the conv the session row tracks — i.e. the rotation is real but was
// never announced by a transition SessionStart the foreign-process guard
// accepts. Claude Code's /remote-control bridge activation is the observed
// producer: it rotates the conversation to a fresh conv-id, copies the full
// history into the new file, and announces it only with a
// SessionStart(source=startup) — indistinguishable by source from a foreign
// one-shot booting in the pane's env, so the guard dropped every later hook
// and the session froze on its pre-rotation status.
//
// Verification reads a bounded head of the new conv's transcript (the hook's
// own transcript_path when it names <conv-id>.jsonl, else the path derived
// from the hook/session cwd) and looks for a top-level entry created under
// the tracked conv but stored in the new conv's file — see convLineageProbe.
// Failure modes all fail closed to the existing drop: a missing file (the
// rotation's own SessionStart can fire before the copied file exists — the
// next hook retries), a marker beyond the scan caps, or a genuinely foreign
// transcript whose entries only ever name its own conv-id.
func isVerifiedConvContinuation(input HookCallbackInput, state *SessionState) bool {
	if input.ConvID == "" || state == nil || state.ConvID == "" {
		return false
	}
	path := input.TranscriptPath
	if filepath.Base(path) != input.ConvID+".jsonl" {
		path = ""
	}
	if path == "" {
		cwd := input.Cwd
		if cwd == "" {
			cwd = state.Cwd
		}
		if cwd == "" {
			return false
		}
		projectDir := convops.GetClaudeProjectPath(cwd)
		if projectDir == "" {
			return false
		}
		path = filepath.Join(projectDir, input.ConvID+".jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(io.LimitReader(f, convContinuationScanMaxBytes))
	for range convContinuationScanMaxLines {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var probe convLineageProbe
			if json.Unmarshal(line, &probe) == nil &&
				probe.SessionID == state.ConvID && probe.SessionIDAlt == input.ConvID {
				return true
			}
		}
		if err != nil {
			return false
		}
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

// Task mode (HookAmbient.InTaskRunnerHook) is exempt from the
// foreign-process guard, the conv-advance
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
//
// Brokered (tclaude-layer) events are never in task mode — see
// BrokeredHookAmbient for why the signal path is not carried across the
// broker, and why that is the safe direction.

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
	// Hooks never rebuild transcripts. Give agentd's incremental fsnotify
	// follower a short chance to commit a just-written direct /rename that is
	// still inside its debounce window, using metadata only as the ordering
	// signal. tclaude-delivered renames also stamp the cache synchronously; if
	// no monitor exists, the fallback below reads only a bounded tail window.
	if !waitForClearedIdentityIndex(state) {
		path := clearedIdentityTranscriptPath(state)
		if path != "" {
			if refreshed, err := convops.RefreshCustomTitleFromTail(path); err != nil {
				slog.Debug("clear-migrate: bounded title-tail recovery failed",
					"conv_id", state.ConvID, "path", path, "error", err)
			} else if refreshed {
				slog.Debug("clear-migrate: recovered title from bounded transcript tail",
					"conv_id", state.ConvID, "path", path)
			}
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

var (
	clearIndexCatchupTimeout = 1100 * time.Millisecond
	clearIndexCatchupPoll    = 25 * time.Millisecond
)

// waitForClearedIdentityIndex closes the direct-/rename → /clear ordering gap
// without reading transcript payload. A custom-title record grows the old
// JSONL; once conv_index records that size/mtime, RotateAgentConv can safely
// carry the corresponding cached title. A false result lets the caller use a
// bounded title-tail recovery rather than replaying the whole transcript.
func waitForClearedIdentityIndex(state *SessionState) bool {
	path := clearedIdentityTranscriptPath(state)
	if path == "" || clearIndexCatchupTimeout <= 0 {
		return false
	}
	deadline := time.Now().Add(clearIndexCatchupTimeout)
	for {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return false
		}
		row, rowErr := db.GetConvIndex(state.ConvID)
		if rowErr == nil && row != nil && row.FileSize == info.Size() &&
			!row.FileMtime.Before(info.ModTime().Round(0).UTC()) {
			return true
		}
		if !time.Now().Before(deadline) {
			slog.Debug("clear-migrate: conv_index did not catch up before rotation",
				"conv_id", state.ConvID, "path", path)
			return false
		}
		time.Sleep(clearIndexCatchupPoll)
	}
}

func clearedIdentityTranscriptPath(state *SessionState) string {
	if state == nil || state.ConvID == "" || state.Cwd == "" {
		return ""
	}
	projectDir := convops.GetClaudeProjectPath(state.Cwd)
	if projectDir == "" {
		return ""
	}
	return filepath.Join(projectDir, state.ConvID+".jsonl")
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
// Uses the same paneinput text-submit primitive as agentd (text → 500 ms gap →
// Enter → 500 ms gap → Enter) so CC's bracketed-paste mode can't coalesce the
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
	if err := paneinput.InjectTextAndSubmit(tmuxSession+":0.0", "/rename "+title, paneinput.Options{}); err != nil {
		slog.Warn("clear-migrate: /rename injection failed; relying on pending_name",
			"error", err, "module", "hooks")
	}
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

	// Route the event. A `tclaude-layer` launch cannot reach the real
	// database from inside its namespace, so its events are handed to
	// agentd, which runs this same dispatch host-side on the caller's
	// behalf. Every other launch keeps writing directly, unchanged.
	if brokerHookEvents() {
		return brokerHookEvent(input, os.Stdout)
	}

	// An ordinary launch applies its own hooks, but a permission prompt
	// auto-permit can answer is a decision only the daemon can make and only
	// the daemon can act on (see auto_permit.go). Hand that one event over the
	// SAME broker every sandboxed launch already uses, and fall back to
	// applying it here when the daemon does not take it — an unreachable
	// daemon must cost the agent its auto-answer, never its status update.
	//
	// Not in task mode. brokerHookEvents() refuses to broker there on purpose,
	// and the refusal is about applying the event, not only about carrying the
	// signal file: a task hook applied host-side arrives without
	// InTaskRunnerHook, which is what earns it the foreign-conv, conv-advance,
	// enrollment and notification exemptions the runner depends on. Taking one
	// event out of that carve-out would half-break the very thing the carve-out
	// exists to keep whole.
	if _, inTaskMode := taskSignalPath(); !inTaskMode &&
		autoPermitNeedsDaemon(input) && brokerHookEventIfDelivered(input, os.Stdout) {
		return nil
	}

	if err := DispatchHookEvent(context.Background(), input, envSessionID, LocalHookAmbient(), os.Stdout); err != nil {
		return err
	}
	MaybeRequestAutoName(input)
	return nil
}

// DispatchHookEvent applies one parsed hook event: the PreCompact gate
// path (which may write a decision document to stdout) or the ordinary
// ApplyHook path. It is the whole of the hook callback below the
// stdin/env/record-hooks IO in runHookCallback, which is exactly the seam
// the agentd broker needs — agentd receives a parsed event and a resolved
// identity and calls straight into here, so the brokered and direct paths
// execute the same code rather than two implementations that must be kept
// in agreement.
//
// stdout receives any hook response document; it is the hook's real stdout
// on the direct path and a response buffer on the brokered one. What gets
// written is decided by dispatchHookEvent as a VALUE — see HookResponse — and
// serialized here, once, at the single edge that owns the byte stream.
func DispatchHookEvent(ctx context.Context, input HookCallbackInput, envSessionID string, amb HookAmbient, stdout io.Writer) error {
	resp, err := PrepareHookEvent(ctx, input, envSessionID, amb)
	// Deferred before the error check so a producer that acquired a lock and
	// then failed cannot strand it.
	defer resp.Release()
	if err != nil {
		return err
	}
	if err := resp.Write(stdout, input.HookEventName); err != nil {
		return err
	}
	// Only now may a producer record that its content was delivered. A write
	// that failed must not leave a durable claim that it succeeded.
	resp.Commit()
	return nil
}

// PrepareHookEvent applies one parsed hook event but leaves response delivery
// and its acknowledgement to the caller.
//
// Direct callbacks should use DispatchHookEvent. The agentd hook broker uses
// this lower-level form because writing to its in-memory HTTP response buffer
// is not proof that the sandboxed client successfully relayed those bytes to
// the harness; it commits only after a separate acknowledgement.
func PrepareHookEvent(ctx context.Context, input HookCallbackInput, envSessionID string, amb HookAmbient) (HookResponse, error) {
	return dispatchHookEvent(ctx, input, envSessionID, amb)
}

// dispatchHookEvent applies one event and reports what tclaude wants the
// harness to do about it. It performs no IO on the response: everything that
// has something to say returns it, and DispatchHookEvent writes it.
func dispatchHookEvent(ctx context.Context, input HookCallbackInput, envSessionID string, amb HookAmbient) (HookResponse, error) {
	// PreCompact is both a gate and, when allowed, a status transition. It
	// may write a {"decision":"block"} back to Claude Code to refuse an
	// early auto-compaction, in which case the visible status must remain
	// unchanged because no compaction follows. Handle it on its own path
	// (it does not flow through ApplyHook's ordinary status machinery).
	// Codex still reports its active model on this event, so persist it
	// first when the hook belongs to the tracked main conversation; a
	// blocked compaction has no PostCompact backstop.
	if input.HookEventName == "PreCompact" {
		sessionKey := envSessionID
		if sessionKey == "" {
			sessionKey = input.ConvID
		}
		if sessionKey != "" {
			unlock, err := acquireHookLockContext(ctx, sessionKey)
			if err != nil {
				return HookResponse{}, fmt.Errorf("failed to acquire PreCompact hook lock: %w", err)
			}
			defer unlock()
		}
		state, stateErr := findTrackedPreCompactSession(envSessionID, input.ConvID)
		if stateErr == nil {
			persistCodexHookModel(state, input)
		}
		resp, err := decidePreCompact(input, envSessionID, amb)
		if err != nil || stateErr != nil ||
			!hookBelongsToTrackedMainConversation(state, input) {
			return resp, err
		}
		// PreCompact bypasses the ordinary ApplyHook path because it may carry
		// a gate decision, but it is still a selectable native hook. It has no
		// tested same-continuation context channel, so a supported selector can
		// only queue a next-turn message here. Keep that side effect separate
		// from the gate document: a block decision remains the sole stdout
		// response, byte-for-byte.
		input = applyStandingOrderTurnOrigin(input, envSessionID)
		orderResp := standingOrderResponse(ctx, input, envSessionID)
		orderResp.Release()
		if resp.Decision != "" {
			return resp, nil
		}
		state.Status = StatusWorking
		state.StatusDetail = "compacting"
		state.LastHook = time.Now()
		return HookResponse{}, SaveSessionState(state)
	}

	if err := applyHook(ctx, input, envSessionID, amb); err != nil {
		return HookResponse{}, err
	}
	input = applyStandingOrderTurnOrigin(input, envSessionID)
	return standingOrderResponse(ctx, input, envSessionID), nil
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
//
// This entry point reads the caller's own ambient process context, which
// is correct for everything that runs inside the agent's pane. The agentd
// broker does not; it goes through DispatchHookEvent with an explicit
// HookAmbient instead.
func ApplyHook(input HookCallbackInput, envSessionID string) error {
	return applyHook(context.Background(), input, envSessionID, LocalHookAmbient())
}

func applyHook(ctx context.Context, input HookCallbackInput, envSessionID string, amb HookAmbient) error {
	// Acquire a per-session exclusive lock to prevent concurrent hook callbacks
	// from racing on the read-modify-write of session state.
	sessionKey := envSessionID
	if sessionKey == "" {
		sessionKey = input.ConvID
	}
	if sessionKey != "" {
		unlock, lockErr := acquireHookLockContext(ctx, sessionKey)
		if lockErr != nil {
			slog.Warn("failed to acquire hook lock", "error", lockErr, "module", "hooks")
			return fmt.Errorf("failed to acquire hook lock: %w", lockErr)
		}

		defer unlock()
	}

	// Log hook event
	slog.Debug("hook received",
		"event", input.HookEventName,
		"conv_id", input.ConvID,
		"notification_type", input.NotificationType,
		"tool_name", input.ToolName,
		"cwd", input.Cwd,
		"sessionId", envSessionID,
		"module", "hooks",
	)

	// Get or create session state
	state, err := getOrCreateSessionState(input, envSessionID, amb)
	if err != nil || state == nil {
		return err
	}
	slog.Debug("session found", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")

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
	// was never announced and cannot match.
	//
	// One rotation is real but NEVER announced by an accepted source:
	// Claude Code's /remote-control bridge handoff rotates the
	// conversation to a fresh conv-id (full history copied into the new
	// file) and fires only SessionStart(source=startup) — by source
	// alone a foreign one-shot. For that case a mismatched hook gets one
	// more chance before the drop: a bounded transcript head-scan that
	// proves the new conv is an in-process continuation of the tracked
	// one (isVerifiedConvContinuation). A verified continuation is
	// recorded as pending_conv exactly like an announced transition, so
	// the same conv-advance/identity-migration/retry machinery runs.
	//
	// PostCompact is exempt — it
	// only resets per-env-session compact state and returns before any
	// status or conv mutation, and it may legitimately arrive carrying
	// a rotated conv-id.
	//
	// `tclaude task run` is exempt too (HookAmbient.InTaskRunnerHook): it drives a
	// sequence of independent conversations under one env-session — one
	// fresh conv per task — so its conv-id rotations are legitimate, not
	// foreign. See HookAmbient.InTaskRunnerHook for the full rationale.
	if !amb.InTaskRunnerHook() &&
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
				return nil
			}
			if isVerifiedConvContinuation(input, state) {
				// An unannounced in-process rotation (the /remote-control
				// bridge handoff) proven by transcript lineage. Record it
				// as pending_conv like an announced transition would —
				// later hooks then pass via the pending match without
				// re-reading the transcript, even if this call's
				// conv-advance/migration fails and has to retry.
				slog.Info("accepting unannounced conv rotation: transcript verified as in-process continuation",
					"event", input.HookEventName, "new_conv", input.ConvID,
					"tracked_conv", state.ConvID, "session_id", state.ID, "module", "hooks")
				if err := db.SetSessionPendingConv(state.ID, input.ConvID); err != nil {
					slog.Warn("failed to record pending conv", "error", err, "module", "hooks")
				}
			} else {
				slog.Info("ignoring hook from foreign claude process",
					"event", input.HookEventName, "foreign_conv", input.ConvID,
					"tracked_conv", state.ConvID, "session_id", state.ID, "module", "hooks")
				// Deliberately NOT stamping last_hook: a foreign process's
				// event is no evidence the host session itself is alive.
				return nil
			}
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
	// A validated hook carrying this launch's conversation is the durable
	// post-thread-creation barrier for the Codex app-server control connection.
	// Agentd deliberately stays disconnected until this write: Codex 0.147
	// auto-subscribes every connection initialized before a fresh thread exists.
	if envSessionID != "" && input.ConvID != "" && state.ConvID == input.ConvID {
		if _, err := db.BindWarmingCodexAppServerRuntimeFromTUI(envSessionID, input.ConvID); err != nil {
			slog.Warn("failed to bind Codex app-server runtime from TUI hook",
				"session_id", envSessionID, "conv_id", input.ConvID, "error", err, "module", "hooks")
		}
	}
	if state.Cwd == "" && input.Cwd != "" {
		state.Cwd = input.Cwd
	}

	// A permitted PreCompact paints "working: compacting". Any later
	// accepted hook proves that phase is over (PostCompact is the usual
	// boundary, but a failed/aborted compaction may only produce another
	// turn hook). Clear the phase eagerly so even an event whose arm below
	// only stamps last_hook cannot leave the dashboard wedged on it.
	wasCompacting := state.Status == StatusWorking && state.StatusDetail == "compacting"
	if wasCompacting {
		state.StatusDetail = ""
		if err := SaveSessionState(state); err != nil {
			slog.Warn("failed to clear compacting status", "error", err, "module", "hooks")
		}
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
	// Whether this SubagentStop names a sub-agent the ledger still knows
	// after the TTL sweep, captured before Remove erases that evidence.
	// Claude Code 2.1.220 ends
	// EVERY main-thread turn with a synthetic SubagentStop carrying a
	// freshly minted agent_id that no SubagentStart ever announced, an
	// empty agent_type, and an agent_transcript_path pointing at a file
	// that does not exist — fired right after the turn's real Stop. It
	// describes the main thread finishing, not a sub-agent.
	//
	// This is HARDENING, not a repair: at the ordering 2.1.220 actually
	// produces (always after Stop) the sub-agent lifecycle arm happens to
	// be a no-op or idempotent, so no misbehaviour was observed. What it
	// removes is the standing hazard of an event that is not a sub-agent
	// stopping being routed through the arm that decides what a sub-agent
	// stopping means to the MAIN thread's status — an ordering tclaude
	// does not control and upstream has already changed once.
	//
	// Ledger membership, not the empty agent_type, is the discriminator
	// for whether the stop may drive stopped-only side effects. A real
	// sub-agent can still arrive unknown when its ledger entry was swept
	// during a long model turn, or when its preceding SessionEnd removed
	// the entry. Such a stop may settle a now-empty main_agent_idle state
	// below, but does not drive the context nudge/task signal because its
	// attribution is ambiguous. An empty agent_id is the legacy shape;
	// Remove falls back to decrement semantics for it, so it keeps its
	// old stopped behaviour.
	trackedSubagentStop := input.HookEventName == "SubagentStop" &&
		(input.AgentID == "" || state.Subagents.Has(input.AgentID))
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

	// ---- Background-shell ledger (db.BgShellSet) ----
	// Strictly lossier than the sub-agent ledger above: Claude Code
	// announces a background shell's LAUNCH (this PostToolUse) and never
	// its exit, so hooks alone can only ever grow the set. What actually
	// retires an entry is the daemon's process-liveness reconcile on the
	// dashboard read path; the two removals available here are the
	// explicit ones — a TaskStop naming the task, and the known-zero
	// boundaries handled in the SessionStart/SessionEnd arms below.
	//
	// Deliberately placed BEFORE the sub-agent gate below: a shell
	// backgrounded by a SUB-agent is a child of the same harness process
	// and keeps running past the parent's turn just the same, so it
	// belongs in the session's ledger. The gate's early return persists
	// full state, so the mutation is not lost.
	//
	// ---- Monitor ledger (db.MonitorSet) ----
	//
	// Same shape, same lossiness, and — because a `Monitor` watch is a
	// background task in the SAME id namespace as a background shell — the
	// same TaskStop event retires from either. A stop is routed to the one
	// ledger that actually knows the id rather than offered to both, so a
	// TaskStop naming a monitor cannot also decrement the shell count.
	//
	// A stop carrying NO id keeps its legacy meaning ("drop the oldest
	// background shell") and is deliberately not extended to monitors: with
	// two ledgers there is no non-arbitrary way to guess which one lost an
	// entry, and dropping from both would double-count a single kill.
	trackShells := harnessTracksBackgroundShells(state.Harness)
	trackMonitors := harnessTracksMonitors(state.Harness)
	if trackShells {
		state.BgShells.Sweep(state.LastHook)
	}
	if trackMonitors {
		state.Monitors.Sweep(state.LastHook)
	}
	shellID, shellCommand, isShellLaunch := bgShellLaunch(input)
	monitor, isMonitorLaunch := monitorLaunch(input, state.LastHook)
	stopID, isStop := bgShellStop(input)
	switch {
	case trackShells && isShellLaunch:
		state.BgShells = state.BgShells.Add(shellID, shellCommand, state.LastHook)
	case trackMonitors && isMonitorLaunch:
		state.Monitors = state.Monitors.Add(
			monitor.ID, monitor.Command, monitor.Label, monitor.WS,
			state.LastHook, monitor.Deadline)
	case isStop && trackMonitors && state.Monitors.Has(stopID):
		state.Monitors.Remove(stopID)
	case isStop && trackShells:
		state.BgShells.Remove(stopID)
	}

	// Hooks fired from INSIDE a sub-agent (agent_id set) must not drive
	// the main thread's status machine: before this gate, a background
	// sub-agent's PreToolUse flipped an idle parent to "working" — and
	// the SubagentStop idle-fallback below only fires from
	// main_agent_idle, so the parent stayed wedged at "working" after
	// the sub-agent finished. Exceptions that DO fall through to the
	// status switch:
	//   - SubagentStart and SubagentStop — their arms below handle the
	//     main status transitions around sub-agent lifecycle. An unknown
	//     SubagentStop gets this dedicated fall-through rather than the
	//     default arm, so Claude Code's synthetic per-turn event cannot
	//     clear awaiting_*; trackedSubagentStop controls whether it may
	//     drive stopped-only side effects;
	//   - PermissionRequest / Notification — a sub-agent's permission
	//     prompt surfaces on the parent (Claude Code parks the prompt in
	//     the parent's UI), so awaiting_permission must still be set.
	if input.AgentID != "" {
		switch input.HookEventName {
		case "SubagentStop":
			// Fall through to the lifecycle settle below. An unknown id
			// may be CC's synthetic turn-end event or a real stop whose
			// ledger entry was already swept/removed; only the latter
			// needs the now-empty main_agent_idle state settled, while
			// neither should take the default awaiting_* arm.
		case "SubagentStart", "PermissionRequest", "Notification", "SessionStart", "SessionEnd":
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
				state.StatusDetail = state.backgroundActivityDetail()
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
			// Unknown-id stops are ambiguous: allow the status settle so
			// a delayed real stop cannot leave stale activity, but reserve
			// context nudges and task signals for tracked/legacy stops.
			stopped = trackedSubagentStop
			if state.hasBackgroundActivity() {
				state.StatusDetail = state.backgroundActivityDetail()
			} else {
				state.Status = StatusIdle
				state.StatusDetail = ""
			}
		}

	case "Stop":
		// The turn is over, but a sub-agent or a background shell can
		// outlive it — settling to plain idle then would report an agent
		// as finished while work it launched is still running. Either
		// ledger therefore holds the session at main_agent_idle; the
		// dashboard badges the counts, and for background shells the
		// daemon's liveness reconcile is what eventually settles it.
		//
		// `stopped` stays keyed on the SUB-AGENT ledger alone. It drives
		// the context nudge, and a background shell is genuinely not the
		// agent working: the main thread really has finished its turn and
		// can act on a nudge. Gating it on background shells too would
		// mute the nudge for as long as the agent keeps a dev server or a
		// test watch running — potentially hours.
		if len(state.Subagents) == 0 {
			stopped = true
		}
		if state.hasBackgroundActivity() {
			state.Status = StatusMainAgentIdle
			state.StatusDetail = state.backgroundActivityDetail()
		} else {
			state.Status = StatusIdle
			state.StatusDetail = ""
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
		// Session started or resumed - update ConvID and normally set to
		// idle. A post-compaction SessionStart is different: it is an
		// in-process continuation boundary, not evidence that the turn
		// stopped. PostCompact already selected working (auto/unknown) or
		// idle (manual), so preserve that status until the next operational
		// hook determines a new one.
		// A (re)starting main thread definitionally has NO sub-agents
		// running yet — this is a known-zero boundary for the ledger, and
		// the reset is what clears phantoms left by lost SubagentStops
		// (e.g. sub-agents that died with a previous process). A /clear
		// or /resume transition lands here too; a background sub-agent
		// that somehow survives one re-adds itself via Sight() on its
		// next hook.
		state.Subagents = nil
		// Background shells are children of the harness PROCESS, and
		// monitors belong to it, which a /clear or /resume does not
		// restart — so unlike sub-agents both can genuinely outlive this
		// boundary, and only a startup SessionStart (a new process) is a
		// true known-zero for them. Clearing on every SessionStart would
		// blank the ledgers on every /clear while the work kept running;
		// the liveness reconcile retires them honestly instead.
		if input.Source == "startup" || input.Source == "" {
			state.BgShells = nil
			state.Monitors = nil
		}
		if input.Source != "compact" && !lateSessionStart(state) {
			state.Status = StatusIdle
			state.StatusDetail = ""
		}
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
			if err := db.ClearSessionExitIntentByConv(state.ConvID); err != nil {
				slog.Warn("exit audit: clear stale lifecycle intent failed",
					"session", state.ID, "error", err, "module", "hooks")
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
		// The reason itself can say this is not an exit: a /clear or an
		// interactive /resume ends the conversation but not the process.
		if !sessionEndIsExit(input.Reason) {
			if err := db.UpdateSessionLastHook(state.ID, state.LastHook); err != nil {
				slog.Warn("failed to persist last_hook", "error", err, "module", "hooks")
			}
			return nil
		}
		// So can the HARNESS. A harness whose SessionEnd is best-effort has not
		// proven the event means the process is going away, and acting as if it
		// had would declare a live pane dead — settling its ledgers, writing an
		// exit observation and raising an "Exited" notification on an agent
		// sitting at a prompt. Exit detection for those harnesses stays where
		// it is provable: the reaper's tmux/PID liveness.
		//
		// The session is still OVER as far as the harness will report, so the
		// status settles to idle. That matters for a turn that ended without a
		// turn-end event at all: a Copilot provider error emits SessionStart →
		// ErrorOccurred → SessionEnd and no Stop, and ErrorOccurred is not in
		// the installed baseline, so without this the agent would sit at
		// "working" until its pane died. Status only — no exit observation, no
		// notification, no ledger reset, since none of those are warranted by
		// an event that has not proven a process ended.
		if !sessionEndProvesExit(state.Harness) {
			if state.Status == StatusWorking {
				state.Status = StatusIdle
				state.StatusDetail = ""
			}
			return SaveSessionState(state)
		}
		// The process is going away — sub-agents and monitors run inside
		// it and background shells are its children, so none of the three
		// can survive. Known-zero boundary, same as the reaper's
		// MarkSessionExitedIfUnchanged.
		state.Subagents = nil
		state.BgShells = nil
		state.Monitors = nil
		state.Status = StatusExited
		state.StatusDetail = ""
		accepted, _, err := db.RecordSessionEndExitObservation(db.AgentExitObservation{
			At:                 time.Now(),
			SessionID:          state.ID,
			TmuxSession:        state.TmuxSession,
			Observer:           db.AgentExitObserverHook,
			CauseKind:          db.AgentExitCauseNormal,
			Reason:             boundedSessionEndReason(input.Reason),
			ObservedState:      StatusExited,
			ExpectedGeneration: amb.ExitGeneration,
		})
		if err != nil {
			slog.Warn("exit audit: persist SessionEnd observation failed",
				"session", state.ID, "observer", db.AgentExitObserverHook,
				"error", err, "module", "hooks")
			return nil
		}
		if !accepted {
			slog.Info("ignoring stale SessionEnd from predecessor launch",
				"session", state.ID, "module", "hooks")
			return nil
		}
		if !amb.InTaskRunnerHook() {
			convTitle := getConvTitle(state.ConvID, state.Cwd)
			notifyOnStateTransition(state.ID, state.ConvID, prevStatus, state.Status,
				state.Cwd, convTitle, state.Harness)
		}
		return nil

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
				slog.Debug("post-compact state reset", "session_id", envSessionID, "module", "hooks")
			}
		}
		// Auto-compaction happens inside an active turn, so the eager
		// compacting-detail clear above intentionally leaves it working.
		// A manual /compact returns the human to an idle prompt instead.
		// SessionStart(source=compact), which follows this hook, preserves
		// whichever status this boundary selected.
		// PostCompact is exempt from the mismatched-conversation guard
		// because a legitimate compaction can rotate the conv-id before its
		// SessionStart(compact) announces that rotation. Do not let that
		// exemption turn a foreign child's manual PostCompact into an idle
		// stamp on the host: require either the ordinary attribution check,
		// or the attributed compacting phase this hook just cleared.
		attributed := wasCompacting || hookBelongsToTrackedMainConversation(state, input)
		if input.Trigger == "manual" && attributed {
			state.Status = StatusIdle
			state.StatusDetail = ""
			if err := SaveSessionState(state); err != nil {
				return err
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
			//
			// The main thread can be idle while work it launched is not.
			// Keep the same main_agent_idle contract as Stop whenever a
			// sub-agent or background shell is still in the ledger. Besides
			// keeping the dashboard honest, this prevents the generic
			// "* → idle" notification rule from announcing completion while
			// background work is active. The daemon's liveness reconcile
			// eventually settles lost SubagentStop events and shell exits.
			if state.hasBackgroundActivity() {
				state.Status = StatusMainAgentIdle
				state.StatusDetail = state.backgroundActivityDetail()
			} else {
				state.Status = StatusIdle
				state.StatusDetail = ""
			}
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
		handleContextNudge(envSessionID, amb)
	}

	state.Updated = time.Now()

	// Update ConvID from hook input (tracks conversation changes). A
	// /clear rotates the conv-id; needsIdentityMigration / migrateClearedIdentity
	// handle moving the agent's identity across that rotation.
	if input.ConvID != "" && state.ConvID != input.ConvID {
		switch {
		case envSessionID == "" || state.ConvID == "" || amb.InTaskRunnerHook():
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
	// HookAmbient.InTaskRunnerHook.
	//
	// Codex has one extra repair path: if the launch/start hooks were missed,
	// a later modeled hook from a non-pending spawned session still proves the
	// conv is alive and should be enrolled. Pending dashboard spawns are skipped
	// here because agentd's sweeper owns their group/name/briefing intent.
	if shouldEnrollLaunchedSessionFromHook(state, input, envSessionID, amb) {
		agentID, created, err := db.EnsureAgentForConv(state.ConvID, "session-start")
		if err != nil {
			slog.Warn("failed to register launched session as agent",
				"conv_id", state.ConvID, "session_id", state.ID, "error", err, "module", "hooks")
		} else {
			AssignFreeFloatingFallback(state, agentID, created)
		}
	}

	// Keep the row keyed by the real harness process, not tmux's shell
	// wrapper pane PID. Spawn records #{pane_pid}; hooks run under the
	// harness, so FindClaudePID can correct wrapper-shaped rows.
	if newPID := amb.HarnessPID(); newPID > 0 && state.PID != newPID {
		state.PID = newPID
	}

	// Save updated state
	slog.Debug("updating session", "session_id", state.ID, "status", state.Status, "subagent_count", state.SubagentCount, "module", "hooks")
	if err := SaveSessionState(state); err != nil {
		return err
	}
	if input.HookEventName == "UserPromptSubmit" {
		if messageID, inline, ok := agentMessagePrompt(input.Prompt); ok {
			if _, err := db.MarkRegularAgentMessageStarted(messageID, state.ConvID, inline, time.Now()); err != nil {
				slog.Warn("failed to mark regular agent message started",
					"error", err, "message_id", messageID, "conv_id", state.ConvID, "module", "hooks")
			}
		}
	}
	// Stop only, deliberately NOT StopFailure: the same reasoning as the
	// stopped=false decision in the StopFailure case above. A turn that ended in
	// an API/auth/billing error consumed nothing, so acknowledging its mail
	// would reopen the sender's capacity into a pane making no progress —
	// rate-limited is precisely when backpressure should hold.
	//
	// Do NOT read that as "an erroring agent receives nothing". It does:
	// isAwaitingHumanInput excludes StatusError on purpose (see delivery_hold.go
	// — an API/billing failure leaves the pane at an ordinary prompt), so
	// deliverablePane still admits it and mail keeps being injected and
	// read-stamped into a rate-limited pane. A long limit window can therefore
	// fill the recipient's queue to 10/10 with nothing showing as unread, which
	// looks exactly like the TCL-737 wedge to an operator. The difference that
	// matters is that it is recoverable rather than a deadlock: the first
	// successful Stop drains it, with no operator intervention.
	if input.HookEventName == "Stop" {
		if _, err := db.MarkReadRegularAgentMessagesProcessed(state.ConvID, time.Now()); err != nil {
			slog.Warn("failed to mark regular agent messages processed",
				"error", err, "conv_id", state.ConvID, "module", "hooks")
		}
	}

	persistHookWorkspaceSnapshot(state, input)

	// Codex rollout telemetry is owned by agentd's incremental follower. Hook
	// callbacks are separate processes and must not replay a potentially huge
	// JSONL at turn boundaries; dashboard/context reads consume only appended
	// bytes and persist a durable cursor for restart recovery.

	// Refresh usage cache when user is likely looking at the status bar.
	// Runs synchronously — hook callbacks are separate processes so this
	// just keeps the process alive a bit longer without blocking Claude.
	// SQLite's TryClaimUsageFetch prevents concurrent API calls.
	if state.Status == StatusIdle || state.Status == StatusAwaitingPermission || state.Status == StatusAwaitingInput {
		usageapi.RefreshCache()
	}

	// Signal task runner when Stop/UserPromptSubmit fires in task mode
	// (writes/removes the signal file the auto-/exit watcher polls).
	handleTaskSignal(stopped, input, amb)

	// In task mode the task runner owns all user-facing notifications — it
	// sends its own targeted messages ("Task failed: X", "All tasks
	// completed!", "plan ready", …) over its own notify path. Suppress the
	// generic per-hook notifications for EVERY task-mode hook, not just the
	// ones handleTaskSignal consumed (Stop / ExitPlanMode): in a task run a
	// SessionEnd "Exited" at each task's auto-/exit, plus the idle stamps as
	// each task finishes, are pure noise (reported by Mikael). A manual
	// `tclaude` /exit is deliberately NOT affected here — that is not task
	// mode, and /exit is the normal interactive/dashboard lifecycle.
	if amb.InTaskRunnerHook() {
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

// AssignFreeFloatingFallback covers both the normal hook-first enrollment and
// the daemon-reconcile race where the same plain session was enrolled moments
// earlier. Managed spawns have another CreatedVia value and are never touched.
// The empty-name CAS is the final guard against overwriting a name installed
// concurrently by spawn completion or an explicit operation.
func AssignFreeFloatingFallback(state *SessionState, agentID string, created bool) {
	actor, err := db.GetAgent(agentID)
	if err != nil || actor == nil || !actor.Active() || actor.PendingName != "" {
		return
	}
	switch actor.CreatedVia {
	case "session-start", "online-reconcile", "cli":
	default:
		if !created {
			return
		}
	}
	createdAt := state.Created
	if createdAt.IsZero() {
		createdAt = state.LastHook
	}
	changed, err := db.ReplaceAgentPendingName(agentID, "", FreeFloatingAgentName(createdAt, agentID))
	if err != nil {
		slog.Warn("failed to assign launched session fallback name",
			"conv_id", state.ConvID, "agent_id", agentID, "error", err, "module", "hooks")
	} else if !changed {
		slog.Debug("launched session fallback name lost a concurrent update",
			"conv_id", state.ConvID, "agent_id", agentID, "module", "hooks")
	}
}

func boundedSessionEndReason(reason string) string {
	switch reason {
	case "logout", "prompt_input_exit", "bypass_permissions_disabled", "other":
		return reason
	case "":
		return ""
	default:
		return "other"
	}
}

// agentMessagePrompt recognizes the server-authored metadata inside a submitted
// prompt without requiring byte-for-byte equality. Harnesses may wrap submitted
// input, while the stable message id and inline marker remain intact.
func agentMessagePrompt(prompt string) (messageID int64, inline bool, ok bool) {
	match := agentMessagePromptRe.FindStringSubmatch(prompt)
	if len(match) != 2 {
		return 0, false, false
	}
	messageID, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || messageID <= 0 {
		return 0, false, false
	}
	return messageID, strings.Contains(match[0], "; delivery: inline"), true
}

// findTrackedPreCompactSession resolves only an existing row. Unlike the
// ordinary hook path it must not auto-register an unknown conversation merely
// because it is about to compact, but an already auto-registered conversation
// (which has no TCLAUDE_SESSION_ID) should still expose its compacting phase.
func findTrackedPreCompactSession(envSessionID, convID string) (*SessionState, error) {
	if envSessionID != "" {
		return LoadSessionState(envSessionID)
	}
	if convID == "" {
		return nil, nil
	}
	return FindSessionByConvID(convID)
}

// hookBelongsToTrackedMainConversation is the attribution gate for exceptional
// hook paths that bypass applyHook's ordinary foreign-process and sub-agent
// guards. A pending conversation is a previously announced /clear or /resume
// rotation and is therefore allowed; an unannounced mismatch is a child process
// that inherited the host's TCLAUDE_SESSION_ID.
func hookBelongsToTrackedMainConversation(state *SessionState, input HookCallbackInput) bool {
	if state == nil || state.ID == "" || state.Harness == ShellHarnessName || input.AgentID != "" {
		return false
	}
	if state.ConvID == "" || input.ConvID == "" || input.ConvID == state.ConvID {
		return true
	}
	pending, err := db.GetSessionPendingConv(state.ID)
	if err != nil {
		slog.Warn("hooks: failed to verify pending conversation",
			"session_id", state.ID, "conv_id", input.ConvID, "error", err, "module", "hooks")
		return false
	}
	return pending == input.ConvID
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

func shouldEnrollLaunchedSessionFromHook(state *SessionState, input HookCallbackInput, envSessionID string, amb HookAmbient) bool {
	if state == nil || envSessionID == "" || state.ConvID == "" || amb.InTaskRunnerHook() {
		return false
	}
	if input.HookEventName == "SessionStart" && !isConvTransitionStart(input) {
		// A SessionStart admitted by the verified-continuation path is a
		// conversation ROTATION announced as pending_conv, not a fresh boot —
		// even though its source says "startup" (the /remote-control bridge
		// handoff announces itself that way). Instant-enrolling it would
		// EnsureAgentForConv a rotated conv exactly like the source=clear
		// promotion regression (#407): an actor the human deliberately
		// retired would come back as a fresh agent row on its next bridge
		// rotation. Give it the same treatment as an announced transition;
		// on a read error, err on not enrolling — the reaper's
		// online-enrollment sweep remains the backstop either way.
		if input.ConvID != "" {
			pending, err := db.GetSessionPendingConv(state.ID)
			if err != nil {
				slog.Warn("failed to check pending conv before hook enrollment",
					"session_id", state.ID, "error", err, "module", "hooks")
				return false
			}
			if pending == input.ConvID {
				return false
			}
		}
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
func handleTaskSignal(isDone bool, input HookCallbackInput, amb HookAmbient) bool {
	// taskSignalPath enforces the CacheDir bound (the same predicate
	// HookAmbient.InTaskRunnerHook gates the hook exemptions on); warn on a
	// set-but-out-of-bounds path, since this is where the path is
	// consumed for a write.
	signalPath := amb.TaskSignalPath
	if signalPath == "" {
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

// persistHookWorkspaceSnapshot replaces Claude Code's command-backed
// statusline workspace write for harnesses without that surface. Codex,
// Copilot, and OpenCode all carry the session cwd on every hook, so resolve git there at
// hook time and publish the same agent_workspace row the dashboard already
// reads. The first branch observed also seeds conv_index.git_branch_startup;
// later observations update only the current branch so the UI can keep showing
// "init" vs "now". Once that branch is present, the dashboard's existing
// asynchronous git/gh enrichment supplies its compare and pull-request links.
func persistHookWorkspaceSnapshot(state *SessionState, input HookCallbackInput) {
	if state == nil || state.ConvID == "" ||
		(state.Harness != harness.CodexName && state.Harness != harness.CopilotName &&
			state.Harness != harness.OpenCodeName) {
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
		slog.Warn("hook-workspace: failed to upsert agent_workspace",
			"conv_id", state.ConvID, "error", err, "module", "hooks")
	}

	projectDir := worktreeRoot
	if projectDir == "" {
		projectDir = cwd
	}
	fullPath := input.TranscriptPath
	var fileMtime time.Time
	var fileSize int64
	if fullPath != "" {
		if info, err := os.Stat(fullPath); err == nil {
			fileMtime = info.ModTime().Round(0).UTC()
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
		Harness:          state.Harness,
		IndexedAt:        now,
	}); err != nil {
		slog.Warn("hook-workspace: failed to upsert conv_index branch snapshot",
			"conv_id", state.ConvID, "branch", branch, "error", err, "module", "hooks")
	}
	if branch == "" {
		return
	}
	if err := db.AppendConvBranchHistoryHook(state.ConvID, branch, worktreeRoot); err != nil {
		slog.Warn("hook-workspace: failed to record branch history",
			"conv_id", state.ConvID, "branch", branch, "error", err, "module", "hooks")
	}
}

// getOrCreateSessionState finds existing session or creates a new one.
// envSessionID is the caller's TCLAUDE_SESSION_ID ("" when the session
// was not launched by tclaude).
func getOrCreateSessionState(input HookCallbackInput, envSessionID string, amb HookAmbient) (*SessionState, error) {
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

	return autoRegisterSessionFromHook(input, amb), nil
}

// autoRegisterSessionFromHook creates a new session state for a Claude session
// that wasn't started via tclaude
func autoRegisterSessionFromHook(input HookCallbackInput, amb HookAmbient) *SessionState {
	claudePID := amb.HarnessPID()
	if claudePID == 0 {
		return nil
	}

	tmuxSession := amb.TmuxSession()

	// The session PK is the conversation's full UUID — never a truncation.
	// Two conversations sharing an 8-char prefix would otherwise collide on
	// the PK (SaveSession's ON CONFLICT overwrite). See JOH-248.
	sessionID := input.ConvID

	cwd := input.Cwd
	if cwd == "" {
		cwd = amb.FallbackCwd()
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

// decidePreCompact implements the pre-compact guard. It refuses an
// auto-compaction whose conversation has not yet reached the configured
// per-window context floor by returning a HookResponse carrying
// Decision "block". It fails OPEN — returns an empty response, letting
// compaction proceed — whenever the guard is off, the trigger is not
// guarded, or the data needed to judge is missing. It never forces a
// compaction; it can only delay an early one.
//
// It returns its verdict rather than writing it so the caller owns the one
// stdout write; a blocked compaction is signalled by a non-empty Decision.
//
// envSessionID is TCLAUDE_SESSION_ID, the key the statusline hook
// stores the context snapshot under (statusbar.UpdateContextSnapshot).
func decidePreCompact(input HookCallbackInput, envSessionID string, amb HookAmbient) (HookResponse, error) {
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("pre-compact guard: config load failed, allowing compaction", "error", err, "module", "hooks")
		return HookResponse{}, nil
	}
	g := cfg.PreCompactGuard
	thresholds := g.ResolvedThresholds() // nil when the guard is nil/disabled
	if thresholds == nil {
		return HookResponse{}, nil // guard off → allow
	}

	// Only Claude Code's automatic compaction is guarded by default; a
	// manual /compact the human typed is honoured unless block_manual is
	// set. An unknown/empty trigger is treated as "not auto" → allow, so
	// we never block a compaction we cannot classify.
	guarded := input.Trigger == "auto" || (input.Trigger == "manual" && g.BlockManual)
	if !guarded {
		return HookResponse{}, nil
	}

	if envSessionID == "" {
		return HookResponse{}, nil // not a tclaude-launched session → no snapshot → allow
	}
	snap, err := db.GetContextSnapshot(envSessionID)
	if err != nil {
		slog.Warn("pre-compact guard: failed to read context snapshot, allowing compaction",
			"error", err, "session_id", envSessionID, "module", "hooks")
		return HookResponse{}, nil
	}
	// Measure against the window compaction ACTUALLY fires at. The stored
	// ContextPct is already re-based onto it by the status line, so pairing it
	// with the model's full window here would overstate used tokens by exactly
	// the ratio between the two — a 1M agent pinned to 450K would look more than
	// twice as full as it is, and the guard would wave through compactions it
	// exists to refuse. The pin is read from this hook's own environment for the
	// same reason the status line does: the hook runs inside the agent's pane, so
	// it sees the variable Claude Code is actually acting on.
	pinnedWindow := int64(0)
	if parsed, err := harness.ParseAutoCompactWindow(amb.AutoCompactWindow); err == nil {
		// Parsed, not read raw, so an out-of-range value in the operator's own shell
		// cannot govern the guard. See the same treatment in the status bar.
		pinnedWindow = harness.AutoCompactWindowTokens(parsed)
	}
	window := harness.EffectiveContextWindow(snap.ContextWindowSize, pinnedWindow)
	if window <= 0 || snap.ContextPct <= 0 {
		return HookResponse{}, nil // no usable usage data yet → allow
	}
	// The floor is looked up by the MODEL's window and then scaled onto the
	// effective one. Both halves matter: the ladder is keyed by model class
	// (≈200000 / ≈1000000) and rejects a match more than 2× away, so handing it a
	// pinned window would either find no class at all (450K is >2× from both, and
	// the guard would silently switch OFF) or match a class whose absolute floor
	// the pinned window can never reach (a 500K pin against the 1M class's 800K
	// floor would block EVERY compaction, inverting the pin's purpose). Scaling
	// keeps the floor at the same FRACTION of the window the operator's ladder
	// asked for, and is an exact no-op when nothing is pinned.
	minTokens, ok := preCompactFloor(thresholds, snap.ContextWindowSize, window)
	if !ok {
		return HookResponse{}, nil // no threshold matches this window → allow
	}

	usedTokens := int64(snap.ContextPct / 100.0 * float64(window))
	if usedTokens >= minTokens {
		return HookResponse{}, nil // enough context has accrued → allow
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
	return HookResponse{Decision: "block", Reason: reason}, nil
}

// preCompactFloor returns the MinTokens floor to apply, choosing the configured
// threshold whose window_size is the closest match by ratio to modelWindow —
// the MODEL's real context window, which is what the ladder is keyed by.
//
// Claude Code reports a model's real window (≈200000 or ≈1000000); matching by
// nearest ratio rather than exact equality tolerates a reported window that
// differs slightly from the round numbers the thresholds are keyed by (e.g.
// 1048576 vs 1000000). A best match more than 2× away in either direction is
// rejected (ok=false) so a ladder listing only one window class never silently
// governs a wildly different window.
//
// effectiveWindow is the window compaction ACTUALLY fires at — the smaller of
// the model's window and any pinned CLAUDE_CODE_AUTO_COMPACT_WINDOW. When a pin
// binds, the matched class's floor is scaled by effectiveWindow/modelWindow so
// the guard keeps enforcing the same FRACTION of the window the operator's
// ladder asked for. That scaling is what keeps the two inputs from fighting:
// the class lookup needs the model window (an operator-chosen pin is not a model
// class and would fall outside the 2× tolerance), while the used-token
// comparison is against the pinned window, so an unscaled class floor could
// exceed the pinned window entirely and block every compaction.
//
// With nothing pinned effectiveWindow == modelWindow and the scaling is an exact
// no-op, so an unpinned agent keeps the floor its ladder literally specifies.
func preCompactFloor(thresholds []config.PreCompactThreshold, modelWindow, effectiveWindow int64) (int64, bool) {
	var best config.PreCompactThreshold
	var bestRatio float64
	found := false
	for _, t := range thresholds {
		if t.WindowSize <= 0 {
			continue
		}
		r := float64(modelWindow) / float64(t.WindowSize)
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
	if effectiveWindow > 0 && modelWindow > 0 && effectiveWindow < modelWindow {
		scaled := int64(float64(best.MinTokens) * float64(effectiveWindow) / float64(modelWindow))
		return scaled, true
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

// formatContextNudgeMessage is the text typed into the pane when a threshold
// crosses. It reads as a system tap-on-shoulder rather than human input.
//
// Pure for unit testing.
func formatContextNudgeMessage(target int) string {
	return fmt.Sprintf("[system: context at %d%%. Consider /reincarnate at the next breakpoint to avoid running out of room mid-task — the fresh agent inherits identity but starts with a clean window.]", target)
}

// handleContextNudge fires an opt-in "consider reincarnating" hint
// when the agent's context crosses a configured threshold. Runs in the
// Stop-hook path, reads the stored context_pct, and delivers directly through
// the shared contention-safe pane injector. It intentionally remains
// daemon-independent: hooks can run when agentd is not running, and this
// ephemeral reminder does not need mailbox durability.
//
// Skips when:
//   - the feature isn't enabled in config
//   - the session id isn't known (callback running outside a tracked session)
//   - context_pct is below the configured min
//   - the same-or-higher threshold has already been fired
//     (sessions.nudged_pct; ResetCompact zeroes it so post-compact climbs re-arm)
func handleContextNudge(sessionID string, amb HookAmbient) {
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

	tmuxSession := amb.TmuxSession()
	if tmuxSession == "" {
		// No pane can receive this ephemeral hint. Stamp the threshold so a
		// later hook does not deliver a stale reminder for the same climb.
		_ = db.SetNudgedPct(sessionID, float64(target))
		return
	}

	msg := formatContextNudgeMessage(target)
	slog.Info("context-nudge: typing hint into pane",
		"session_id", sessionID, "tmux_session", tmuxSession,
		"context_pct", contextPct, "target", target,
		"min_pct", minPct, "interval_pct", intervalPct,
		"module", "hooks")
	if err := paneinput.InjectTextAndSubmit(tmuxSession+":0.0", msg, paneinput.Options{}); err != nil {
		slog.Warn("context-nudge: pane injection failed",
			"error", err, "module", "hooks")
		return
	}
	if err := db.SetNudgedPct(sessionID, float64(target)); err != nil {
		slog.Warn("context-nudge: failed to stamp nudged_pct",
			"error", err, "module", "hooks")
	}
}
