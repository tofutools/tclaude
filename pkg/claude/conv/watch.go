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
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

var (
	wSelectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("238")) // Dark gray background, preserves row foreground color
	wHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	wHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	wSearchStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	wConfirmStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	wSemanticStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39")) // cyan
)

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

	// Settings
	global      bool   // Search all projects
	projectPath string // Current project path
	since       string // Filter: modified after
	before      string // Filter: modified before

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
	s.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	s.Focused.Text = wSearchStyle
	s.Blurred.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
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
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '/') {
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
		Placeholder: lipgloss.NewStyle().Foreground(lipgloss.Color("241")),
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
		semanticInput:    newSemanticInput(),
		global:           global,
		projectPath:      cwd,
		claudeProjectDir: claudeProjectDir,
		since:            since,
		before:           before,
		viewportHeight:   20, // Will be adjusted based on terminal size
	}
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
			} else {
				m.confirmMode = watchConfirmQuit
			}
		case "/":
			m.clearSemanticMode()
			m.searchFocused = true
			m.searchInput.Focus()
		case "s":
			m.semanticChecking = true
			return m, m.semanticPreCheck()
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
		case "delete", "backspace", "x", "ctrl+d":
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
	} else if m.searchInput.Value() != "" {
		b.WriteString(wSearchStyle.Render("Search: [" + m.searchInput.Value() + "]"))
	} else {
		b.WriteString(wHelpStyle.Render("/ to search"))
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

	// Build table using shared column definitions
	tableWidth := max(m.width-3, 60)
	cols := m.columns()
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
		sessionMark := "  "
		if state, ok := m.activeSessions[e.SessionID]; ok {
			tmuxAlive := state.TmuxSession != "" && session.IsTmuxSessionAlive(state.TmuxSession)
			if !tmuxAlive {
				sessionMark = " ◉"
			} else if state.Attached > 0 {
				sessionMark = "⚡"
			} else {
				sessionMark = " ▷"
			}
		}

		id := e.SessionID[:8]
		modified := formatDate(e.Modified)
		var titleStr string
		if e.HasTitle() {
			titleStr = e.DisplayTitle()
		}
		title := convindex.FormatTitleAndPrompt(titleStr, e.FirstPrompt)
		size := formatFileSize(e.FileSize)

		var cells []string
		if m.global {
			cells = []string{sessionMark, id, e.ProjectPath, title, size, modified}
		} else {
			cells = []string{sessionMark, id, title, size, modified}
		}
		if m.semanticMode {
			score := ""
			if s, ok := m.semanticScores[e.SessionID]; ok {
				score = fmt.Sprintf("%.4f", s)
			}
			cells = append(cells, score)
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
			b.WriteString(wHelpStyle.Render("  / search • s semantic • enter attach • h help • esc/q quit"))
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
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
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
	} else {
		index, err := LoadSessionsIndex(m.claudeProjectDir)
		if err != nil {
			m.entries = []SessionEntry{}
			m.filtered = []SessionEntry{}
			return
		}
		allEntries = index.Entries
	}

	m.setEntries(allEntries)
}

// reloadFromDB loads entries from the SQLite cache only (no filesystem access).
// Used when DB polling detects changes made by another tclaude instance.
func (m *watchModel) reloadFromDB() {
	var dbProjectDir string
	if !m.global {
		dbProjectDir = m.claudeProjectDir
	}
	allEntries, err := LoadEntriesFromDB(dbProjectDir)
	if err != nil {
		return
	}
	m.setEntries(allEntries)
}

// setEntries replaces the entries list, applies time filter, sort, and search filter.
func (m *watchModel) setEntries(allEntries []SessionEntry) {
	allEntries, _ = FilterEntriesByTime(allEntries, m.since, m.before)
	m.sortInPlace(allEntries)
	m.entries = allEntries
	m.applySearchFilter()
	m.refreshActiveSessions()
}

// --- Sorting (in-memory only) ---

// sortInPlace sorts the given slice using current sort state (or default modified desc).
func (m *watchModel) sortInPlace(entries []SessionEntry) {
	if m.sort.Key != "" {
		sortConvEntriesByKey(entries, m.sort.Key, m.sort.Direction)
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
	timers := make(map[string]*time.Timer)
	known := make(map[string]bool) // tracks files we've already seen

	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()

	for {
		select {
		case <-closeCh:
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			path := event.Name

			// Auto-watch new project subdirectories created under projectsDir.
			// In single-project mode this catches the project dir being created
			// after startup; in global mode it watches all new projects.
			if event.Op&fsnotify.Create != 0 && filepath.Dir(path) == projectsDir {
				if info, err := os.Stat(path); err == nil && info.IsDir() {
					_ = w.Add(path)
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
				if t, ok := timers[path]; ok {
					t.Stop()
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
				if t, ok := timers[path]; ok {
					t.Stop()
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
			if t, ok := timers[path]; ok {
				t.Stop()
			}
			timers[path] = time.AfterFunc(fsDebounceDelay, func() {
				select {
				case outCh <- fsFileChangeMsg{FilePath: path, Removed: false}:
				case <-closeCh:
				}
				delete(timers, path)
			})

		case <-w.Errors:
			// Ignore errors, keep listening
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
		m.watcher.Close()
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
	if searchVal == "" {
		m.filtered = m.entries
		return
	}

	query := strings.ToLower(searchVal)
	m.filtered = make([]SessionEntry, 0, len(m.entries))
	for _, e := range m.entries {
		if matchesSearch(e, query) {
			m.filtered = append(m.filtered, e)
		}
	}

	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
	m.viewportOffset = 0
}

// rebuildSemanticFiltered reconstructs the filtered list from entries matching
// saved semantic scores, sorted by similarity descending.
func (m *watchModel) rebuildSemanticFiltered() {
	m.filtered = m.filtered[:0]
	for _, e := range m.entries {
		if _, ok := m.semanticScores[e.SessionID]; ok {
			m.filtered = append(m.filtered, e)
		}
	}
	sort.Slice(m.filtered, func(i, j int) bool {
		return m.semanticScores[m.filtered[i].SessionID] > m.semanticScores[m.filtered[j].SessionID]
	})
}

func matchesSearch(e SessionEntry, query string) bool {
	return strings.Contains(strings.ToLower(e.DisplayTitle()), query) ||
		strings.Contains(strings.ToLower(e.FirstPrompt), query) ||
		strings.Contains(strings.ToLower(e.ProjectPath), query) ||
		strings.Contains(strings.ToLower(e.GitBranch), query) ||
		strings.Contains(strings.ToLower(e.SessionID), query)
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
	if err := os.Remove(convFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	convDir := projectPath + "/" + conv.SessionID
	if info, err := os.Stat(convDir); err == nil && info.IsDir() {
		if err := os.RemoveAll(convDir); err != nil {
			return fmt.Errorf("failed to delete directory: %w", err)
		}
	}

	index, err := LoadSessionsIndex(projectPath)
	if err == nil && RemoveSessionByID(index, conv.SessionID) {
		if err := SaveSessionsIndex(projectPath, index); err != nil {
			return fmt.Errorf("failed to save index: %w", err)
		}
	}

	_ = db.DeleteConvIndex(conv.SessionID)
	m.removeEntry(conv.SessionID)

	if syncutil.IsInitialized() {
		if err := AddTombstoneForProject(projectPath, conv.SessionID); err != nil {
			fmt.Printf("Warning: failed to add tombstone: %v\n", err)
		}
	}

	return nil
}

func (m *watchModel) stopSession(state *session.SessionState) error {
	if session.IsTmuxSessionAlive(state.TmuxSession) {
		cmd := clcommon.TmuxCommand("kill-session", "-t", state.TmuxSession)
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

func (m *watchModel) columns() []table.Column {
	if m.global {
		cols := []table.Column{
			{Header: "", Width: 2},
			{Header: "ID", Width: 10, SortKey: "id"},
			{Header: "PROJECT", MinWidth: 20, Weight: 0.4, Truncate: true, TruncateMode: table.TruncateStart, SortKey: "project"},
			{Header: "TITLE/PROMPT", MinWidth: 30, Weight: 0.6, Truncate: true, SortKey: "title"},
			{Header: "SIZE", Width: 8, SortKey: "size"},
			{Header: "MODIFIED", Width: 16, SortKey: "modified"},
		}
		if m.semanticMode {
			cols = append(cols, table.Column{Header: "SCORE", Width: 10, Align: table.AlignRight})
		}
		return cols
	}
	cols := []table.Column{
		{Header: "", Width: 2},
		{Header: "ID", Width: 10, SortKey: "id"},
		{Header: "TITLE/PROMPT", MinWidth: 30, Truncate: true, SortKey: "title"},
		{Header: "SIZE", Width: 8, SortKey: "size"},
		{Header: "MODIFIED", Width: 16, SortKey: "modified"},
	}
	if m.semanticMode {
		cols = append(cols, table.Column{Header: "SCORE", Width: 10, Align: table.AlignRight})
	}
	return cols
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
	b.WriteString("    esc       Clear search / exit search mode\n")
	b.WriteString("    ^U        Clear search input\n")
	b.WriteString("\n")

	b.WriteString(wHeaderStyle.Render("  Actions"))
	b.WriteString("\n")
	b.WriteString("    W         Create git worktree with this conversation\n")
	b.WriteString("    del/x     Delete conversation (with confirmation)\n")
	b.WriteString("              If has session: y=delete+stop, s=stop only, n=cancel\n")
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
			os.Setenv("TCLAUDE_SESSION_ID", result.FocusSessionID)
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

func sortConvEntriesByKey(entries []SessionEntry, key string, dir table.SortDirection) {
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

func createSessionForConv(conv *SessionEntry) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}

	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	cwd := conv.ProjectPath
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	sessionID := conv.SessionID[:8]
	tmuxSession := sessionID

	claudeCmd := fmt.Sprintf("TCLAUDE_SESSION_ID=%s claude --resume %s", sessionID, conv.SessionID)
	if extraArgs := clcommon.ExtractClaudeExtraArgs(); len(extraArgs) > 0 {
		quoted := make([]string, len(extraArgs))
		for i, a := range extraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		claudeCmd += " " + strings.Join(quoted, " ")
	}

	tmuxArgs := []string{
		"new-session", "-d",
		"-s", tmuxSession,
		"-c", cwd,
		"sh", "-c", claudeCmd,
	}

	cmd := clcommon.TmuxCommand(tmuxArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	pid := session.ParsePIDFromTmux(tmuxSession)
	state := &session.SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         cwd,
		ConvID:      conv.SessionID,
		Status:      session.StatusIdle,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	fmt.Printf("Created session %s\n", sessionID)
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
