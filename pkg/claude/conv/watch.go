package conv

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/fsnotify/fsnotify"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// Watch-view styles. Their colors come from the active TUI color scheme
// (config tui.color_scheme) resolved through tuistyle: init() seeds the
// default scheme and RunConvWatch re-applies the configured one before the
// program starts. See applyTUIColorScheme.
var (
	wSelectedStyle lipgloss.Style
	wHeaderStyle   lipgloss.Style
	wHelpStyle     lipgloss.Style
	wSearchStyle   lipgloss.Style
	wConfirmStyle  lipgloss.Style
	wSemanticStyle lipgloss.Style
)

func init() { applyTUIColorScheme(config.TUIColorSchemeDefault) }

// applyTUIColorScheme (re)builds the watch-view styles from the palette for
// the given color scheme (config tui.color_scheme). It is called at package
// init with the default scheme, then again from RunConvWatch with the
// configured scheme before the bubbletea program starts. The watch view is a
// per-process singleton, so mutating these package-level styles once at
// startup is safe.
func applyTUIColorScheme(scheme string) {
	p := tuistyle.Resolve(scheme)
	// Selected row: bold on a dark-gray background. The default scheme paints
	// its foreground white; the high-contrast scheme leaves the row's own
	// color (empty SelectedFg) as it did before #738.
	sel := lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(p.SelectedBg))
	if p.SelectedFg != "" {
		sel = sel.Foreground(lipgloss.Color(p.SelectedFg))
	}
	wSelectedStyle = sel
	wHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Header))
	wHelpStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Help))
	wSearchStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Accent))
	wConfirmStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Danger))
	wSemanticStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.Info)) // cyan
}

type watchTickMsg time.Time

// fsFileChangeMsg is sent when fsnotify detects a .jsonl file change.
// New files and deletes arrive immediately; writes to existing files
// are debounced (only sent after fsDebounceDelay of inactivity).
type fsFileChangeMsg struct {
	FilePath string
	Removed  bool
}

const fsDebounceDelay = 30 * time.Second

// Semantic search message types
type semanticCheckMsg struct {
	Unindexed int
	Total     int
	Err       error
}

type semanticIndexProgressMsg struct {
	Done   int
	Total  int
	Errors int
}

type semanticIndexDoneMsg struct {
	Indexed int
	Errors  int
}

type semanticSearchResultMsg struct {
	Results []EmbedSearchResult
	Query   string
	Err     error
}

// watchConfirmMode represents confirmation dialogs
type watchConfirmMode int

const (
	watchConfirmNone              watchConfirmMode = iota
	watchConfirmAttachForce                        // Session already attached, confirm force attach
	watchConfirmDelete                             // Delete conversation (no active session)
	watchConfirmDeleteWithSession                  // Delete conversation that has an active session
	watchConfirmNoTmux                             // Session has no tmux, cannot attach
	watchConfirmQuit                               // Confirm exit via ESC
)

type watchModel struct {
	// Data
	entries        []SessionEntry
	filtered       []SessionEntry                   // After search filter
	activeSessions map[string]*session.SessionState // convID -> session
	groupsByConv   map[string][]string              // convID -> group names; empty if no groups configured
	pendingByConv  map[string]string                // convID -> spawn-time agent name; title fallback (convDisplayTitle)

	// Navigation
	cursor         int
	viewportOffset int
	viewportHeight int

	// Search
	searchInput   textinput.Model
	searchFocused bool

	// Worktree branch input
	worktreeInput   textinput.Model
	worktreeFocused bool

	// Group filter — narrows the visible rows to convs that are members
	// of the named group. Composes with the text search and the archived
	// toggle. groupFilter is the committed value (empty = no filter);
	// groupFilterInput is the focused input where the user types.
	groupFilterInput   textinput.Model
	groupFilterFocused bool
	groupFilter        string

	// Semantic search
	semanticChecking       bool
	semanticFocused        bool
	semanticInput          textarea.Model
	semanticQuery          string // last submitted query (for display after search)
	semanticMode           bool
	semanticResults        []EmbedSearchResult
	semanticScores         map[string]float32
	semanticError          string
	semanticIndexing       bool
	semanticIndexTotal     int
	semanticIndexDone      int
	semanticIndexErrors    int
	semanticIndexChan      chan tea.Msg
	semanticCancelCh       chan struct{}
	semanticIndexPrompt    bool
	semanticUnindexedCount int
	semanticTotalCount     int

	// Sort
	sort table.SortState

	// UI state
	width       int
	height      int
	confirmMode watchConfirmMode
	helpView    bool

	// Column selector (the `c` overlay). colOverrides maps a toggleable
	// column key to an explicit show/hide that shadows the column's smart
	// auto-default; absent keys follow the auto rule. Loaded from / persisted
	// to ~/.tclaude/config.json so choices stick across launches.
	columnSelector bool
	columnCursor   int
	colOverrides   map[string]bool

	// Settings
	global       bool   // Search all projects
	projectPath  string // Current project path
	since        string // Filter: modified after
	before       string // Filter: modified before
	showArchived bool   // Include archived convs (conv_index.archived_at set) in the listing; toggled with `x`

	// Result
	selectedConv   *SessionEntry
	shouldCreate   bool // true = create new session, false = attach to existing
	forceAttach    bool
	focusOnly      bool   // Just focus the window, don't attach
	focusTmux      string // Tmux session to focus
	focusSessionID string // Session ID for focus (needed for WSL window title search)
	createWorktree bool   // true = create worktree for selected conv
	worktreeBranch string // Branch name for worktree

	// Status message (shown briefly after actions)
	statusMsg string

	// DB polling
	lastSessionUpdatedAt   time.Time
	lastConvIndexUpdatedAt time.Time
	claudeProjectDir       string // resolved Claude project dir for single-project mode

	// fsnotify — debounced channel-based approach
	watcher   *fsnotify.Watcher
	fsChan    chan fsFileChangeMsg // debounced events delivered here
	fsCloseCh chan struct{}        // closed to stop the debounce goroutine
}

func newSearchInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	s := ti.Styles()
	s.Focused.Placeholder = wHelpStyle
	s.Focused.Text = wSearchStyle
	s.Blurred.Placeholder = wHelpStyle
	s.Blurred.Text = wSearchStyle
	ti.SetStyles(s)
	return ti
}

func newWorktreeInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	s := ti.Styles()
	s.Focused.Text = wSearchStyle
	s.Blurred.Text = wSearchStyle
	ti.SetStyles(s)
	ti.Validate = func(s string) error {
		for _, c := range s {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '/' {
				return fmt.Errorf("invalid branch character: %c", c)
			}
		}
		return nil
	}
	return ti
}

func newSemanticInput() textarea.Model {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.SetHeight(1) // starts at 1 line, grows with content up to 3
	ta.MaxHeight = 3
	ta.CharLimit = 0
	// Disable default enter for newline insertion — enter submits the search.
	// Newlines via shift+enter, alt+enter, or paste.
	ta.KeyMap.InsertNewline = key.NewBinding()
	// Style: match semantic color scheme
	focused := textarea.StyleState{
		Base:        lipgloss.NewStyle(),
		Text:        wSemanticStyle,
		Placeholder: wHelpStyle,
	}
	styles := ta.Styles()
	styles.Focused = focused
	styles.Blurred = focused
	ta.SetStyles(styles)
	return ta
}

func initialWatchModel(global bool, since, before string) watchModel {
	cwd, _ := os.Getwd()

	var claudeProjectDir string
	if !global {
		claudeProjectDir = GetClaudeProjectPath(cwd)
	}

	return watchModel{
		entries:          []SessionEntry{},
		filtered:         []SessionEntry{},
		activeSessions:   make(map[string]*session.SessionState),
		searchInput:      newSearchInput(),
		worktreeInput:    newWorktreeInput(),
		groupFilterInput: newSearchInput(),
		semanticInput:    newSemanticInput(),
		global:           global,
		projectPath:      cwd,
		claudeProjectDir: claudeProjectDir,
		since:            since,
		before:           before,
		viewportHeight:   20, // Will be adjusted based on terminal size
		colOverrides:     loadColumnOverrides(),
	}
}

// loadColumnOverrides reads the persisted watch-view column visibility
// overrides from ~/.tclaude/config.json. A load error or absent block yields
// an empty map (every column follows its auto-default).
func loadColumnOverrides() map[string]bool {
	overrides := map[string]bool{}
	cfg, err := config.Load()
	if err != nil || cfg.ConvWatch == nil {
		return overrides
	}
	for k, v := range cfg.ConvWatch.Columns {
		overrides[k] = v
	}
	return overrides
}

// --- tea.Model interface (pointer receiver) ---

func (m *watchModel) Init() tea.Cmd {
	cmds := []tea.Cmd{watchTickCmd()}
	if fsCmd := m.startFSWatcher(); fsCmd != nil {
		cmds = append(cmds, fsCmd)
	}
	return tea.Batch(cmds...)
}

func (m *watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		// Clear transient semantic error on any key
		m.semanticError = ""

		// Handle confirmation dialogs first
		if m.confirmMode != watchConfirmNone {
			if m.confirmMode == watchConfirmQuit {
				switch msg.String() {
				case "enter", "y", "Y":
					return m, tea.Quit
				default:
					m.confirmMode = watchConfirmNone
				}
				return m, nil
			}
			switch msg.String() {
			case "y", "Y":
				if m.cursor < len(m.filtered) {
					conv := m.filtered[m.cursor]
					switch m.confirmMode {
					case watchConfirmAttachForce:
						m.selectedConv = &conv
						m.shouldCreate = false
						m.forceAttach = true
						m.confirmMode = watchConfirmNone
						return m, tea.Quit
					case watchConfirmDelete:
						if err := m.deleteConversation(&conv); err != nil {
							m.statusMsg = "Error: " + err.Error()
						} else {
							m.statusMsg = "Deleted conversation " + conv.SessionID[:8]
						}
						m.confirmMode = watchConfirmNone
						m.reloadFromDB()
					case watchConfirmDeleteWithSession:
						if state, ok := m.activeSessions[conv.SessionID]; ok {
							if err := m.stopSession(state); err != nil {
								m.statusMsg = "Error stopping session: " + err.Error()
							}
						}
						if err := m.deleteConversation(&conv); err != nil {
							m.statusMsg = "Error: " + err.Error()
						} else {
							m.statusMsg = "Stopped session and deleted conversation " + conv.SessionID[:8]
						}
						m.confirmMode = watchConfirmNone
						m.reloadFromDB()
					}
				}
			case "s", "S":
				if m.confirmMode == watchConfirmDeleteWithSession && m.cursor < len(m.filtered) {
					conv := m.filtered[m.cursor]
					if state, ok := m.activeSessions[conv.SessionID]; ok {
						if err := m.stopSession(state); err != nil {
							m.statusMsg = "Error: " + err.Error()
						} else {
							m.statusMsg = "Stopped session " + state.ID
						}
					}
					m.confirmMode = watchConfirmNone
					m.refreshActiveSessions()
				}
			case "n", "N", "esc", " ":
				m.confirmMode = watchConfirmNone
			}
			if m.confirmMode == watchConfirmNoTmux {
				m.confirmMode = watchConfirmNone
			}
			return m, nil
		}

		// Handle help view
		if m.helpView {
			m.helpView = false
			return m, nil
		}

		// Handle column selector overlay (the `c` menu)
		if m.columnSelector {
			toggleable := m.toggleableColumns()
			switch msg.String() {
			case "up", "k":
				if m.columnCursor > 0 {
					m.columnCursor--
				}
			case "down", "j":
				if m.columnCursor < len(toggleable)-1 {
					m.columnCursor++
				}
			case " ", "space":
				if m.columnCursor < len(toggleable) {
					c := toggleable[m.columnCursor]
					m.setColumnOverride(c.key, !c.visible)
				}
			case "r":
				m.resetColumnOverrides()
			case "enter", "c", "esc", "q", "ctrl+c":
				m.columnSelector = false
			}
			return m, nil
		}

		// Handle semantic indexing (only esc/ctrl+c to cancel)
		if m.semanticIndexing {
			if msg.String() == "esc" || msg.String() == "ctrl+c" {
				if m.semanticCancelCh != nil {
					close(m.semanticCancelCh)
					m.semanticCancelCh = nil
				}
				m.semanticIndexing = false
				m.semanticFocused = true
				m.semanticInput.Focus()
			}
			return m, nil
		}

		// Handle semantic index prompt
		if m.semanticIndexPrompt {
			switch msg.String() {
			case "y", "Y":
				return m, m.semanticStartIndex()
			case "n", "N", "esc", "ctrl+c":
				m.semanticIndexPrompt = false
				m.semanticFocused = true
				m.semanticInput.Focus()
			}
			return m, nil
		}

		// Handle semantic search input
		if m.semanticFocused {
			switch msg.String() {
			case "esc", "ctrl+c":
				m.semanticFocused = false
				m.semanticInput.Blur()
				m.semanticInput.Reset()
			case "shift+enter", "alt+enter", "ctrl+j":
				// Insert newline in multiline query
				// Note: many terminals report shift+enter as ctrl+j
				m.semanticInput.InsertString("\n")
				m.updateSemanticInputHeight()
				return m, nil
			case "enter":
				query := strings.TrimSpace(m.semanticInput.Value())
				if query != "" {
					m.semanticFocused = false
					m.semanticInput.Blur()
					m.semanticQuery = query
					return m, m.semanticRunSearch(query)
				}
			default:
				var cmd tea.Cmd
				m.semanticInput, cmd = m.semanticInput.Update(msg)
				m.updateSemanticInputHeight()
				return m, cmd
			}
			return m, nil
		}

		// Handle search mode
		if m.searchFocused {
			switch msg.String() {
			case "esc", "ctrl+c":
				if m.searchInput.Value() != "" {
					m.searchInput.SetValue("")
					m.applySearchFilter()
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
					m.ensureCursorVisible()
				}
			case "down":
				m.searchFocused = false
				m.searchInput.Blur()
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
					m.ensureCursorVisible()
				}
			default:
				prevVal := m.searchInput.Value()
				var cmd tea.Cmd
				m.searchInput, cmd = m.searchInput.Update(msg)
				if m.searchInput.Value() != prevVal {
					m.applySearchFilter()
				}
				return m, cmd
			}
			return m, nil
		}

		// Handle group-filter input mode. Press `f` from normal mode to
		// open it. Typing committed via Enter; Esc clears + blurs.
		// Composes with searchInput / archived toggle on apply.
		if m.groupFilterFocused {
			switch msg.String() {
			case "esc", "ctrl+c":
				if m.groupFilterInput.Value() != "" {
					m.groupFilterInput.SetValue("")
				} else {
					m.groupFilterFocused = false
					m.groupFilterInput.Blur()
					if m.groupFilter != "" {
						m.groupFilter = ""
						if m.semanticMode {
							m.rebuildSemanticFiltered()
						} else {
							m.applySearchFilter()
						}
						m.statusMsg = "group filter cleared"
					}
				}
			case "enter":
				m.groupFilter = strings.TrimSpace(m.groupFilterInput.Value())
				m.groupFilterFocused = false
				m.groupFilterInput.Blur()
				if m.semanticMode {
					m.rebuildSemanticFiltered()
				} else {
					m.applySearchFilter()
				}
				if m.groupFilter == "" {
					m.statusMsg = "group filter cleared"
				} else {
					m.statusMsg = "filtering by group: " + m.groupFilter
				}
			case "up":
				m.groupFilterFocused = false
				m.groupFilterInput.Blur()
				if m.cursor > 0 {
					m.cursor--
					m.ensureCursorVisible()
				}
			case "down":
				m.groupFilterFocused = false
				m.groupFilterInput.Blur()
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
					m.ensureCursorVisible()
				}
			default:
				var cmd tea.Cmd
				m.groupFilterInput, cmd = m.groupFilterInput.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// Handle worktree branch input mode
		if m.worktreeFocused {
			switch msg.String() {
			case "esc", "ctrl+c":
				m.worktreeFocused = false
				m.worktreeInput.Blur()
				m.worktreeInput.SetValue("")
			case "enter":
				if m.worktreeInput.Value() != "" && m.cursor < len(m.filtered) {
					conv := m.filtered[m.cursor]
					m.selectedConv = &conv
					m.createWorktree = true
					m.worktreeBranch = m.worktreeInput.Value()
					return m, tea.Quit
				}
				m.worktreeFocused = false
				m.worktreeInput.Blur()
			default:
				var cmd tea.Cmd
				m.worktreeInput, cmd = m.worktreeInput.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// Normal mode
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.semanticMode {
				m.clearSemanticMode()
			} else if m.searchInput.Value() != "" {
				m.searchInput.SetValue("")
				m.applySearchFilter()
			} else if m.groupFilter != "" {
				// Same priority order as the search clear: a single Esc
				// dismisses the topmost filter, then a second Esc opens
				// the quit confirm. Group filter takes precedence over
				// quit-confirm so a user with an active filter doesn't
				// accidentally start quitting.
				m.groupFilter = ""
				m.applySearchFilter()
				m.statusMsg = "group filter cleared"
			} else {
				m.confirmMode = watchConfirmQuit
			}
		case "/":
			m.clearSemanticMode()
			m.searchFocused = true
			m.searchInput.Focus()
		case "f":
			// Open the group-name input. Composes with the existing
			// search bar and archived toggle so the user can stack
			// filters: text search across all columns, group narrows to
			// a specific membership, archived toggle reveals -x rows.
			// Pre-fills with the current filter so reopening shows what
			// was active.
			m.groupFilterInput.SetValue(m.groupFilter)
			m.groupFilterFocused = true
			m.groupFilterInput.Focus()
		case "s":
			m.semanticChecking = true
			return m, m.semanticPreCheck()
		case "c":
			// Open the column-selector overlay. Lets the user show/hide the
			// optional columns (harness, groups, size, modified, project)
			// and persists the choice.
			m.columnSelector = true
			m.columnCursor = 0
		case "x":
			// Toggle visibility of archived convs (conv_index.archived_at
			// set). Default-hidden; useful when forensically tracking down a
			// reincarnated instance's history. Mnemonic: press `x` to reveal
			// the e`x`pired generations (displayed with a `-x` title).
			// Delete actions (`del` / `backspace` /
			// `ctrl+d`) still work — `x` was freed up specifically for
			// this toggle. Re-runs the active filter (search or
			// semantic) so the result composes with whatever else is
			// on.
			m.showArchived = !m.showArchived
			if m.semanticMode {
				m.rebuildSemanticFiltered()
			} else {
				m.applySearchFilter()
			}
			if m.showArchived {
				m.statusMsg = "showing archived convs (press x to hide)"
			} else {
				m.statusMsg = "hiding archived convs (press x to show)"
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.ensureCursorVisible()
			}
		case "down", "j":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.ensureCursorVisible()
			}
		case "pgup", "ctrl+b":
			m.cursor -= m.viewportHeight
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.ensureCursorVisible()
		case "pgdown", "ctrl+f":
			m.cursor += m.viewportHeight
			if m.cursor >= len(m.filtered) {
				m.cursor = len(m.filtered) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.ensureCursorVisible()
		case "home", "g":
			m.cursor = 0
			m.ensureCursorVisible()
		case "end", "G":
			if len(m.filtered) > 0 {
				m.cursor = len(m.filtered) - 1
			}
			m.ensureCursorVisible()
		case "enter":
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				conv := m.filtered[m.cursor]
				if existing, ok := m.activeSessions[conv.SessionID]; ok {
					tmuxAlive := existing.TmuxSession != "" && session.IsTmuxSessionAlive(existing.TmuxSession)
					if !tmuxAlive {
						m.confirmMode = watchConfirmNoTmux
					} else if existing.Attached > 0 {
						m.focusOnly = true
						m.focusTmux = existing.TmuxSession
						m.focusSessionID = existing.ID
						return m, tea.Quit
					} else {
						m.selectedConv = &conv
						m.shouldCreate = false
						return m, tea.Quit
					}
				} else {
					m.selectedConv = &conv
					m.shouldCreate = true
					return m, tea.Quit
				}
			}
		case "r":
			m.fullReloadConversations()
			m.statusMsg = ""
		case "h", "?":
			m.helpView = true
		case "W", "w":
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				m.worktreeFocused = true
				m.worktreeInput.SetValue("")
				m.worktreeInput.Focus()
			}
		case "delete", "backspace", "ctrl+d":
			m.triggerDelete()
		default:
			if m.semanticMode {
				// Sort keys exit semantic mode
				ch := msg.String()
				if len(ch) == 1 && ch[0] >= '1' && ch[0] <= '9' {
					m.clearSemanticMode()
					m.sort.HandleSortKey(m.columns(), ch)
					m.resortAndFilter()
				}
			} else if m.sort.HandleSortKey(m.columns(), msg.String()) {
				// In-memory re-sort only — no disk or DB access
				m.resortAndFilter()
			}
		}

	case tea.PasteMsg:
		if m.semanticFocused {
			var cmd tea.Cmd
			m.semanticInput, cmd = m.semanticInput.Update(msg)
			m.updateSemanticInputHeight()
			return m, cmd
		}
		if m.searchFocused {
			prevVal := m.searchInput.Value()
			var cmd tea.Cmd
			m.searchInput, cmd = m.searchInput.Update(msg)
			if m.searchInput.Value() != prevVal {
				m.applySearchFilter()
			}
			return m, cmd
		}
		if m.worktreeFocused {
			var cmd tea.Cmd
			m.worktreeInput, cmd = m.worktreeInput.Update(msg)
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewportHeight = max(msg.Height-10, 5)
		m.ensureCursorVisible()

	case watchTickMsg:
		// Poll session DB: refresh active session indicators if changed
		maxSessionUpdated, err := session.MaxUpdatedAt()
		if err == nil && maxSessionUpdated.After(m.lastSessionUpdatedAt) {
			m.lastSessionUpdatedAt = maxSessionUpdated
			m.refreshActiveSessions()
		}

		// Poll conv_index DB: detect changes from other tclaude instances
		var maxConvUpdated time.Time
		if m.global {
			maxConvUpdated, _ = db.MaxConvIndexUpdatedAt()
		} else {
			maxConvUpdated, _ = db.MaxConvIndexUpdatedAtForProject(m.claudeProjectDir)
		}
		if !maxConvUpdated.IsZero() && maxConvUpdated.After(m.lastConvIndexUpdatedAt) {
			m.lastConvIndexUpdatedAt = maxConvUpdated
			m.reloadFromDB()
		}

		return m, watchTickCmd()

	case fsFileChangeMsg:
		if m.shouldAcceptFSEvent(msg.FilePath) {
			m.handleFSChange(msg.FilePath, msg.Removed)
		}
		return m, m.continueListenFSEvents()

	case semanticCheckMsg:
		m.semanticChecking = false
		if msg.Err != nil {
			m.semanticError = "Ollama error: " + msg.Err.Error()
			return m, nil
		}
		if msg.Unindexed > 0 {
			m.semanticIndexPrompt = true
			m.semanticUnindexedCount = msg.Unindexed
			m.semanticTotalCount = msg.Total
		} else {
			m.semanticFocused = true
			m.semanticInput.Focus()
		}
		return m, nil

	case semanticIndexProgressMsg:
		m.semanticIndexDone = msg.Done
		m.semanticIndexTotal = msg.Total
		m.semanticIndexErrors = msg.Errors
		return m, waitForSemanticIndex(m.semanticIndexChan)

	case semanticIndexDoneMsg:
		m.semanticIndexing = false
		m.semanticFocused = true
		m.semanticInput.Focus()
		return m, nil

	case semanticSearchResultMsg:
		if msg.Err != nil {
			m.semanticError = "Search error: " + msg.Err.Error()
			m.semanticQuery = ""
			return m, nil
		}
		m.semanticResults = msg.Results
		m.semanticScores = make(map[string]float32, len(msg.Results))
		m.filtered = m.filtered[:0]
		for _, r := range msg.Results {
			m.semanticScores[r.Entry.SessionID] = r.Similarity
			m.filtered = append(m.filtered, r.Entry)
		}
		m.semanticMode = true
		m.semanticQuery = msg.Query
		m.cursor = 0
		m.viewportOffset = 0
		return m, nil
	}

	return m, nil
}

func (m *watchModel) View() tea.View {
	if m.helpView {
		return tea.View{Content: m.renderHelpView(), AltScreen: true}
	}
	if m.columnSelector {
		return tea.View{Content: m.renderColumnSelector(), AltScreen: true}
	}

	var b strings.Builder

	// Search box
	b.WriteString("\n  ")
	if m.semanticChecking {
		b.WriteString(wSemanticStyle.Render("Checking embedding model is available..."))
	} else if m.semanticIndexing {
		progress := semanticProgressBar(m.semanticIndexDone, m.semanticIndexTotal)
		b.WriteString(wSemanticStyle.Render(fmt.Sprintf("Indexing: [%s] %d/%d (esc to cancel)", progress, m.semanticIndexDone, m.semanticIndexTotal)))
	} else if m.semanticIndexPrompt {
		b.WriteString(wSemanticStyle.Render(fmt.Sprintf("%d of %d conversations not indexed. Index now? [y/n]", m.semanticUnindexedCount, m.semanticTotalCount)))
	} else if m.semanticFocused {
		b.WriteString(wSemanticStyle.Render("Semantic: "))
		b.WriteString(m.semanticInput.View())
	} else if m.semanticQuery != "" && !m.semanticMode {
		b.WriteString(wSemanticStyle.Render("Searching: [" + m.semanticQuery + "]..."))
	} else if m.semanticMode {
		b.WriteString(wSemanticStyle.Render("Semantic: [" + m.semanticQuery + "]"))
	} else if m.semanticError != "" {
		b.WriteString(wConfirmStyle.Render(m.semanticError))
	} else if m.searchFocused {
		b.WriteString(wSearchStyle.Render("Search: "))
		b.WriteString(m.searchInput.View())
	} else if m.groupFilterFocused {
		b.WriteString(wSearchStyle.Render("Group: "))
		b.WriteString(m.groupFilterInput.View())
	} else if m.searchInput.Value() != "" {
		b.WriteString(wSearchStyle.Render("Search: [" + m.searchInput.Value() + "]"))
		if m.groupFilter != "" {
			b.WriteString(wSearchStyle.Render(" + Group: [" + m.groupFilter + "]"))
		}
	} else if m.groupFilter != "" {
		b.WriteString(wSearchStyle.Render("Group: [" + m.groupFilter + "]"))
	} else {
		b.WriteString(wHelpStyle.Render("/ to search • f to filter by group"))
	}

	// Show count
	if len(m.filtered) != len(m.entries) {
		b.WriteString(wHelpStyle.Render(fmt.Sprintf("  [showing %d of %d]", len(m.filtered), len(m.entries))))
	} else if len(m.entries) > 0 {
		b.WriteString(wHelpStyle.Render(fmt.Sprintf("  [%d conversations]", len(m.entries))))
	}
	b.WriteString("\n\n")

	// Show empty state
	if len(m.filtered) == 0 {
		if len(m.entries) == 0 {
			b.WriteString("  No conversations found\n")
		} else {
			b.WriteString("  No matches for \"" + m.searchInput.Value() + "\"\n")
		}
		b.WriteString("\n")
		b.WriteString(wHelpStyle.Render("  r refresh • / search • s semantic • q quit"))
		b.WriteString("\n")
		return tea.View{Content: b.String(), AltScreen: true}
	}

	// Build table from a single ordered column list, so the headers and the
	// row cells are guaranteed to stay in lockstep.
	tableWidth := max(m.width-3, 60)
	defs := m.orderedColumns()
	cols := make([]table.Column, 0, len(defs))
	for _, d := range defs {
		if d.visible {
			cols = append(cols, d.col)
		}
	}
	tbl := table.New(cols...)
	tbl.Padding = 3
	tbl.SetTerminalWidth(tableWidth)
	tbl.HeaderStyle = wHeaderStyle
	tbl.SelectedStyle = wSelectedStyle
	tbl.SelectedIndex = m.cursor
	tbl.ViewportOffset = m.viewportOffset
	tbl.ViewportHeight = m.viewportHeight
	tbl.Sort = m.sort.ToConfig(cols)

	for _, e := range m.filtered {
		cells := make([]string, 0, len(cols))
		for _, d := range defs {
			if d.visible {
				cells = append(cells, d.cell(m, e))
			}
		}
		tbl.AddRow(table.Row{Cells: cells})
	}

	b.WriteString(tbl.RenderWithScroll(&wHelpStyle))
	b.WriteString("\n\n")
	switch m.confirmMode {
	case watchConfirmQuit:
		b.WriteString(wConfirmStyle.Render("  Exit? [enter/y=yes / any key=cancel]"))
	case watchConfirmAttachForce:
		b.WriteString(wConfirmStyle.Render("  Session already attached. Detach others? [y/n]"))
	case watchConfirmDelete:
		b.WriteString(wConfirmStyle.Render("  Delete conversation? [y/n]"))
	case watchConfirmDeleteWithSession:
		b.WriteString(wConfirmStyle.Render("  Has active session. Delete+stop (y), stop only (s), cancel (n)?"))
	case watchConfirmNoTmux:
		b.WriteString(wConfirmStyle.Render("  Session was started outside tclaude/tmux (◉) - already in its terminal. [press any key]"))
	default:
		if m.worktreeFocused {
			b.WriteString(wSearchStyle.Render("  Branch: "))
			b.WriteString(m.worktreeInput.View())
			b.WriteString(wHelpStyle.Render(" (enter to create, esc to cancel)"))
		} else if m.statusMsg != "" {
			b.WriteString(wSearchStyle.Render("  " + m.statusMsg))
		} else if m.semanticMode {
			b.WriteString(wHelpStyle.Render("  s new search • / text search • esc exit • h help • esc/q quit"))
		} else {
			b.WriteString(wHelpStyle.Render("  / search • s semantic • c columns • enter attach • h help • esc/q quit"))
		}
	}
	b.WriteString("\n")

	return tea.View{Content: b.String(), AltScreen: true}
}

// --- Data loading ---

// fullReloadConversations does a full disk+DB scan. Used for initial load and manual refresh (r key).
func (m *watchModel) fullReloadConversations() {
	var allEntries []SessionEntry

	if m.global {
		// A missing projects dir is not fatal — there may still be
		// other-harness (Codex) convs to merge below. Mirrors list.go.
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil && !os.IsNotExist(err) {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projectPath := projectsDir + "/" + entry.Name()
			index, err := LoadSessionsIndex(projectPath)
			if err != nil {
				continue
			}
			allEntries = append(allEntries, index.Entries...)
		}
		// Merge every other registered harness (Codex, …), all dirs.
		allEntries = appendNonClaudeHarnessEntries(allEntries, "")
	} else {
		// A missing Claude project dir is not fatal either — the cwd may
		// have only non-Claude (Codex) convs. Load Claude when its project
		// dir exists, then always merge other harnesses for the real cwd.
		if _, err := os.Stat(m.claudeProjectDir); err == nil {
			index, err := LoadSessionsIndex(m.claudeProjectDir)
			if err != nil {
				m.entries = []SessionEntry{}
				m.filtered = []SessionEntry{}
				return
			}
			allEntries = index.Entries
		}
		// Merge other harnesses for the real cwd (m.projectPath) — NOT the
		// encoded claudeProjectDir: the harness cwd filter is an exact-string
		// match, so the encoded dir would silently drop the Codex convs.
		allEntries = appendNonClaudeHarnessEntries(allEntries, m.projectPath)
	}

	m.setEntries(allEntries)
}

// reloadFromDB loads entries from the SQLite cache only (no filesystem access).
// Used when DB polling detects changes made by another tclaude instance.
func (m *watchModel) reloadFromDB() {
	var dbProjectDir, cwd string
	if !m.global {
		dbProjectDir = m.claudeProjectDir
		cwd = m.projectPath
	}
	allEntries, err := LoadEntriesFromDB(dbProjectDir)
	if err != nil {
		return
	}
	// Merge non-Claude harnesses live, mirroring list.go. conv_index only
	// caches Claude convs — the Codex rows the hook callback snapshots are
	// stubs (no `created`) that LoadEntriesFromDB skips — so there is no
	// duplicate to dedupe. The scoped case must pass the real cwd, not the
	// encoded claudeProjectDir: the harness cwd filter is an exact-string
	// match, so the encoded dir would silently drop the Codex convs.
	allEntries = appendNonClaudeHarnessEntries(allEntries, cwd)
	m.setEntries(allEntries)
}

// setEntries replaces the entries list, applies time filter, sort, and search filter.
func (m *watchModel) setEntries(allEntries []SessionEntry) {
	allEntries, _ = FilterEntriesByTime(allEntries, m.since, m.before)
	m.sortInPlace(allEntries)
	m.entries = allEntries
	m.applySearchFilter()
	m.refreshActiveSessions()
	m.refreshGroups()
	m.refreshPending()
}

// refreshGroups reloads agent group memberships from the DB. Errors are
// non-fatal — the worst that happens is the GROUPS column is empty.
func (m *watchModel) refreshGroups() {
	g, err := db.GroupNamesByConv()
	if err != nil {
		return
	}
	m.groupsByConv = g
}

// refreshPending reloads spawn-time agent names from the DB. Errors are
// non-fatal — the worst that happens is a not-yet-renamed agent shows its
// first prompt instead of its designated name (convDisplayTitle).
func (m *watchModel) refreshPending() {
	p, err := db.PendingNamesByConv()
	if err != nil {
		return
	}
	m.pendingByConv = p
}

// hasGroups reports whether any conv in the current entry list belongs to a
// group. Used to decide whether to render the GROUPS column at all.
func (m *watchModel) hasGroups() bool {
	for _, e := range m.entries {
		if len(m.groupsByConv[e.SessionID]) > 0 {
			return true
		}
	}
	return false
}

// --- Sorting (in-memory only) ---

// sortInPlace sorts the given slice using current sort state (or default modified desc).
func (m *watchModel) sortInPlace(entries []SessionEntry) {
	if m.sort.Key != "" {
		sortConvEntriesByKey(entries, m.sort.Key, m.sort.Direction, m.groupsByConv)
	} else {
		sortEntriesByModifiedDesc(entries)
	}
}

// resortAndFilter re-sorts the in-memory entries and reapplies the search filter.
func (m *watchModel) resortAndFilter() {
	m.sortInPlace(m.entries)
	m.applySearchFilter()
}

// --- fsnotify ---

func watchTickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return watchTickMsg(t)
	})
}

// startFSWatcher sets up an fsnotify watcher on ~/.claude/projects and starts
// a background goroutine that debounces events per file.
func (m *watchModel) startFSWatcher() tea.Cmd {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("watch: failed to create fsnotify watcher", "error", err)
		return nil
	}
	m.watcher = w
	m.fsChan = make(chan fsFileChangeMsg, 64)
	m.fsCloseCh = make(chan struct{})

	projectsDir := ClaudeProjectsDir()
	if projectsDir == "" {
		return nil
	}

	// Always watch the projects root so we detect new project subdirs
	// (and in single-project mode, detect the project dir being created).
	if err := w.Add(projectsDir); err != nil {
		slog.Warn("watch: failed to watch projects dir", "path", projectsDir, "error", err)
	}

	if m.global {
		// Watch each existing subdirectory
		entries, err := os.ReadDir(projectsDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					if err := w.Add(filepath.Join(projectsDir, e.Name())); err != nil {
						slog.Debug("watch: failed to watch project subdir", "name", e.Name(), "error", err)
					}
				}
			}
		}
	} else if m.claudeProjectDir != "" {
		// Watch the specific project dir (may not exist yet — that's OK,
		// the debounce loop will add it when we see it created under projectsDir).
		if err := w.Add(m.claudeProjectDir); err != nil {
			slog.Debug("watch: project dir not watchable yet, will detect via parent", "path", m.claudeProjectDir, "error", err)
		}
	}

	go fsDebounceLoop(w, projectsDir, m.fsChan, m.fsCloseCh)

	return waitForFSEvent(m.fsChan)
}

// fsDebounceLoop reads raw fsnotify events and dispatches them:
//   - New files (Create) and deletes (Remove/Rename) are sent immediately.
//   - Writes to existing files are debounced per-file: only forwarded after
//     fsDebounceDelay of inactivity, since active conversations write constantly.
func fsDebounceLoop(w *fsnotify.Watcher, projectsDir string, outCh chan<- fsFileChangeMsg, closeCh <-chan struct{}) {
	fsDebounceEvents(w.Events, w.Errors, w.Add, projectsDir, outCh, closeCh)
}

type pendingFSEvent struct {
	path  string
	timer *time.Timer
}

// fsDebounceEvents owns the in-process debounce state machine. Keeping the
// fsnotify channels and Add boundary as arguments lets tests drive the
// contained timer behavior with synthetic channels while fsDebounceLoop keeps
// the real OS watcher integration.
func fsDebounceEvents(
	events <-chan fsnotify.Event,
	errors <-chan error,
	addWatch func(string) error,
	projectsDir string,
	outCh chan<- fsFileChangeMsg,
	closeCh <-chan struct{},
) {
	timers := make(map[string]*pendingFSEvent)
	timerFired := make(chan *pendingFSEvent)
	known := make(map[string]bool) // tracks files we've already seen

	defer func() {
		for _, pending := range timers {
			pending.timer.Stop()
		}
	}()

	for {
		select {
		case <-closeCh:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			path := event.Name

			// Auto-watch new project subdirectories created under projectsDir.
			// In single-project mode this catches the project dir being created
			// after startup; in global mode it watches all new projects.
			if event.Op&fsnotify.Create != 0 && filepath.Dir(path) == projectsDir {
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					if addWatch != nil {
						_ = addWatch(path)
					}
					continue // directory event, not a .jsonl file
				}
			}

			// Only care about .jsonl files
			if !strings.HasSuffix(path, ".jsonl") {
				continue
			}

			// Verify depth: parent must be a direct child of projectsDir
			if filepath.Dir(filepath.Dir(path)) != projectsDir {
				continue
			}

			removed := event.Op&fsnotify.Remove != 0 || event.Op&fsnotify.Rename != 0
			isCreate := event.Op&fsnotify.Create != 0

			if removed {
				if pending, ok := timers[path]; ok {
					pending.timer.Stop()
					delete(timers, path)
				}
				delete(known, path)
				select {
				case outCh <- fsFileChangeMsg{FilePath: path, Removed: true}:
				case <-closeCh:
					return
				}
				continue
			}

			if isCreate && !known[path] {
				// New file — send immediately
				known[path] = true
				if pending, ok := timers[path]; ok {
					pending.timer.Stop()
					delete(timers, path)
				}
				select {
				case outCh <- fsFileChangeMsg{FilePath: path, Removed: false}:
				case <-closeCh:
					return
				}
				continue
			}

			// Write to existing file — debounce
			known[path] = true
			if pending, ok := timers[path]; ok {
				pending.timer.Stop()
			}
			pending := &pendingFSEvent{path: path}
			pending.timer = time.AfterFunc(fsDebounceDelay, func() {
				select {
				case timerFired <- pending:
				case <-closeCh:
				}
			})
			timers[path] = pending

		case pending := <-timerFired:
			// A stopped timer may already have queued its callback. Only the
			// latest generation for this path is allowed to emit.
			if timers[pending.path] != pending {
				continue
			}
			delete(timers, pending.path)
			select {
			case outCh <- fsFileChangeMsg{FilePath: pending.path, Removed: false}:
			case <-closeCh:
				return
			}

		case _, ok := <-errors:
			// Ignore errors, keep listening
			if !ok {
				errors = nil
			}
		}
	}
}

func waitForFSEvent(ch <-chan fsFileChangeMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *watchModel) continueListenFSEvents() tea.Cmd {
	if m.fsChan == nil {
		return nil
	}
	return waitForFSEvent(m.fsChan)
}

// handleFSChange processes a single file change event from fsnotify.
func (m *watchModel) handleFSChange(filePath string, removed bool) {
	convID := strings.TrimSuffix(filepath.Base(filePath), ".jsonl")
	if len(convID) != 36 {
		return
	}

	if removed {
		_ = db.DeleteConvIndex(convID)
		_ = db.DeleteConvBranchHistory(convID)
		m.removeEntry(convID)
		m.applySearchFilter()
		return
	}

	entry := ScanAndUpsertFile(filePath)
	if entry == nil {
		return
	}

	m.upsertEntry(*entry)
	m.sortInPlace(m.entries)
	m.applySearchFilter()
}

func (m *watchModel) shouldAcceptFSEvent(filePath string) bool {
	if m.global {
		return true
	}
	return filepath.Dir(filePath) == m.claudeProjectDir
}

// closeWatcher cleans up the fsnotify watcher and debounce goroutine.
func (m *watchModel) closeWatcher() {
	if m.fsCloseCh != nil {
		close(m.fsCloseCh)
		m.fsCloseCh = nil
	}
	if m.watcher != nil {
		_ = m.watcher.Close()
		m.watcher = nil
	}
}

// --- Semantic search ---

// semanticPreCheck tests Ollama connectivity and counts unindexed conversations.
func (m *watchModel) semanticPreCheck() tea.Cmd {
	entries := make([]SessionEntry, len(m.entries))
	copy(entries, m.entries)
	return func() tea.Msg {
		client := NewOllamaClient("", "")
		_, err := client.EmbedOne("test")
		if err != nil {
			return semanticCheckMsg{Err: err}
		}

		embeddedConvs, err := db.ListEmbeddedConvIDs()
		if err != nil {
			return semanticCheckMsg{Err: err}
		}

		unindexed := 0
		for _, e := range entries {
			embeddedAt, exists := embeddedConvs[e.SessionID]
			if !exists || e.FileMtime > embeddedAt.Unix() {
				unindexed++
			}
		}

		return semanticCheckMsg{
			Unindexed: unindexed,
			Total:     len(entries),
		}
	}
}

// semanticStartIndex begins background indexing of unindexed conversations.
func (m *watchModel) semanticStartIndex() tea.Cmd {
	entries := make([]SessionEntry, len(m.entries))
	copy(entries, m.entries)

	ch := make(chan tea.Msg, 1)
	cancelCh := make(chan struct{})
	m.semanticIndexChan = ch
	m.semanticCancelCh = cancelCh
	m.semanticIndexing = true
	m.semanticIndexPrompt = false
	m.semanticIndexDone = 0
	m.semanticIndexTotal = m.semanticUnindexedCount // initial estimate from precheck
	m.semanticIndexErrors = 0

	go func() {
		defer close(ch)
		client := NewOllamaClient("", "")

		embeddedConvs, err := db.ListEmbeddedConvIDs()
		if err != nil {
			ch <- semanticIndexDoneMsg{Errors: 1}
			return
		}

		var toIndex []SessionEntry
		for _, e := range entries {
			embeddedAt, exists := embeddedConvs[e.SessionID]
			if !exists || e.FileMtime > embeddedAt.Unix() {
				toIndex = append(toIndex, e)
			}
		}

		total := len(toIndex)
		done := 0
		errors := 0

		for _, entry := range toIndex {
			select {
			case <-cancelCh:
				ch <- semanticIndexDoneMsg{Indexed: done, Errors: errors}
				return
			default:
			}
			_, err := IndexConversation(entry, client)
			if err != nil {
				errors++
			} else {
				done++
			}
			ch <- semanticIndexProgressMsg{Done: done + errors, Total: total, Errors: errors}
		}
		ch <- semanticIndexDoneMsg{Indexed: done, Errors: errors}
	}()

	return waitForSemanticIndex(ch)
}

// semanticRunSearch embeds the query and searches stored embeddings.
func (m *watchModel) semanticRunSearch(query string) tea.Cmd {
	entries := make([]SessionEntry, len(m.entries))
	copy(entries, m.entries)
	return func() tea.Msg {
		client := NewOllamaClient("", "")

		// Check for model mismatch
		if models, err := db.ListEmbeddingModels(); err == nil && len(models) > 0 {
			for _, mdl := range models {
				if mdl != client.Model {
					return semanticSearchResultMsg{
						Query: query,
						Err:   fmt.Errorf("index built with %q, but searching with %q — run 'tclaude conv index-embeddings --reindex'", models[0], client.Model),
					}
				}
			}
		}

		queryEmbedding, err := client.EmbedOne(query)
		if err != nil {
			return semanticSearchResultMsg{Query: query, Err: err}
		}
		results, err := SearchEmbeddings(queryEmbedding, entries, 0)
		return semanticSearchResultMsg{Results: results, Query: query, Err: err}
	}
}

// clearSemanticMode resets all semantic search state and restores normal listing.
func (m *watchModel) clearSemanticMode() {
	m.semanticChecking = false
	m.semanticFocused = false
	m.semanticInput.Blur()
	m.semanticInput.Reset()
	m.semanticQuery = ""
	m.semanticMode = false
	m.semanticResults = nil
	m.semanticScores = nil
	m.semanticError = ""
	m.semanticIndexing = false
	m.semanticIndexPrompt = false
	m.applySearchFilter()
}

func waitForSemanticIndex(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func semanticProgressBar(done, total int) string {
	width := 20
	filled := 0
	if total > 0 {
		filled = done * width / total
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// --- In-memory entry management ---

func (m *watchModel) removeEntry(convID string) {
	for i, e := range m.entries {
		if e.SessionID == convID {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return
		}
	}
}

func (m *watchModel) upsertEntry(entry SessionEntry) {
	for i, e := range m.entries {
		if e.SessionID == entry.SessionID {
			m.entries[i] = entry
			return
		}
	}
	m.entries = append(m.entries, entry)
}

// --- Search filter ---

// updateSemanticInputHeight auto-grows the textarea from 1 to 3 lines based on content.
// Beyond 3 lines, the textarea's internal viewport handles scrolling.
func (m *watchModel) updateSemanticInputHeight() {
	lines := min(max(m.semanticInput.LineCount(), 1), 3)
	m.semanticInput.SetHeight(lines)
}

func (m *watchModel) applySearchFilter() {
	if m.semanticMode {
		return
	}
	searchVal := m.searchInput.Value()
	query := strings.ToLower(searchVal)

	// Compose three independent filters:
	//   1. Hide archived entries (conv_index.archived_at set) unless
	//      m.showArchived is on. Reincarnated old convs persist on disk for
	//      history, but listing them by default just clutters the table —
	//      toggle with `x`.
	//   2. The text-search filter (matchesSearch).
	//   3. The group filter (matchesGroupFilter) — when set, only convs
	//      whose membership list includes that group pass.
	// When no filter is active we can short-circuit to a slice
	// reference, avoiding the allocation.
	if query == "" && m.showArchived && m.groupFilter == "" {
		m.filtered = m.entries
		return
	}

	m.filtered = make([]SessionEntry, 0, len(m.entries))
	for _, e := range m.entries {
		if !m.showArchived && e.IsArchived() {
			continue
		}
		if query != "" && !m.matchesSearch(e, query) {
			continue
		}
		if m.groupFilter != "" && !m.matchesGroupFilter(e) {
			continue
		}
		m.filtered = append(m.filtered, e)
	}

	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
	m.viewportOffset = 0
}

// matchesGroupFilter returns true when the active groupFilter is empty
// (no filter) OR e is a member of a group named by m.groupFilter.
// Comparison is case-insensitive — group names are case-sensitive at
// the DB layer but most users won't remember the exact casing when
// typing into the picker.
func (m *watchModel) matchesGroupFilter(e SessionEntry) bool {
	if m.groupFilter == "" {
		return true
	}
	target := strings.ToLower(m.groupFilter)
	for _, gname := range m.groupsByConv[e.SessionID] {
		if strings.ToLower(gname) == target {
			return true
		}
	}
	return false
}

// rebuildSemanticFiltered reconstructs the filtered list from entries matching
// saved semantic scores, sorted by similarity descending. The archived-filter
// toggle and the group filter both apply here too — semantic results for
// archived convs stay hidden until the user opts in, and the group filter
// narrows in the same way it does for plain text search.
func (m *watchModel) rebuildSemanticFiltered() {
	m.filtered = m.filtered[:0]
	for _, e := range m.entries {
		if _, ok := m.semanticScores[e.SessionID]; !ok {
			continue
		}
		if !m.showArchived && e.IsArchived() {
			continue
		}
		if m.groupFilter != "" && !m.matchesGroupFilter(e) {
			continue
		}
		m.filtered = append(m.filtered, e)
	}
	sort.Slice(m.filtered, func(i, j int) bool {
		return m.semanticScores[m.filtered[i].SessionID] > m.semanticScores[m.filtered[j].SessionID]
	})
}

// matchesSearch returns true when query (already lower-cased) is a
// substring of any of: the display title, first prompt, project
// path, git branch, session id, or any group name the conv belongs
// to. Group membership is looked up from the model's groupsByConv
// snapshot — same source the GROUPS column renders from, so what's
// visible in the column is what's searchable.
func (m *watchModel) matchesSearch(e SessionEntry, query string) bool {
	if strings.Contains(strings.ToLower(e.DisplayTitle()), query) ||
		strings.Contains(strings.ToLower(e.FirstPrompt), query) ||
		strings.Contains(strings.ToLower(e.ProjectPath), query) ||
		strings.Contains(strings.ToLower(e.GitBranch), query) ||
		strings.Contains(strings.ToLower(e.SessionID), query) {
		return true
	}
	for _, gname := range m.groupsByConv[e.SessionID] {
		if strings.Contains(strings.ToLower(gname), query) {
			return true
		}
	}
	return false
}

// --- Session state ---

func (m *watchModel) refreshActiveSessions() {
	states, _ := session.ListSessionStates()
	m.activeSessions = make(map[string]*session.SessionState)
	for _, state := range states {
		session.RefreshSessionStatus(state)
		if state.Status != session.StatusExited && state.ConvID != "" {
			m.activeSessions[state.ConvID] = state
		}
	}
}

// --- Navigation ---

func (m *watchModel) ensureCursorVisible() {
	if m.cursor < m.viewportOffset {
		m.viewportOffset = m.cursor
	}
	if m.cursor >= m.viewportOffset+m.viewportHeight {
		m.viewportOffset = m.cursor - m.viewportHeight + 1
	}
}

// --- Actions ---

func (m *watchModel) triggerDelete() {
	if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
		conv := m.filtered[m.cursor]
		if _, hasSession := m.activeSessions[conv.SessionID]; hasSession {
			m.confirmMode = watchConfirmDeleteWithSession
		} else {
			m.confirmMode = watchConfirmDelete
		}
		m.statusMsg = ""
	}
}

func (m *watchModel) deleteConversation(conv *SessionEntry) error {
	var projectPath string
	if conv.FullPath != "" {
		projectPath = filepath.Dir(conv.FullPath)
	} else if m.global {
		projectPath = GetClaudeProjectPath(conv.ProjectPath)
	} else {
		projectPath = GetClaudeProjectPath(m.projectPath)
	}

	convFile := projectPath + "/" + conv.SessionID + ".jsonl"
	// Stat first so we don't surface remove-errors for files that
	// were never there (e.g. a stale ProjectPath whose directory
	// happens to exist but doesn't contain this conv). The bare
	// os.Remove path raised "permission denied" / "read-only
	// filesystem" on some sandboxed setups even when the file was
	// absent — checking existence avoids that path entirely.
	if _, err := os.Stat(convFile); err == nil {
		if err := os.Remove(convFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	convDir := projectPath + "/" + conv.SessionID
	if info, err := os.Stat(convDir); err == nil && info.IsDir() {
		if err := os.RemoveAll(convDir); err != nil {
			return fmt.Errorf("failed to delete directory: %w", err)
		}
	}

	_ = db.DeleteConvIndex(conv.SessionID)
	_ = db.DeleteConvBranchHistory(conv.SessionID)
	m.removeEntry(conv.SessionID)

	// Surgically drop the entry from legacy sessions-index.json for
	// external tooling. No-op if the file doesn't exist.
	if err := RemoveSessionsIndexEntry(projectPath, conv.SessionID); err != nil {
		slog.Warn("watch: update legacy sessions-index.json", "project", projectPath, "error", err)
	}

	return nil
}

func (m *watchModel) stopSession(state *session.SessionState) error {
	if session.IsTmuxSessionAlive(state.TmuxSession) {
		cmd := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(state.TmuxSession))
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to kill tmux session: %w", err)
		}
	}
	if err := session.DeleteSessionState(state.ID); err != nil {
		return fmt.Errorf("failed to delete session state: %w", err)
	}
	return nil
}

// --- Column definitions ---

// Toggleable column keys — the stable identifiers persisted in
// config.ConvWatchConfig.Columns and shown in the `c` selector overlay.
const (
	colKeyHarness  = "harness"
	colKeyProject  = "project"
	colKeySize     = "size"
	colKeyModified = "modified"
	colKeyGroups   = "groups"
)

// colDef fully describes one watch-view column: its display order, current
// visibility, table header/layout, and how to extract its cell text for a
// row. A non-empty key marks the column as user-toggleable (and is its
// persistence/selector identifier); an empty key is a structural column
// (status mark, ID, title, semantic score) that the user cannot hide.
//
// Both columns() (the headers) and the View row builder iterate the SAME
// orderedColumns() slice, so a column can never appear in the header but be
// missing from the cells (or vice versa) — the divergence that an earlier
// twin-switch layout invited.
type colDef struct {
	key     string
	visible bool
	col     table.Column
	cell    func(m *watchModel, e SessionEntry) string
}

// orderedColumns returns every column in display order with its resolved
// visibility, header spec, and cell extractor. Toggleable columns resolve
// their visibility through colVisible (explicit override else auto-default);
// mode-gated columns (PROJECT in global mode, SCORE in semantic mode) are
// only emitted when that mode is active.
func (m *watchModel) orderedColumns() []colDef {
	nonClaude := m.hasNonClaudeHarness()
	grouped := m.hasGroups()

	defs := []colDef{
		{visible: true, col: table.Column{Header: "", Width: 2},
			cell: func(m *watchModel, e SessionEntry) string { return m.sessionMark(e) }},
		{visible: true, col: table.Column{Header: "ID", Width: 10, SortKey: "id"},
			cell: func(m *watchModel, e SessionEntry) string { return e.SessionID[:8] }},
		{key: colKeyHarness, visible: m.colVisible(colKeyHarness, nonClaude),
			col:  table.Column{Header: "HARNESS", Width: 8},
			cell: func(m *watchModel, e SessionEntry) string { return harnessBadge(e.Harness) }},
	}

	if m.global {
		defs = append(defs, colDef{key: colKeyProject, visible: m.colVisible(colKeyProject, true),
			col:  table.Column{Header: "PROJECT", MinWidth: 20, Weight: 0.4, Truncate: true, TruncateMode: table.TruncateStart, SortKey: "project"},
			cell: func(m *watchModel, e SessionEntry) string { return e.ProjectPath }})
	}

	titleCol := table.Column{Header: "TITLE/PROMPT", MinWidth: 30, Truncate: true, SortKey: "title"}
	if m.global {
		titleCol.Weight = 0.6 // share the flexible width with PROJECT
	}
	defs = append(defs,
		colDef{visible: true, col: titleCol,
			cell: func(m *watchModel, e SessionEntry) string {
				// Canonical "[title]: prompt" rendering — shared with conv ls
				// and the web dashboard via convindex.FormatConvTitle — with
				// the agent pending-name fallback (convDisplayTitle).
				return convDisplayTitle(e, m.pendingByConv)
			}},
		colDef{key: colKeySize, visible: m.colVisible(colKeySize, true),
			col:  table.Column{Header: "SIZE", Width: 8, SortKey: "size"},
			cell: func(m *watchModel, e SessionEntry) string { return formatFileSize(e.FileSize) }},
		colDef{key: colKeyModified, visible: m.colVisible(colKeyModified, true),
			col:  table.Column{Header: "MODIFIED", Width: 16, SortKey: "modified"},
			cell: func(m *watchModel, e SessionEntry) string { return formatDate(e.Modified) }},
		colDef{key: colKeyGroups, visible: m.colVisible(colKeyGroups, grouped),
			col:  table.Column{Header: "GROUPS", MinWidth: 6, Weight: 0.3, Truncate: true, SortKey: "groups"},
			cell: func(m *watchModel, e SessionEntry) string { return strings.Join(m.groupsByConv[e.SessionID], ",") }},
	)

	if m.semanticMode {
		defs = append(defs, colDef{visible: true, col: table.Column{Header: "SCORE", Width: 10, Align: table.AlignRight},
			cell: func(m *watchModel, e SessionEntry) string {
				if s, ok := m.semanticScores[e.SessionID]; ok {
					return fmt.Sprintf("%.4f", s)
				}
				return ""
			}})
	}
	return defs
}

// columns returns the visible table headers, derived from orderedColumns.
func (m *watchModel) columns() []table.Column {
	defs := m.orderedColumns()
	cols := make([]table.Column, 0, len(defs))
	for _, d := range defs {
		if d.visible {
			cols = append(cols, d.col)
		}
	}
	return cols
}

// colVisible resolves a toggleable column's visibility: an explicit user
// override (from the selector / config) wins, otherwise the smart auto value.
func (m *watchModel) colVisible(key string, auto bool) bool {
	if v, ok := m.colOverrides[key]; ok {
		return v
	}
	return auto
}

// hasNonClaudeHarness reports whether any loaded conv belongs to a harness
// other than Claude Code — the auto-default that surfaces the HARNESS column.
func (m *watchModel) hasNonClaudeHarness() bool {
	for _, e := range m.entries {
		if e.Harness != "" && e.Harness != harness.DefaultName {
			return true
		}
	}
	return false
}

// sessionMark returns the leading status glyph for a conv's row.
func (m *watchModel) sessionMark(e SessionEntry) string {
	state, ok := m.activeSessions[e.SessionID]
	if !ok {
		return "  "
	}
	switch {
	case state.TmuxSession == "" || !session.IsTmuxSessionAlive(state.TmuxSession):
		return " ◉"
	case state.Attached > 0:
		return "⚡"
	default:
		return " ▷"
	}
}

// --- Column selector ---

// toggleCol is one row in the column-selector overlay.
type toggleCol struct {
	key     string
	label   string
	visible bool
}

// toggleableColumns lists the user-toggleable columns in display order with
// their current effective visibility — the rows the `c` overlay renders.
// Mode-gated columns (PROJECT) are only listed when applicable.
func (m *watchModel) toggleableColumns() []toggleCol {
	var out []toggleCol
	for _, d := range m.orderedColumns() {
		if d.key == "" {
			continue // structural column — not user-toggleable
		}
		out = append(out, toggleCol{key: d.key, label: d.col.Header, visible: d.visible})
	}
	return out
}

// setColumnOverride records an explicit show/hide for a column and persists it.
func (m *watchModel) setColumnOverride(key string, visible bool) {
	if m.colOverrides == nil {
		m.colOverrides = map[string]bool{}
	}
	m.colOverrides[key] = visible
	m.persistColumnPrefs()
}

// resetColumnOverrides drops every explicit override, returning all columns to
// their smart auto-defaults, and persists the cleared state.
func (m *watchModel) resetColumnOverrides() {
	m.colOverrides = map[string]bool{}
	m.persistColumnPrefs()
}

// persistColumnPrefs writes the current overrides to ~/.tclaude/config.json.
// Best-effort: a failed write only costs the persistence, not the in-session
// state. Uses config.Update so a concurrent writer's change isn't dropped.
func (m *watchModel) persistColumnPrefs() {
	overrides := make(map[string]bool, len(m.colOverrides))
	for k, v := range m.colOverrides {
		overrides[k] = v
	}
	if _, err := config.Update(func(cfg *config.Config, _ error) error {
		if cfg.ConvWatch == nil {
			cfg.ConvWatch = &config.ConvWatchConfig{}
		}
		if len(overrides) == 0 {
			cfg.ConvWatch.Columns = nil
		} else {
			cfg.ConvWatch.Columns = overrides
		}
		return nil
	}); err != nil {
		slog.Warn("watch: failed to persist column preferences", "error", err)
		m.statusMsg = "Failed to save column prefs: " + err.Error()
	}
}

// renderColumnSelector renders the `c` overlay: a checklist of toggleable
// columns with the cursor row highlighted.
func (m *watchModel) renderColumnSelector() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(wSearchStyle.Render("  Columns"))
	b.WriteString("\n\n")

	toggleable := m.toggleableColumns()
	if m.columnCursor >= len(toggleable) {
		m.columnCursor = 0
	}
	for i, c := range toggleable {
		box := "[ ]"
		if c.visible {
			box = "[x]"
		}
		row := box + " " + c.label
		if i == m.columnCursor {
			b.WriteString("  " + wSelectedStyle.Render("▸ "+row) + "\n")
		} else {
			b.WriteString("    " + row + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(wHelpStyle.Render("  ↑/↓ move • space toggle • r reset to defaults • enter/esc/c close"))
	b.WriteString("\n")
	return b.String()
}

// --- Help view ---

func (m *watchModel) renderHelpView() string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(wSearchStyle.Render("  Conversation Watch - Keyboard Shortcuts"))
	b.WriteString("\n\n")

	b.WriteString(wHeaderStyle.Render("  Navigation"))
	b.WriteString("\n")
	b.WriteString("    ↑/k       Move cursor up\n")
	b.WriteString("    ↓/j       Move cursor down\n")
	b.WriteString("    PgUp/^B   Page up\n")
	b.WriteString("    PgDn/^F   Page down\n")
	b.WriteString("    g/Home    Go to first\n")
	b.WriteString("    G/End     Go to last\n")
	b.WriteString("    enter     Create session or attach to existing\n")
	b.WriteString("    q/esc     Quit watch mode\n")
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Search"))
	b.WriteString("\n")
	b.WriteString("    /         Start search\n")
	b.WriteString("    f         Filter by group name (composes with /)\n")
	b.WriteString("    esc       Clear search/group / exit input mode\n")
	b.WriteString("    ^U        Clear search input\n")
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Actions"))
	b.WriteString("\n")
	b.WriteString("    W         Create git worktree with this conversation\n")
	b.WriteString("    del/^D    Delete conversation (with confirmation)\n")
	b.WriteString("              If has session: y=delete+stop, s=stop only, n=cancel\n")
	b.WriteString("    x         Toggle archived convs (default: hidden)\n")
	b.WriteString("    c         Choose visible columns (persisted)\n")
	b.WriteString("    r         Refresh conversation list\n")
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Sorting"))
	b.WriteString("\n")
	for _, line := range table.SortableColumnsHelp(m.columns()) {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Semantic Search"))
	b.WriteString("\n")
	b.WriteString("    s         Start semantic search (requires Ollama)\n")
	b.WriteString("    enter     Submit search query\n")
	b.WriteString("    shift+enter Insert newline (multiline query)\n")
	b.WriteString("    esc       Exit semantic results\n")
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Indicators"))
	b.WriteString("\n")
	b.WriteString("    ⚡        Tmux session with attached clients\n")
	b.WriteString("    ▷         Tmux session, detached (can attach)\n")
	b.WriteString("    ◉         Non-tmux session (in-terminal, can't attach)\n")
	b.WriteString("\n")

	b.WriteString(wHelpStyle.Render("  Press any key to close"))
	b.WriteString("\n")
	return b.String()
}

// --- Public API ---

// WatchResult holds the result of the watch mode selection
type WatchResult struct {
	Conv           *SessionEntry
	ShouldCreate   bool   // true = create new session, false = attach to existing
	ForceAttach    bool   // Detach other clients when attaching
	FocusOnly      bool   // Just focus the window, don't attach
	TmuxSession    string // Tmux session to focus (when FocusOnly is true)
	FocusSessionID string // Session ID for focus (needed for WSL window title search)
	CreateWorktree bool   // true = create worktree for selected conv
	WorktreeBranch string // Branch name for worktree
}

// ConvWatchState holds state that persists between attach cycles
type ConvWatchState struct {
	SearchInput    string
	Cursor         int
	ViewportOffset int
	Sort           table.SortState
	SemanticMode   bool
	SemanticQuery  string
	SemanticScores map[string]float32
}

// RunConvWatch runs the interactive watch mode and returns the result
func RunConvWatch(global bool, since, before string, state ConvWatchState) (WatchResult, ConvWatchState, error) {
	// Apply the configured TUI color scheme before building the program so the
	// styles are set once, up front (Load is nil-safe on error → default).
	cfg, _ := config.Load()
	applyTUIColorScheme(cfg.TUIColorScheme())

	m := initialWatchModel(global, since, before)

	// Restore previous state
	m.searchInput.SetValue(state.SearchInput)
	m.cursor = state.Cursor
	m.viewportOffset = state.ViewportOffset
	m.sort = state.Sort
	m.semanticMode = state.SemanticMode
	m.semanticQuery = state.SemanticQuery
	m.semanticScores = state.SemanticScores

	// Initial full load from disk+DB (populates cache for new/changed files)
	m.fullReloadConversations()

	// If returning in semantic mode, rebuild filtered list from saved scores
	if m.semanticMode && len(m.semanticScores) > 0 {
		m.rebuildSemanticFiltered()
	}

	// Snapshot the conv_index timestamp so the tick doesn't immediately reload
	if m.global {
		m.lastConvIndexUpdatedAt, _ = db.MaxConvIndexUpdatedAt()
	} else {
		m.lastConvIndexUpdatedAt, _ = db.MaxConvIndexUpdatedAtForProject(m.claudeProjectDir)
	}

	// Ensure cursor is still valid after loading (list may have changed)
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m.ensureCursorVisible()

	p := tea.NewProgram(&m)
	finalModel, err := p.Run()
	if err != nil {
		return WatchResult{}, state, err
	}

	fm := finalModel.(*watchModel)
	fm.closeWatcher()

	newState := ConvWatchState{
		SearchInput:    fm.searchInput.Value(),
		Cursor:         fm.cursor,
		ViewportOffset: fm.viewportOffset,
		Sort:           fm.sort,
		SemanticMode:   fm.semanticMode,
		SemanticQuery:  fm.semanticQuery,
		SemanticScores: fm.semanticScores,
	}
	return WatchResult{
		Conv:           fm.selectedConv,
		ShouldCreate:   fm.shouldCreate,
		ForceAttach:    fm.forceAttach,
		FocusOnly:      fm.focusOnly,
		TmuxSession:    fm.focusTmux,
		FocusSessionID: fm.focusSessionID,
		CreateWorktree: fm.createWorktree,
		WorktreeBranch: fm.worktreeBranch,
	}, newState, nil
}

// RunConvWatchMode runs the interactive watch mode with create/attach loop
func RunConvWatchMode(global bool, since, before string) error {
	var watchState ConvWatchState
	for {
		result, newState, err := RunConvWatch(global, since, before, watchState)
		watchState = newState
		if err != nil {
			return err
		}

		if result.Conv == nil && !result.FocusOnly && !result.CreateWorktree {
			return nil
		}

		if result.FocusOnly {
			_ = os.Setenv("TCLAUDE_SESSION_ID", result.FocusSessionID)
			session.TryFocusAttachedSession(result.TmuxSession)
			continue
		}

		if result.CreateWorktree {
			if err := createWorktreeForConv(result.Conv, result.WorktreeBranch); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating worktree: %v\n", err)
			}
			return nil
		}

		if result.ShouldCreate {
			if err := createSessionForConv(result.Conv); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
				continue
			}
		} else {
			sessState := findSessionForConv(result.Conv.SessionID)
			if sessState == nil {
				fmt.Fprintf(os.Stderr, "Session not found, creating new...\n")
				if err := createSessionForConv(result.Conv); err != nil {
					fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
					continue
				}
			} else {
				if result.ForceAttach {
					fmt.Printf("Attaching to %s (detaching others)... (Ctrl+B D to detach)\n", sessState.ID)
				} else {
					fmt.Printf("Attaching to %s... (Ctrl+B D to detach)\n", sessState.ID)
				}
				if err := session.AttachToSession(sessState.ID, sessState.TmuxSession, result.ForceAttach); err != nil {
					continue
				}
			}
		}
	}
}

// --- Sorting (package-level helpers) ---

func sortConvEntriesByKey(entries []SessionEntry, key string, dir table.SortDirection, groupsByConv map[string][]string) {
	if key == "" || len(entries) < 2 {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		var less bool
		switch key {
		case "id":
			less = entries[i].SessionID < entries[j].SessionID
		case "project":
			less = entries[i].ProjectPath < entries[j].ProjectPath
		case "title":
			less = strings.ToLower(entries[i].DisplayTitle()) < strings.ToLower(entries[j].DisplayTitle())
		case "size":
			less = entries[i].FileSize < entries[j].FileSize
		case "modified":
			less = entries[i].Modified < entries[j].Modified
		case "groups":
			// Sort by the same comma-joined string the cell renders, so
			// what the user sees lines up with the order. Convs with no
			// groups get the empty string and naturally cluster at one
			// end of the list (top in asc, bottom in desc).
			less = strings.ToLower(strings.Join(groupsByConv[entries[i].SessionID], ",")) <
				strings.ToLower(strings.Join(groupsByConv[entries[j].SessionID], ","))
		default:
			return false
		}
		if dir == table.SortDesc {
			return !less
		}
		return less
	})
}

func sortEntriesByModifiedDesc(entries []SessionEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Modified > entries[j].Modified
	})
}

// --- Session/worktree helpers ---

// resumeLaunchCmd resolves a conversation's recorded harness and builds the
// in-tmux launch command that resumes it, mirroring the spawn path
// (session.runNew): the harness's Spawner turns a SpawnSpec{ResumeID,…} into
// the concrete launch form — `claude --resume <id>` for Claude Code, the
// `codex resume <id>` SUBCOMMAND for Codex — so watch-mode resume is no longer
// hardcoded to Claude Code (JOH-217). EnvExports carries TCLAUDE_SESSION_ID
// alongside the inherited environment, exactly like the spawn path, and the
// passthrough args ride SpawnSpec.ExtraArgs so the Spawner shell-quotes them.
//
// ResolveSpawnable also gates on resume support: a harness with no Spawner
// can't be relaunched, so an unknown/unspawnable tag fails with a clear error
// here rather than silently spawning `claude --resume` against a foreign conv
// id. An empty tag resolves to the default harness (Claude Code).
func resumeLaunchCmd(harnessName, sessionID, convID string, extraArgs []string) (string, string, *harness.Harness, error) {
	h, err := harness.ResolveSpawnable(harnessName)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot resume conversation %s: %w", convID, err)
	}
	resumeEnv := map[string]string{"TCLAUDE_SESSION_ID": sessionID}
	var shellEnvironment map[string]string
	effectiveSandbox, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil {
		return "", "", nil, fmt.Errorf("load effective sandbox snapshot for conversation %s: %w", convID, err)
	}
	var readDirs, writeDirs, denyDirs []string
	// Agentd resolves the currently selected sandbox profile before invoking the
	// watch/session renderer and persists that exact snapshot. This boundary
	// consumes the current persisted decision; it does not merge stale ordinary
	// exclusions from an earlier launch. Protected break-glass clamping remains
	// an agentd lifecycle responsibility.
	var breakGlassReadDirs, breakGlassWriteDirs []string
	var breakGlassGrants []sandboxpolicy.BreakGlassGrant
	var launchGrants []sandboxpolicy.FilesystemGrant
	var agentDirPaths []string
	networkAccess := sandboxpolicy.NetworkAccessInherit
	if effectiveSandbox != nil {
		validated, err := sandboxpolicy.RevalidateSnapshot(*effectiveSandbox)
		if err != nil {
			return "", "", nil, fmt.Errorf("sandbox_profile_changed: %w", err)
		}
		shellEnvironment = make(map[string]string, len(validated.Effective.Environment))
		for _, entry := range validated.Effective.Environment {
			resumeEnv[entry.Name] = entry.Value
			shellEnvironment[entry.Name] = entry.Value
		}
		for _, name := range validated.Effective.AgentDirectories {
			if path, ok := shellEnvironment[name]; ok {
				agentDirPaths = append(agentDirPaths, path)
			}
		}
		networkAccess = validated.Effective.NetworkAccess
		launchFilesystem, err := sandboxpolicy.FilesystemForLaunch(validated.Effective)
		if err != nil {
			return "", "", nil, fmt.Errorf("sandbox_profile_changed: %w", err)
		}
		launchGrants = launchFilesystem
		for _, grant := range launchFilesystem {
			switch grant.Access {
			case sandboxpolicy.AccessWrite:
				writeDirs = append(writeDirs, grant.Path)
			case sandboxpolicy.AccessRead:
				readDirs = append(readDirs, grant.Path)
			case sandboxpolicy.AccessDeny:
				denyDirs = append(denyDirs, grant.Path)
			}
		}
		breakGlassGrants = validated.Effective.BreakGlassFilesystem
		if len(breakGlassGrants) > 0 {
			breakGlass, err := sandboxpolicy.BreakGlassForLaunch(validated.Effective)
			if err != nil {
				return "", "", nil, fmt.Errorf("sandbox_profile_changed: %w", err)
			}
			for _, grant := range breakGlass {
				if grant.Access == sandboxpolicy.AccessWrite {
					breakGlassWriteDirs = append(breakGlassWriteDirs, grant.Path)
				} else {
					breakGlassReadDirs = append(breakGlassReadDirs, grant.Path)
				}
			}
		}
	}
	// Mirror the spawn path: keep Claude Code's "Resume from summary" chooser
	// from interrupting this resume. No-op for non-Claude harnesses. See
	// session.ApplyClaudeResumeEnv.
	session.ApplyClaudeResumeEnv(h, resumeEnv)
	// Mirror the spawn path's auto-memory posture. Reading it back off the
	// session row (rather than re-defaulting) keeps a session that explicitly
	// opted INTO Claude Code's auto memory from silently losing it on resume;
	// a conv with no recorded posture reads false, which is tclaude's
	// recommended default anyway. No-op for non-Claude harnesses.
	autoMemory, err := db.AutoMemoryForConv(convID)
	if err != nil {
		return "", "", nil, fmt.Errorf("load auto-memory posture for conversation %s: %w", convID, err)
	}
	session.ApplyAutoMemoryEnv(h, autoMemory, resumeEnv)
	sandboxMode, resumeCwd := resumeSandboxState(convID)
	approvalPolicy, autoReview, err := resumeApprovalState(h, convID)
	if err != nil {
		return "", "", nil, err
	}
	if err := harness.ValidateSandboxReopenUnderDeny(h.Name, sandboxMode, launchGrants); err != nil {
		return "", "", nil, err
	}
	if len(breakGlassGrants) > 0 {
		if err := harness.ValidateSandboxBreakGlassWithReopenUnderDeny(h.Name, sandboxMode, breakGlassGrants, launchGrants); err != nil {
			return "", "", nil, err
		}
	}
	if h.Name == harness.DefaultName && len(denyDirs) > 0 && sandboxMode != harness.ClaudeSandboxOn {
		return "", "", nil, fmt.Errorf("unsupported_sandbox_profile_filesystem: Claude filesystem deny rules require sandbox %s", harness.ClaudeSandboxOn)
	}
	if networkAccess != sandboxpolicy.NetworkAccessInherit && h.Name != harness.CodexName {
		return "", "", nil, fmt.Errorf("unsupported_sandbox_profile_network: network policies are currently supported only by the Codex managed sandbox")
	}
	if networkAccess != sandboxpolicy.NetworkAccessInherit && sandboxMode != harness.SandboxManagedProfile {
		return "", "", nil, fmt.Errorf("unsupported_sandbox_profile_network: codex network rules require sandbox %s", harness.SandboxManagedProfile)
	}
	if networkAccess != sandboxpolicy.NetworkAccessInherit {
		if err := harness.ValidateCodexAgentNetworkAccess(networkAccess); err != nil {
			return "", "", nil, fmt.Errorf("unsupported_sandbox_profile_network: %w", err)
		}
	}
	// A deny covering the workspace narrows the Git grants the same way the
	// spawn path does: the historical repository container would reopen every
	// sibling repo beneath the deny.
	workspaceDenied := resumeDenyCoversPath(launchGrants, resumeCwd)
	if (h.Name == harness.CodexName && sandboxMode == harness.SandboxManagedProfile) ||
		(h.Name == harness.DefaultName && sandboxMode != harness.ClaudeSandboxOff) {
		gitWriteDirs, err := resumeGitWorktreeWriteDirs(resumeCwd, workspaceDenied)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve sandboxed resume Git grants: %w", err)
		}
		writeDirs = append(gitWriteDirs, writeDirs...)
	}
	// Launch contract: pair explicit read reopens for everything tclaude
	// requires the resumed agent to reach when a deny covers it. Mirrors the
	// spawn path — see session.sandboxLaunchContractReadDirs.
	readDirs = append(readDirs, resumeLaunchContractReadDirs(launchGrants, agentDirPaths, append([]string{resumeCwd}, writeDirs...))...)
	spec := harness.SpawnSpec{
		EnvExports:       clcommon.BuildEnvExports(resumeEnv),
		ShellEnvironment: shellEnvironment,
		ResumeID:         convID,
		ExtraArgs:        extraArgs,
		SandboxMode:      sandboxMode,
		SandboxReadDirs:  readDirs,
		SandboxWriteDirs: writeDirs,
		SandboxDenyDirs:  denyDirs,
		ApprovalPolicy:   approvalPolicy,
		AutoReview:       autoReview,

		SandboxBreakGlassReadDirs:  breakGlassReadDirs,
		SandboxBreakGlassWriteDirs: breakGlassWriteDirs,
	}
	cleanupPath := ""
	var splitCapability *harness.CodexSplitPolicyCapability
	if h.Name == harness.CodexName && sandboxMode == harness.SandboxManagedProfile {
		resumeWriteDirs := writeDirs
		requireSplitPolicy := sandboxpolicy.HasReopenUnderDeny(launchGrants)
		if requireSplitPolicy {
			verified, runtimeErr := harness.VerifyCodexHomeSplitPolicy()
			if runtimeErr != nil {
				return "", "", nil, runtimeErr
			}
			splitCapability = &verified
			if verified.RequiresExecutableReopen {
				readDirs = append(readDirs, verified.ExecutablePath)
			}
		}
		if workspaceDenied {
			// Same invariant as the spawn path: a deny covering the workspace
			// masks what `extends = ":workspace"` grants, and GitWorktreeWriteDirs
			// is empty outside a Git repository. Resume must not strand an agent
			// with no workspace.
			if workspace := canonicalResumeWorkspace(resumeCwd); workspace != "" {
				resumeWriteDirs = appendUniqueResumeDir(resumeWriteDirs, workspace)
			}
		}
		profileName, profilePath, err := harness.EnsureCodexAgentLaunchProfileForRules(harness.CodexSandboxRules{
			ReadDirs:            readDirs,
			WriteDirs:           resumeWriteDirs,
			DenyDirs:            denyDirs,
			BreakGlassReadDirs:  breakGlassReadDirs,
			BreakGlassWriteDirs: breakGlassWriteDirs,
			RequireSplitPolicy:  requireSplitPolicy,
		}, networkAccess, session.GenerateSessionID())
		if err != nil {
			return "", "", nil, fmt.Errorf("prepare managed Codex resume profile: %w", err)
		}
		spec.SandboxMode = ""
		spec.PermissionProfile = profileName
		if splitCapability != nil {
			spec.ExecutablePath = splitCapability.ExecutablePath
		}
		cleanupPath = profilePath
	} else if h.Name == harness.CodexName && len(readDirs)+len(writeDirs)+len(denyDirs) > 0 {
		return "", "", nil, fmt.Errorf("unsupported_sandbox_profile_filesystem: Codex filesystem rules require sandbox %s", harness.SandboxManagedProfile)
	}
	if splitCapability != nil {
		if err := harness.RevalidateCodexHomeSplitPolicyCapability(*splitCapability); err != nil {
			return "", "", nil, fmt.Errorf("revalidate strict-Home Codex resume capability: %w", err)
		}
	}
	cmd := h.Spawn.BuildCommand(spec)
	if cleanupPath != "" {
		cmd = resumeCommandWithFileCleanup(cmd, cleanupPath)
	}
	return cmd, cleanupPath, h, nil
}

// canonicalResumeWorkspace mirrors the spawn path's minimal-workspace grant.
// Symlink-resolved so the emitted rule names the path the sandbox will see.
func canonicalResumeWorkspace(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}

// resumeDenyCoversPath reports whether a STRICT ancestor of path is denied by
// the launch grants. Mirrors session.sandboxDenyCoversPath.
func resumeDenyCoversPath(grants []sandboxpolicy.FilesystemGrant, path string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return false
	}
	for _, grant := range grants {
		if grant.Access != sandboxpolicy.AccessDeny || grant.Path == path {
			continue
		}
		if sandboxpolicy.PathContainsOrEqual(grant.Path, path) {
			return true
		}
	}
	return false
}

// resumeLaunchContractReadDirs mirrors session.sandboxLaunchContractReadDirs on
// the resume path: every directory tclaude requires the agent to reach gets an
// explicit read reopen when a deny covers it, because write access does not
// imply read beneath a denied ancestor on either harness. Agent-owned
// directories arrive here as their already-resolved paths.
//
// Only agent-OWNED directories are eligible: a literal environment value that
// happens to look like a path under a deny must never mint a read reopen, or an
// operator could widen the sandbox through the environment table.
func resumeLaunchContractReadDirs(grants []sandboxpolicy.FilesystemGrant, agentDirs []string, candidates []string) []string {
	if len(grants) == 0 {
		return nil
	}
	all := append(append([]string(nil), candidates...), agentDirs...)
	var out []string
	for _, candidate := range all {
		candidate = filepath.Clean(strings.TrimSpace(candidate))
		if candidate == "." || !filepath.IsAbs(candidate) || !resumeDenyCoversPath(grants, candidate) {
			continue
		}
		out = appendUniqueResumeDir(out, candidate)
	}
	sort.Strings(out)
	return out
}

func appendUniqueResumeDir(dirs []string, dir string) []string {
	for _, existing := range dirs {
		if existing == dir {
			return dirs
		}
	}
	return append(dirs, dir)
}

func resumeSandboxState(convID string) (mode, cwd string) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil || row == nil {
		return "", ""
	}
	return strings.TrimSpace(row.SandboxMode), strings.TrimSpace(row.Cwd)
}

func resumeSandboxMode(convID string) string {
	mode, _ := resumeSandboxState(convID)
	return mode
}

// resumeApprovalState carries the most recently recorded posture onto the
// next session generation. Legacy rows have no posture; pin those to the
// harness's daemon-safe default instead of creating another unknown row.
func resumeApprovalState(h *harness.Harness, convID string) (string, bool, error) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil {
		return "", false, fmt.Errorf("load approval posture for conversation %s: %w", convID, err)
	}
	policy := ""
	autoReview := false
	if row != nil {
		policy = strings.TrimSpace(row.ApprovalPolicy)
		autoReview = row.ApprovalAutoReview
	}
	if policy == "" {
		policy, err = harness.ResolveApprovalPolicy(h, "")
	} else {
		policy, err = harness.ValidateApprovalPolicy(h, policy)
	}
	if err != nil {
		return "", false, fmt.Errorf("invalid recorded approval posture for conversation %s: %w", convID, err)
	}
	autoReview, err = harness.ResolveAutoReview(h, autoReview)
	if err != nil {
		return "", false, fmt.Errorf("invalid recorded auto-review posture for conversation %s: %w", convID, err)
	}
	return policy, autoReview, nil
}

func resumeGitWorktreeWriteDirs(cwd string, strictHome bool) ([]string, error) {
	commonDir, err := harness.GitCommonDir(cwd)
	if err != nil {
		return nil, err
	}
	if commonDir == "" {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dirs := harness.GitWorktreeWriteDirs(cwd, commonDir, home)
	if !strictHome {
		return dirs, nil
	}
	// Strict Home cannot inherit the historical whole-container grant used for
	// direct sibling-worktree creation. Retain the active workspace plus exact
	// Git common/admin descendants only.
	var narrow []string
	if workspace := canonicalResumeWorkspace(cwd); workspace != "" {
		narrow = appendUniqueResumeDir(narrow, workspace)
	}
	commonDir = filepath.Clean(commonDir)
	narrow = appendUniqueResumeDir(narrow, commonDir)
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if sandboxpolicy.PathContainsOrEqual(commonDir, dir) {
			narrow = appendUniqueResumeDir(narrow, dir)
		}
	}
	return narrow, nil
}

func resumeEffectiveSandboxForState(convID string) *sandboxpolicy.Snapshot {
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil {
		return nil
	}
	return snapshot
}

func resumeCommandWithFileCleanup(cmd, path string) string {
	const statusVar = "tclaude_resume_status"
	return cmd + "; " + statusVar + "=$?; " + session.CodexProfileCleanupShell(path) +
		"; exit $" + statusVar
}

func createSessionForConv(conv *SessionEntry) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}

	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	// Sync the configured Claude Code transcript-retention override
	// (claude_cleanup_period_days) into ~/.claude/settings.json. No-op unless
	// set; logs and continues on failure.
	_ = session.EnsureClaudeCleanupPeriod()

	cwd := conv.ProjectPath
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	// The session PK carries the FULL conversation identity — never a
	// truncation (a shared 8-char prefix would collide on the PK). The tmux
	// name is the short, human-facing handle. See JOH-248.
	sessionID := conv.SessionID

	// Reserve the conversation before launching: this rejects an already-live
	// conv AND serializes against a concurrent resume (otherwise two resumes
	// could both `claude --resume` the same .jsonl → corruption). Keyed on
	// conv_id, it catches the live session whatever its PK shape; the lock is
	// held until the launch returns and the OS frees it if this process dies.
	// See JOH-332.
	release, reject := session.ReserveConvForLaunch(sessionID)
	if reject != nil {
		return reject
	}
	defer release()

	tmuxSession := session.UniqueTmuxSessionName(session.TmuxNameBase(sessionID, "", cwd))

	// Resume through the conv's recorded harness, not a hardcoded
	// `claude --resume`: a Codex conv selected in the watch view must be
	// relaunched with `codex resume <id>`, the way the web-dashboard / agentd
	// resume path already does (JOH-217). Resolution failures (an unspawnable
	// or unknown harness) surface here rather than spawning a broken command.
	launchCmd, profilePath, h, err := resumeLaunchCmd(conv.Harness, sessionID, conv.SessionID, clcommon.ExtractClaudeExtraArgs())
	if err != nil {
		return err
	}
	approvalPolicy, autoReview, err := resumeApprovalState(h, conv.SessionID)
	if err != nil {
		return err
	}

	// Launch through the shared script mechanism, not an inline `sh -c`: the
	// resume command carries the same env exports and sandbox dir lists as a
	// fresh launch, so it has the same tmux ~16KB argv cliff and the same
	// ps-visible-credentials exposure the spawn path already fixed.
	if err := session.LaunchDetachedTmuxSession(tmuxSession, cwd, launchCmd,
		session.CodexProfileMarkerArgs(profilePath)...); err != nil {
		return err
	}

	pid := session.ParsePIDFromTmux(tmuxSession)
	// Carry the resolved harness onto the saved row so a non-claude tag is not
	// coalesced back to "claude" — closes the inline TODO(JOH-155) for the
	// watch-resume path now that codex resume lands here.
	state := &session.SessionState{
		ID:                 sessionID,
		TmuxSession:        tmuxSession,
		PID:                pid,
		Cwd:                cwd,
		ConvID:             conv.SessionID,
		Status:             session.StatusIdle,
		Harness:            h.Name,
		SandboxMode:        resumeSandboxMode(conv.SessionID),
		EffectiveSandbox:   resumeEffectiveSandboxForState(conv.SessionID),
		ApprovalPolicy:     approvalPolicy,
		ApprovalAutoReview: autoReview,
		Created:            time.Now(),
		Updated:            time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	fmt.Printf("Created session %s\n", tmuxSession)
	fmt.Println("Attaching... (Ctrl+B D to detach)")

	return session.AttachToSession(sessionID, tmuxSession, false)
}

func findSessionForConv(convID string) *session.SessionState {
	states, _ := session.ListSessionStates()
	for _, state := range states {
		session.RefreshSessionStatus(state)
		if state.Status != session.StatusExited && state.ConvID == convID {
			return state
		}
	}
	return nil
}

func formatFileSize(size int64) string {
	if size <= 0 {
		return ""
	}
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%dkB", size/1024)
	}
	mb := float64(size) / (1024 * 1024)
	return fmt.Sprintf("%.1fMB", mb)
}

func createWorktreeForConv(conv *SessionEntry, branch string) error {
	return worktree.RunAdd(branch, "", conv.SessionID, "", false, false)
}
