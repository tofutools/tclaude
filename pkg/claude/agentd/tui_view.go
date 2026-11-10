package agentd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
)

func (m tuiModel) columns() []table.Column {
	return []table.Column{
		{Header: "AGENT", MinWidth: 12, MaxWidth: 28, Weight: 0.2, Truncate: true},
		{Header: "GROUP", MinWidth: 8, MaxWidth: 20, Weight: 0.15, Truncate: true},
		{Header: "STATUS", MinWidth: 8, MaxWidth: 18, Weight: 0.15, Truncate: true},
		{Header: "HARNESS", Width: 8},
		{Header: "DIRECTORY", MinWidth: 15, Weight: 0.35, Truncate: true, TruncateMode: table.TruncateStart},
		{Header: "BRANCH", MinWidth: 10, MaxWidth: 24, Weight: 0.15, Truncate: true},
	}
}

// tuiOperatorTokenLines renders the operator-token block, mirroring the TTY
// branch of writeOperatorTokenBanner (same phrasing, same provenance note).
//
// Emitting the secret here keeps that function's rule intact rather than
// bending it: the token is shown only on a terminal the operator is sitting
// at, which is the one thing a bubbletea program is guaranteed to have.
func tuiOperatorTokenLines(tok string, src tokenSource) []string {
	if tok == "" {
		return nil
	}
	lines := []string{
		"Operator token — sign in to the web dashboard with it, or export it for the CLI:",
		"  export " + humanTokenEnvVar + "=" + strconv.Quote(tok),
	}
	switch src.kind {
	case tokenSourceKeychain:
		lines = append(lines, "  (persisted in the OS keychain — stable across restarts, export once)")
	case tokenSourceFile:
		lines = append(lines, "  (persisted at "+src.path+" — stable across restarts, export once)")
	}
	return lines
}

// writeTokenBlock writes the token lines at the given indent.
func writeTokenBlock(b *strings.Builder, lines []string, indent string) {
	for _, line := range lines {
		b.WriteString(indent + line + "\n")
	}
}

func (m tuiModel) renderList() string {
	var b strings.Builder
	b.WriteString("\n  tclaude terminal dashboard")
	if m.remoteConsole() {
		b.WriteString(" • connected to " + m.connectionLabel)
	} else if addr := m.dashboardAddressLine(); addr != "" {
		// Its own line: with the init token on it the address is too long to
		// share the title line without wrapping, and a wrapped header throws
		// off the row budget viewportHeight computes.
		b.WriteString(" • web dashboard:\n    " + addr)
	} else {
		b.WriteString(" (no web dashboard in this mode)")
	}
	b.WriteString("\n\n")
	if m.showTokenBanner {
		writeTokenBlock(&b, m.tokenLines, "  ")
		b.WriteString("  (hidden on your next keystroke — press ? to see it again)\n\n")
	}
	if m.identityWarning != "" {
		b.WriteString(m.renderWrapped(m.identityWarning) + "\n\n")
	}

	visible := m.visibleRows()
	if len(visible) == 0 {
		if m.filterActive && len(m.agents) > 0 {
			b.WriteString("  No active agents.\n")
		} else {
			b.WriteString("  " + m.emptyListingLine() + "\n")
		}
	} else {
		tbl := table.New(m.columns()...)
		tbl.SetTerminalWidth(max(m.width-4, 60))
		tbl.SelectedIndex = m.cursor
		tbl.ViewportOffset = m.viewportOffset
		tbl.ViewportHeight = m.viewportHeight()
		for _, row := range visible {
			tbl.AddRow(table.Row{Cells: []string{
				row.name(),
				row.groupCell(),
				row.statusCell(),
				row.harnessCell(),
				row.dirCell(),
				row.branchCell(),
			}})
		}
		for line := range strings.SplitSeq(tbl.Render(), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\n  " + m.summaryLine() + "\n")
	if m.refreshErr != "" {
		b.WriteString(m.renderWrapped(m.refreshErr) + "\n")
	}
	if m.notice != "" {
		b.WriteString(m.renderWrapped(m.notice) + "\n")
	}
	b.WriteString("\n")
	if usage := m.usageLine(); usage != "" {
		b.WriteString("  " + usage + "\n")
	}
	b.WriteString("  " + m.keyHintLine() + "\n")

	if prompt := m.confirmPrompt(); prompt != "" {
		b.WriteString("\n  " + prompt + "\n")
	}
	return b.String()
}

// emptyListingLine is what an empty listing says, which depends on what this
// console was looking at: a console that lists the host's sessions has checked
// for those too, and saying only "no agents" would leave the operator
// wondering whether the shell they started is somewhere off screen.
func (m tuiModel) emptyListingLine() string {
	if m.listsLocalSessions() {
		return "No agents or sessions yet."
	}
	return "No agents yet."
}

// confirmPrompt is the question the list view is waiting on, empty when it is
// waiting on none. Every prompt is one line by contract — viewportHeight pays
// for exactly two (the blank line and the question), so a second line would
// overflow the terminal.
func (m tuiModel) confirmPrompt() string {
	switch m.mode {
	case tuiModeConfirmQuit:
		if !m.capabilities.shutdownOnQuit {
			return "Quit this remote console? [y / any other key = cancel]"
		}
		if m.startingShell {
			// A shell launch runs in THIS process, unlike an agent spawn — which
			// forks its own `tclaude session new`. Quitting mid-launch kills it
			// between writing its session row and arming the pane's exit guard, so
			// the rollback never runs and a launch-pending row is left behind.
			// Cheap to clean up, worth a word before it happens. Kept inside 80
			// columns: confirmPrompt is budgeted as exactly one line.
			return "Shell still starting — quit and shut down agentd? [y / any other key = cancel]"
		}
		if m.ownsTmuxServer {
			// This daemon started the tmux server, so quitting ends it too — but
			// only if nothing is left on it, so agent panes still outlive the quit
			// (see startTUITmuxServer). Saying so is the difference between an
			// operator who knows the empty server goes away with the console and
			// one who finds a stray tmux process later and wonders whose it is.
			return "Quit + shut down agentd (and tmux if empty)? [y / any other key = cancel]"
		}
		return "Quit and shut down agentd? [y / any other key = cancel]"
	case tuiModeConfirmStop:
		const prefix = "Take "
		const suffix = " offline? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	case tuiModeConfirmRetire:
		const prefix = "Retire "
		const suffix = "? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	case tuiModeConfirmKillSession:
		// "Kill" rather than the agents' softer wording on purpose: there is no
		// graceful exit to ask a shell for and nothing to resume it from
		// afterwards, so whatever is running in that pane stops now. The
		// prompt is budgeted as one line like the others, so the consequence
		// is spelled out in the help rather than here.
		const prefix = "Kill session "
		const suffix = "? [y / any other key = cancel]"
		return prefix +
			tuiTruncate(m.lifecycleTarget.name(), m.promptNameBudget(len(prefix)+len(suffix))) +
			suffix
	default:
		return ""
	}
}

// promptNameBudget is how many columns a lifecycle prompt may spend on the
// agent's name: what the terminal leaves after the indent and the fixed text,
// capped at the AGENT column's own width so a name that fits the listing reads
// identically here.
//
// The name is a conversation title — harness- or operator-authored, routinely
// long — and the prompt is budgeted as exactly one line, so an uncapped title
// is the one thing that could push the list view past the terminal's last row.
// On a terminal too narrow for even the fixed text the floor wins and the line
// wraps; that is the quit prompt's existing behaviour, not a new failure.
func (m tuiModel) promptNameBudget(fixed int) int {
	if m.width <= 0 {
		// No WindowSizeMsg yet: fall back to the listing's own cap.
		return tuiPromptNameWidth
	}
	const indent = 2
	return max(min(m.width-indent-fixed, tuiPromptNameWidth), tuiPromptNameFloor)
}

const (
	// tuiPromptNameWidth matches the AGENT column's MaxWidth.
	tuiPromptNameWidth = 28
	// tuiPromptNameFloor is the shortest name worth showing: below this the
	// prompt would name an agent the operator cannot recognise.
	tuiPromptNameFloor = 8
)

// tuiTruncate shortens s to at most width columns, marking a cut with "…".
func tuiTruncate(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	for len(runes) > 0 {
		candidate := string(runes) + "…"
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
		runes = runes[:len(runes)-1]
	}
	return ""
}

// enterHint names what enter does on the row the cursor is on, empty when
// it would do nothing: an empty list has no row, and only an operator console
// can attach to a live agent's pane. Starting an offline one is advertised to
// every console — like retire, it goes through the daemon's own verb, and the
// daemon decides whether this caller may.
func (m tuiModel) enterHint() string {
	if m.reconcilingMutation {
		return ""
	}
	row, ok := m.selectedRow()
	if !ok {
		return ""
	}
	if row.isSession() {
		// A session row is only ever listed by a console that can go to it,
		// and it is always live, so enter always means the handover.
		if insideTmux() {
			return "enter switch to"
		}
		return "enter attach"
	}
	switch {
	case !row.agent.Online:
		return "enter start"
	case !m.operator || !m.capabilities.attachAgent:
		return ""
	case m.capabilities.attachLocalPane && insideTmux():
		return "enter switch to"
	case !m.capabilities.attachLocalPane:
		return "enter remote attach"
	default:
		return "enter attach"
	}
}

// deleteHint names what delete does on the row the cursor is on, empty when
// there is no row. The two kinds have different lifecycles behind that key —
// an agent steps offline then retired, a session just ends — so the hint is
// per-row rather than a fixed label for a non-empty list.
func (m tuiModel) deleteHint() string {
	row, ok := m.selectedRow()
	switch {
	case !ok:
		return ""
	case row.isSession():
		return "del kill session"
	default:
		// Unlike attaching, agent lifecycle moves are not statically
		// operator-only: an agent-class console can hold stop/retire
		// permissions over its own group's members, so the daemon decides and
		// the key stays advertised.
		return "del offline / retire"
	}
}

// keyHintLine names enter only when it can do something — see enterHint.
func (m tuiModel) keyHintLine() string {
	filterLabel := "f filter active"
	if m.filterActive {
		filterLabel = "f show all"
	}
	quitHint := "q quit"
	if m.capabilities.shutdownOnQuit {
		quitHint += " (shuts down agentd)"
	}
	if m.reconcilingMutation {
		return "reconciling unknown outcome • r refresh • " + filterLabel + " • ↑/↓ move • ? help • " + quitHint
	}
	newHints := "n new agent"
	if m.operator && m.capabilities.startLocalShell {
		newHints += " • s new shell"
	}
	hints := newHints + " • " + filterLabel + " • r refresh • ↑/↓ move • ? help • " + quitHint
	if del := m.deleteHint(); del != "" {
		hints = del + " • " + hints
	}
	if enter := m.enterHint(); enter != "" {
		hints = enter + " • " + hints
	}
	return hints
}

func (m tuiModel) summaryLine() string {
	online := 0
	for _, a := range m.agents {
		if a.Online {
			online++
		}
	}
	agentsShown := len(m.visibleAgents())
	shown := len(m.visibleRows())
	var line string
	if m.filterActive {
		line = fmt.Sprintf("%d active agents • %d groups", agentsShown, len(m.groups))
	} else {
		line = fmt.Sprintf("%d agents (%d online) • %d groups", agentsShown, online, len(m.groups))
	}
	// Sessions are counted separately from the agents they sit under: they are
	// a different kind of thing, and folding them into "N agents" would make
	// the roster read as bigger than it is.
	if m.listsLocalSessions() {
		line += fmt.Sprintf(" • %d sessions", len(m.sessions))
		if m.sessionsErr != "" {
			line += " (stale)"
		}
	}
	if !m.lastRefresh.IsZero() {
		line += " • updated " + tuiAgo(time.Since(m.lastRefresh))
	}
	if height := m.viewportHeight(); shown > height {
		first := m.viewportOffset + 1
		last := min(m.viewportOffset+height, shown)
		line += fmt.Sprintf(" • showing %d-%d", first, last)
	}
	if m.spawning {
		line += " • spawning…"
	}
	if m.resuming {
		line += " • starting…"
	}
	if m.stopping {
		line += " • taking offline…"
	}
	if m.startingShell {
		line += " • starting a shell…"
	}
	if m.killingSession {
		line += " • ending a session…"
	}
	return line
}

// tuiAgo renders how long ago the last successful refresh landed. It stays
// coarse on purpose: the point is telling a live listing apart from one
// frozen by a failing poll, not counting seconds.
func tuiAgo(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
