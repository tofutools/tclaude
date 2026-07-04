package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Watch-view styles. Their colors come from the active TUI color scheme
// (config tui.color_scheme) resolved through tuistyle: init() seeds the
// default scheme and RunInteractive re-applies the configured one before the
// program starts. See applyTUIColorScheme.
var (
	selectedStyle lipgloss.Style
	idleStyle     lipgloss.Style
	workingStyle  lipgloss.Style
	needsInput    lipgloss.Style
	exitedStyle   lipgloss.Style
	headerStyle   lipgloss.Style
	helpStyle     lipgloss.Style
	searchStyle   lipgloss.Style
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
	{StatusError, "Error", StatusError},
	{StatusExited, "Exited", StatusExited},
}

type model struct {
	allSessions       []*SessionState // all sessions before search filter
	sessions          []*SessionState // sessions after search filter
	cursor            int
	width             int
	height            int
	viewportOffset    int
	viewportHeight    int
	shouldAttach      string // tmux session name to attach to after quitting
	shouldAttachID    string // session ID for inbox watcher
	forceAttach       bool   // detach other clients when attaching
	focusOnly         bool   // just focus, don't attach (session already attached elsewhere)
	createNew         bool   // create a new session after quitting
	includeAll        bool
	sort              table.SortState
	statusFilter      []string        // which statuses to show (empty = all)
	hideFilter        []string        // which statuses to hide
	confirmMode       confirmMode     // current confirmation dialog
	confirmTarget     *SessionState   // the session a selection dialog was opened FOR — pinned at open time, because the 500ms tick re-sorts rows under the cursor while the dialog is up (acting on m.sessions[m.cursor] could kill/detach whatever drifted onto that row)
	filterMenu        bool            // showing filter menu
	filterCursor      int             // cursor position in filter menu
	filterChecked     map[string]bool // checked items in filter menu
	helpView          bool            // showing help view
	searchInput       textinput.Model // search query input
	searchFocused     bool            // whether search box is focused
	lastUpdatedAt     time.Time       // tracks DB changes for polling
	newSessionMode    bool            // showing the "new session" prompt
	newSessionDir     string          // chosen directory for the new session (set on confirm)
	newSessionLabel   string          // chosen label for the new session (set on confirm)
	newSessionName    string          // chosen display name for the new session (set on confirm)
	newSessionHarness string          // chosen harness for the new session (set on confirm)
	newSessionField   int             // focused field in the new-session prompt: 0=dir, 1=label, 2=name, 3=harness
	dirInput          textinput.Model // directory input for the new session prompt
	dirSuggestions    []string        // ambiguous tab-completion candidates for dirInput
	labelInput        textinput.Model // label input for the new session prompt
	nameInput         textinput.Model // display-name input for the new session prompt
	harnessOptions    []string        // spawnable harness names, cycled by the harness field
	harnessIdx        int             // index into harnessOptions currently selected
}

// numNewSessionFields is the count of fields the new-session prompt cycles
// through with up/down/tab (directory, label, name, harness).
const numNewSessionFields = 4

func newSessionSearchInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	s := ti.Styles()
	s.Focused.Text = searchStyle
	s.Blurred.Text = searchStyle
	ti.SetStyles(s)
	return ti
}

// newDirInput builds the text input used by the "new session" directory
// prompt, prefilled with the current working directory so enter-to-accept
// reproduces the old (no-prompt) behavior.
func newDirInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "  Directory: "
	if wd, err := os.Getwd(); err == nil {
		ti.SetValue(wd)
	}
	ti.SetWidth(80)
	return ti
}

// newLabelInput builds the text input used by the "new session" label
// prompt. Empty by default — an unset label falls back to runNew's own
// synthetic/resumed-conv id.
func newLabelInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "  Label:     "
	ti.SetWidth(80)
	return ti
}

// newNameInput builds the text input used by the "new session" display-name
// prompt (claude --name; becomes the session's conversation title). Empty
// by default — an unset name omits the flag entirely.
func newNameInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "  Name:      "
	ti.SetWidth(80)
	return ti
}

// spawnableHarnessNames returns the registered harness names that can
// actually launch a session (have a Spawner), sorted, for the "new session"
// prompt's harness field to cycle through.
func spawnableHarnessNames() []string {
	var out []string
	for _, name := range harness.Names() {
		if h, ok := harness.Get(name); ok && h.Spawn != nil {
			out = append(out, name)
		}
	}
	return out
}

// expandHomePrefix expands a leading "~" or "~/" in p to the user's home
// directory, for filesystem lookups during directory tab-completion. The
// returned path is only used to stat/list the filesystem; the text the user
// sees and edits keeps whatever they typed (e.g. "~/Doc" stays "~/Doc").
func expandHomePrefix(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// completeDirPath performs bash-like Tab completion of a directory path: it
// matches the final path segment against sibling directory names and
// extends input to their longest common prefix. When exactly one directory
// matches, the result is completed all the way through a trailing "/" (so
// pressing Tab repeatedly walks down the tree); when several match, input is
// extended as far as unambiguous and the candidate names are returned for
// display, mirroring bash's "partial-complete, then list" behavior.
func completeDirPath(input string) (completed string, candidates []string) {
	if input == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home + "/", nil
		}
		return input, nil
	}

	dir, prefix := filepath.Split(expandHomePrefix(input))
	if dir == "" {
		dir = "."
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return input, nil
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return input, nil
	}
	sort.Strings(names)

	common := names[0]
	for _, n := range names[1:] {
		common = commonStringPrefix(common, n)
	}

	// input's final path segment is always `prefix` verbatim (home
	// expansion only ever rewrites the directory portion before it), so
	// trimming that many characters off the end of input recovers the
	// unexpanded lead-in (e.g. "~/" stays "~/" rather than becoming the
	// resolved home directory).
	head := input[:len(input)-len(prefix)]
	completed = head + common
	if len(names) == 1 {
		return completed + "/", nil
	}
	if completed != input {
		return completed, names
	}
	return input, names
}

// commonStringPrefix returns the longest common prefix of a and b.
func commonStringPrefix(a, b string) string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

func initialModel(includeAll bool, statusFilter, hideFilter []string) model {
	return model{
		sessions:     []*SessionState{},
		cursor:       0,
		includeAll:   includeAll,
		sort:         table.SortState{},
		statusFilter: statusFilter,
		hideFilter:   hideFilter,
		searchInput:  newSessionSearchInput(),
		dirInput:     newDirInput(),
	}
}

func (m model) Init() tea.Cmd {
	return tickCmd()
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
	searchVal := m.searchInput.Value()
	if searchVal == "" {
		m.sessions = m.allSessions
	} else {
		query := strings.ToLower(searchVal)
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

	// Drop a selection-bound confirm dialog whose target vanished from the
	// visible list (exited row pruned, filtered out, killed elsewhere): its
	// prompt would otherwise render blank while the dialog kept swallowing
	// keys. Presence is checked by ID — a refresh rebuilds the row structs,
	// and the target still being listed (on whatever row) keeps the dialog
	// valid since actions key on confirmTarget, not the cursor. confirmQuit
	// is selection-independent and survives refreshes.
	if m.confirmMode != confirmNone && m.confirmMode != confirmQuit {
		found := false
		if m.confirmTarget != nil {
			for _, s := range m.sessions {
				if s.ID == m.confirmTarget.ID {
					found = true
					break
				}
			}
		}
		if !found {
			m.confirmMode = confirmNone
			m.confirmTarget = nil
		}
	}

	return m
}

// sessionMatchesSearch checks if a session matches the search query. The
// tmux name is matched alongside the PK because it is what the ID column
// renders (sessionHandle) — typing the name you see must filter to it.
func sessionMatchesSearch(s *SessionState, query string) bool {
	return strings.Contains(strings.ToLower(s.ID), query) ||
		strings.Contains(strings.ToLower(s.TmuxSession), query) ||
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
		if f == "attention" && (status == StatusAwaitingPermission || status == StatusAwaitingInput || status == StatusError) {
			return true
		}
		if f == StatusWorking && status == StatusMainAgentIdle {
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
		if f == "attention" && (status == StatusAwaitingPermission || status == StatusAwaitingInput || status == StatusError) {
			return true
		}
		if f == StatusWorking && status == StatusMainAgentIdle {
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
		m.confirmTarget = m.sessions[m.cursor]
	}
	return m
}

// moveNewSessionField shifts focus among the new-session prompt's fields
// (directory/label/name/harness), wrapping around, and moves the
// text-input focus to match — only the dir/label/name fields are real
// textinput.Models; the harness field has no cursor, so all three blur.
func (m model) moveNewSessionField(delta int) model {
	m.newSessionField = ((m.newSessionField+delta)%numNewSessionFields + numNewSessionFields) % numNewSessionFields
	m.dirInput.Blur()
	m.labelInput.Blur()
	m.nameInput.Blur()
	switch m.newSessionField {
	case 0:
		m.dirInput.Focus()
	case 1:
		m.labelInput.Focus()
	case 2:
		m.nameInput.Focus()
	}
	return m
}

// cycleHarness steps the harness field's selection by delta, wrapping
// around harnessOptions.
func (m model) cycleHarness(delta int) model {
	n := len(m.harnessOptions)
	if n == 0 {
		return m
	}
	m.harnessIdx = ((m.harnessIdx+delta)%n + n) % n
	return m
}

// updateFocusedNewSessionInput forwards msg to whichever of dirInput/
// labelInput/nameInput currently has focus in the new-session prompt (the
// harness field has no textinput.Model backing it, so callers only reach
// here for fields 0/1/2).
func (m model) updateFocusedNewSessionInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.newSessionField {
	case 1:
		m.labelInput, cmd = m.labelInput.Update(msg)
	case 2:
		m.nameInput, cmd = m.nameInput.Update(msg)
	default:
		m.dirInput, cmd = m.dirInput.Update(msg)
	}
	return m, cmd
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
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
			// confirmNoTmux is informational — dismiss on any key, as its
			// "[press any key]" prompt promises.
			if m.confirmMode == confirmNoTmux {
				m.confirmMode = confirmNone
				m.confirmTarget = nil
				return m, nil
			}
			// Selection dialogs act on the session they were opened for
			// (confirmTarget), NOT m.sessions[m.cursor]: the 500ms tick
			// re-sorts rows under an open dialog, so the cursor row can be
			// a different session by the time "y" lands.
			switch msg.String() {
			case "y", "Y":
				if m.confirmTarget != nil {
					switch m.confirmMode {
					case confirmKill:
						_ = killSession(m.confirmTarget)
						m.confirmMode = confirmNone
						m.confirmTarget = nil
						m = m.refreshSessions()
					case confirmAttachForce:
						m.shouldAttach = m.confirmTarget.TmuxSession
						m.shouldAttachID = m.confirmTarget.ID
						m.forceAttach = true
						m.confirmMode = confirmNone
						m.confirmTarget = nil
						return m, tea.Quit
					case confirmDetach:
						_, _ = DetachSessionClients(m.confirmTarget.TmuxSession)
						m.confirmMode = confirmNone
						m.confirmTarget = nil
						m = m.refreshSessions()
					}
				}
			case "n", "N", "esc", "q", "enter", " ":
				m.confirmMode = confirmNone
				m.confirmTarget = nil
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
				if m.searchInput.Value() != "" {
					m.searchInput.SetValue("")
					m = m.applySearchFilter()
				} else {
					m.searchFocused = false
					m.searchInput.Blur()
				}
			case "enter":
				m.searchFocused = false
				m.searchInput.Blur()
			case "up":
				m.searchFocused = false
				m.searchInput.Blur()
				if m.cursor > 0 {
					m.cursor--
				}
			case "down":
				m.searchFocused = false
				m.searchInput.Blur()
				if m.cursor < len(m.sessions)-1 {
					m.cursor++
				}
			default:
				prevVal := m.searchInput.Value()
				var cmd tea.Cmd
				m.searchInput, cmd = m.searchInput.Update(msg)
				if m.searchInput.Value() != prevVal {
					m = m.applySearchFilter()
				}
				return m, cmd
			}
			return m, nil
		}

		// Handle new-session prompt (directory / label / name / harness fields)
		if m.newSessionMode {
			const harnessField = 3
			switch msg.String() {
			case "esc", "ctrl+c":
				m.newSessionMode = false
				m.dirInput.Blur()
				m.labelInput.Blur()
				m.nameInput.Blur()
			case "enter":
				m.newSessionMode = false
				m.dirInput.Blur()
				m.labelInput.Blur()
				m.nameInput.Blur()
				m.newSessionDir = expandHomePrefix(strings.TrimSpace(m.dirInput.Value()))
				m.newSessionLabel = strings.TrimSpace(m.labelInput.Value())
				m.newSessionName = strings.TrimSpace(m.nameInput.Value())
				if len(m.harnessOptions) > 0 {
					m.newSessionHarness = m.harnessOptions[m.harnessIdx]
				}
				m.createNew = true
				return m, tea.Quit
			case "up":
				m = m.moveNewSessionField(-1)
			case "down":
				m = m.moveNewSessionField(1)
			case "shift+tab":
				m = m.moveNewSessionField(-1)
			case "tab":
				if m.newSessionField == 0 {
					completed, candidates := completeDirPath(m.dirInput.Value())
					m.dirInput.SetValue(completed)
					m.dirInput.CursorEnd()
					m.dirSuggestions = candidates
				} else {
					m = m.moveNewSessionField(1)
				}
			case "left":
				if m.newSessionField == harnessField {
					m = m.cycleHarness(-1)
				} else {
					return m.updateFocusedNewSessionInput(msg)
				}
			case "right":
				if m.newSessionField == harnessField {
					m = m.cycleHarness(1)
				} else {
					return m.updateFocusedNewSessionInput(msg)
				}
			case " ":
				if m.newSessionField == harnessField {
					m = m.cycleHarness(1)
				} else {
					return m.updateFocusedNewSessionInput(msg)
				}
			default:
				m.dirSuggestions = nil
				if m.newSessionField != harnessField {
					return m.updateFocusedNewSessionInput(msg)
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
			if m.searchInput.Value() != "" {
				m.searchInput.SetValue("")
				m = m.applySearchFilter()
			} else {
				m.confirmMode = confirmQuit
			}
		case "/":
			m.searchFocused = true
			m.searchInput.Focus()
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
					m.confirmTarget = state
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
					m.confirmTarget = state
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
			// Prompt for the directory/label/name/harness to start the new session with
			m.dirInput = newDirInput()
			m.dirInput.Focus()
			m.dirSuggestions = nil
			m.labelInput = newLabelInput()
			m.nameInput = newNameInput()
			m.harnessOptions = spawnableHarnessNames()
			m.harnessIdx = 0
			for i, name := range m.harnessOptions {
				if name == harness.DefaultName {
					m.harnessIdx = i
					break
				}
			}
			m.newSessionField = 0
			m.newSessionMode = true
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
	confirmStyle lipgloss.Style
	menuStyle    lipgloss.Style
	filterBadge  lipgloss.Style
)

func init() { applyTUIColorScheme(config.TUIColorSchemeDefault) }

// applyTUIColorScheme (re)builds the watch-view styles from the palette for
// the given color scheme (config tui.color_scheme). It is called at package
// init with the default scheme, then again from RunInteractive with the
// configured scheme before the bubbletea program starts. The watch view is a
// per-process singleton, so mutating these package-level styles once at
// startup is safe.
func applyTUIColorScheme(scheme string) {
	p := tuistyle.Resolve(scheme)
	// The selected row keeps the row's own foreground color (bold, on a
	// dark-gray background) in both schemes — it never sets a foreground of
	// its own.
	selectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(p.SelectedBg))
	idleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Idle))
	workingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Working))
	needsInput = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Danger))
	exitedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Exited))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Header))
	helpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Help))
	searchStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Accent))
	confirmStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Danger))
	menuStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Accent))
	filterBadge = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Accent))
}

// columns returns the column definitions for the session table.
// Used by both Update (for sort key handling) and View (for rendering).
func (m model) columns() []table.Column {
	return []table.Column{
		{Header: "", Width: 2}, // Attached indicator
		// The ID column renders sessionHandle (the tmux name when set) — the
		// same handle `session ls` prints and attach/kill accept. Flexible
		// width because handles are no longer uniformly 8-10 chars: labels
		// and dir-style names (session.tmux_name_style="dir") run up to 32.
		{Header: "ID", MinWidth: 10, MaxWidth: 32, Weight: 0.12, Truncate: true, SortKey: "id"},
		{Header: "PROJECT", MinWidth: 15, Weight: 0.25, Truncate: true, TruncateMode: table.TruncateStart, SortKey: "project"}, // Project
		{Header: "TITLE/PROMPT", MinWidth: 20, Weight: 0.5, Truncate: true},                                                    // Title/prompt (not sortable)
		{Header: "STATUS", MinWidth: 15, Weight: 0.25, Truncate: true, SortKey: "status"},                                      // Status
		{Header: "UPDATED", Width: 10, SortKey: "updated"},                                                                     // Updated
	}
}

func (m model) View() tea.View {
	// Help view overlay
	if m.helpView {
		return tea.View{Content: m.renderHelpView(), AltScreen: true}
	}

	// Filter menu overlay
	if m.filterMenu {
		return tea.View{Content: m.renderFilterMenu(), AltScreen: true}
	}

	// New-session directory prompt overlay
	if m.newSessionMode {
		return tea.View{Content: m.renderNewSessionPrompt(), AltScreen: true}
	}

	var b strings.Builder

	// Search box
	b.WriteString("\n  ")
	if m.searchFocused {
		b.WriteString(searchStyle.Render("Search: "))
		b.WriteString(m.searchInput.View())
	} else if m.searchInput.Value() != "" {
		b.WriteString(searchStyle.Render("Search: [" + m.searchInput.Value() + "]"))
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
				fmt.Fprintf(&b, " (filter: %s)", strings.Join(m.statusFilter, ", "))
			}
			b.WriteString("\n")
		} else {
			b.WriteString("  No matches for \"" + m.searchInput.Value() + "\"\n")
		}
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  n new • / search • f filter • r refresh • q quit"))
		b.WriteString("\n")
		return tea.View{Content: b.String(), AltScreen: true}
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
		var attachedMark string
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
			Cells: []string{attachedMark, sessionHandle(state), project, title, status, updated},
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
		if m.confirmTarget != nil {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Kill session %s? [y/n]", sessionHandle(m.confirmTarget))))
		}
	case confirmAttachForce:
		if m.confirmTarget != nil {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Session %s already attached. Detach other clients? [y/n]", sessionHandle(m.confirmTarget))))
		}
	case confirmDetach:
		if m.confirmTarget != nil {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Detach all clients from session %s? [y/n]", sessionHandle(m.confirmTarget))))
		}
	case confirmNoTmux:
		if m.confirmTarget != nil {
			b.WriteString(confirmStyle.Render(fmt.Sprintf("  Session %s was started outside tclaude/tmux (◉) - already in its terminal. [press any key]", sessionHandle(m.confirmTarget))))
		}
	default:
		b.WriteString(helpStyle.Render("  h help • n new • / search • ↑/↓ navigate • enter attach • esc/q quit"))
	}
	b.WriteString("\n")

	return tea.View{Content: b.String(), AltScreen: true}
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

func (m model) renderNewSessionPrompt() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(menuStyle.Render("  New session"))
	b.WriteString("\n\n")

	b.WriteString(m.dirInput.View())
	b.WriteString("\n")
	// Always emit this line, blank or not, so the suggestion list appearing/
	// disappearing doesn't shift the fields below it up and down.
	if len(m.dirSuggestions) > 0 {
		b.WriteString("  " + strings.Join(m.dirSuggestions, "  "))
	}
	b.WriteString("\n")

	b.WriteString(m.labelInput.View())
	b.WriteString("\n")

	b.WriteString(m.nameInput.View())
	b.WriteString("\n")

	harnessLine := "  Harness:   "
	if len(m.harnessOptions) == 0 {
		harnessLine += "(none available)"
	} else {
		harnessLine += "< " + m.harnessOptions[m.harnessIdx] + " >"
	}
	if m.newSessionField == 3 {
		// menuStyle (foreground-only) rather than selectedStyle (background
		// fill): selectedStyle relies on the terminal's default foreground
		// contrasting with its dark-gray background, which holds for a
		// dark-themed terminal but reads as low-contrast dark-on-dark on a
		// light/white background. A foreground-only accent stays legible
		// either way.
		b.WriteString(menuStyle.Render(harnessLine))
	} else {
		b.WriteString(harnessLine)
	}
	b.WriteString("\n\n")

	b.WriteString(helpStyle.Render("  enter start • ↑/↓/tab next field • tab complete dir • ←/→ change harness • esc cancel"))
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
	b.WriteString("    n         New session (prompts for directory/label/name/harness, default: current dir)\n")
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
	case StatusMainAgentIdle:
		return workingStyle
	case StatusWorking:
		return workingStyle
	case StatusAwaitingPermission, StatusAwaitingInput, StatusError:
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
	TmuxSession       string
	SessionID         string // session ID for inbox watcher
	ForceAttach       bool   // true if we should detach other clients
	CreateNew         bool   // true if user wants to create a new session
	NewSessionDir     string // directory to start the new session in (with CreateNew)
	NewSessionLabel   string // label for the new session (with CreateNew)
	NewSessionName    string // display name for the new session (with CreateNew)
	NewSessionHarness string // harness for the new session (with CreateNew)
	FocusOnly         bool   // true if we should just focus (session already attached)
}

// RunInteractive starts the interactive session viewer
// Returns the attach result (if any) and the final watch state
func RunInteractive(includeAll bool, state WatchState) (AttachResult, WatchState, error) {
	// Apply the configured TUI color scheme before building the program so the
	// styles are set once, up front (Load is nil-safe on error → default).
	cfg, _ := config.Load()
	applyTUIColorScheme(cfg.TUIColorScheme())

	m := initialModel(includeAll, state.StatusFilter, state.HideFilter)
	m.sort = state.Sort
	m.searchInput.SetValue(state.SearchInput)
	m.cursor = state.Cursor
	m = m.refreshSessions()

	// Ensure cursor is still valid after loading
	if m.cursor >= len(m.sessions) {
		m.cursor = max(0, len(m.sessions)-1)
	}

	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return AttachResult{}, state, err
	}

	fm := finalModel.(model)
	result := AttachResult{
		TmuxSession:       fm.shouldAttach,
		SessionID:         fm.shouldAttachID,
		ForceAttach:       fm.forceAttach,
		CreateNew:         fm.createNew,
		NewSessionDir:     fm.newSessionDir,
		NewSessionLabel:   fm.newSessionLabel,
		NewSessionName:    fm.newSessionName,
		NewSessionHarness: fm.newSessionHarness,
		FocusOnly:         fm.focusOnly,
	}
	newState := WatchState{
		Sort:         fm.sort,
		StatusFilter: fm.statusFilter,
		HideFilter:   fm.hideFilter,
		SearchInput:  fm.searchInput.Value(),
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
			params := &NewParams{
				Detached: true,
				Dir:      result.NewSessionDir,
				Label:    result.NewSessionLabel,
				Name:     result.NewSessionName,
				Harness:  result.NewSessionHarness,
			}
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
			_ = os.Setenv("TCLAUDE_SESSION_ID", result.SessionID)
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
