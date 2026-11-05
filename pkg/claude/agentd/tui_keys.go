package agentd

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m tuiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.reconcilingMutation && tuiReconciliationBlocksMutation(m.mode, msg.String()) {
		// A second modal can have opened while the original request was still
		// in flight. Once that request becomes ambiguous, cancel the stale
		// modal as well as blocking list-mode mutation shortcuts.
		m.mode = tuiModeList
		m.lifecycleTarget = tuiListRow{}
		m.notice = "Waiting for a successful refresh to reconcile the previous action before another mutation."
		return m, nil
	}

	switch m.mode {
	case tuiModeHelp:
		return m.handleHelpKey(msg)

	case tuiModeConfirmQuit:
		// Only "y" or a second Ctrl-C confirms. Enter is the reflexive dismiss key and the
		// prompt promises that anything other than "y" cancels — accepting
		// it here would turn a misread prompt into a daemon shutdown, taking
		// every managed agent's pane with it.
		switch msg.String() {
		case "y", "Y", "ctrl+c":
			return m, tea.Quit
		default:
			m.mode = tuiModeList
		}
		return m, nil

	case tuiModeConfirmStop:
		switch msg.String() {
		case "y", "Y":
			return m.stopConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeConfirmKillSession:
		// Same contract as the retire prompt: only "y" goes through, and
		// ctrl-c means "get me out of this" rather than a second yes.
		switch msg.String() {
		case "y", "Y":
			return m.killSessionConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeConfirmRetire:
		// Same contract as the quit prompt: only "y" goes through, and the
		// prompt says so. Ctrl-C is a cancel here rather than a second
		// confirmation — it means "get me out of this", and the destructive
		// reading of it belongs to the prompt that offers it.
		switch msg.String() {
		case "y", "Y":
			return m.retireConfirmed()
		default:
			m.mode = tuiModeList
			m.lifecycleTarget = tuiListRow{}
		}
		return m, nil

	case tuiModeSpawn:
		return m.handleSpawnKey(msg)

	case tuiModeShell:
		return m.handleShellKey(msg)
	}

	// List mode.
	visible := m.visibleRows()
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		// The embedded console owns the daemon lifetime; a standalone remote
		// console owns only itself. Both ask so q remains hard to hit by
		// accident, while the prompt says exactly what will stop.
		m.mode = tuiModeConfirmQuit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.ensureCursorVisible()
		}
	case "down", "j":
		if m.cursor < len(visible)-1 {
			m.cursor++
			m.ensureCursorVisible()
		}
	case "f":
		var selectedKey string
		if m.cursor >= 0 && m.cursor < len(visible) {
			selectedKey = visible[m.cursor].key()
		}
		m.filterActive = !m.filterActive
		m.cursor = 0
		// Same key match — and the same skip of the empty one — restoreCursor
		// makes after a refresh: an identity-less placeholder row must not
		// hand the cursor to a different placeholder.
		m.restoreCursor(selectedKey)
		m.ensureCursorVisible()
		return m, nil
	case "enter":
		m.notice = ""
		return m.enterSelected()
	case "n", "N":
		m.notice = ""
		return m.openSpawnForm(), nil
	case "s", "S":
		return m.startShellSelected(), nil
	case "delete":
		m.notice = ""
		return m.confirmDeleteStepSelected(), nil
	case "r":
		if !m.refreshing {
			m.notice = ""
			return m.beginRefresh()
		}
	case "?", "h":
		m.mode = tuiModeHelp
		// Always opens at the top: the view is opened to read it, and where a
		// previous reading was left is no use two minutes later.
		m.helpOffset = 0
	}
	return m, nil
}

func tuiReconciliationBlocksMutation(mode tuiMode, key string) bool {
	switch mode {
	case tuiModeList:
		return key == "enter" || key == "delete" || strings.EqualFold(key, "n")
	case tuiModeSpawn:
		return key == "enter"
	case tuiModeConfirmStop:
		return strings.EqualFold(key, "y")
	case tuiModeConfirmRetire:
		return strings.EqualFold(key, "y")
	case tuiModeConfirmKillSession:
		return strings.EqualFold(key, "y")
	default:
		return false
	}
}

// handleHelpKey scrolls the help view, or closes it. Anything that is not a
// scroll key closes it, which is the contract the view has always had and the
// footer still states: the help is a glance rather than a mode to get stuck
// in, so the keys it swallows are only the ones it needs.
//
// The scroll keys are the listing's own — ↑/k, ↓/j, pgup/pgdn, home/g, end/G
// — so nothing new has to be learned to read the rest of a page.
func (m tuiModel) handleHelpKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	lines := m.helpBodyLines()
	maxOffset := m.helpMaxOffset(lines)
	if maxOffset <= 0 {
		// The whole text is on screen, so the footer promises that any key
		// closes it — and a scroll key must not be the exception that leaves an
		// operator pressing ↓ at a view that neither scrolls nor closes.
		m.mode = tuiModeList
		m.helpOffset = 0
		return m, nil
	}
	// A page is what is on screen less one line, so the line the eye stopped on
	// is still there after the jump.
	page := max(len(m.helpWindow(lines, m.helpOffset))-1, 1)
	switch msg.String() {
	case "up", "k":
		m.helpOffset = max(m.helpOffset-1, 0)
	case "down", "j":
		m.helpOffset = min(m.helpOffset+1, maxOffset)
	case "pgup", "ctrl+b":
		m.helpOffset = max(m.helpOffset-page, 0)
	case "pgdown", "ctrl+f":
		m.helpOffset = min(m.helpOffset+page, maxOffset)
	case "home", "g":
		m.helpOffset = 0
	case "end", "G":
		m.helpOffset = maxOffset
	default:
		m.mode = tuiModeList
		m.helpOffset = 0
	}
	return m, nil
}

func (m tuiModel) handleSpawnKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// The candidate list belongs to the Tab that produced it: anything else
	// (including a Tab on another field) retires it, so it never lingers as a
	// stale answer to a path the operator has since edited.
	if key != "tab" || !m.completingDir() {
		m.form.dirSuggestions = nil
	}
	switch key {
	case "esc", "ctrl+c":
		// Closing the form while its worktree is being cut does not cancel the
		// spawn behind it — no mutation this console starts is cancellable —
		// it just gives up the typed fields. The outcome then lands on the
		// listing like every other one, and a second spawn is still refused
		// while the first is in flight.
		m.mode = tuiModeList
		return m, nil
	case "enter":
		return m.submitSpawn()
	case "up", "shift+tab":
		return m.moveSpawnField(-1), nil
	case "down":
		return m.moveSpawnField(1), nil
	case "tab":
		// Tab only completes a directory the operator has typed into; on the
		// field as the form left it — blank, or the group's own directory —
		// it keeps its ordinary next-field job (see completingDir).
		if m.completingDir() {
			return m.completeDir(), nil
		}
		return m.moveSpawnField(1), nil
	case "left":
		if tuiIsChoiceField(m.form.field) {
			return m.cycleChoice(-1), nil
		}
	case "right", " ":
		if tuiIsChoiceField(m.form.field) {
			return m.cycleChoice(1), nil
		}
	}
	updated, cmd := m.updateFocusedInput(msg)
	return updated, cmd
}

func (m tuiModel) handleShellKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// The candidate list belongs to the Tab that produced it, exactly as in the
	// spawn form: anything else retires it, so it never lingers as a stale
	// answer to a path the operator has since edited.
	if key != "tab" || !m.completingShellDir() {
		m.shell.dirSuggestions = nil
	}
	switch key {
	case "esc", "ctrl+c":
		m.mode = tuiModeList
		return m, nil
	case "enter":
		return m.submitShell()
	case "up", "shift+tab":
		return m.moveShellField(-1), nil
	case "down":
		return m.moveShellField(1), nil
	case "tab":
		if m.completingShellDir() {
			return m.completeShellDir(), nil
		}
		return m.moveShellField(1), nil
	}
	return m.updateFocusedShellInput(msg)
}
