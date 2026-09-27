package pickup

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/common/tuistyle"
	"github.com/tofutools/tclaude/pkg/common"
)

type watchParams struct {
	Interval time.Duration `long:"interval" default:"5s" help:"How often to refresh the status"`
}

func watchCmd() *cobra.Command {
	return boa.CmdT[watchParams]{
		Use:         "watch",
		Short:       "Interactive view of the pickup processes, with reset",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *watchParams, _ *cobra.Command, _ []string) {
			if err := runWatch(p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(agent.RCIOFailure)
			}
		},
	}.ToCobra()
}

type watchStyles struct {
	header, selected, help, danger, good, warn, dim, info lipgloss.Style
}

func newWatchStyles(scheme string) watchStyles {
	p := tuistyle.Resolve(scheme)
	selected := lipgloss.NewStyle().Bold(true).Background(lipgloss.Color(p.SelectedBg))
	if p.SelectedFg != "" {
		selected = selected.Foreground(lipgloss.Color(p.SelectedFg))
	}
	return watchStyles{
		header:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Header)),
		selected: selected,
		help:     lipgloss.NewStyle().Foreground(lipgloss.Color(p.Help)),
		danger:   lipgloss.NewStyle().Foreground(lipgloss.Color(p.Danger)),
		good:     lipgloss.NewStyle().Foreground(lipgloss.Color(p.Working)),
		warn:     lipgloss.NewStyle().Foreground(lipgloss.Color(p.Idle)),
		dim:      lipgloss.NewStyle().Foreground(lipgloss.Color(p.Exited)),
		info:     lipgloss.NewStyle().Foreground(lipgloss.Color(p.Info)),
	}
}

type (
	tickMsg   time.Time
	statusMsg struct {
		list agent.AWBPickupList
		err  error
	}
	resetMsg struct {
		resp agent.AWBPickupResetResponse
		err  error
	}
	fetchFunc func() (agent.AWBPickupList, error)
	resetFunc func(process, issueID string) (agent.AWBPickupResetResponse, error)
)

// resetTarget pins the process AND issue the confirmation was opened for, so
// a refresh that lands while the prompt is up cannot redirect the reset onto a
// different issue the process picked up in the meantime.
type resetTarget struct{ process, issueID string }

type watchModel struct {
	fetch      fetchFunc
	reset      resetFunc
	interval   time.Duration
	styles     watchStyles
	procs      []agent.AWBPickupProcess
	cursor     int
	width      int
	height     int
	loading    bool
	lastErr    string
	notice     string
	updatedAt  time.Time
	confirming *resetTarget
}

func newWatchModel(fetch fetchFunc, reset resetFunc, interval time.Duration, styles watchStyles) watchModel {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return watchModel{fetch: fetch, reset: reset, interval: interval, styles: styles, loading: true, width: 120}
}

func runWatch(p *watchParams) error {
	scheme := ""
	if cfg, err := config.Load(); err == nil {
		scheme = cfg.TUIColorScheme()
	}
	m := newWatchModel(fetchStatus, resetProcess, p.Interval, newWatchStyles(scheme))
	_, err := tea.NewProgram(m).Run()
	return err
}

func (m watchModel) Init() tea.Cmd { return m.fetchCmd() }

func (m watchModel) fetchCmd() tea.Cmd {
	fetch := m.fetch
	return func() tea.Msg {
		list, err := fetch()
		return statusMsg{list: list, err: err}
	}
}

func (m watchModel) tickCmd() tea.Cmd {
	return tea.Tick(m.interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m watchModel) resetCmd(t resetTarget) tea.Cmd {
	reset := m.reset
	return func() tea.Msg {
		resp, err := reset(t.process, t.issueID)
		return resetMsg{resp: resp, err: err}
	}
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		if m.loading {
			return m, m.tickCmd()
		}
		m.loading = true
		return m, m.fetchCmd()
	case statusMsg:
		m.loading = false
		if msg.err != nil {
			m.lastErr = msg.err.Error()
		} else {
			m.lastErr = ""
			m.procs = msg.list.Processes
			m.updatedAt = time.Now()
		}
		m.cursor = min(m.cursor, max(0, len(m.procs)-1))
		return m, m.tickCmd()
	case resetMsg:
		if msg.err != nil {
			m.notice = "Reset failed: " + msg.err.Error()
		} else {
			m.notice = resetSummary(msg.resp)
		}
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.fetchCmd()
	case tea.KeyPressMsg:
		return m.handleKey(msg.String())
	}
	return m, nil
}

func (m watchModel) handleKey(key string) (tea.Model, tea.Cmd) {
	if m.confirming != nil {
		t := *m.confirming
		m.confirming = nil
		if key == "y" || key == "Y" {
			m.notice = "Resetting " + t.process + "…"
			return m, m.resetCmd(t)
		}
		m.notice = "Reset cancelled."
		return m, nil
	}
	m.notice = ""
	switch key {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.procs)-1 {
			m.cursor++
		}
	case "g", "f5":
		if !m.loading {
			m.loading = true
			return m, m.fetchCmd()
		}
	case "r", "x":
		if m.cursor >= len(m.procs) {
			return m, nil
		}
		p := m.procs[m.cursor]
		if p.Dispatch == nil {
			m.notice = "Process " + p.Process + " has no dispatch in flight; nothing to reset."
			return m, nil
		}
		m.confirming = &resetTarget{process: p.Process, issueID: p.Dispatch.IssueID}
	}
	return m, nil
}

func (m watchModel) stateStyle(state string) lipgloss.Style {
	switch state {
	case agent.AWBPickupStateStuck, agent.AWBPickupStateError, agent.AWBPickupStateOrphaned:
		return m.styles.danger
	case agent.AWBPickupStateWorking:
		return m.styles.good
	case agent.AWBPickupStateAgentIdle, agent.AWBPickupStateHeld, agent.AWBPickupStateAwaiting:
		return m.styles.warn
	case agent.AWBPickupStatePolling:
		return m.styles.dim
	}
	return m.styles.info
}

func (m watchModel) View() tea.View {
	var b strings.Builder
	b.WriteString(m.styles.header.Render("AWB pickup processes"))
	switch {
	case m.loading && m.updatedAt.IsZero():
		b.WriteString(m.styles.help.Render("  loading…"))
	case !m.updatedAt.IsZero():
		b.WriteString(m.styles.help.Render("  updated " + m.updatedAt.Format("15:04:05")))
	}
	b.WriteString("\n\n")
	if m.lastErr != "" {
		b.WriteString(m.styles.danger.Render("  " + m.lastErr))
		b.WriteString("\n\n")
	}

	if len(m.procs) == 0 && !m.updatedAt.IsZero() {
		b.WriteString("  No AWB pickup processes configured (agent.awb_proxy.ready_polling).\n")
	} else if len(m.procs) > 0 {
		tbl := table.New(
			table.Column{Header: "Process", MinWidth: 8, Weight: 1, Truncate: true},
			table.Column{Header: "Workspace", MinWidth: 6, Weight: 0.6, Truncate: true},
			table.Column{Header: "State", Width: 9},
			table.Column{Header: "Issue", MinWidth: 8, Weight: 0.8, Truncate: true},
			table.Column{Header: "Issue status", Width: 12, Truncate: true},
			table.Column{Header: "Phase", Width: 8},
			table.Column{Header: "Agent", MinWidth: 8, Weight: 1.2, Truncate: true},
			table.Column{Header: "Session", Width: 10, Truncate: true},
			table.Column{Header: "Since", MinWidth: 6, Weight: 0.6, Truncate: true},
		)
		tbl.Padding = 2
		tbl.SetTerminalWidth(max(m.width-3, 60))
		tbl.HeaderStyle = m.styles.header
		tbl.SelectedStyle = m.styles.selected
		tbl.SelectedIndex = m.cursor
		now := time.Now()
		for _, p := range m.procs {
			r := rowFor(p, now)
			tbl.AddRow(table.Row{
				Cells: []string{r.process, r.workspace, r.state, r.issue, r.issueStatus, r.phase, r.agent, r.session, r.since},
				Style: m.stateStyle(p.State),
			})
		}
		b.WriteString(tbl.Render())
		b.WriteString("\n\n")
		if m.cursor < len(m.procs) {
			b.WriteString(m.detail(m.procs[m.cursor]))
		}
	}

	b.WriteString("\n")
	switch {
	case m.confirming != nil:
		b.WriteString(m.styles.danger.Render(fmt.Sprintf(
			"  Reset %s and release issue %s? The issue and agent are left as they are. [y/n]",
			m.confirming.process, m.confirming.issueID)))
	case m.notice != "":
		b.WriteString(m.styles.info.Render("  " + m.notice))
	default:
		b.WriteString(m.styles.help.Render("  ↑/↓ navigate • r reset selected • g refresh • q quit"))
	}
	b.WriteString("\n")
	return tea.View{Content: b.String(), AltScreen: true}
}

// detail renders everything about the selected process that does not fit a
// table cell: the hint, the issue title and link, and any recorded errors.
func (m watchModel) detail(p agent.AWBPickupProcess) string {
	var lines []string
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, m.styles.header.Render(fmt.Sprintf("  %-12s", label))+" "+value)
		}
	}
	add("Process", p.Process)
	if len(p.Labels) > 0 {
		add("Labels", strings.Join(p.Labels, ", "))
	}
	add("Group", p.Group)
	if p.LastPollAt != nil {
		add("Last poll", p.LastPollAt.Local().Format("15:04:05")+" (every "+p.Interval+")")
	}
	add("Note", p.Hint)
	if d := p.Dispatch; d != nil {
		if d.Issue != nil {
			add("Issue", d.IssueID+" — "+d.Issue.Title)
			add("Link", d.Issue.URL)
			add("Assignees", strings.Join(d.Issue.Assignees, ", "))
			add("PR", d.Issue.PullRequestURL)
			add("Commit", d.Issue.CommitHash)
		} else {
			add("Issue", d.IssueID)
		}
		add("AWB error", d.IssueError)
		add("Agent id", d.AgentID)
		add("Picked up", d.CreatedAt.Local().Format("2006-01-02 15:04:05"))
		add("Dispatch err", d.LatestError)
	}
	if p.Dispatch == nil || p.LastError != p.Hint {
		add("Poll error", p.LastError)
	}
	return strings.Join(lines, "\n") + "\n"
}
