package agentd

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func (m tuiModel) renderSpawnForm() string {
	var b strings.Builder
	b.WriteString("\n  New agent\n\n")

	group := m.selectedGroup()
	if group == "" {
		group = "(no groups — create one with `tclaude agent groups create <name>`)"
	}
	b.WriteString(tuiChoiceLine("  Group:     ", group, "", m.form.field == tuiFieldGroup))
	b.WriteString(tuiChoiceLine("  Profile:   ", m.profileChoice(), m.profileChoiceHint(),
		m.form.field == tuiFieldProfile))
	b.WriteString(m.form.name.View() + tuiHint(m.form.name.Value() == "", "  (blank = auto-generated)"))
	b.WriteString("\n")
	b.WriteString(m.form.dir.View() + m.dirHint())
	b.WriteString("\n")
	// Always emit this line, blank or not, so a candidate list appearing and
	// disappearing doesn't shift the fields below it up and down.
	b.WriteString(m.dirSuggestionLine(m.form.dirSuggestions) + "\n")
	b.WriteString(m.worktreeLines())
	harnessChoice := tuiHarnessDefault
	if m.form.harnessIdx >= 0 && m.form.harnessIdx < len(m.form.harnessNames) {
		harnessChoice = m.form.harnessNames[m.form.harnessIdx]
	}
	b.WriteString(tuiChoiceLine("  Harness:   ", harnessChoice,
		tuiHint(harnessChoice == tuiHarnessDefault,
			"  (the profile chain decides — "+harness.DefaultName+" if nothing pins one)"),
		m.form.field == tuiFieldHarness))
	b.WriteString(m.form.model.View() + tuiHint(m.form.model.Value() == "", "  (blank = profile or harness default)") + "\n")
	b.WriteString(m.form.effort.View() + tuiHint(m.form.effort.Value() == "", "  (blank = profile or harness default)") + "\n")
	b.WriteString(m.form.brief.View() + m.briefHint())
	b.WriteString("\n")

	if m.notice != "" {
		b.WriteString("\n  " + m.notice + "\n")
	}
	// Name tab-completion only where it works, the same way keyHintLine names
	// enter only for a console that can attach.
	completeHint := ""
	if m.operator && m.capabilities.completeLocalDir {
		completeHint = "tab complete dir • "
	}
	spawnHint := "enter spawn"
	if m.operator && m.capabilities.attachAgent {
		spawnHint = "enter spawn + go to its pane"
	}
	cycleHint := "←/→ change group/profile/harness"
	if m.canCreateWorktree() {
		cycleHint = "←/→ change group/profile/worktree/harness"
	}
	b.WriteString("\n  " + spawnHint + " • ↑/↓/tab next field • " + completeHint +
		cycleHint + " • esc cancel\n")
	return b.String()
}

// worktreeLines renders the worktree picker and, when it is asking for a new
// worktree, the branch field under it.
//
// A console that may not create worktrees still gets the line, saying so: the
// field is one of the form's answers, and silently dropping it would leave an
// operator wondering whether this console simply has an older form.
func (m tuiModel) worktreeLines() string {
	if !m.canCreateWorktree() {
		// Rendered without the < > a picker has: there is nothing to cycle.
		return "  Worktree:  " + tuiWorktreeNone +
			"  (operator consoles only — the worktree is created on the daemon's host)\n"
	}
	choice := tuiWorktreeNone
	if m.form.worktreeIdx >= 0 && m.form.worktreeIdx < len(m.form.worktreeNames) {
		choice = m.form.worktreeNames[m.form.worktreeIdx]
	}
	out := tuiChoiceLine("  Worktree:  ", choice, m.worktreeChoiceHint(),
		m.form.field == tuiFieldWorktree)
	if !m.creatingWorktree() {
		return out
	}
	return out + m.form.branch.View() + m.branchHint() + "\n"
}

// worktreeChoiceHint says what each setting of the picker does, in the terms
// the operator is choosing between: where the agent ends up.
func (m tuiModel) worktreeChoiceHint() string {
	if !m.creatingWorktree() {
		return "  (the agent starts in the directory above)"
	}
	return "  (cut from the repo the directory above is in; the agent starts inside it)"
}

// branchHint explains the branch field's state: whether it is still following
// the name, that an existing branch is checked out rather than refused, and —
// on a blank one — that the submit will ask for a name rather than invent one.
func (m tuiModel) branchHint() string {
	switch {
	case strings.TrimSpace(m.form.branch.Value()) == "":
		return "  (name the branch, or set Worktree to " + tuiWorktreeNone + ")"
	case m.form.branchSynced:
		return "  (following the name — type here to choose your own)"
	default:
		return "  (an existing branch is checked out; a new one is cut from the default branch)"
	}
}

// dirHint explains the directory field's state: a blank field still lands in
// the group's default directory (the daemon's own fallback), and an untouched
// prefill says it is that same directory rather than a path someone typed —
// which is what makes it obvious it can be extended.
func (m tuiModel) dirHint() string {
	value := m.form.dir.Value()
	switch {
	case value == "":
		return "  (blank = the group's default directory)"
	case m.form.dirPrefill != "" && value == m.form.dirPrefill:
		return "  (the group's directory — add a subdirectory to start below it)"
	default:
		return ""
	}
}

// profileChoice is the profile picker's current value.
func (m tuiModel) profileChoice() string {
	if m.form.profileIdx >= 0 && m.form.profileIdx < len(m.form.profileNames) {
		return m.form.profileNames[m.form.profileIdx]
	}
	return tuiProfileDefault
}

// profileChoiceHint says what the picker's state means. "(default)" is the
// easy one to misread — it looks like "no profile", when what it actually
// does is leave the group's and the global default profile in force.
//
// An empty picker is the other misreadable state, and it has three causes
// that ask different things of the operator: the list has not arrived, the
// list could not be read, or there really are no profiles. Only the last one
// is worth pointing at `profiles create`.
func (m tuiModel) profileChoiceHint() string {
	if m.profileChoice() != tuiProfileDefault {
		return ""
	}
	if len(m.form.profileNames) <= 1 {
		switch {
		case m.profilesErr != "":
			return "  (profile list unavailable — press r to retry)"
		case m.lastRefresh.IsZero():
			return "  (profile list has not loaded yet — press r)"
		default:
			return "  (none saved — `tclaude agent profiles create`)"
		}
	}
	return "  (the group's or global default profile still applies)"
}

// dirSuggestionLine renders the Tab-completion candidates on a single line,
// dropping the ones that don't fit. A directory with many children would
// otherwise wrap and push the rest of the form around — the one thing the
// always-emitted line above is there to prevent.
func (m tuiModel) dirSuggestionLine(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	const indent = "  "
	if m.width <= len(indent) {
		// No width yet (no WindowSizeMsg): show them all rather than nothing.
		return indent + strings.Join(suggestions, "  ")
	}
	line := indent
	for i, name := range suggestions {
		next := name
		if i > 0 {
			next = "  " + name
		}
		more := fmt.Sprintf("  (+%d more)", len(suggestions)-i)
		if lipgloss.Width(line+next) > m.width {
			if lipgloss.Width(line+more) <= m.width {
				line += more
			}
			break
		}
		line += next
	}
	return line
}

// tuiChoiceLine renders one of the cycled fields, with an optional trailing
// hint. The focused one is marked with a caret rather than a color, since the
// console has no palette (the text fields mark focus with their own cursor).
func tuiChoiceLine(label, value, hint string, focused bool) string {
	if focused {
		label = "> " + strings.TrimPrefix(label, "  ")
	}
	return label + "< " + value + " >" + hint + "\n"
}

// tuiHint appends an inline explanation to an empty field.
// briefHint says what an empty Brief box actually produces. Usually nothing —
// but a selected profile carrying a task-brief default fills it, and an agent
// arriving with a brief the operator never typed, from a box that read "blank =
// no startup brief", is exactly the surprise this line exists to prevent.
func (m tuiModel) briefHint() string {
	if m.form.brief.Value() != "" {
		return ""
	}
	if prof, ok := m.selectedProfileRow(); ok && strings.TrimSpace(prof.InitialMessage) != "" {
		return "  (blank = the profile's own brief)"
	}
	return "  (blank = no startup brief)"
}

func tuiHint(show bool, hint string) string {
	if !show {
		return ""
	}
	return hint
}
