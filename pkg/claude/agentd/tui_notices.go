package agentd

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func tuiMutationOutcomeUnknown(err error) bool {
	var ambiguous *tuiAmbiguousMutationError
	return errors.As(err, &ambiguous)
}

// focusSpawned goes straight to a freshly spawned agent's pane — what the
// operator would do next anyway, and the reason they started it. It is enter
// on the new row, taken automatically: the same handover, the same gate.
//
// It stays out of the way in the two cases where that gate says no. A console
// the daemon does not classify as the human cannot attach at all (see
// attachSelected), and a spawn that has not produced a tmux session yet —
// a Codex agent held behind a startup gate — has no pane to go to. Both fall
// back to the ordinary "spawned, here it is in the listing" path.
func (m tuiModel) focusSpawned(msg tuiSpawnedMsg) (tuiModel, tea.Cmd) {
	session := msg.resp.TmuxSession
	convID := msg.resp.ConvID
	if !m.operator || !m.capabilities.attachAgent ||
		(m.capabilities.attachLocalPane && session == "") ||
		(!m.capabilities.attachLocalPane && convID == "") {
		return m, nil
	}
	name := msg.resp.AgentID
	if name == "" {
		name = msg.resp.ConvID
	}
	if name == "" {
		// Nothing has an identity yet; the pane's own name is what the
		// operator has to go on.
		name = session
	}
	if m.capabilities.attachLocalPane && insideTmux() {
		m.notice += " — switching to " + session + "…"
		return m, m.attachCmd(name, convID, session)
	}
	if m.capabilities.attachLocalPane {
		m.notice += " — attaching; detach (ctrl-b d) to come back."
	} else {
		m.notice += " — opening its remote terminal; press ctrl-] d to come back."
	}
	return m, m.attachCmd(name, convID, session)
}

// tuiSpawnSummary describes a landed spawn in one line. A Codex agent held
// behind a startup gate reports no conv-id yet, so say so rather than
// printing a blank identity.
func tuiSpawnSummary(msg tuiSpawnedMsg) string {
	id := msg.resp.AgentID
	if id == "" {
		id = msg.resp.ConvID
	}
	if id == "" {
		return fmt.Sprintf("Spawn accepted in group %s — the agent is still registering.", msg.group)
	}
	out := fmt.Sprintf("Spawned %s in group %s", id, msg.group)
	if msg.resp.TmuxSession != "" {
		out += " (tmux " + msg.resp.TmuxSession + ")"
	}
	return out
}

// tuiSpawnWarningNote renders the daemon's resolved launch warnings on the
// console's status line. These are launch-gate and degradation disclosures,
// not debug detail; dropping them here made the TUI disagree with the CLI and
// with the frozen launch plan.
func tuiSpawnWarningNote(resp agent.SpawnResponse) string {
	if resp.Resolved == nil {
		return ""
	}
	warnings := tuiTrimmedLines(resp.Resolved.Warnings)
	if len(warnings) == 0 {
		return ""
	}
	for i := range warnings {
		warnings[i] = strings.TrimSuffix(warnings[i], ".")
	}
	return " Note: " + strings.Join(warnings, "; ") + "."
}

// tuiAttachCarryingSpawnNotice binds a launch disclosure to the exact
// automatic handover it belongs to. Commands complete asynchronously, so a
// model-level latch could be consumed by an unrelated attach that finishes
// first.
func tuiAttachCarryingSpawnNotice(cmd tea.Cmd, notice string) tea.Cmd {
	if cmd == nil || notice == "" {
		return cmd
	}
	return func() tea.Msg {
		msg := cmd()
		attached, ok := msg.(tuiAttachedMsg)
		if !ok {
			return msg
		}
		attached.spawnNotice = notice
		return attached
	}
}

// tuiWorktreeReadyNotice says which of the two things the resolve step did.
// Reuse is not a corner case: naming the branch of a worktree that already
// exists is how this form picks an existing worktree, since it has no list to
// pick one from.
func tuiWorktreeReadyNotice(wt tuiWorktreeResponse) string {
	if wt.Created {
		return "Created worktree " + wt.Path + " on " + wt.Branch + "."
	}
	return "Reusing the existing worktree " + wt.Path + " on " + wt.Branch + "."
}

// worktreeLandedNote names the worktree the new agent launched in, so the
// listing's DIRECTORY column is not the first the operator hears of it.
func (m tuiModel) worktreeLandedNote() string {
	if m.spawnWorktree.Path == "" {
		return ""
	}
	return " — in " + m.spawnWorktree.Path
}

// worktreeKeptNote is what a failed spawn owes an operator whose worktree was
// created moments earlier: the directory is still there, and it is theirs to
// keep or remove.
//
// The console deliberately does not remove it. A failed spawn here is often an
// ambiguous one — the console latches reconcilingMutation precisely because it
// cannot tell a rejected request from a lost answer — and force-removing a
// directory a session may be starting up in is the one mistake that costs
// work. `tclaude worktree rm` removes it when the operator has decided.
func (m tuiModel) worktreeKeptNote() string {
	if !m.spawnWorktree.Created || m.spawnWorktree.Path == "" {
		return ""
	}
	return " The worktree " + m.spawnWorktree.Path + " was created and has been kept."
}
