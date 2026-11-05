package agentd

import (
	"errors"
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// prepareLaunchCommand picks how the pane is forked (a fresh session, or a
// resume of an imported conversation) and runs the last checks before the
// fork: the launch authority's recheck and the Claude subscription hold. An
// error is a confirmed launch failure for executeSpawn's launchFailed.
func prepareLaunchCommand(p *spawnParams, spawnArgs *clcommon.SpawnArgs, importedID, preConvID string) (func(clcommon.SpawnArgs) error, error) {
	launch := SpawnDetachedTclaudeNew
	if importedID != "" {
		spawnArgs.ConvID = importedID
		spawnArgs.SessionID = ""
		launch = SpawnDetachedTclaudeResume
	}
	if p.launchAuthority != nil {
		if err := p.launchAuthority(); err != nil {
			return nil, bundleLaunchPreparationFailed(*spawnArgs, err)
		}
	}
	if p.ClaudeSubscription.token != "" {
		agentID := p.AgentID
		if agentID == "" && preConvID != "" {
			agentID, _ = db.AgentIDForConv(preConvID)
		}
		if agentID == "" {
			return nil, errors.New("could not resolve the agent identity that holds the Claude subscription token")
		}
		if err := holdClaudeSubscription(agentID, p.ClaudeSubscription); err != nil {
			return nil, err
		}
		spawnArgs.ClaudeSubscriptionAgentID = agentID
	}
	return launch, nil
}

// spawnFocus is a spawn's auto-focus: when the caller asked for it, open a
// terminal window attached to the freshly-spawned agent — via `tclaude
// session attach`, never raw tmux, so the reattached session keeps its
// tclaude features. A detached spawn has no window of its own, so this is
// what lets the human watch and talk to the new agent right away and, for a
// pending Codex spawn, clear whatever startup gate (dir-trust /
// new-hooks-config / OpenAI auth modal) is holding its first turn.
//
// It is label-based and conv-id-independent, so it fires the moment the pane
// exists — before the conv-id, which is precisely when a gated pane needs a
// human at it. Fired at most once; best-effort, a failure to pop a window is
// logged, never bubbled.
type spawnFocus struct {
	label     string
	auto, web bool
	focused   bool
	// mode records what open actually did, for the spawnOutcome to report
	// back to the caller — see spawnOutcome.FocusMode. Left "" when AutoFocus
	// is off or the pane never came up within the poll, so open never ran.
	mode string
}

// open focuses the pane in tmuxSession. A persisted tmux name is launch
// intent, not readiness: session new writes it before creating the pane.
// Every caller must first have observed that live pane.
func (f *spawnFocus) open(tmuxSession string) {
	if !f.auto || f.focused || tmuxSession == "" {
		return
	}
	f.focused = true
	if f.web {
		// The requesting dashboard will attach the browser terminal from
		// focus_ws in the spawn response. In particular, do not briefly pop
		// a native window before handing the session back to the browser.
		f.mode = "browser"
		return
	}
	if err := openTerminal(openAttachCmd(f.label)); err != nil {
		// No native window — headless agentd (no DISPLAY/WAYLAND_DISPLAY)
		// or no terminal emulator installed. Don't just log and drop it:
		// report "browser" so the caller (handleGroupSpawn) can point the
		// dashboard at the in-browser terminal fallback, the same
		// mode:"browser" handshake handleDashboardOpenWindowAPI already
		// uses — otherwise auto-focus silently does nothing on a headless
		// host while claiming success.
		slog.Warn("spawn: auto-focus terminal failed to open natively; falling back to in-browser terminal",
			"label", f.label, "error", err)
		f.mode = "browser"
		return
	}
	f.mode = "native"
}

// launchedOutcome is the spawnOutcome of a launch that forked.
func launchedOutcome(p spawnParams, label, convID, tmuxSession, focusMode string) *spawnOutcome {
	return &spawnOutcome{AgentID: p.AgentID, ConvID: convID, Label: label, TmuxSession: tmuxSession, FocusMode: focusMode,
		Harness: p.Harness, Model: p.Model, Effort: p.Effort}
}
