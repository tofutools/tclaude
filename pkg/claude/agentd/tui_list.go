package agentd

import (
	"time"

	"charm.land/lipgloss/v2"
)

// tuiDashboardLinkRotate is how long a minted sign-in link stays on screen
// before the console replaces it. Well under initTokenTTL, so the link being
// looked at always has time left to be opened; well over tuiRefreshInterval,
// so the line is not rewritten under a mouse dragging a selection across it.
const tuiDashboardLinkRotate = 40 * time.Second

// canMintDashboardLink reports whether this console may put a dashboard
// sign-in link on its screen.
//
// The link is a capability: redeeming it at the dashboard root buys a session
// cookie, and with it the whole authenticated /api surface that
// handleDashboardOpen guards with peer-cred requireHuman precisely so agents
// cannot reach it. So it goes on screen only when the screen is the human's:
//
//   - m.operator — the daemon classifies this console as the human. An agentd
//     started from inside a harness pane is classified as that agent instead
//     (see identityWarning), and that agent can read the pane it is running in:
//     minting there would hand it, via tmux capture-pane, the very authority
//     the permission system exists to withhold. Such a console still gets the
//     dashboard's address, just no token on it.
//   - secret output allowed by the token-output policy: auto requires a
//     terminal, always opts in explicitly, and never excludes credentials.
//
// A remote console is excluded for a different reason: mintInitToken writes
// this process's in-memory store, which is the dashboard's store exactly when
// the daemon is this process. A token from here would be worthless there.
func (m tuiModel) canMintDashboardLink() bool {
	return m.dashboardURL != "" && !m.remoteConsole() && m.operator && !m.suppressSecrets
}

// refreshDashboardLink keeps the console's ready-to-open dashboard link
// openable, minting a replacement once the current one has been up for
// tuiDashboardLinkRotate.
//
// Init tokens are single-use and expire in a minute (see inittoken.go), so one
// minted at startup and left there would be dead long before the operator got
// round to it. Rotating on age rather than on every tick is what keeps the
// line stable enough to select and copy, and leaves at most a couple of
// unredeemed tokens in the store at a time; the cost is that the link visibly
// on screen may already have been spent by an earlier open, which the
// dashboard answers with its "expired or was already used" sign-in page rather
// than a dead end.
func (m tuiModel) refreshDashboardLink(now time.Time) tuiModel {
	if !m.canMintDashboardLink() {
		m.dashboardLink, m.dashboardLinkMinted = "", time.Time{}
		return m
	}
	if m.dashboardLink != "" && now.Sub(m.dashboardLinkMinted) < tuiDashboardLinkRotate {
		return m
	}
	m.dashboardLink = m.dashboardURL + "/?init_token=" + mintInitToken(initScopeDashboard)
	m.dashboardLinkMinted = now
	return m
}

// dashboardAddressLine is the address the console prints for the co-running
// web dashboard: the signed-in link when it has one, the bare URL otherwise.
// Empty means there is nothing to print — no dashboard, or a remote console,
// which names its connection instead.
func (m tuiModel) dashboardAddressLine() string {
	if m.dashboardURL == "" || m.remoteConsole() {
		return ""
	}
	if m.dashboardLink != "" {
		return m.dashboardLink
	}
	return m.dashboardURL
}

// dashboardAddressIndent is the address line's leading indent, counted into
// its width because the terminal wraps on the whole line.
const dashboardAddressIndent = 4

// dashboardAddressRows is how many terminal rows the address line occupies. A
// URL only works whole, so a narrow terminal wraps it rather than the console
// truncating it — and then the row budget has to pay for every row it took,
// or the list overflows the alt screen (see viewportHeight).
func (m tuiModel) dashboardAddressRows() int {
	addr := m.dashboardAddressLine()
	if addr == "" {
		return 0
	}
	w := lipgloss.Width(addr) + dashboardAddressIndent
	if m.width <= 0 || w <= m.width {
		return 1
	}
	return (w + m.width - 1) / m.width
}

func (m tuiModel) visibleAgents() []tuiAgentRow {
	if !m.filterActive {
		return m.agents
	}
	var out []tuiAgentRow
	for _, a := range m.agents {
		if a.Online {
			out = append(out, a)
		}
	}
	return out
}

// listsLocalSessions reports whether this console shows the daemon host's
// non-agent sessions. Both gates matter, and for the same reasons the shell
// form has them: the listing is read off the host's own session store and tmux
// server (so a remote console cannot), and it is the operator's own working
// directories (so an agent-class console may not — see completingDir).
func (m tuiModel) listsLocalSessions() bool {
	return m.operator && m.capabilities.localSessions
}

// visibleRows is the listing the cursor moves over: the agents the filter
// leaves, then the live non-agent sessions.
//
// Sessions come last as a block rather than interleaved by name. They are a
// different kind of thing with a different set of keys, and keeping them
// together means the operator's mental "the agents are the top N rows" holds
// between refreshes — where a name-interleaved order would slide a session
// into the middle of the roster and back out again as agents come and go.
//
// The active filter does not apply to them: only live sessions are listed at
// all, so every session row is already an active one.
func (m tuiModel) visibleRows() []tuiListRow {
	agents := m.visibleAgents()
	rows := make([]tuiListRow, 0, len(agents)+len(m.sessions))
	for _, a := range agents {
		rows = append(rows, agentListRow(a))
	}
	for _, s := range m.sessions {
		rows = append(rows, sessionListRow(s))
	}
	return rows
}

// tuiListChrome is what renderList spends on everything that is not a table
// row: 3 lines of header, the table's own header + separator, a blank line
// and the summary, and a blank line and the key line.
const tuiListChrome = 9

// viewportHeight is how many agent rows fit right now. It is derived rather
// than stored because two of renderList's lines are conditional: a refresh
// error and the two-line quit prompt. Ignoring them overflowed the terminal
// exactly when one was showing, which scrolls the screen out from under
// bubbletea's diff renderer and leaves later partial repaints on the wrong
// lines.
func (m tuiModel) viewportHeight() int {
	chrome := tuiListChrome
	// The dashboard address takes header lines of its own when there is one.
	chrome += m.dashboardAddressRows()
	if m.showTokenBanner {
		chrome += len(m.tokenLines) + 2
	}
	if m.identityWarning != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.identityWarning)) + 1
	}
	if m.refreshErr != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.refreshErr))
	}
	if m.notice != "" {
		chrome += lipgloss.Height(m.renderWrapped(m.notice))
	}
	// The usage line is one row by contract — fitUsageLine trims it to the
	// terminal rather than wrapping — and is absent entirely until the console
	// has a readout to show.
	if m.usageLine() != "" {
		chrome++
	}
	if m.confirmPrompt() != "" {
		chrome += 2
	}
	return max(m.height-chrome, 1)
}

func (m tuiModel) renderWrapped(s string) string {
	if s == "" {
		return ""
	}
	indent := 2
	w := m.width - indent
	if w <= 0 {
		indent = 0
		w = max(m.width, 1)
	}
	return lipgloss.NewStyle().Width(w).PaddingLeft(indent).Render(s)
}

// selectedRow is the row the cursor is on, and whether there is one — the
// listing can be empty, and it re-sorts under the cursor every two seconds.
func (m tuiModel) selectedRow() (tuiListRow, bool) {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return tuiListRow{}, false
	}
	return rows[m.cursor], true
}

// selectedAgent is selectedRow narrowed to an agent: false on an empty
// listing and false on a non-agent session row, so every agent-only move
// (resume, stop, retire, attach-to-an-agent) is inert on a session rather
// than acting on one under an agent's verbs.
func (m tuiModel) selectedAgent() (tuiAgentRow, bool) {
	row, ok := m.selectedRow()
	if !ok || row.isSession() {
		return tuiAgentRow{}, false
	}
	return row.agent, true
}

// restoreCursor puts the cursor back on the row it was on, by row key, after
// a refresh has replaced the listing under it. A row that is no longer listed
// (an agent retired, a session ended, or either filtered out) leaves the
// cursor where it was, for the clamp above to bring back into range.
//
// The listing re-sorts on every poll — online agents first, then by name, then
// the sessions — so an index alone does not name a row for longer than two
// seconds. That is the same hazard the lifecycle prompts capture their target
// for, and it bites hardest right after enter starts an offline agent: that
// agent jumps from the bottom of the agents to the top, and a cursor left at
// the old index would put the next keystroke on whichever row slid into its
// place.
func (m *tuiModel) restoreCursor(key string) {
	if key == "" {
		return
	}
	for i, row := range m.visibleRows() {
		if row.key() == key {
			m.cursor = i
			return
		}
	}
}

func (m *tuiModel) ensureCursorVisible() {
	height := m.viewportHeight()
	if m.cursor < m.viewportOffset {
		m.viewportOffset = m.cursor
	}
	if m.cursor >= m.viewportOffset+height {
		m.viewportOffset = m.cursor - height + 1
	}
	if m.viewportOffset < 0 {
		m.viewportOffset = 0
	}
}
