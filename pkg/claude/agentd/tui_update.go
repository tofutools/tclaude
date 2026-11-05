package agentd

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// onWindowSize takes the terminal's new size.
func (m tuiModel) onWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.ensureCursorVisible()
	// A terminal that grew leaves the help view scrolled further than there
	// is now text below, which would show a screen of blank rows. Only the
	// open view pays for the recount; every other mode opens at the top
	// anyway.
	if m.mode == tuiModeHelp {
		m.helpOffset = min(m.helpOffset, m.helpMaxOffset(m.helpBodyLines()))
	}
	return m, nil
}

// onTick renews the dashboard link, polls usage when due and starts a refresh.
func (m tuiModel) onTick(msg tuiTickMsg) (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{tuiTickCmd()}
	// Keep the header's dashboard link openable: a one-minute token has to
	// be replaced before it expires under the operator.
	m = m.refreshDashboardLink(time.Time(msg))
	// The usage readout rides this tick but on its own much slower cadence
	// — see tuiUsageInterval — and independently of the listing poll, so a
	// slow refresh cannot starve it.
	if m.usageDue() {
		m.usageFetching = true
		m.lastUsageAttempt = time.Now()
		cmds = append(cmds, m.usageCmd())
	}
	if !m.refreshing {
		var refresh tea.Cmd
		m, refresh = m.beginRefresh()
		cmds = append(cmds, refresh)
	}
	return m, tea.Batch(cmds...)
}

// onUsage takes a usage poll's result.
func (m tuiModel) onUsage(msg tuiUsageMsg) (tea.Model, tea.Cmd) {
	m.usageFetching = false
	if msg.err != nil {
		// A daemon without the endpoint is not a broken readout, it is a
		// readout that does not exist here: say nothing, and drop whatever
		// an earlier daemon told us — those figures have no source now.
		if tuiEndpointUnsupported(msg.err) {
			m.usageUnsupported = true
			m.usageLoaded = false
			m.usageFailed = false
			return m, nil
		}
		// Keep the last good figures if there are any: they are cached
		// readings to begin with, and one failed poll of an optional
		// readout must not blank a line the operator is reading.
		m.usageFailed = !m.usageLoaded
		return m, nil
	}
	m.usage = msg.usage
	m.usageLoaded = true
	m.usageFailed = false
	m.usageUnsupported = false
	return m, nil
}

// onData takes a refresh's listing, dropping a stale one.
func (m tuiModel) onData(msg tuiDataMsg) (tea.Model, tea.Cmd) {
	// Tests and other direct model callers may leave the generation at
	// zero; real refresh commands always carry one. A late result is
	// otherwise wholly stale, including its error and reconciliation
	// implications.
	if msg.refreshGeneration != 0 && msg.refreshGeneration < m.refreshGeneration {
		return m, nil
	}
	m.refreshing = false
	if msg.err != nil {
		m.refreshErr = "Refresh failed: " + msg.err.Error()
		return m, nil
	}
	m.refreshErr = ""
	if m.reconcilingMutation &&
		(msg.refreshGeneration == 0 || msg.refreshGeneration >= m.reconciliationRefreshGen) {
		m.reconcilingMutation = false
		m.reconciliationRefreshGen = 0
	}
	// Which row the cursor is on is decided before the listing under it
	// is replaced — see restoreCursor.
	selected, hadSelection := m.selectedRow()
	m.agents = msg.agents
	m.groups = msg.groups
	// A failed profile read keeps the last list it managed to fetch: a
	// stale picker is worth more than an empty one, and the form says so.
	if msg.profilesErr != nil {
		m.profilesErr = msg.profilesErr.Error()
	} else {
		m.profilesErr = ""
		m.profiles = msg.profiles
	}
	// Same disposition for the host's own sessions: a failed read keeps
	// the rows it last managed to list rather than dropping them out from
	// under the cursor, and the summary line says the listing is stale.
	if msg.sessionsErr != nil {
		m.sessionsErr = msg.sessionsErr.Error()
	} else {
		m.sessionsErr = ""
		m.sessions = msg.sessions
	}
	m.lastRefresh = time.Now()
	if hadSelection {
		m.restoreCursor(selected.key())
	}
	visible := m.visibleRows()
	if m.cursor >= len(visible) {
		m.cursor = max(len(visible)-1, 0)
	}
	m.ensureCursorVisible()
	return m, nil
}

// onWorktreeResolved sends the spawn into a resolved worktree, or reports why it failed.
func (m tuiModel) onWorktreeResolved(msg tuiWorktreeResolvedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// The form is still open on the fields that produced this, so the
		// operator fixes the branch or the directory and presses enter
		// again. Nothing has been spawned.
		m.spawning = false
		m.notice = "Worktree failed: " + msg.err.Error()
		if tuiMutationOutcomeUnknown(msg.err) {
			// The request may well have landed: git creates the worktree
			// before the answer is lost, and the retries replay a recorded
			// response rather than re-cutting it. Saying only "failed"
			// would leave a directory nobody has been told about. Pressing
			// enter again converges either way — a worktree that exists on
			// the branch is reused.
			m.notice += " The worktree may have been created; asking for the same branch again reuses it."
		}
		return m, nil
	}
	m.spawnWorktree = msg.wt
	req := msg.req
	req.Cwd = msg.wt.Path
	// The form that asked has done its job, so it closes. A form the
	// operator has opened since is a different one, with different things
	// typed into it, and is theirs to keep — as is anything else they
	// opened instead.
	if m.mode == tuiModeSpawn && m.form.gen == msg.formGen {
		m.mode = tuiModeList
	}
	m.notice = tuiWorktreeReadyNotice(msg.wt) + " Spawning in group " + msg.group + "…"
	return m, tuiSpawnCmd(m.api, msg.group, req)
}

// onSpawned reports a spawn and goes to the new agent's pane.
func (m tuiModel) onSpawned(msg tuiSpawnedMsg) (tea.Model, tea.Cmd) {
	m.spawning = false
	if msg.err != nil {
		m.notice = "Spawn failed: " + msg.err.Error() + m.worktreeKeptNote()
		if tuiMutationOutcomeUnknown(msg.err) {
			m.notice = "Spawn outcome unknown: the connection was lost after the request; " +
				"refreshing before another action." + m.worktreeKeptNote()
			m.reconcilingMutation = true
			var refresh tea.Cmd
			m, refresh = m.beginRefresh()
			m.reconciliationRefreshGen = m.refreshGeneration
			return m, refresh
		}
		return m, nil
	}
	m.notice = tuiSpawnSummary(msg) + m.worktreeLandedNote()
	warningNote := tuiSpawnWarningNote(msg.resp)
	m.notice += warningNote
	if focused, cmd := m.focusSpawned(msg); cmd != nil {
		// Going to the pane ends in a tuiAttachedMsg, which refreshes.
		return focused, tuiAttachCarryingSpawnNotice(cmd, warningNote)
	}
	// Pull the new agent in now rather than waiting out the tick.
	return m.beginRefresh()
}

// onRetired reports a retire.
func (m tuiModel) onRetired(msg tuiRetiredMsg) (tea.Model, tea.Cmd) {
	m.retiring = false
	if msg.err != nil {
		m.notice = "Retire failed: " + msg.err.Error()
		if tuiMutationOutcomeUnknown(msg.err) {
			m.notice = "Retire outcome unknown: the connection was lost after the request; refreshing before another action."
			m.reconcilingMutation = true
			var refresh tea.Cmd
			m, refresh = m.beginRefresh()
			m.reconciliationRefreshGen = m.refreshGeneration
			return m, refresh
		}
		return m, nil
	}
	m.notice = tuiRetireSummary(msg)
	// The row is gone (or offline) now — show that rather than leaving a
	// retired agent listed until the next tick.
	return m.beginRefresh()
}

// onStopped reports a take-offline.
func (m tuiModel) onStopped(msg tuiStoppedMsg) (tea.Model, tea.Cmd) {
	m.stopping = false
	if msg.err != nil {
		m.notice = "Take offline failed: " + msg.err.Error()
		if tuiMutationOutcomeUnknown(msg.err) {
			m.notice = "Take-offline outcome unknown: the connection was lost after the request; refreshing before another action."
			m.reconcilingMutation = true
			var refresh tea.Cmd
			m, refresh = m.beginRefresh()
			m.reconciliationRefreshGen = m.refreshGeneration
			return m, refresh
		}
		return m, nil
	}
	m.notice = tuiStopSummary(msg)
	// Reconcile promptly: a graceful stop acknowledges delivery of the
	// exit request, but the pane may take a moment to disappear.
	return m.beginRefresh()
}

// onResumed reports a start.
func (m tuiModel) onResumed(msg tuiResumedMsg) (tea.Model, tea.Cmd) {
	m.resuming = false
	if msg.err != nil {
		m.notice = "Start failed: " + msg.err.Error()
		if tuiMutationOutcomeUnknown(msg.err) {
			m.notice = "Start outcome unknown: the connection was lost after the request; refreshing before another action."
			m.reconcilingMutation = true
			var refresh tea.Cmd
			m, refresh = m.beginRefresh()
			m.reconciliationRefreshGen = m.refreshGeneration
			return m, refresh
		}
		return m, nil
	}
	m.notice = tuiResumeSummary(msg)
	// The row is online now — show that rather than leaving it reading
	// "offline" until the next tick.
	return m.beginRefresh()
}

// onShellStarted reports a shell session and goes to it.
func (m tuiModel) onShellStarted(msg tuiShellStartedMsg) (tea.Model, tea.Cmd) {
	m.startingShell = false
	if msg.err != nil {
		m.notice = "Could not start a shell session: " + msg.err.Error()
		return m, nil
	}
	// A shell session is not an agent, so it lands in the listing as a
	// (session) row rather than beside the agents — and naming its tmux
	// handle is what also lets the operator find it from outside this
	// console (`tclaude session attach <handle>`).
	m.notice = "Started shell session " + msg.created.TmuxSession + " in " + msg.created.Cwd + "."
	if insideTmux() {
		m.notice += " Switching to it…"
	} else {
		m.notice += " Attaching; detach (ctrl-b d) to come back."
	}
	return m, tuiAttachToPane(msg.created.SessionID, msg.created.TmuxSession, insideTmux())
}

// onSessionKilled reports an ended session.
func (m tuiModel) onSessionKilled(msg tuiSessionKilledMsg) (tea.Model, tea.Cmd) {
	m.killingSession = false
	if msg.err != nil {
		m.notice = "Could not end session " + msg.session + ": " + msg.err.Error()
		return m, nil
	}
	// The pane is gone, so the row is: the listing carries live sessions
	// only. Refresh now rather than leaving a dead session on screen for
	// up to a tick. The session row itself is left for the daemon's reaper
	// to mark exited, exactly as when the shell exits on its own.
	m.notice = "Ended session " + msg.session + "."
	return m.beginRefresh()
}

// onAttached reports the return from a pane.
func (m tuiModel) onAttached(msg tuiAttachedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if msg.remote {
			m.notice = "Could not reach the remote terminal for " + msg.agent + ": " + msg.err.Error()
		} else {
			m.notice = "Could not reach " + msg.session + ": " + msg.err.Error()
		}
		m.notice += msg.spawnNotice
		return m, nil
	}
	if msg.remote {
		m.notice = "Back from the remote terminal for " + msg.agent + "."
	} else if insideTmux() {
		m.notice = "Switched to " + msg.session + " (" + msg.agent + ")."
	} else {
		m.notice = "Back from " + msg.session + " (" + msg.agent + ")."
	}
	m.notice += msg.spawnNotice
	// The pane may have ended while the operator was on it.
	return m.beginRefresh()
}
