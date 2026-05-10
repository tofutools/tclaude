package agent

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent inbox watch` — interactive auto-refreshing inbox view.
// v1 supports: list + nav + select-to-read. Reply/delete/search defer
// to follow-ups. Reuses the existing /v1/inbox and /v1/messages/{id}
// daemon endpoints; no new server-side work.

type inboxWatchParams struct {
	Limit  int  `long:"limit" short:"n" help:"Max number of messages to show" default:"50"`
	Unread bool `long:"unread" help:"Only show messages without read_at"`
}

func inboxWatchCmd() *cobra.Command {
	return boa.CmdT[inboxWatchParams]{
		Use:         "watch",
		Short:       "Interactive auto-refreshing inbox table (alias: -w)",
		Long: "Renders the inbox in a bubbletea TUI. Up/down to navigate, Enter to " +
			"read a message (marks it read), Esc/q to return to the list or quit.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inboxWatchParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxWatch(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runInboxWatch(p *inboxWatchParams, _, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	m := newInboxWatchModel(p)
	prog := tea.NewProgram(m)
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}

// --- Model ---

type inboxTickMsg time.Time

type inboxLoadedMsg struct {
	entries []inboxEntry
	err     error
}

type inboxMessageLoadedMsg struct {
	id   int64
	body string
	err  error
}

// inboxReplySentMsg flips back from reply mode after the daemon
// responds. err is nil on successful send.
type inboxReplySentMsg struct {
	id  int64
	err error
}

type inboxWatchModel struct {
	params    inboxWatchParams
	entries   []inboxEntry
	cursor    int
	width     int
	height    int
	loadErr   string
	statusMsg string

	// Read view: when readingID is non-zero, the View renders the
	// loaded message body instead of the table.
	readingID   int64
	readingBody string

	// Reply mode (active only while in the read view): a textarea
	// stacked under the body. ctrl+enter / alt+enter submits, esc
	// cancels. While replyFocused, list-mode keys don't fire.
	replyFocused  bool
	replyTextarea textarea.Model
}

func newInboxWatchModel(p *inboxWatchParams) *inboxWatchModel {
	ta := textarea.New()
	ta.Placeholder = "Reply… (ctrl+enter submit, esc cancel)"
	ta.SetHeight(4)
	return &inboxWatchModel{
		params:        *p,
		replyTextarea: ta,
	}
}

func (m *inboxWatchModel) Init() tea.Cmd {
	return tea.Batch(m.loadCmd(), inboxTickCmd())
}

func inboxTickCmd() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return inboxTickMsg(t)
	})
}

func (m *inboxWatchModel) loadCmd() tea.Cmd {
	limit := m.params.Limit
	if limit <= 0 {
		limit = 50
	}
	unread := m.params.Unread
	return func() tea.Msg {
		q := fmt.Sprintf("/v1/inbox?limit=%d", limit)
		if unread {
			q += "&unread=1"
		}
		var out []inboxEntry
		if err := DaemonGet(q, &out); err != nil {
			return inboxLoadedMsg{err: err}
		}
		return inboxLoadedMsg{entries: out}
	}
}

// submitReplyCmd POSTs the textarea value to the reply endpoint.
// Returns inboxReplySentMsg on completion. Empty body short-circuits
// to a "no-op" result so the user can press ctrl+enter on an empty
// reply and see a clear status without spamming the daemon.
func (m *inboxWatchModel) submitReplyCmd(id int64, body string) tea.Cmd {
	body = strings.TrimSpace(body)
	if body == "" {
		return func() tea.Msg {
			return inboxReplySentMsg{id: id, err: fmt.Errorf("empty reply ignored")}
		}
	}
	return func() tea.Msg {
		var resp struct {
			ID int64 `json:"id"`
		}
		err := DaemonPost(fmt.Sprintf("/v1/messages/%d/reply", id),
			map[string]string{"body": body}, &resp)
		return inboxReplySentMsg{id: id, err: err}
	}
}

func (m *inboxWatchModel) loadMessageCmd(id int64) tea.Cmd {
	return func() tea.Msg {
		path := fmt.Sprintf("/v1/messages/%d", id)
		var msg struct {
			ID      int64  `json:"id"`
			From    string `json:"from"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
		}
		if err := DaemonGet(path, &msg); err != nil {
			return inboxMessageLoadedMsg{id: id, err: err}
		}
		body := msg.Body
		// Header line so the read pane carries enough context to
		// understand the body without flipping back.
		meta := fmt.Sprintf("From: %s · Subject: %s",
			short(msg.From), strings.TrimSpace(msg.Subject))
		return inboxMessageLoadedMsg{id: id, body: meta + "\n\n" + body}
	}
}

// --- Update ---

func (m *inboxWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case inboxTickMsg:
		// Only auto-refresh when we're showing the list. Skipping the
		// background poll while the user is reading avoids list-shuffle
		// surprises if a new message arrives mid-read.
		if m.readingID == 0 {
			return m, tea.Batch(m.loadCmd(), inboxTickCmd())
		}
		return m, inboxTickCmd()

	case inboxLoadedMsg:
		if msg.err != nil {
			m.loadErr = msg.err.Error()
			return m, nil
		}
		m.loadErr = ""
		m.entries = msg.entries
		if m.cursor >= len(m.entries) {
			m.cursor = 0
		}
		return m, nil

	case inboxMessageLoadedMsg:
		if msg.err != nil {
			m.statusMsg = "read failed: " + msg.err.Error()
			m.readingID = 0
			return m, nil
		}
		m.readingID = msg.id
		m.readingBody = msg.body
		// Reload the list so the read marker updates next refresh.
		return m, m.loadCmd()

	case inboxReplySentMsg:
		if msg.err != nil {
			m.statusMsg = "reply failed: " + msg.err.Error()
			// Keep the reply textarea open on failure so the user can
			// retry / edit without re-typing.
			return m, nil
		}
		m.statusMsg = fmt.Sprintf("reply to #%d sent", msg.id)
		m.replyFocused = false
		m.replyTextarea.Blur()
		m.replyTextarea.SetValue("")
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// While reply textarea is focused, route every other event through
	// it so paste / cursor / etc. work. (Key events are handled in
	// handleKey, which checks m.replyFocused first; non-key events
	// land here.)
	if m.replyFocused {
		var cmd tea.Cmd
		m.replyTextarea, cmd = m.replyTextarea.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *inboxWatchModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Reply textarea takes priority while focused: ctrl+enter submits,
	// esc cancels, everything else routes to the textarea so the user
	// can type freely.
	if m.replyFocused {
		switch msg.String() {
		case "esc":
			m.replyFocused = false
			m.replyTextarea.Blur()
			m.replyTextarea.SetValue("")
			return m, nil
		case "ctrl+enter", "alt+enter":
			body := m.replyTextarea.Value()
			id := m.readingID
			m.statusMsg = "sending reply..."
			return m, m.submitReplyCmd(id, body)
		default:
			var cmd tea.Cmd
			m.replyTextarea, cmd = m.replyTextarea.Update(msg)
			return m, cmd
		}
	}

	// Read view has its own keymap (without reply).
	if m.readingID != 0 {
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			m.readingID = 0
			m.readingBody = ""
			return m, nil
		case "r":
			m.replyFocused = true
			m.replyTextarea.Focus()
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.entries)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		if len(m.entries) > 0 {
			m.cursor = len(m.entries) - 1
		}
	case "enter":
		if m.cursor < len(m.entries) {
			id := m.entries[m.cursor].ID
			m.readingID = id // optimistic — flips back on error
			m.readingBody = "(loading...)"
			return m, m.loadMessageCmd(id)
		}
	case "r":
		m.statusMsg = "refreshing..."
		return m, m.loadCmd()
	}
	return m, nil
}

// --- View ---

var (
	inboxHeaderStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	inboxHelpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	inboxReadingStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	inboxSelectedStyle = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("238"))
	inboxErrorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

func (m *inboxWatchModel) View() tea.View {
	if m.readingID != 0 {
		return tea.View{Content: m.renderReadView(), AltScreen: true}
	}
	return tea.View{Content: m.renderListView(), AltScreen: true}
}

func (m *inboxWatchModel) renderListView() string {
	var b strings.Builder
	b.WriteString(inboxHeaderStyle.Render("  Inbox"))
	if m.params.Unread {
		b.WriteString(inboxHelpStyle.Render("  (unread only)"))
	}
	b.WriteString(inboxHelpStyle.Render(fmt.Sprintf("  [%d messages]", len(m.entries))))
	if m.statusMsg != "" {
		b.WriteString("  ")
		b.WriteString(inboxHelpStyle.Render(m.statusMsg))
	}
	b.WriteString("\n\n")

	if m.loadErr != "" {
		b.WriteString(inboxErrorStyle.Render("Error: " + m.loadErr))
		b.WriteString("\n\n")
	}

	if len(m.entries) == 0 {
		b.WriteString("  (empty inbox)\n\n")
		b.WriteString(inboxHelpStyle.Render("  r refresh • q quit"))
		return b.String()
	}

	tableWidth := max(m.width-3, 60)
	tbl := table.New(
		table.Column{Header: "", Width: 1},
		table.Column{Header: "ID", Width: 5, Align: table.AlignRight},
		table.Column{Header: "FROM", Width: 8},
		table.Column{Header: "GROUP", MinWidth: 6, Weight: 0.4, Truncate: true},
		table.Column{Header: "SUBJECT", MinWidth: 10, Weight: 1.6, Truncate: true},
	)
	tbl.SetTerminalWidth(tableWidth)
	tbl.HeaderStyle = inboxHeaderStyle
	tbl.SelectedStyle = inboxSelectedStyle
	tbl.SelectedIndex = m.cursor
	for _, e := range m.entries {
		marker := "*" // unread
		if e.Read {
			marker = " "
		}
		subj := e.Subject
		if subj == "" {
			subj = e.Preview
		}
		if e.ParentID > 0 {
			subj = "↳ " + subj
		}
		tbl.AddRow(table.Row{Cells: []string{
			marker,
			fmt.Sprintf("%d", e.ID),
			e.FromShort,
			e.Group,
			subj,
		}})
	}
	b.WriteString(tbl.Render())
	b.WriteString("\n\n")
	b.WriteString(inboxHelpStyle.Render(
		"  ↑/↓ nav • enter read • r refresh • q quit"))
	return b.String()
}

func (m *inboxWatchModel) renderReadView() string {
	var b strings.Builder
	b.WriteString(inboxReadingStyle.Render(fmt.Sprintf("  Reading message #%d", m.readingID)))
	if m.statusMsg != "" {
		b.WriteString("  ")
		b.WriteString(inboxHelpStyle.Render(m.statusMsg))
	}
	b.WriteString("\n\n")
	b.WriteString(m.readingBody)
	b.WriteString("\n\n")
	if m.replyFocused {
		b.WriteString(inboxReadingStyle.Render("  Reply:"))
		b.WriteString("\n")
		b.WriteString(m.replyTextarea.View())
		b.WriteString("\n\n")
		b.WriteString(inboxHelpStyle.Render("  ctrl+enter send • esc cancel"))
	} else {
		b.WriteString(inboxHelpStyle.Render("  r reply • esc/q back to list"))
	}
	return b.String()
}
