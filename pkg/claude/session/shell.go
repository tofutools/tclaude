package session

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// ShellHarnessName is the sentinel `--harness` value that starts a plain,
// ephemeral interactive shell instead of a coding harness. It is
// deliberately NOT registered in pkg/claude/harness: a shell session has no
// conversation, no hooks, and no model/sandbox/approval concepts, so
// folding it into the harness registry would surface it across the
// daemon's agent spawn/clone/reincarnate, spawn profiles, and conv
// listing — machinery built around a coding agent's conversation. runNew
// branches to runNewShell for this value before any harness resolution
// happens, so none of that machinery is affected.
const ShellHarnessName = "shell"

// ShellSession identifies a shell session that has just been created: the
// row's primary key, the tmux session to attach to, and the directory it was
// started in.
type ShellSession struct {
	SessionID   string
	TmuxSession string
	Cwd         string
}

// StartShellSession creates a detached shell session and returns its handle,
// printing nothing and attaching nothing — for callers that already own the
// terminal (the `agentd serve --tui` console) and do their own announcing and
// attaching. dir may be empty (the caller's working directory), relative, or
// "~"-prefixed; label may be empty (a synthetic session id is generated).
//
// It is the same launch `tclaude session new --shell --detached` performs,
// minus the CLI flag rejection: the parameters a shell session cannot carry
// are simply not accepted here. The label is charset-gated by the shared
// launch below.
func StartShellSession(dir, label string) (ShellSession, error) {
	return startShellSession(&NewParams{
		Dir:      clcommon.ExpandHomePrefix(dir),
		Label:    label,
		Detached: true,
	})
}

// runNewShell starts a plain interactive shell in a new tmux session: same
// detach/reattach, `session ls`/watch visibility, and attach/kill as a
// coding-harness session, but with no conversation, no hooks, and none of
// the model/sandbox/approval/rename/compact machinery those sessions carry.
// Only --dir/-C, --label and --detached apply; every other NewParams field
// is coding-harness-only and is rejected here with a clear error rather than
// silently ignored.
func runNewShell(params *NewParams) error {
	if err := rejectShellUnsupportedFlags(params); err != nil {
		return err
	}
	created, err := startShellSession(params)
	if err != nil {
		return err
	}
	return announceAndAttach(fmt.Sprintf("Created shell session %s", created.TmuxSession),
		created.SessionID, created.TmuxSession, created.Cwd, params.Detached)
}

// startShellSession is the launch itself, shared by the CLI path above and
// StartShellSession. It creates the pane and its session row and returns once
// both are live; announcing and attaching belong to the caller.
func startShellSession(params *NewParams) (ShellSession, error) {
	// The label is charset-gated first, before any guard, probe or state is
	// touched: it is used verbatim as the tmux session name and becomes the
	// session id that reaches tmux's set-titles-string (see
	// ValidateSessionLabel). Gating here rather than at each entry point is
	// what makes it hold for `session new --shell --label` as well as for
	// StartShellSession — the two paths build the same tmux name from the same
	// field.
	if err := ValidateSessionLabel(params.Label); err != nil {
		return ShellSession{}, err
	}

	// Same nested-spawn guard and tmux-presence check as a coding-harness
	// launch — a plain shell is still a tmux session tclaude is about to
	// create.
	if err := GuardAgainstNestedSpawn(); err != nil {
		return ShellSession{}, err
	}
	if err := CheckTmuxInstalled(); err != nil {
		return ShellSession{}, err
	}

	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return ShellSession{}, err
	}

	// No conversation exists to resume, so the session id is always either
	// the chosen label or a fresh synthetic id — never a resumed conv UUID.
	sessionID := GenerateSessionID()
	if params.Label != "" {
		sessionID = params.Label
	}

	// Guard a reused --label the same way runNew does: a label is a fresh
	// identity each launch, so it must not collide with a different live
	// session's PK (SaveSessionState's ON CONFLICT(id) would otherwise
	// silently overwrite it). See JOH-248/JOH-332 (liveSessionOwningID).
	owner, err := liveOwnerConflict(sessionID, params.Label)
	if err != nil {
		return ShellSession{}, err
	}
	if owner != nil {
		return ShellSession{}, fmt.Errorf("session %s already exists; attach with: tclaude session attach %s", owner.TmuxSession, owner.TmuxSession)
	}

	tmuxSession := UniqueTmuxSessionName(TmuxNameBase(sessionID, params.Label, cwd))

	shellBin := shellBinary()

	// TCLAUDE_SESSION_ID is a stable session-row/routing key for local session
	// operations. It is caller-controlled compatibility state, never proof of
	// daemon caller identity.
	exitGeneration := newExitLaunchGeneration(sessionID, tmuxSession)
	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID":      sessionID,
		"TCLAUDE_EXIT_GENERATION": exitGeneration,
	}
	envExports := clcommon.BuildEnvExports(additionalEnv)
	// `exec` matters here, unlike the coding-harness spawners (claudeSpawner/
	// codexSpawner), which leave their wrapper `sh` in place and instead
	// self-correct #{pane_pid} afterward via their SessionStart hook +
	// FindClaudePID (see the comment on that in status_callback.go). A shell
	// session has no hook to run that correction, so without `exec` the pane's
	// real process tree would be sh -> shellBin (an extra, permanent wrapper
	// process) and ParsePIDFromTmux below would key liveness off the wrapper,
	// not the shell the user is actually typing into. `exec` replaces the
	// wrapper's own process image with shellBin — same PID, one process.
	shellCmd := envExports + "exec " + clcommon.ShellQuoteArg(shellBin)
	launchCreated := time.Now()
	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		Cwd:         cwd,
		Status:      StatusRunning,
		Harness:     ShellHarnessName,
		Created:     launchCreated,
		Updated:     launchCreated,
	}
	// Same launch-row rollback contract as runNew: a launch that fails before
	// its pane survives deletes the row it just created, so a failed shell
	// launch cannot strand a ghost session row. Pre-existing rows (a reused
	// dead label) keep their prior state on failure, exactly as before.
	priorRow, err := db.LoadSession(sessionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ShellSession{}, fmt.Errorf("check for existing session row: %w", err)
	}
	launchRowOwned := priorRow == nil
	launchRowCommitted := false
	defer func() {
		if launchRowCommitted || !launchRowOwned {
			return
		}
		// Generation-conditional for the same reason as runNew: never take a
		// concurrent same-label winner's row down with this failed launch.
		if derr := db.DeleteSessionForLaunchGeneration(sessionID, exitGeneration); derr != nil {
			slog.Warn("shell launch failed; could not remove its session row",
				"session_id", sessionID, "error", derr)
		}
	}()
	if err := SaveSessionStateForLaunch(state, exitGeneration, db.SessionExitGateUngated); err != nil {
		return ShellSession{}, fmt.Errorf("prepare managed pane exit identity: %w", err)
	}
	exitGuard, err := newExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	if err != nil {
		slog.Warn("exit audit: private launch setup unavailable; continuing without callback",
			"session_id", sessionID, "tmux_session", tmuxSession, "error", err)
		exitGuard = disabledExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	} else if err := db.MarkSessionExitLaunchPending(sessionID, exitGeneration); err != nil {
		slog.Warn("exit audit: launch gate state unavailable; continuing without callback",
			"session_id", sessionID, "tmux_session", tmuxSession, "error", err)
		exitGuard.abort()
		exitGuard = disabledExitLaunchGuard(sessionID, tmuxSession, exitGeneration)
	}
	defer exitGuard.abort()
	shellCmd = exitGuard.wrap(shellCmd)

	if err := launchDetachedTmuxSession(tmuxSession, cwd, shellCmd); err != nil {
		return ShellSession{}, err
	}
	exitGuard.armPaneHook()
	exitGuard.bind()

	applyTmuxWindowTitle(tmuxSession, sessionID)

	// A plain shell has no self-managed scrollback (unlike Claude Code's
	// TUI), so always enable tmux mouse mode for it — unconditionally,
	// unlike ConfigureTmuxScrollback's per-harness gate.
	enableTmuxMouseScrollback(tmuxSession)

	ConfigureTmuxKeybindings()

	pid := ParsePIDFromTmux(tmuxSession)

	state.PID = pid
	if err := SaveSessionState(state); err != nil {
		// Kill the pane like the exit-audit failure path below: leaving it
		// alive while the launch-row defer rolls the row back would orphan a
		// live pane tclaude can no longer see.
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return ShellSession{}, fmt.Errorf("failed to save session state: %w", err)
	}
	if err := exitGuard.release(); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return ShellSession{}, fmt.Errorf("bind managed pane exit audit: %w", err)
	}

	// The pane is up and bound; from here the row belongs to the live session
	// (the caller's own announce/attach failing must not delete it).
	launchRowCommitted = true
	return ShellSession{SessionID: sessionID, TmuxSession: tmuxSession, Cwd: cwd}, nil
}

// shellBinary picks the interactive shell to launch: $SHELL, falling back
// to /bin/sh when unset (e.g. a minimal environment with no login shell
// configured).
func shellBinary() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// rejectShellUnsupportedFlags errors out on any NewParams field that only
// makes sense for a coding harness (conversation resume, model/effort,
// sandbox/approval posture, launch enrollment, …) — a plain shell has none
// of those concepts, so setting one explicitly is a mistake worth surfacing
// clearly rather than silently dropping.
func rejectShellUnsupportedFlags(params *NewParams) error {
	const notApplicable = "%s is not applicable to a shell session (--harness %s): %s"
	switch {
	case params.Resume != "":
		return fmt.Errorf(notApplicable, "--resume", ShellHarnessName, "it is ephemeral and has no conversation to resume")
	case params.Global:
		return fmt.Errorf(notApplicable, "--global/-g", ShellHarnessName, "it only widens --resume's conversation lookup, and a shell session has no conversation to resume")
	case params.Model != "":
		return fmt.Errorf(notApplicable, "--model", ShellHarnessName, "a shell session has no model")
	case params.Effort != "":
		return fmt.Errorf(notApplicable, "--effort", ShellHarnessName, "a shell session has no model")
	case params.Sandbox != "":
		return fmt.Errorf(notApplicable, "--sandbox", ShellHarnessName, "it has no launch-time sandbox mode")
	case params.PermissionProfile != "":
		return fmt.Errorf(notApplicable, "--permission-profile", ShellHarnessName, "it has no permission profiles")
	case params.Approval != "":
		return fmt.Errorf(notApplicable, "--ask-for-approval", ShellHarnessName, "it has no approval policy")
	case params.ToolGovernance != "":
		return fmt.Errorf(notApplicable, "--tools", ShellHarnessName, "it has no tool-governance policy")
	case params.AutoReview:
		return fmt.Errorf(notApplicable, "--auto-review", ShellHarnessName, "it has no approvals reviewer")
	case params.TrustDir:
		return fmt.Errorf(notApplicable, "--trust-dir", ShellHarnessName, "it has no trust-folder concept")
	case params.RemoteControl:
		return fmt.Errorf(notApplicable, "--remote-control", ShellHarnessName, "it has no built-in remote access")
	case params.AutoMemory:
		return fmt.Errorf(notApplicable, "--auto-memory", ShellHarnessName, "it has no auto-memory system")
	case params.WaitForRateLimit:
		return fmt.Errorf(notApplicable, "--wait-for-rate-limit", ShellHarnessName, "it has no API rate limit to wait on")
	case params.JoinGroup != "":
		return fmt.Errorf(notApplicable, "--join-group", ShellHarnessName, "it has no conversation to enroll in an agent group")
	case params.Name != "":
		return fmt.Errorf(notApplicable, "--name", ShellHarnessName, "it has no conversation title; use --label to name the tmux handle")
	case params.Role != "":
		return fmt.Errorf(notApplicable, "--role", ShellHarnessName, "it only tags a member joining an agent group, and a shell session has no conversation to enroll")
	case params.Descr != "":
		return fmt.Errorf(notApplicable, "--descr", ShellHarnessName, "it only describes a member joining an agent group, and a shell session has no conversation to enroll")
	case params.InitialPrompt != "":
		return fmt.Errorf(notApplicable, "--initial-prompt", ShellHarnessName, "it has no first-turn prompt")
	case params.SessionID != "":
		return fmt.Errorf(notApplicable, "--session-id", ShellHarnessName, "it has no conversation id")
	case len(clcommon.ExtractClaudeExtraArgs()) > 0:
		return fmt.Errorf("passthrough args after -- are not supported for a shell session (--harness %s)", ShellHarnessName)
	}
	return nil
}
