package session

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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

	// Same nested-spawn guard and tmux-presence check as a coding-harness
	// launch — a plain shell is still a tmux session tclaude is about to
	// create.
	if err := GuardAgainstNestedSpawn(); err != nil {
		return err
	}
	if err := CheckTmuxInstalled(); err != nil {
		return err
	}

	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return err
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
		return err
	}
	if owner != nil {
		return fmt.Errorf("session %s already exists; attach with: tclaude session attach %s", owner.TmuxSession, owner.TmuxSession)
	}

	tmuxSession := UniqueTmuxSessionName(TmuxNameBase(sessionID, params.Label, cwd))

	shellBin := shellBinary()

	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID": sessionID,
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
	exitGuard, err := newExitLaunchGuard(sessionID, tmuxSession)
	if err != nil {
		slog.Warn("exit audit: private launch setup unavailable; continuing without callback",
			"session_id", sessionID, "tmux_session", tmuxSession, "error", err)
		exitGuard = disabledExitLaunchGuard(sessionID, tmuxSession)
	}
	defer exitGuard.abort()
	shellCmd = exitGuard.wrap(shellCmd)

	if err := launchDetachedTmuxSession(tmuxSession, cwd, shellCmd); err != nil {
		return err
	}
	exitGuard.armPaneHook()

	applyTmuxWindowTitle(tmuxSession, sessionID)

	// A plain shell has no self-managed scrollback (unlike Claude Code's
	// TUI), so always enable tmux mouse mode for it — unconditionally,
	// unlike ConfigureTmuxScrollback's per-harness gate.
	enableTmuxMouseScrollback(tmuxSession)

	ConfigureTmuxKeybindings()

	pid := ParsePIDFromTmux(tmuxSession)

	state := &SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         cwd,
		Status:      StatusRunning,
		Harness:     ShellHarnessName,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	if err := exitGuard.bindAndRelease(); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return fmt.Errorf("bind managed pane exit audit: %w", err)
	}

	return announceAndAttach(fmt.Sprintf("Created shell session %s", tmuxSession), sessionID, tmuxSession, cwd, params.Detached)
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
	case params.AutoReview:
		return fmt.Errorf(notApplicable, "--auto-review", ShellHarnessName, "it has no approvals reviewer")
	case params.TrustDir:
		return fmt.Errorf(notApplicable, "--trust-dir", ShellHarnessName, "it has no trust-folder concept")
	case params.RemoteControl:
		return fmt.Errorf(notApplicable, "--remote-control", ShellHarnessName, "it has no built-in remote access")
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
