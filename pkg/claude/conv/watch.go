package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
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
)

type watchTickMsg time.Time
type watchFileChangeMsg struct {
	sessionID string              // ID of the session that changed (empty for delete)
	deleted   bool                // true if the file was deleted
	watcher   *fsnotify.Watcher  // reuse watcher across events
}

// watchConfirmMode represents confirmation dialogs
type watchConfirmMode int

const (
	watchConfirmNone watchConfirmMode = iota
	watchConfirmAttachForce       // Session already attached, confirm force attach
	watchConfirmDelete            // Delete conversation (no active session)
	watchConfirmDeleteWithSession // Delete conversation that has an active session
	watchConfirmNoTmux            // Session has no tmux, cannot attach
)

type watchModel struct {
	// Data
	entries        []SessionEntry
	filtered       []SessionEntry                    // After search filter
	activeSessions map[string]*session.SessionState  // convID -> session

	// Navigation
	cursor         int
	viewportOffset int
	viewportHeight int

	// Search
	searchInput   string
	searchFocused bool

	// Worktree branch input
	worktreeInput   string
	worktreeFocused bool

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
	selectedConv    *SessionEntry
	shouldCreate    bool   // true = create new session, false = attach to existing
	forceAttach     bool
	focusOnly       bool   // Just focus the window, don't attach
	focusTmux       string // Tmux session to focus
	focusSessionID  string // Session ID for focus (needed for WSL window title search)
	createWorktree  bool   // true = create worktree for selected conv
	worktreeBranch  string // Branch name for worktree

	// Status message (shown briefly after actions)
	statusMsg string
}

func initialWatchModel(global bool, since, before string) watchModel {
	cwd, _ := os.Getwd()
	return watchModel{
		entries:        []SessionEntry{},
		filtered:       []SessionEntry{},
		activeSessions: make(map[string]*session.SessionState),
		global:         global,
		projectPath:    cwd,
		since:          since,
		before:         before,
		viewportHeight: 20, // Will be adjusted based on terminal size
	}
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(watchTickCmd(), watchSessionFilesCmd(nil), tea.EnterAltScreen)
}

func watchTickCmd() tea.Cmd {
	// Fallback tick - slower now that we have file watching
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return watchTickMsg(t)
	})
}

// watchSessionFilesCmd starts or reuses a file watcher on the session state directory.
// The watcher is kept alive across events to avoid missing rapid event sequences
// (e.g., REMOVE+CREATE from atomic file renames).
func watchSessionFilesCmd(existing *fsnotify.Watcher) tea.Cmd {
	return func() tea.Msg {
		watcher := existing
		if watcher == nil {
			var err error
			watcher, err = fsnotify.NewWatcher()
			if err != nil {
				return nil // Fall back to polling
			}

			dir := session.SessionsDir()
			if dir == "" {
				watcher.Close()
				return nil
			}

			if err := session.EnsureSessionsDir(); err != nil {
				watcher.Close()
				return nil
			}

			if err := watcher.Add(dir); err != nil {
				watcher.Close()
				return nil
			}
		}

		// Wait for a relevant file change
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return nil
				}
				// Only care about .json files (session state files)
				if filepath.Ext(event.Name) != ".json" {
					continue
				}
				sessionID := filepath.Base(event.Name)
				sessionID = sessionID[:len(sessionID)-5] // remove .json

				// For remove/rename: atomic writes generate REMOVE then CREATE.
				// Wait briefly for the replacement file to appear.
				if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					time.Sleep(50 * time.Millisecond)
					if isValidSessionStateFile(event.Name) {
						return watchFileChangeMsg{sessionID: sessionID, deleted: false, watcher: watcher}
					}
					return watchFileChangeMsg{sessionID: sessionID, deleted: true, watcher: watcher}
				}

				// For write/create, verify the file is complete and parseable
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					if isValidSessionStateFile(event.Name) {
						return watchFileChangeMsg{sessionID: sessionID, deleted: false, watcher: watcher}
					}
					// File not yet complete, wait for next event
				}

			case _, ok := <-watcher.Errors:
				if !ok {
					return nil
				}
				// On error, close and fall back to polling
				watcher.Close()
				return nil
			}
		}
	}
}

// isValidSessionStateFile checks if a session state file is complete and parseable
func isValidSessionStateFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var state session.SessionState
	return json.Unmarshal(data, &state) == nil
}

// loadConversations loads all conversations based on settings
func (m watchModel) loadConversations() watchModel {
	var allEntries []SessionEntry

	if m.global {
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			return m
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
		projectPath := GetClaudeProjectPath(m.projectPath)
		index, err := LoadSessionsIndex(projectPath)
		if err != nil {
			m.entries = []SessionEntry{}
			m.filtered = []SessionEntry{}
			return m
		}
		allEntries = index.Entries
	}

	// Filter by time if specified
	allEntries, _ = FilterEntriesByTime(allEntries, m.since, m.before)

	// Sort by current sort state, defaulting to modified descending
	if m.sort.Key != "" {
		sortConvEntriesByKey(allEntries, m.sort.Key, m.sort.Direction)
	} else {
		sortEntries(allEntries, "modified", false)
	}

	m.entries = allEntries
	m = m.applySearchFilter()
	m = m.refreshActiveSessions()
	return m
}

// applySearchFilter filters entries based on search input
func (m watchModel) applySearchFilter() watchModel {
	if m.searchInput == "" {
		m.filtered = m.entries
		return m
	}

	query := strings.ToLower(m.searchInput)
	var filtered []SessionEntry
	for _, e := range m.entries {
		if matchesSearch(e, query) {
			filtered = append(filtered, e)
		}
	}
	m.filtered = filtered

	// Reset cursor/viewport if needed
	if m.cursor >= len(m.filtered) {
		m.cursor = 0
	}
	m.viewportOffset = 0

	return m
}

func matchesSearch(e SessionEntry, query string) bool {
	return strings.Contains(strings.ToLower(e.DisplayTitle()), query) ||
		strings.Contains(strings.ToLower(e.FirstPrompt), query) ||
		strings.Contains(strings.ToLower(e.ProjectPath), query) ||
		strings.Contains(strings.ToLower(e.GitBranch), query) ||
		strings.Contains(strings.ToLower(e.SessionID), query)
}

// refreshActiveSessions updates which conversations have active sessions
func (m watchModel) refreshActiveSessions() watchModel {
	states, _ := session.ListSessionStates()
	m.activeSessions = make(map[string]*session.SessionState)

	for _, state := range states {
		session.RefreshSessionStatus(state)
		if state.Status != session.StatusExited && state.ConvID != "" {
			m.activeSessions[state.ConvID] = state
		}
	}

	return m
}

// updateSingleSession updates just one session in the activeSessions map
func (m watchModel) updateSingleSession(sessionID string, deleted bool) watchModel {
	if deleted {
		// Find and remove any session with this ID
		for convID, state := range m.activeSessions {
			if state.ID == sessionID {
				delete(m.activeSessions, convID)
				break
			}
		}
		return m
	}

	// Load the updated session state
	state, err := session.LoadSessionState(sessionID)
	if err != nil {
		return m
	}

	session.RefreshSessionStatus(state)

	// Update or remove from map based on status
	if state.Status == session.StatusExited {
		// Remove from active sessions
		if state.ConvID != "" {
			delete(m.activeSessions, state.ConvID)
		}
	} else if state.ConvID != "" {
		// Update in active sessions
		m.activeSessions[state.ConvID] = state
	}

	return m
}

// ensureCursorVisible scrolls viewport to keep cursor visible
func (m watchModel) ensureCursorVisible() watchModel {
	if m.cursor < m.viewportOffset {
		m.viewportOffset = m.cursor
	}
	if m.cursor >= m.viewportOffset+m.viewportHeight {
		m.viewportOffset = m.cursor - m.viewportHeight + 1
	}
	return m
}

// triggerDelete initiates delete confirmation for the selected conversation
func (m watchModel) triggerDelete() watchModel {
	if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
		conv := m.filtered[m.cursor]
		if _, hasSession := m.activeSessions[conv.SessionID]; hasSession {
			m.confirmMode = watchConfirmDeleteWithSession
		} else {
			m.confirmMode = watchConfirmDelete
		}
		m.statusMsg = ""
	}
	return m
}

// deleteConversation deletes a conversation's files and removes it from the index
func (m watchModel) deleteConversation(conv *SessionEntry) error {
	// Determine project path
	var projectPath string
	if m.global {
		// For global mode, derive project path from the conversation's ProjectPath
		projectPath = GetClaudeProjectPath(conv.ProjectPath)
	} else {
		projectPath = GetClaudeProjectPath(m.projectPath)
	}

	// Load index
	index, err := LoadSessionsIndex(projectPath)
	if err != nil {
		return fmt.Errorf("failed to load index: %w", err)
	}

	// Delete conversation file
	convFile := projectPath + "/" + conv.SessionID + ".jsonl"
	if err := os.Remove(convFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	// Delete conversation directory if it exists
	convDir := projectPath + "/" + conv.SessionID
	if info, err := os.Stat(convDir); err == nil && info.IsDir() {
		if err := os.RemoveAll(convDir); err != nil {
			return fmt.Errorf("failed to delete directory: %w", err)
		}
	}

	// Remove from index and save
	RemoveSessionByID(index, conv.SessionID)
	if err := SaveSessionsIndex(projectPath, index); err != nil {
		return fmt.Errorf("failed to save index: %w", err)
	}

	// Add tombstone if sync is initialized
	if syncutil.IsInitialized() {
		if err := AddTombstoneForProject(projectPath, conv.SessionID); err != nil {
			// Log but don't fail - tombstone is best-effort
			fmt.Printf("Warning: failed to add tombstone: %v\n", err)
		}
	}

	return nil
}

// stopSession stops a session's tmux session and removes its state file
func (m watchModel) stopSession(state *session.SessionState) error {
	// Kill tmux session if alive
	if session.IsTmuxSessionAlive(state.TmuxSession) {
		cmd := clcommon.TmuxCommand("kill-session", "-t", state.TmuxSession)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to kill tmux session: %w", err)
		}
	}

	// Remove state file
	if err := session.DeleteSessionState(state.ID); err != nil {
		return fmt.Errorf("failed to delete session state: %w", err)
	}

	return nil
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Handle confirmation dialogs first
		if m.confirmMode != watchConfirmNone {
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
						// Delete conversation (no session)
						if err := m.deleteConversation(&conv); err != nil {
							m.statusMsg = "Error: " + err.Error()
						} else {
							m.statusMsg = "Deleted conversation " + conv.SessionID[:8]
						}
						m.confirmMode = watchConfirmNone
						m = m.loadConversations()
					case watchConfirmDeleteWithSession:
						// Stop session AND delete conversation
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
						m = m.loadConversations()
					}
				}
			case "s", "S":
				// Stop session only (when there's an active session)
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
					m = m.refreshActiveSessions()
				}
			case "n", "N", "esc", " ":
				m.confirmMode = watchConfirmNone
			}
			// Any key dismisses the no-tmux message
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

		// Handle search mode
		if m.searchFocused {
			switch msg.String() {
			case "esc":
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
					m = m.ensureCursorVisible()
				}
			case "down":
				// Exit search and navigate down
				m.searchFocused = false
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
					m = m.ensureCursorVisible()
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

		// Handle worktree branch input mode
		if m.worktreeFocused {
			switch msg.String() {
			case "esc":
				m.worktreeFocused = false
				m.worktreeInput = ""
			case "enter":
				if m.worktreeInput != "" && m.cursor < len(m.filtered) {
					conv := m.filtered[m.cursor]
					m.selectedConv = &conv
					m.createWorktree = true
					m.worktreeBranch = m.worktreeInput
					return m, tea.Quit
				}
				m.worktreeFocused = false
			case "backspace":
				if len(m.worktreeInput) > 0 {
					m.worktreeInput = m.worktreeInput[:len(m.worktreeInput)-1]
				}
			case "ctrl+u":
				m.worktreeInput = ""
			default:
				// Add valid branch name characters
				ch := msg.String()
				if len(ch) == 1 {
					c := ch[0]
					// Allow alphanumeric, dash, underscore, slash
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '/' {
						m.worktreeInput += ch
					}
				}
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
			}
		case "/":
			m.searchFocused = true
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m = m.ensureCursorVisible()
			}
		case "down", "j":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m = m.ensureCursorVisible()
			}
		case "pgup", "ctrl+b":
			m.cursor -= m.viewportHeight
			if m.cursor < 0 {
				m.cursor = 0
			}
			m = m.ensureCursorVisible()
		case "pgdown", "ctrl+f":
			m.cursor += m.viewportHeight
			if m.cursor >= len(m.filtered) {
				m.cursor = len(m.filtered) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			m = m.ensureCursorVisible()
		case "home", "g":
			m.cursor = 0
			m = m.ensureCursorVisible()
		case "end", "G":
			if len(m.filtered) > 0 {
				m.cursor = len(m.filtered) - 1
			}
			m = m.ensureCursorVisible()
		case "enter":
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				conv := m.filtered[m.cursor]
				// Check if session already exists for this conversation
				if existing, ok := m.activeSessions[conv.SessionID]; ok {
					tmuxAlive := existing.TmuxSession != "" && session.IsTmuxSessionAlive(existing.TmuxSession)
					if !tmuxAlive {
						// Non-tmux or dead tmux session, cannot attach
						m.confirmMode = watchConfirmNoTmux
					} else if existing.Attached > 0 {
						// Session already has clients attached - just focus the window
						m.focusOnly = true
						m.focusTmux = existing.TmuxSession
						m.focusSessionID = existing.ID
						return m, tea.Quit
					} else {
						m.selectedConv = &conv
						m.shouldCreate = false // Just attach to existing
						return m, tea.Quit
					}
				} else {
					m.selectedConv = &conv
					m.shouldCreate = true // Create new session
					return m, tea.Quit
				}
			}
		case "r":
			// Force refresh conversations
			m = m.loadConversations()
			m.statusMsg = ""
		case "h", "?":
			m.helpView = true
		case "W", "w":
			// Create worktree for selected conversation
			if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
				m.worktreeFocused = true
				m.worktreeInput = ""
			}
		case "delete", "backspace", "x", "ctrl+d":
			// Delete conversation (with confirmation)
			// Note: ctrl+d is what macOS sends for forward delete key
			m = m.triggerDelete()
		default:
			if m.sort.HandleSortKey(m.columns(), msg.String()) {
				m = m.loadConversations()
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Calculate viewport height (terminal height - search box - header - separator - scroll - footer)
		m.viewportHeight = max(msg.Height-10, 5)
		m = m.ensureCursorVisible()

	case watchTickMsg:
		// Only refresh session state (lightweight), not full conversation list
		m = m.refreshActiveSessions()
		return m, watchTickCmd()

	case watchFileChangeMsg:
		// Session state file changed - update just that session
		m = m.updateSingleSession(msg.sessionID, msg.deleted)
		return m, watchSessionFilesCmd(msg.watcher)
	}

	return m, nil
}

// columns returns the column definitions for the conversation table.
// Used by both Update (for sort key handling) and View (for rendering).
func (m watchModel) columns() []table.Column {
	if m.global {
		return []table.Column{
			{Header: "", Width: 2},                                                                                  // Session indicator
			{Header: "ID", Width: 10, SortKey: "id"},                                                                // ID
			{Header: "PROJECT", MinWidth: 20, Weight: 0.4, Truncate: true, TruncateMode: table.TruncateStart, SortKey: "project"}, // Project
			{Header: "TITLE/PROMPT", MinWidth: 30, Weight: 0.6, Truncate: true, SortKey: "title"},                   // Title
			{Header: "SIZE", Width: 8, SortKey: "size"},                                                             // File size
			{Header: "MODIFIED", Width: 16, SortKey: "modified"},                                                    // Modified
		}
	}
	return []table.Column{
		{Header: "", Width: 2},                                                    // Session indicator
		{Header: "ID", Width: 10, SortKey: "id"},                                  // ID
		{Header: "TITLE/PROMPT", MinWidth: 30, Truncate: true, SortKey: "title"},  // Title
		{Header: "SIZE", Width: 8, SortKey: "size"},                               // File size
		{Header: "MODIFIED", Width: 16, SortKey: "modified"},                      // Modified
	}
}

func (m watchModel) View() string {
	// Help view overlay
	if m.helpView {
		return m.renderHelpView()
	}

	var b strings.Builder

	// Search box
	b.WriteString("\n  ")
	if m.searchFocused {
		b.WriteString(wSearchStyle.Render("Search: "))
		b.WriteString(wSearchStyle.Render("[" + m.searchInput + "_]"))
	} else if m.searchInput != "" {
		b.WriteString(wSearchStyle.Render("Search: [" + m.searchInput + "]"))
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
			b.WriteString("  No matches for \"" + m.searchInput + "\"\n")
		}
		b.WriteString("\n")
		b.WriteString(wHelpStyle.Render("  r refresh • / search • q quit"))
		b.WriteString("\n")
		return b.String()
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

	// Add rows for all filtered entries
	for _, e := range m.filtered {
		// Session indicator (plain text - ANSI in cells breaks row selection highlight)
		sessionMark := "  "
		if state, ok := m.activeSessions[e.SessionID]; ok {
			tmuxAlive := state.TmuxSession != "" && session.IsTmuxSessionAlive(state.TmuxSession)
			if !tmuxAlive {
				sessionMark = " ◉" // Non-tmux or dead tmux
			} else if state.Attached > 0 {
				sessionMark = "⚡" // Tmux with attached clients
			} else {
				sessionMark = " ▷" // Tmux detached
			}
		}

		// Format fields
		id := e.SessionID[:8]
		modified := formatDate(e.Modified)

		// Build title: [title]: prompt, or just prompt (table handles truncation)
		var titleStr string
		if e.HasTitle() {
			titleStr = e.DisplayTitle()
		}
		title := convindex.FormatTitleAndPrompt(titleStr, e.FirstPrompt)

		size := formatFileSize(e.FileSize)

		if m.global {
			tbl.AddRow(table.Row{
				Cells: []string{sessionMark, id, e.ProjectPath, title, size, modified},
			})
		} else {
			tbl.AddRow(table.Row{
				Cells: []string{sessionMark, id, title, size, modified},
			})
		}
	}

	// Render table with scroll indicator
	b.WriteString(tbl.RenderWithScroll(&wHelpStyle))
	b.WriteString("\n\n")
	switch m.confirmMode {
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
			b.WriteString(wSearchStyle.Render("  Branch name: [" + m.worktreeInput + "_] (enter to create, esc to cancel)"))
		} else if m.statusMsg != "" {
			b.WriteString(wSearchStyle.Render("  " + m.statusMsg))
		} else {
			b.WriteString(wHelpStyle.Render("  h help • ↑/↓ navigate • enter attach • W worktree • del delete • q quit"))
		}
	}
	b.WriteString("\n")

	return b.String()
}

func (m watchModel) renderHelpView() string {
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
}

// RunConvWatch runs the interactive watch mode and returns the result
func RunConvWatch(global bool, since, before string, state ConvWatchState) (WatchResult, ConvWatchState, error) {
	m := initialWatchModel(global, since, before)

	// Restore previous state
	m.searchInput = state.SearchInput
	m.cursor = state.Cursor
	m.viewportOffset = state.ViewportOffset
	m.sort = state.Sort

	m = m.loadConversations()

	// Ensure cursor is still valid after loading (list may have changed)
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
	m = m.ensureCursorVisible()

	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return WatchResult{}, state, err
	}

	fm := finalModel.(watchModel)
	newState := ConvWatchState{
		SearchInput:    fm.searchInput,
		Cursor:         fm.cursor,
		ViewportOffset: fm.viewportOffset,
		Sort:           fm.sort,
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
		watchState = newState // Preserve state between cycles
		if err != nil {
			return err
		}

		if result.Conv == nil && !result.FocusOnly && !result.CreateWorktree {
			// User quit without selecting
			return nil
		}

		// Focus only - just focus the window and return to watch mode
		if result.FocusOnly {
			os.Setenv("TCLAUDE_SESSION_ID", result.FocusSessionID)
			session.TryFocusAttachedSession(result.TmuxSession)
			continue
		}

		// Create worktree - exits watch mode to run git commands
		if result.CreateWorktree {
			if err := createWorktreeForConv(result.Conv, result.WorktreeBranch); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating worktree: %v\n", err)
			}
			// Don't return to watch mode after worktree creation
			// (the new session will be in the worktree directory)
			return nil
		}

		if result.ShouldCreate {
			// Create a new session for this conversation
			if err := createSessionForConv(result.Conv); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
				// Continue back to watch mode
				continue
			}
		} else {
			// Attach to existing session
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
					// Session may have exited, continue to watch mode
					continue
				}
			}
		}

		// After detach or session end, loop back to watch mode
	}
}

// sortConvEntriesByKey sorts conversation entries by the given sort key and direction.
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

// createSessionForConv creates a new session for a conversation
func createSessionForConv(conv *SessionEntry) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}

	// Ensure hooks are installed
	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	cwd := conv.ProjectPath
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	// Use conv ID prefix as session ID
	sessionID := conv.SessionID[:8]
	tmuxSession := "tclaude-" + sessionID

	// Build claude command with TCLAUDE_SESSION_ID env var
	claudeCmd := fmt.Sprintf("TCLAUDE_SESSION_ID=%s claude --resume %s", sessionID, conv.SessionID)
	if extraArgs := clcommon.ExtractClaudeExtraArgs(); len(extraArgs) > 0 {
		quoted := make([]string, len(extraArgs))
		for i, a := range extraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		claudeCmd += " " + strings.Join(quoted, " ")
	}

	// Create tmux session
	tmuxArgs := []string{
		"new-session",
		"-d",
		"-s", tmuxSession,
		"-c", cwd,
		"sh", "-c", claudeCmd,
	}

	cmd := exec.Command("tmux", clcommon.TmuxArgs(tmuxArgs...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Get PID and save state
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

// findSessionForConv finds an existing session for a conversation ID
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

// formatFileSize formats bytes as a human-readable size string
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

// createWorktreeForConv creates a git worktree and starts a session with the conversation
func createWorktreeForConv(conv *SessionEntry, branch string) error {
	// Call the worktree add function directly
	// branch, fromBranch, fromConv, path, global, detached
	return worktree.RunAdd(branch, "", conv.SessionID, "", false, false)
}

