package agentd

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tuiShellForm is the "new shell session" prompt: a plain interactive shell in
// its own tmux session, which is a session and not an agent — no conversation,
// no group, no permissions, and so a "(session)" row in the listing rather
// than one of the agents. It asks only what a shell session actually has:
// where to start, and what to call the tmux handle.
type tuiShellForm struct {
	field int

	dir   textinput.Model
	label textinput.Model
	// dirPrefill is the value openShellForm wrote into dir — the directory the
	// console itself was started in. As in the spawn form it separates an
	// untouched field from one the operator has typed into, which is what
	// decides whether Tab completes a path or moves to the next field.
	dirPrefill string
	// dirSuggestions holds the ambiguous Tab-completion candidates for dir,
	// listed under the field until the next keystroke.
	dirSuggestions []string
}

// openShellForm builds a fresh "new shell session" prompt, with the directory
// field on the one the console was started in — the directory the operator was
// standing in when they ran `agentd serve --tui`, and so the one they most
// likely mean. Tab extends it from there.
func (m tuiModel) openShellForm() tuiModel {
	form := tuiShellForm{
		dir:        newTUITextInput("  Directory: "),
		label:      newTUITextInput("  Label:     "),
		dirPrefill: m.startupDir,
	}
	form.dir.SetValue(m.startupDir)
	form.dir.CursorEnd()
	form.dir.Focus()
	m.shell = form
	m.mode = tuiModeShell
	return m
}

// moveShellField shifts focus through the shell form, wrapping, and moves the
// text-input focus with it. Both fields are text inputs, so unlike the spawn
// form there is no choice field to blur into.
func (m tuiModel) moveShellField(delta int) tuiModel {
	m.shell.field = ((m.shell.field+delta)%tuiShellFieldCount + tuiShellFieldCount) % tuiShellFieldCount
	m.shell.dir.Blur()
	m.shell.label.Blur()
	switch m.shell.field {
	case tuiShellFieldDir:
		m.shell.dir.Focus()
	case tuiShellFieldLabel:
		m.shell.label.Focus()
	}
	return m
}

func (m tuiModel) updateFocusedShellInput(msg tea.Msg) (tuiModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.shell.field {
	case tuiShellFieldDir:
		m.shell.dir, cmd = m.shell.dir.Update(msg)
	case tuiShellFieldLabel:
		m.shell.label, cmd = m.shell.label.Update(msg)
	}
	return m, cmd
}

// completingShellDir is the shell form's half of completingDir, with the same
// gates for the same reasons: completion reads the daemon host's filesystem
// outside any agent sandbox, so it is offered to an operator console on the
// daemon host only, and only on a path the operator has typed into — on the
// field as the form left it, Tab keeps its ordinary next-field job, which is
// the only way to reach Label at all.
func (m tuiModel) completingShellDir() bool {
	if !m.operator || !m.capabilities.completeLocalDir || m.shell.field != tuiShellFieldDir {
		return false
	}
	value := m.shell.dir.Value()
	return value != "" && value != m.shell.dirPrefill
}

func (m tuiModel) completeShellDir() tuiModel {
	completed, candidates := clcommon.CompleteDirPath(m.shell.dir.Value())
	m.shell.dir.SetValue(completed)
	m.shell.dir.CursorEnd()
	m.shell.dirSuggestions = candidates
	return m
}

// startShellSelected opens the shell form, refusing consoles that cannot run
// the launch. Starting a shell is host-local like attaching: it creates a tmux
// session on the daemon's own host, in the daemon's own filesystem and outside
// any agent sandbox, and then hands this terminal to it. It also goes nowhere
// near the daemon's /v1 API — there is no HTTP shape for it and no permission
// to check, because a shell session is not an agent — so the operator gate here
// is the whole gate, exactly as it is for attachSelected.
func (m tuiModel) startShellSelected() tuiModel {
	if !m.operator {
		m.notice = "Only an operator console can start a shell session."
		return m
	}
	if !m.capabilities.startLocalShell {
		m.notice = "This console cannot start a shell session — it does not share the daemon's host."
		return m
	}
	if m.startingShell {
		m.notice = "A shell session is already starting."
		return m
	}
	m.notice = ""
	return m.openShellForm()
}

// submitShell turns the shell form into a launch. A blank directory is left
// blank on purpose: session.StartShellSession then falls back to the daemon
// process's own working directory, which is what the field was prefilled with
// anyway.
func (m tuiModel) submitShell() (tuiModel, tea.Cmd) {
	if m.startingShell {
		m.notice = "A shell session is already starting."
		return m, nil
	}
	dir := strings.TrimSpace(m.shell.dir.Value())
	label := strings.TrimSpace(m.shell.label.Value())
	// The label is the tmux handle verbatim, so it is charset-gated before the
	// launch. The launch itself refuses an unsafe one anyway; checking here
	// keeps the form open on the field the operator has to fix, instead of
	// closing it and reporting the refusal over the listing.
	if err := session.ValidateSessionLabel(label); err != nil {
		m.notice = "Cannot use that label: " + err.Error()
		return m, nil
	}
	m.mode = tuiModeList
	m.startingShell = true
	m.notice = "Starting a shell session…"
	return m, tuiStartShellCmd(dir, label)
}

// tuiStartShell is the shell-session launch, indirected through a package var
// so tests can drive the form without a tmux server.
var tuiStartShell = session.StartShellSession

// tuiStartShellCmd creates the shell session on its own goroutine: the launch
// talks to tmux and the session DB, and the console must stay responsive while
// it does. It creates the session detached and returns its handle; attaching is
// the caller's own step, so bubbletea still owns the terminal when this lands.
func tuiStartShellCmd(dir, label string) tea.Cmd {
	return func() tea.Msg {
		created, err := tuiStartShell(dir, label)
		return tuiShellStartedMsg{created: created, err: err}
	}
}

func (m tuiModel) renderShellForm() string {
	var b strings.Builder
	b.WriteString("\n  New shell session\n\n")
	b.WriteString("  A plain interactive shell in its own tmux session — a session, not an\n")
	b.WriteString("  agent: no conversation and no group, so it joins the list behind this\n")
	b.WriteString("  form as a \"" + tuiSessionGroupCell + "\" row. Also `tclaude session ls`.\n\n")

	b.WriteString(m.shell.dir.View() + m.shellDirHint())
	b.WriteString("\n")
	// Always emit this line, blank or not, so a candidate list appearing and
	// disappearing doesn't shift the fields below it up and down.
	b.WriteString(m.dirSuggestionLine(m.shell.dirSuggestions) + "\n")
	b.WriteString(m.shell.label.View() +
		tuiHint(m.shell.label.Value() == "",
			"  (the tmux handle: letters, digits, '-' and '_'; blank generates one)"))
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString("\n  " + m.notice + "\n")
	}
	completeHint := ""
	if m.operator && m.capabilities.completeLocalDir {
		completeHint = "tab complete dir • "
	}
	b.WriteString("\n  enter start + attach • ↑/↓/tab next field • " + completeHint + "esc cancel\n")
	return b.String()
}

// shellDirHint explains the directory field's state: a blank field lands in the
// daemon's own working directory, and an untouched prefill says it is the
// directory this console was started in rather than a path someone typed.
func (m tuiModel) shellDirHint() string {
	value := m.shell.dir.Value()
	switch {
	case value == "":
		return "  (blank = the directory agentd is running in)"
	case m.shell.dirPrefill != "" && value == m.shell.dirPrefill:
		return "  (where this console was started — edit it to start elsewhere)"
	default:
		return ""
	}
}
