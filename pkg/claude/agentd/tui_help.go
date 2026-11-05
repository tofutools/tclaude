package agentd

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderHelp is the whole help text: every body line with the closing hint
// under it. renderHelpView is what the console actually shows — it windows the
// same body to the terminal — so this is the text in full, whatever the
// terminal can hold of it.
func (m tuiModel) renderHelp() string {
	return strings.Join(m.helpBodyLines(), "\n") + "\n\n" + tuiHelpCloseHint + "\n"
}

// tuiHelpCloseHint is the footer under a help view that fits on the terminal,
// and tuiHelpScrollHint the one under a view that does not. Both are one row
// by contract — renderHelpView trims rather than wraps them, as the usage line
// does — so the body's row budget is fixed at tuiHelpChrome.
const (
	tuiHelpCloseHint  = "  Press any key to close."
	tuiHelpScrollHint = "  ↑/↓ scroll • pgup/pgdn page • home/end jump • any other key closes"
)

// tuiHelpChrome is what renderHelpView spends on everything that is not a body
// line: the blank line above the footer, and the footer.
const tuiHelpChrome = 2

// helpBodyLines is the help text as terminal lines, without the footer pinned
// under it. The console scrolls this slice rather than the rendered string, so
// a body taller than the terminal stays reachable to its last line.
func (m tuiModel) helpBodyLines() []string {
	var b strings.Builder
	b.WriteString("\n  tclaude terminal dashboard — keys\n\n")
	b.WriteString("  List\n")
	if m.listsLocalSessions() {
		b.WriteString("    Two kinds of row. Agents first; below them this host's plain tmux\n")
		b.WriteString("    sessions — the ones s starts here and any `tclaude session new` —\n")
		b.WriteString("    marked \"" + tuiSessionGroupCell + "\" in the GROUP column they have nothing to put in.\n")
		b.WriteString("    Live ones only: `tclaude session ls -a` has the rest.\n")
	}
	b.WriteString("    ↑/k, ↓/j   Move the cursor\n")
	b.WriteString("    enter      On an offline agent: start it again, in the directory and\n")
	b.WriteString("               conversation it was last running.\n")
	if m.capabilities.attachAgent {
		if m.capabilities.attachLocalPane {
			b.WriteString("               On a live one: go to its tmux session — switch-client when\n")
			b.WriteString("               agentd runs inside tmux, otherwise attach until you detach\n")
			b.WriteString("               with ctrl-b d.\n")
		} else {
			b.WriteString("               On a live one: stream its daemon-host tmux session through\n")
			b.WriteString("               the dashboard's authenticated terminal WebSocket. Press\n")
			b.WriteString("               ctrl-] d to close only that stream and return; press\n")
			b.WriteString("               ctrl-] ctrl-] to send a literal ctrl-] remotely.\n")
		}
		b.WriteString("               Going to a pane is operator consoles only;\n")
		b.WriteString("               starting one is whatever the daemon grants this console.\n")
	}
	if m.listsLocalSessions() {
		b.WriteString("               On a session: go to its pane the same way (it is always\n")
		b.WriteString("               live, so enter never means anything else there).\n")
	}
	b.WriteString("    n          Start a new agent\n")
	if m.capabilities.startLocalShell {
		b.WriteString("    s          Start a plain interactive shell session and attach to it.\n")
		b.WriteString("               Operator consoles only.\n")
	}
	b.WriteString("    delete     Move the selected agent one step toward removal (asks first):\n")
	b.WriteString("               a live agent is taken offline; an already-offline agent is\n")
	b.WriteString("               retired, leaving its groups and losing its grants. Retiring\n")
	b.WriteString("               keeps the conversation and any worktree.\n")
	if m.listsLocalSessions() {
		b.WriteString("               A session has no such ladder, so delete ends it: that is\n")
		b.WriteString("               `tmux kill-session` — whatever runs in the pane stops, no\n")
		b.WriteString("               graceful exit, no starting it again. It asks first too.\n")
	}
	b.WriteString("    f          Filter the list: only show active agents (toggle)\n")
	if m.listsLocalSessions() {
		b.WriteString("               Sessions are unaffected: every listed one is already live.\n")
	}
	b.WriteString("    r          Refresh now (the list also polls every 2s)\n")
	b.WriteString("    ?/h        This help\n")
	if m.capabilities.shutdownOnQuit {
		b.WriteString("    q/esc      Quit — this SHUTS DOWN the daemon (asks first)\n\n")
	} else {
		b.WriteString("    q/esc      Quit this console; agentd keeps running (asks first)\n\n")
	}
	b.WriteString("  New agent\n")
	b.WriteString("    tab/↑/↓    Next / previous field\n")
	if m.capabilities.completeLocalDir {
		b.WriteString("    tab        On a Directory you have typed into, complete the path\n")
		b.WriteString("               instead: one match completes it, several list below\n")
		b.WriteString("               the field. Operator consoles only. On the field as the\n")
		b.WriteString("               form left it, tab still moves to the next field.\n")
	}
	b.WriteString("    ←/→        Change the group, spawn profile, worktree or harness\n")
	b.WriteString("               A profile of \"(default)\" names none, which leaves the\n")
	b.WriteString("               group's and the global default profile in force.\n")
	b.WriteString("               Directory starts on the group's own default directory\n")
	b.WriteString("               (add a subdirectory to start below it) and follows the\n")
	b.WriteString("               group picker until you type a path of your own.\n")
	b.WriteString("               Worktree \"create new worktree\" cuts a git worktree in the\n")
	b.WriteString("               repo that directory is in and starts the agent inside it.\n")
	b.WriteString("               Its Branch follows the Name until you type one of your\n")
	b.WriteString("               own; naming a branch that already has a worktree reuses\n")
	b.WriteString("               that worktree instead of making another. Operator\n")
	b.WriteString("               consoles only — the worktree is created on the daemon's\n")
	b.WriteString("               host, and a failed spawn leaves it for you to keep or\n")
	b.WriteString("               remove.\n")
	b.WriteString("    Model/     Type launch overrides; leave blank to use the profile\n")
	b.WriteString("    Effort     chain or harness default.\n")
	if m.capabilities.attachAgent {
		b.WriteString("    enter      Spawn, then go straight to the new agent's pane —\n")
		b.WriteString("               the same move enter makes on its row. Operator\n")
		b.WriteString("               consoles only; an agent still starting up (no pane\n")
		b.WriteString("               yet) just lands in the list.\n")
	} else {
		b.WriteString("    enter      Spawn; the new agent lands in the remote listing.\n")
	}
	b.WriteString("    esc        Cancel\n\n")
	if m.capabilities.startLocalShell {
		b.WriteString("  New shell session\n")
		b.WriteString("    A session, not an agent: a plain interactive shell in its own tmux\n")
		b.WriteString("    session, with no conversation, no group and no permissions — so it\n")
		b.WriteString("    joins the list above as a \"" + tuiSessionGroupCell + "\" row rather than beside the\n")
		b.WriteString("    agents. Reach it from outside this console with\n")
		b.WriteString("    `tclaude session ls` and `tclaude session attach <handle>`.\n")
		b.WriteString("    tab/↑/↓    Next / previous field\n")
		if m.capabilities.completeLocalDir {
			b.WriteString("    tab        On a Directory you have typed into, complete the path\n")
			b.WriteString("               instead, exactly as in the new-agent form. Directory\n")
			b.WriteString("               starts on the directory this console was started in.\n")
		}
		b.WriteString("    Label names the tmux handle — letters, digits, '-' and '_', since\n")
		b.WriteString("               it is used verbatim; blank generates one.\n")
		b.WriteString("    enter      Start it, then go to its pane — the same handover enter\n")
		b.WriteString("               makes on an agent's row\n")
		b.WriteString("    esc        Cancel\n\n")
	}
	if m.remoteConsole() {
		b.WriteString("  Connected to " + m.connectionLabel + ". The console keeps polling\n")
		b.WriteString("  through outages and reconnects when agentd returns at this address.\n\n")
	} else if addr := m.dashboardAddressLine(); addr != "" {
		b.WriteString("  The web dashboard is running alongside this console:\n")
		b.WriteString("    " + addr + "\n")
		if m.dashboardLink != "" {
			b.WriteString("  That link signs you in as you open it, so there is no token to\n")
			b.WriteString("  paste. It is good for one use and about a minute, and the console\n")
			b.WriteString("  replaces it as it ages — so open the one on screen, and if the\n")
			b.WriteString("  browser says the link was already used, come back for the next.\n")
		}
		b.WriteString("  Both drive the same daemon, so human-approval requests still\n")
		b.WriteString("  appear in its Messages tab.\n\n")
	} else {
		b.WriteString("  This mode runs without the web dashboard: browser deep links and\n")
		b.WriteString("  in-dashboard human-approval requests are unavailable, so grant an\n")
		b.WriteString("  agent access with `tclaude agent permissions grant` instead.\n\n")
	}
	if len(m.tokenLines) > 0 {
		writeTokenBlock(&b, m.tokenLines, "  ")
		b.WriteString("\n")
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// helpLineRows is how many terminal rows a body line takes once the terminal
// has wrapped it. The help text is hand-wrapped to a comfortable width rather
// than the terminal's, so a narrow one wraps it again — and the viewport has
// to pay for every row a line took, or the footer is pushed off the bottom.
func (m tuiModel) helpLineRows(line string) int {
	w := lipgloss.Width(line)
	if m.width <= 0 || w <= m.width {
		return 1
	}
	return (w + m.width - 1) / m.width
}

// helpBodyRows is how many rows the help body has to fit in.
func (m tuiModel) helpBodyRows() int {
	return max(m.height-tuiHelpChrome, 1)
}

// helpWindow is the run of body lines visible at offset off: as many as the
// row budget holds. A line the budget cannot hold even on its own still shows,
// because leaving the viewport empty would strand every line after it — but
// cut to the rows there are, since overflowing the alt screen is the thing
// this viewport exists to stop.
func (m tuiModel) helpWindow(lines []string, off int) []string {
	off = min(max(off, 0), max(len(lines)-1, 0))
	budget := m.helpBodyRows()
	rows := 0
	out := make([]string, 0, len(lines)-off)
	for _, line := range lines[off:] {
		r := m.helpLineRows(line)
		if rows+r > budget {
			if len(out) > 0 {
				break
			}
			if m.width > 0 {
				line = tuiTruncate(line, budget*m.width)
			}
			return []string{line}
		}
		rows += r
		out = append(out, line)
	}
	return out
}

// helpMaxOffset is the furthest the help view scrolls: the offset that puts
// the last body line on the last body row. Zero means the whole text fits, and
// so does a console that has had no WindowSizeMsg yet — with no height to
// window to, renderHelpView shows everything rather than one line of it.
//
// It never goes past the last line, even when that line alone is taller than
// the budget: an offset with no line to name is one the range readout would
// count past the end of the text, and one that end/G would park a dead
// keystroke on.
func (m tuiModel) helpMaxOffset(lines []string) int {
	if m.height <= 0 || len(lines) == 0 {
		return 0
	}
	budget := m.helpBodyRows()
	rows := 0
	for i := len(lines) - 1; i >= 0; i-- {
		rows += m.helpLineRows(lines[i])
		if rows > budget {
			return min(i+1, len(lines)-1)
		}
	}
	return 0
}

// renderHelpView is the help text windowed to this terminal, which is what the
// console shows. Below a body that fits it says so and nothing more; below one
// that does not it names the scroll keys and where in the text the view is.
func (m tuiModel) renderHelpView() string {
	lines := m.helpBodyLines()
	maxOffset := m.helpMaxOffset(lines)
	if maxOffset <= 0 {
		return m.renderHelp()
	}
	off := min(max(m.helpOffset, 0), maxOffset)
	window := m.helpWindow(lines, off)
	body := strings.Join(window, "\n") + "\n"
	if m.height <= tuiHelpChrome {
		// No room for both a line of help and the footer under it. The help is
		// what the view is for, so the footer is what goes — and the keys it
		// would have named still work.
		return body
	}
	hint := tuiHelpScrollHint + fmt.Sprintf("  (%d–%d of %d)", off+1, off+len(window), len(lines))
	if m.width > 0 {
		hint = tuiTruncate(hint, m.width)
	}
	return body + "\n" + hint + "\n"
}
