package session

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/ratelimit"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// preflightNewLaunch runs runNew's host checks once the launch options are
// known and the no-session shortcuts are behind it, and resolves the working
// directory.
func preflightNewLaunch(dir string) (string, error) {
	// Self-guard: a Claude Code instance must not directly launch
	// another Claude Code session. Placed after the --join-group and
	// pass-through branches on purpose: --join-group delegates to the
	// agentd daemon (gated there by the `groups.members.spawn` permission), and
	// pass-through only prints `claude --help`/`--version`. Daemon-forked
	// spawns are unaffected — see GuardAgainstNestedSpawn.
	if err := GuardAgainstNestedSpawn(); err != nil {
		return "", err
	}

	// Check tmux is installed
	if err := CheckTmuxInstalled(); err != nil {
		return "", err
	}

	// Check if hooks are installed (warn if not)
	EnsureHooksInstalled(false, cliStdout, cliStderr)

	// Sync the configured Claude Code transcript-retention override
	// (claude_cleanup_period_days) into ~/.claude/settings.json. No-op unless
	// set; logs and continues on failure.
	_ = EnsureClaudeCleanupPeriod()

	// Determine working directory
	return resolveSessionDir(dir)
}

// newLaunchContext catches SIGINT and SIGTERM for the rest of the launch and
// bridges them into a context so WaitForRateLimit can be interrupted by
// Ctrl-C. The caller defers stop, which keeps the signals caught until runNew
// returns.
func newLaunchContext() (ctx context.Context, stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		signal.Stop(sigCh)
	}
}

// launchIdentity is who a launch is: the resumed conversation (if any), the
// session row's primary key, its tmux handle, and the working directory a
// resume moved it to.
type launchIdentity struct {
	fullConvID  string
	sessionID   string
	tmuxSession string // set by claimLaunchIdentity
	cwd         string
}

// resolveLaunchIdentity resolves a resume to its full conversation and picks
// the session row's primary key.
func resolveLaunchIdentity(h *harness.Harness, params *NewParams, cwd string) (launchIdentity, error) {
	// Extract just the ID from autocomplete format (e.g.,
	// "0459cd73_[title]_prompt..." -> "0459cd73"). OpenCode's native
	// server-issued IDs contain an underscore themselves (`ses_…`), so a
	// managed resume carrying the full ID must not go through the historical
	// underscore-delimited completion decoder.
	shortID := resumeIDFromParam(h, params.Resume)

	// Resolve to full UUID and get project path, via the harness's
	// conversation source (CC's cwd-indexed resolver, or another harness's
	// ConvStore — e.g. Codex's rollout/state DB).
	var fullConvID string
	var convProjectPath string
	if shortID != "" {
		var err error
		fullConvID, convProjectPath, err = resolveResumeConv(h, shortID, params.Global, cwd)
		if err != nil {
			return launchIdentity{}, err
		}
		// Use conversation's project directory instead of cwd
		if convProjectPath != "" {
			cwd = convProjectPath
		}
	}

	// The session row's primary key carries the FULL identity — never a
	// truncation. On resume that's the conversation's full UUID (already in
	// sessions.conv_id); collapsing it to 8 chars made two conversations
	// sharing a hex prefix collide on the PK (SaveSession's ON CONFLICT(id)
	// silently overwrote the row → wrong-session reattach + conflated
	// notify_state / session_cost_daily). See JOH-248.
	// Priority: explicit label > resumed conv UUID > random synthetic.
	sessionID := GenerateSessionID()
	if fullConvID != "" {
		sessionID = fullConvID
	}
	if params.Label != "" {
		sessionID = params.Label
	}
	return launchIdentity{fullConvID: fullConvID, sessionID: sessionID, cwd: cwd}, nil
}

// claimLaunchIdentity makes the identity this launch's own: it reserves a
// resumed conversation, refuses a primary key a live session already owns,
// picks a unique tmux name, and waits out a rate limit when asked. On success
// the caller defers release, which holds the conversation reservation until
// runNew returns; on failure the reservation is already released.
func claimLaunchIdentity(ctx context.Context, params *NewParams, id *launchIdentity) (release func(), err error) {
	held := func() {}
	defer func() {
		if err != nil {
			held()
		}
	}()

	// When resuming, reserve the conversation before launching. This both
	// rejects an already-live conv AND serializes against a concurrent resume:
	// two `session new -r` of the same conv that both pass a bare read-guard
	// would otherwise both run `claude --resume` on the same .jsonl (interleaved
	// appends → corruption), because the disambiguated tmux name no longer makes
	// the second `new-session` clean-fail. Keyed on conv_id, it catches the live
	// session whatever its PK shape (full-UUID / synthetic / old convID[:8]).
	// The lock is held (defer) until the session row is written and runNew
	// returns; the OS frees it if this process dies. The PK guard below still
	// backstops a reused --label. See JOH-332.
	if id.fullConvID != "" {
		reserved, reject := ReserveConvForLaunch(id.fullConvID)
		if reject != nil {
			return nil, reject
		}
		held = reserved
		if !params.ManagedLaunch {
			if err := ReinstateRetiredForResume(id.fullConvID, id.cwd); err != nil {
				return nil, err
			}
		}
	}

	// The session PK is now final (priority above: label > resumed conv UUID >
	// random synthetic). Reject if a LIVE session already owns it. The tmux
	// name is disambiguated below (UniqueTmuxSessionName), so without this
	// guard SaveSessionState's ON CONFLICT(id) would silently overwrite that
	// live session's row. Before JOH-248 the PK and tmux name were identical,
	// so the duplicate `new-session` clean-failed; this restores that now that
	// the names diverge. A row owned only by a DEAD session is fine to reuse.
	// (This PK guard primarily backstops a reused --label; the resumed-conv
	// case is covered by the conv_id guard above. See JOH-332.)
	owner, err := liveOwnerConflict(id.sessionID, params.Label)
	if err != nil {
		return nil, err
	}
	if owner != nil {
		return nil, fmt.Errorf("session %s already exists for this conversation; attach with: tclaude session attach %s", owner.TmuxSession, owner.TmuxSession)
	}

	// The tmux session name is the short, human-facing handle (tmux status
	// line, `session ls`, attach target). Render it short here while the PK
	// stays full, and keep it unique among live tmux sessions.
	id.tmuxSession = UniqueTmuxSessionName(TmuxNameBase(id.sessionID, params.Label, id.cwd))

	if params.WaitForRateLimit {
		if ratelimit.WaitForRateLimit(ctx, cliStdout, id.sessionID, id.cwd) {
			return nil, fmt.Errorf("interrupted")
		}
	}
	return held, nil
}
