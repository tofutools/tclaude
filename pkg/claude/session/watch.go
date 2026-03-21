package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
)

var (
	selectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("238")) // Dark gray background, preserves row foreground color
	idleStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))            // Yellow
	workingStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("46"))             // Green
	needsInput    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))            // Bright red
	exitedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))            // Gray
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	searchStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

type tickMsg time.Time

// Confirmation modes
type confirmMode int

const (
	confirmNone confirmMode = iota
	confirmKill
	confirmAttachForce // Session already attached, confirm force attach
	confirmNoTmux      // Session has no tmux, cannot attach
	confirmDetach      // Confirm detaching clients from session
	confirmQuit        // Confirm exit via ESC
)

// Filter options for the checkbox menu
var filterOptions = []struct {
	key    string
	label  string
	status string // internal status constant (empty for "all")
}{
	{"all", "All (no filter)", ""},
	{StatusIdle, "Idle", StatusIdle},
	{StatusWorking, "Working", StatusWorking},
	{StatusAwaitingPermission, "Awaiting permission", StatusAwaitingPermission},
	{StatusAwaitingInput, "Awaiting input", StatusAwaitingInput},
	{StatusExited, "Exited", StatusExited},
}

type model struct {
	allSessions    []*SessionState // all sessions before search filter
	sessions       []*SessionState // sessions after search filter
	cursor         int
	width          int
	height         int
	viewportOffset int
	viewportHeight int
	shouldAttach   string // tmux session name to attach to after quitting
	shouldAttachID string // session ID for inbox watcher
	forceAttach    bool   // detach other clients when attaching
	focusOnly      bool   // just focus, don't attach (session already attached elsewhere)
	createNew      bool   // create a new session after quitting
	includeAll     bool
	sort           table.SortState
	statusFilter   []string        // which statuses to show (empty = all)
	hideFilter     []string        // which statuses to hide
	confirmMode    confirmMode     // current confirmation dialog
	filterMenu     bool            // showing filter menu
	filterCursor   int             // cursor position in filter menu
	filterChecked  map[string]bool // checked items in filter menu
	helpView       bool            // showing help view
	searchInput    string          // current search query
	searchFocused  bool            // whether search box is focused
	lastUpdatedAt  time.Time       // tracks DB changes for polling
}

func initialModel(includeAll bool, statusFilter, hideFilter []string) model {
	return model{
		sessions:     []*SessionState{},
		cursor:       0,
		includeAll:   includeAll,
		sort:         table.SortState{},
		statusFilter: statusFilter,
		hideFilter:   hideFilter,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), tea.EnterAltScreen)
}

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) refreshSessions() model {
	states, err := ListSessionStates()
	if err != nil {
		return m
	}

	var filtered []*SessionState
	for _, state := range states {
		RefreshSessionStatus(state)
		if !m.includeAll && state.Status == StatusExited {
			continue
		}
		// Apply status filter (show)
		if !m.matchesShowFilter(state.Status) {
			continue
		}
		// Apply hide filter
		if m.matchesHideFilter(state.Status) {
			continue
		}
		filtered = append(filtered, state)
	}

	// Apply sorting
	SortSessionsByKey(filtered, m.sort.Key, m.sort.Direction)
	m.allSessions = filtered

	// Apply search filter
	m = m.applySearchFilter()

	return m
}

// applySearchFilter filters sessions based on search input
func (m model) applySearchFilter() model {
	if m.searchInput == "" {
		m.sessions = m.allSessions
	} else {
		query := strings.ToLower(m.searchInput)
		var filtered []*SessionState
		for _, s := range m.allSessions {
			if sessionMatchesSearch(s, query) {
				filtered = append(filtered, s)
			}
		}
		m.sessions = filtered
	}

	// Keep cursor in bounds
	if m.cursor >= len(m.sessions) {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}

	return m
}

// sessionMatchesSearch checks if a session matches the search query
func sessionMatchesSearch(s *SessionState, query string) bool {
	return strings.Contains(strings.ToLower(s.ID), query) ||
		strings.Contains(strings.ToLower(s.Cwd), query) ||
		strings.Contains(strings.ToLower(s.Status), query) ||
		strings.Contains(strings.ToLower(s.StatusDetail), query) ||
		strings.Contains(strings.ToLower(s.ConvID), query)
}

// matchesShowFilter checks if a status matches the show filter
func (m model) matchesShowFilter(status string) bool {
	if len(m.statusFilter) == 0 {
		return true // no filter = show all
	}
	for _, f := range m.statusFilter {
		if f == "all" {
			return true
		}
		if f == status {
			return true
		}
		// Handle grouped filters
		if f == "attention" && (status == StatusAwaitingPermission || status == StatusAwaitingInput) {
			return true
		}
	}
	return false
}

// matchesHideFilter checks if a status should be hidden
func (m model) matchesHideFilter(status string) bool {
	if len(m.hideFilter) == 0 {
		return false // no filter = hide nothing
	}
	for _, f := range m.hideFilter {
		if f == status {
			return true
		}
		// Handle grouped filters
		if f == "attention" && (status == StatusAwaitingPermission || status == StatusAwaitingInput) {
			return true
		}
	}
	return false
}

// ensureCursorVisible adjusts viewport to keep cursor in view
func (m *model) ensureCursorVisible() {
	if m.viewportHeight <= 0 {
		return
	}
	if m.cursor < m.viewportOffset {
		m.viewportOffset = m.cursor
	}
	if m.cursor >= m.viewportOffset+m.viewportHeight {
		m.viewportOffset = m.cursor - m.viewportHeight + 1
	}
}

// triggerKill initiates kill confirmation for the selected session
func (m model) triggerKill() model {
	if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
		m.confirmMode = confirmKill
	}
	return m
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle confirmation dialogs first
		if m.confirmMode != confirmNone {
			if m.confirmMode == confirmQuit {
				switch msg.String() {
				case "enter", "y", "Y":
					return m, tea.Quit
				default:
					m.confirmMode = confirmNone
				}
				return m, nil
			}
			switch msg.String() {
			case "y", "Y":
				if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
					if m.confirmMode == confirmKill {
						state := m.sessions[m.cursor]
						_ = killSession(state)
						m.confirmMode = confirmNone
						m = m.refreshSessions()
					} else if m.confirmMode == confirmAttachForce {
						m.shouldAttach = m.sessions[m.cursor].TmuxSession
						m.shouldAttachID = m.sessions[m.cursor].ID
						m.forceAttach = true
						m.confirmMode = confirmNone
						return m, tea.Quit
					} else if m.confirmMode == confirmDetach {
						state := m.sessions[m.cursor]
						_ = DetachSessionClients(state.TmuxSession)
						m.confirmMode = confirmNone
						m = m.refreshSessions()
					}
				}
			case "n", "N", "esc", "q", "enter", " ":
				m.confirmMode = confirmNone
			}
			return m, nil
		}

		// Handle help view
		if m.helpView {
			m.helpView = false
			return m, nil
		}

		// Handle search mode
		if m.searchFocused {
			switch msg.String() {
			case "esc", "ctrl+c":
				if m.searchInput != "" {
					m.searchInput = ""
					m = m.applySearchFilter()
				} else {
					m.searchFocused = false
				}
			case "enter":
				m.searchFocused = false
			case "up":
				// Exit search and navigate up
				m.searchFocused = false
				if m.cursor > 0 {
					m.cursor--
				}
			case "down":
				// Exit search and navigate down
				m.searchFocused = false
				if m.cursor < len(m.sessions)-1 {
					m.cursor++
				}
			case "backspace":
				if len(m.searchInput) > 0 {
					m.searchInput = m.searchInput[:len(m.searchInput)-1]
					m = m.applySearchFilter()
				}
			case "ctrl+u":
				m.searchInput = ""
				m = m.applySearchFilter()
			default:
				// Add printable characters to search
				if len(msg.String()) == 1 && msg.String()[0] >= 32 && msg.String()[0] < 127 {
					m.searchInput += msg.String()
					m = m.applySearchFilter()
				}
			}
			return m, nil
		}

		// Handle filter menu
		if m.filterMenu {
			switch msg.String() {
			case "esc", "q":
				m.filterMenu = false
			case "up", "k":
				if m.filterCursor > 0 {
					m.filterCursor--
				}
			case "down", "j":
				if m.filterCursor < len(filterOptions)-1 {
					m.filterCursor++
				}
			case " ", "x":
				// Toggle checkbox
				key := filterOptions[m.filterCursor].key
				if key == "all" {
					// "All" clears all other selections
					m.filterChecked = make(map[string]bool)
					m.filterChecked["all"] = true
				} else {
					// Toggle this option, clear "all" if set
					delete(m.filterChecked, "all")
					m.filterChecked[key] = !m.filterChecked[key]
					if !m.filterChecked[key] {
						delete(m.filterChecked, key)
					}
					// If nothing selected, select "all"
					if len(m.filterChecked) == 0 {
						m.filterChecked["all"] = true
					}
				}
			case "enter", "f":
				// Apply filter and close
				m.statusFilter = nil
				if !m.filterChecked["all"] {
					for key, checked := range m.filterChecked {
						if checked && key != "all" {
							m.statusFilter = append(m.statusFilter, key)
						}
					}
				}
				// Enable includeAll if exited is selected
				if m.filterChecked[StatusExited] {
					m.includeAll = true
				}
				m.filterMenu = false
				m = m.refreshSessions()
			}
			return m, nil
		}

		// Normal mode
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.searchInput != "" {
				m.searchInput = ""
				m = m.applySearchFilter()
			} else {
				m.confirmMode = confirmQuit
			}
		case "/":
			m.searchFocused = true
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.ensureCursorVisible()
			}
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
				m.ensureCursorVisible()
			}
		case "enter":
			if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
				state := m.sessions[m.cursor]
				tmuxAlive := state.TmuxSession != "" && IsTmuxSessionAlive(state.TmuxSession)
				if !tmuxAlive {
					// Non-tmux or dead tmux session, cannot attach
					m.confirmMode = confirmNoTmux
				} else if state.Attached > 0 {
					// Session already has clients attached - just focus the window
					m.shouldAttach = state.TmuxSession
					m.shouldAttachID = state.ID
					m.focusOnly = true
					return m, tea.Quit
				} else {
					m.shouldAttach = state.TmuxSession
					m.shouldAttachID = state.ID
					return m, tea.Quit
				}
			}
		case "delete", "backspace", "x", "ctrl+d":
			// Note: ctrl+d is what macOS sends for forward delete key
			m = m.triggerKill()
		case "d", "D":
			// Detach clients from session
			if len(m.sessions) > 0 && m.cursor < len(m.sessions) {
				state := m.sessions[m.cursor]
				if state.Attached > 0 && state.TmuxSession != "" {
					m.confirmMode = confirmDetach
				}
			}
		case "f":
			// Initialize filter checkboxes from current filter state
			m.filterChecked = make(map[string]bool)
			if len(m.statusFilter) == 0 {
				m.filterChecked["all"] = true
			} else {
				for _, f := range m.statusFilter {
					m.filterChecked[f] = true
				}
			}
			m.filterCursor = 0
			m.filterMenu = true
		case "h", "?":
			m.helpView = true
		case "r":
			// Force refresh
			m = m.refreshSessions()
		case "n", "N":
			// Create a new session in current directory
			m.createNew = true
			return m, tea.Quit
		default:
			if m.sort.HandleSortKey(m.columns(), msg.String()) {
				m = m.refreshSessions()
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewportHeight = max(msg.Height-10, 5) // Reserve space for header, footer, etc.

	case tickMsg:
		// Poll DB: only do full refresh if data has changed
		maxUpdated, err := MaxUpdatedAt()
		if err == nil && maxUpdated.After(m.lastUpdatedAt) {
			m.lastUpdatedAt = maxUpdated
			m = m.refreshSessions()
		}
		return m, tickCmd()
	}

	return m, nil
}

var (
	confirmStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	menuStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	filterBadge  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// columns returns the column definitions for the session table.
// Used by both Update (for sort key handling) and View (for rendering).
func (m model) columns() []table.Column {
	return []table.Column{
		{Header: "", Width: 2},                   // Attached indicator
		{Header: "ID", Width: 10, SortKey: "id"}, // ID
		{Header: "PROJECT", MinWidth: 15, Weight: 0.25, Truncate: true, TruncateMode: table.TruncateStart, SortKey: "project"}, // Project
		{Header: "TITLE/PROMPT", MinWidth: 20, Weight: 0.5, Truncate: true},                                                    // Title/prompt (not sortable)
		{Header: "STATUS", MinWidth: 15, Weight: 0.25, Truncate: true, SortKey: "status"},                                      // Status
		{Header: "UPDATED", Width: 10, SortKey: "updated"},                                                                     // Updated
	}
}

func (m model) View() string {
	// Help view overlay
	if m.helpView {
		return m.renderHelpView()
	}

	// Filter menu overlay
	if m.filterMenu {
		return m.renderFilterMenu()
	}

	var b strings.Builder

	// Search box
	b.WriteString("\n  ")
	if m.searchFocused {
		b.WriteString(searchStyle.Render("Search: "))
		b.WriteString(searchStyle.Render("[" + m.searchInput + "_]"))
	} else if m.searchInput != "" {
		b.WriteString(searchStyle.Render("Search: [" + m.searchInput + "]"))
	} else {
		b.WriteString(helpStyle.Render("/ to search"))
	}

	// Show count
	if len(m.sessions) != len(m.allSessions) {
		b.WriteString(helpStyle.Render(fmt.Sprintf("  [showing %d of %d]", len(m.sessions), len(m.allSessions))))
	} else if len(m.allSessions) > 0 {
		b.WriteString(helpStyle.Render(fmt.Sprintf("  [%d sessions]", len(m.allSessions))))
	}
	b.WriteString("\n\n")

	// Show empty state
	if len(m.sessions) == 0 {
		if len(m.allSessions) == 0 {
			b.WriteString("  No active sessions")
			if len(m.statusFilter) > 0 {
				b.WriteString(fmt.Sprintf(" (filter: %s)", strings.Join(m.statusFilter, ", ")))
			}
			b.WriteString("\n")
		} else {
			b.WriteString("  No matches for \"" + m.searchInput + "\"\n")
		}
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  n new • / search • f filter • r refresh • q quit"))
		b.WriteString("\n")
		return b.String()
	}

	// Build table using shared column definitions
	tableWidth := max(m.width-3, 60)
	cols := m.columns()
	tbl := table.New(cols...)
	tbl.Padding = 3
	tbl.SetTerminalWidth(tableWidth)
	tbl.HeaderStyle = headerStyle
	tbl.SelectedStyle = selectedStyle
	tbl.SelectedIndex = m.cursor
	tbl.ViewportOffset = m.viewportOffset
	tbl.ViewportHeight = m.viewportHeight
	tbl.Sort = m.sort.ToConfig(cols)

	// Add rows
	for _, state := range m.sessions {
		status := state.Status
		if state.StatusDetail != "" {
			status = status + ": " + state.StatusDetail
		}

		// Add attached/type indicator
		attachedMark := "  "
		tmuxAlive := state.TmuxSession != "" && IsTmuxSessionAlive(state.TmuxSession)
		if !tmuxAlive {
			attachedMark = " ◉" // Non-tmux or dead tmux (in-terminal, can't attach)
		} else if state.Attached > 0 {
			attachedMark = "⚡" // Tmux with attached clients
		} else {
			attachedMark = " ▷" // Tmux detached (can attach)
		}

		// Get project path (full path, table will truncate from start)
		project := state.Cwd

		// Get title/prompt from conversation if available
		title := convindex.GetConvTitleAndPrompt(state.ConvID, state.Cwd)
		if title == "" {
			title = "-"
		}

		updated := FormatDuration(time.Since(state.Updated))

		tbl.AddRow(table.Row{
			Cells: []string{attachedMark, state.ID, project, title, status, updated},
			Style: getRowStyle(state.Status),
		})
	}

	// Show filter badge (if filtering by status)
	if len(m.statusFilter) > 0 {
		b.WriteString(filterBadge.Render(fmt.Sprintf("[filter: %s]", strings.Join(m.statusFilter, ", "))))
		b.WriteString("\n")
	}

	// Render table
	b.WriteString(tbl.RenderWithScroll(&helpStyle))
	b.WriteString("\n\n")
	switch m.confirmMode {
	case confirmQuit:
		b.WriteString(confirmStyle.Render("  Exit? [enter/y=yes / any key=cancel]"))
	case confirmKill:
		if m.cursor < len(m.sessions) {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Kill session %s? [y/n]", m.sessions[m.cursor].ID)))
		}
	case confirmAttachForce:
		if m.cursor < len(m.sessions) {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Session %s already attached. Detach other clients? [y/n]", m.sessions[m.cursor].ID)))
		}
	case confirmDetach:
		if m.cursor < len(m.sessions) {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Detach all clients from session %s? [y/n]", m.sessions[m.cursor].ID)))
		}
	case confirmNoTmux:
		if m.cursor < len(m.sessions) {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Session %s was started outside tclaude/tmux (◉) - already in its terminal. [press any key]", m.sessions[m.cursor].ID)))
		}
	default:
		b.WriteString(helpStyle.Render("  h help • n new • / search • ↑/↓ navigate • enter attach • esc/q quit"))
	}
	b.WriteString("\n")

	return b.String()
}

func (m model) renderFilterMenu() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(menuStyle.Render("  Filter by status:"))
	b.WriteString("\n\n")

	for i, opt := range filterOptions {
		// Cursor indicator
		if i == m.filterCursor {
			b.WriteString(selectedStyle.Render(" >"))
		} else {
			b.WriteString("  ")
		}

		// Checkbox
		if m.filterChecked[opt.key] {
			b.WriteString(" [x] ")
		} else {
			b.WriteString(" [ ] ")
		}

		// Label
		if i == m.filterCursor {
			b.WriteString(selectedStyle.Render(opt.label))
		} else {
			b.WriteString(opt.label)
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  ↑/↓ navigate • space toggle • enter apply • esc cancel"))
	b.WriteString("\n")
	return b.String()
}

func (m model) renderHelpView() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(menuStyle.Render("  Session Watch - Keyboard Shortcuts"))
	b.WriteString("\n\n")

	b.WriteString(headerStyle.Render("  Navigation"))
	b.WriteString("\n")
	b.WriteString("    ↑/k       Move cursor up\n")
	b.WriteString("    ↓/j       Move cursor down\n")
	b.WriteString("    enter     Attach to selected session\n")
	b.WriteString("    q/esc     Quit watch mode\n")
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("  Search"))
	b.WriteString("\n")
	b.WriteString("    /         Start search\n")
	b.WriteString("    esc       Clear search / exit search mode\n")
	b.WriteString("    ^U        Clear search input\n")
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("  Actions"))
	b.WriteString("\n")
	b.WriteString("    n         New session (in current directory)\n")
	b.WriteString("    del/x     Kill selected session (with confirmation)\n")
	b.WriteString("    r         Refresh session list\n")
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("  Filtering"))
	b.WriteString("\n")
	b.WriteString("    f         Open filter menu\n")
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("  Sorting"))
	b.WriteString("\n")
	for _, line := range table.SortableColumnsHelp(m.columns()) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString(headerStyle.Render("  Session Indicators"))
	b.WriteString("\n")
	b.WriteString("    ⚡        Tmux session with clients attached\n")
	b.WriteString("    ▷         Tmux session, detached (can attach)\n")
	b.WriteString("    ◉         Non-tmux session (in-terminal, can't attach)\n")
	b.WriteString("\n")

	b.WriteString(helpStyle.Render("  Press any key to close"))
	b.WriteString("\n")
	return b.String()
}

func getRowStyle(status string) lipgloss.Style {
	switch status {
	case StatusIdle:
		return idleStyle
	case StatusWorking:
		return workingStyle
	case StatusAwaitingPermission, StatusAwaitingInput:
		return needsInput
	case StatusExited:
		return exitedStyle
	default:
		return needsInput
	}
}

// WatchState holds state that persists between attach cycles
type WatchState struct {
	Sort         table.SortState
	StatusFilter []string
	HideFilter   []string
	SearchInput  string
	Cursor       int
}

// AttachResult holds the result of selecting a session to attach
type AttachResult struct {
	TmuxSession string
	SessionID   string // session ID for inbox watcher
	ForceAttach bool   // true if we should detach other clients
	CreateNew   bool   // true if user wants to create a new session
	FocusOnly   bool   // true if we should just focus (session already attached)
}

// RunInteractive starts the interactive session viewer
// Returns the attach result (if any) and the final watch state
func RunInteractive(includeAll bool, state WatchState) (AttachResult, WatchState, error) {
	m := initialModel(includeAll, state.StatusFilter, state.HideFilter)
	m.sort = state.Sort
	m.searchInput = state.SearchInput
	m.cursor = state.Cursor
	m = m.refreshSessions()

	// Ensure cursor is still valid after loading
	if m.cursor >= len(m.sessions) {
		m.cursor = max(0, len(m.sessions)-1)
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return AttachResult{}, state, err
	}

	fm := finalModel.(model)
	result := AttachResult{
		TmuxSession: fm.shouldAttach,
		SessionID:   fm.shouldAttachID,
		ForceAttach: fm.forceAttach,
		CreateNew:   fm.createNew,
		FocusOnly:   fm.focusOnly,
	}
	newState := WatchState{
		Sort:         fm.sort,
		StatusFilter: fm.statusFilter,
		HideFilter:   fm.hideFilter,
		SearchInput:  fm.searchInput,
		Cursor:       fm.cursor,
	}
	return result, newState, nil
}

// RunWatchMode runs the interactive watch mode with attach support
func RunWatchMode(includeAll bool, initialSort table.SortState, initialFilter, initialHide []string) error {
	_, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}

	state := WatchState{Sort: initialSort, StatusFilter: initialFilter, HideFilter: initialHide}
	for {
		result, newState, err := RunInteractive(includeAll, state)
		state = newState // Preserve state between attach cycles
		if err != nil {
			return err
		}

		// Handle creating a new session
		if result.CreateNew {
			// Create session detached, then attach as subprocess so we return here
			params := &NewParams{Detached: true}
			if err := runNew(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
				continue
			}

			// Find the newly created session and attach to it
			states, _ := ListSessionStates()
			if len(states) > 0 {
				// Get the most recently created session
				var newest *SessionState
				for _, s := range states {
					if newest == nil || s.Created.After(newest.Created) {
						newest = s
					}
				}
				if newest != nil && newest.TmuxSession != "" {
					fmt.Printf("Attaching to %s... (Ctrl+B D to detach)\n", newest.TmuxSession)
					_ = AttachToSession(newest.ID, newest.TmuxSession, false)
				}
			}
			// After session ends or user detaches, continue back to watch
			continue
		}

		if result.TmuxSession == "" {
			// User quit without selecting - auto-prune exited sessions
			pruneExitedSessionsSilent()
			return nil
		}

		// Focus only - just focus the window and return to watch mode
		if result.FocusOnly {
			os.Setenv("TCLAUDE_SESSION_ID", result.SessionID)
			TryFocusAttachedSession(result.TmuxSession)
			continue
		}

		// Attach to the session
		if result.ForceAttach {
			fmt.Printf("Attaching to %s (detaching others)... (Ctrl+B D to detach)\n", result.TmuxSession)
		} else {
			fmt.Printf("Attaching to %s... (Ctrl+B D to detach)\n", result.TmuxSession)
		}

		err = AttachToSession(result.SessionID, result.TmuxSession, result.ForceAttach)
		if err != nil {
			// Session might have ended, continue to interactive view
			continue
		}

		// After detach or session end, loop back to interactive view
	}
}
