package agentd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// tuiAttachToPane hands this terminal to the tmux session named by target,
// indirected through a package var so tests can observe the target without a
// tmux server. inTmux reports whether agentd itself is running inside tmux.
var tuiAttachToPane = realTUIAttachToPane

func (m tuiModel) attachCmd(agentName, convID, tmuxSession string) tea.Cmd {
	if m.api != nil {
		return m.api.attach(agentName, convID, tmuxSession)
	}
	return tuiAttachToPane(agentName, tmuxSession, insideTmux())
}

// realTUIAttachToPane is the production handover, and it has two shapes
// because agentd's own terminal may already be a tmux client:
//
//   - Inside tmux, `switch-client` moves that client to the agent's session.
//     It returns immediately and the console keeps running in its own window,
//     so the operator can switch back with tmux's own keys. Attaching instead
//     would nest one session inside another, which tmux refuses.
//   - Outside tmux, `attach-session` under tea.ExecProcess: bubbletea releases
//     the terminal, tmux owns it until the operator detaches, and the console
//     repaints itself afterwards.
func realTUIAttachToPane(agentName, tmuxSession string, inTmux bool) tea.Cmd {
	target := clcommon.ExactTarget(tmuxSession)
	if inTmux {
		return func() tea.Msg {
			out, err := clcommon.TmuxCommand("switch-client", "-t", target).CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return tuiAttachedMsg{agent: agentName, session: tmuxSession, err: err}
		}
	}
	args := append(clcommon.NativeTmuxClientFeatureArgs(os.Getenv("TERM")), "attach-session", "-t", target)
	cmd := clcommon.TmuxCommand(args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		// tmux exits 1 on a plain detach, which is not a failure — the same
		// reading session.attachToSessionWithFlags applies.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			err = nil
		}
		return tuiAttachedMsg{agent: agentName, session: tmuxSession, err: err}
	})
}

// insideTmux reports whether this process is itself a tmux client.
func insideTmux() bool { return os.Getenv("TMUX") != "" }

// enterSelected is what enter does on a row. On an agent it depends on
// whether it is up: an offline one is turned back on, a live one gets this
// terminal handed to its pane. Both are the move the operator wants next
// after picking that row, and an offline agent has no pane to attach to
// anyway — enter on it used to be a dead key that only said so. On a
// non-agent session there is only ever a live pane, so enter goes to it.
func (m tuiModel) enterSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedRow()
	if !ok {
		return m, nil
	}
	if row.isSession() {
		return m.attachSessionSelected(row.session)
	}
	if !row.agent.Online {
		return m.resumeSelected(row.agent)
	}
	return m.attachSelected()
}

// attachSessionSelected hands this terminal to a non-agent session's pane —
// the console's `tclaude session attach <handle>`, and the same handover enter
// makes on a live agent's row.
//
// The gates are the shell form's, for the shell form's reasons: the pane is on
// the daemon's host, outside any agent sandbox, and it is the operator's own
// shell. A console the daemon does not classify as the human is refused, and
// so is one that does not share the host. In practice neither refusal is
// reachable from the listing — a console that fails either never sees a
// session row at all (listsLocalSessions) — but the check lives with the
// action rather than depending on the listing to have withheld it.
func (m tuiModel) attachSessionSelected(row tuiSessionRow) (tuiModel, tea.Cmd) {
	if !m.operator {
		m.notice = "Only an operator console can go to a session's terminal."
		return m, nil
	}
	if !m.capabilities.localSessions || !m.capabilities.attachLocalPane {
		m.notice = "This console cannot go to a session — it does not share the daemon's host."
		return m, nil
	}
	if row.TmuxSession == "" {
		m.notice = row.name() + " has no tmux session to go to."
		return m, nil
	}
	if insideTmux() {
		m.notice = "Switching to " + row.TmuxSession + "…"
	} else {
		m.notice = "Attaching to " + row.name() + " — detach (ctrl-b d) to come back."
	}
	return m, tuiAttachToPane(row.name(), row.TmuxSession, insideTmux())
}

// resumeSelected starts the selected agent's session again through the
// daemon's resume verb. It asks nothing first: resume is not destructive (it
// relaunches the agent in its recorded directory and conversation) and it is
// what the grey status dot does on the web dashboard.
//
// Permission is the daemon's call, not the console's — same as retire. An
// agent-class console holding agent.resume over its own group's members may
// use this; one that does not gets the endpoint's own refusal.
func (m tuiModel) resumeSelected(row tuiAgentRow) (tuiModel, tea.Cmd) {
	if m.resuming {
		m.notice = "A resume is already in flight."
		return m, nil
	}
	if row.ConvID == "" {
		// A member that has never had a conversation (a placeholder row) has
		// nothing to resume, and the daemon's own answer would be a 404 on a
		// path built from an empty selector.
		m.notice = row.name() + " has no conversation to start."
		return m, nil
	}
	m.resuming = true
	m.notice = "Starting " + row.name() + "…"
	return m, tuiResumeCmd(m.api, row.ConvID, row.name())
}

// tuiResumeSummary describes a landed resume in one line. Anything that is
// not a plain "resumed" carries its reason in Detail — a launch directory
// that no longer exists, provenance the daemon will not trust unattended —
// and this console has no browser to go and read that in, so the reason
// travels with the verdict or it is lost.
func tuiResumeSummary(msg tuiResumedMsg) string {
	var out string
	switch action := msg.res.Action; action {
	case "resumed":
		out = "Started " + msg.agent + "."
	case "":
		// No action at all: an older daemon, or a response shape that did not
		// carry one. Claiming a start that may not have happened is the one
		// reading to avoid — the listing settles it either way.
		out = "Asked the daemon to start " + msg.agent + "."
	case "skipped:already_online":
		out = msg.agent + " was already running."
	case "skipped:not_active_agent":
		// The agent was retired out from under the listing. That is not a
		// failure to report as one, and the raw wire token means nothing to
		// an operator.
		out = msg.agent + " has been retired, so it cannot be started from here."
	default:
		out = "Could not start " + msg.agent + ": " + action
		if detail := strings.TrimSpace(msg.res.Detail); detail != "" {
			out += " (" + detail + ")"
		}
		out += "."
	}
	if warnings := tuiTrimmedLines(msg.res.Warnings); len(warnings) > 0 {
		out += " Note: " + strings.Join(warnings, "; ") + "."
	}
	return out
}

// tuiTrimmedLines drops the blank entries from a wire string list and trims
// the rest, so a stray empty warning cannot render as an empty clause.
func tuiTrimmedLines(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// attachSelected puts the operator on the selected agent's tmux session — the
// console's answer to "let me actually go look at what this agent is doing".
//
// Both transports hand THIS terminal to the agent: the embedded API resolves
// the daemon-host tmux session directly, while the remote API streams the web
// dashboard's authenticated terminal WebSocket. The operator gate applies to
// both, so an agent-class console cannot use either path to reach a peer's pane
// and drive it.
func (m tuiModel) attachSelected() (tuiModel, tea.Cmd) {
	row, ok := m.selectedAgent()
	if !ok {
		return m, nil
	}
	if !m.operator {
		m.notice = "Only an operator console can attach to an agent's terminal."
		return m, nil
	}
	if !m.capabilities.attachAgent {
		m.notice = "This console cannot attach to an agent's terminal."
		return m, nil
	}
	if m.capabilities.attachLocalPane {
		sess := pickAliveSession(row.ConvID)
		if sess == nil || sess.TmuxSession == "" {
			m.notice = row.name() + " has no live tmux session to attach to."
			return m, nil
		}
		if insideTmux() {
			m.notice = "Switching to " + sess.TmuxSession + "…"
			return m, m.attachCmd(row.name(), row.ConvID, sess.TmuxSession)
		}
		m.notice = "Attaching to " + row.name() + " — detach (ctrl-b d) to come back."
		return m, m.attachCmd(row.name(), row.ConvID, sess.TmuxSession)
	}
	m.notice = "Opening remote terminal for " + row.name() + " — press ctrl-] d to come back."
	return m, m.attachCmd(row.name(), row.ConvID, "")
}

// confirmDeleteStepSelected moves the selected row one step toward removal.
// On an agent that is the lifecycle it already had: online agents are taken
// offline, while agents already offline are retired. On a non-agent session
// there is no such ladder — a session has no offline state to park in and
// nothing to retire from — so the one step is ending it. Every action asks
// first, and the target is captured here so a background refresh cannot move
// another row under the cursor before confirmation.
func (m tuiModel) confirmDeleteStepSelected() tuiModel {
	row, ok := m.selectedRow()
	if !ok {
		return m
	}
	if row.isSession() {
		if !m.operator || !m.capabilities.localSessions {
			m.notice = "Only an operator console on the daemon's host can end a session."
			return m
		}
		if m.killingSession {
			m.notice = "A session is already being ended."
			return m
		}
		m.lifecycleTarget = row
		m.mode = tuiModeConfirmKillSession
		return m
	}
	if row.agent.Online {
		if m.stopping {
			m.notice = "A stop is already in flight."
			return m
		}
		m.lifecycleTarget = row
		m.mode = tuiModeConfirmStop
		return m
	}
	if m.retiring {
		m.notice = "A retire is already in flight."
		return m
	}
	m.lifecycleTarget = row
	m.mode = tuiModeConfirmRetire
	return m
}

// stopConfirmed fires the graceful stop the operator just confirmed.
func (m tuiModel) stopConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.agent
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.ConvID == "" {
		return m, nil
	}
	m.stopping = true
	m.notice = "Taking " + row.name() + " offline…"
	return m, tuiStopCmd(m.api, row.ConvID, row.name())
}

// killSessionConfirmed ends the non-agent session the operator just confirmed.
// Unlike the agent verbs beside it this goes nowhere near the daemon's API:
// there is no permission to check on a session and no HTTP shape for it, so
// the operator gate captured when the prompt opened is the whole gate — the
// same arrangement attaching and the shell launch have.
func (m tuiModel) killSessionConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.session
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.TmuxSession == "" {
		return m, nil
	}
	m.killingSession = true
	m.notice = "Ending session " + row.name() + "…"
	return m, tuiKillSessionCmd(row)
}

// retireConfirmed fires the retire the operator just confirmed. Permission is
// the daemon's call, not the console's: the endpoint gates every caller class
// itself, so an agent-class console gets the same refusal here it would get
// over the socket rather than a second, divergent rule.
func (m tuiModel) retireConfirmed() (tuiModel, tea.Cmd) {
	row := m.lifecycleTarget.agent
	m.lifecycleTarget = tuiListRow{}
	m.mode = tuiModeList
	if row.ConvID == "" {
		return m, nil
	}
	m.retiring = true
	m.notice = "Retiring " + row.name() + "…"
	return m, tuiRetireCmd(m.api, row.ConvID, row.name())
}

// tuiStopSummary describes a landed stop in one line.
func tuiStopSummary(msg tuiStoppedMsg) string {
	detail := strings.TrimSpace(msg.res.Detail)
	withDetail := func(summary string) string {
		if detail != "" {
			summary += " Note: " + detail + "."
		}
		return summary
	}
	switch msg.res.Action {
	case "soft_stopped":
		return withDetail("Asked " + msg.agent + " to go offline — its session was asked to exit.")
	case "killed_no_soft_exit":
		return withDetail("Took " + msg.agent + " offline — its harness has no graceful exit, so the session was stopped.")
	case "skipped:already_offline":
		return withDetail(msg.agent + " was already offline.")
	case "":
		return withDetail("Asked the daemon to take " + msg.agent + " offline.")
	default:
		out := "Could not take " + msg.agent + " offline: " + msg.res.Action
		if detail != "" {
			out += " (" + detail + ")"
		}
		return out + "."
	}
}

// tuiRetireSummary describes a landed retire in one line: what the demotion
// gave up, and what happened to the pane.
func tuiRetireSummary(msg tuiRetiredMsg) string {
	out := "Retired " + msg.agent
	if groups := msg.res.Outcome.GroupsLeft; len(groups) > 0 {
		out += " — left " + strings.Join(groups, ", ")
	}
	switch msg.res.Shutdown.Action {
	case "":
		// No shutdown block at all: an older daemon, or a response shape
		// that did not carry one. Say nothing rather than guess.
	case "skipped:already_offline":
		out += "; it had no live session"
	case "soft_stopped":
		out += "; its session was asked to exit"
	default:
		// Anything else is a shutdown that did not go to plan — "error" above
		// all, which carries its reason in Detail. This console has no browser
		// to go and read that in, so the reason travels with the verdict or it
		// is lost: the operator would otherwise be told only that something
		// went wrong with a pane that is, in that case, still running.
		out += "; session: " + msg.res.Shutdown.Action
		if detail := strings.TrimSpace(msg.res.Shutdown.Detail); detail != "" {
			out += " (" + detail + ")"
		}
	}
	return out + "."
}
