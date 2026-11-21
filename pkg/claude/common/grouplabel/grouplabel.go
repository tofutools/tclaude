// Package grouplabel names an agent the way its terminal does: its name after
// its full group, "tclaude/lead" (tk_c501cda3, tk_0a10cbcb). The dashboard's
// title bars name it the same way (dashboard/js/agent-group-prefix.js); this
// is the tmux window title's and the status bar's.
package grouplabel

import (
	"strings"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Label is an agent's name and its group; Group is empty for an agent in no
// group.
type Label struct {
	Group string
	Name  string
}

// String is "tclaude/lead", or the bare name.
func (l Label) String() string {
	if l.Group == "" {
		return l.Name
	}
	return l.Group + "/" + l.Name
}

// ForSession is the label of the agent a tclaude session runs, read from the
// database: the name it was given (its custom title, else its spawn-time
// name), and the group it was spawned into, else its first group by name. ok
// is false for a session with no agent (a plain shell) or no given name: a
// conversation's summary or first prompt is not a name, and stays out of
// window titles.
func ForSession(sessionID string) (Label, bool) {
	if sessionID == "" {
		return Label{}, false
	}
	convID, _, err := db.GetSessionConvAttribution(sessionID)
	if err != nil || convID == "" {
		return Label{}, false
	}
	agent, err := db.GetAgentByConv(convID)
	if err != nil || agent == nil || agent.AgentID == "" {
		return Label{}, false
	}
	name := agent.PendingName
	if row, _ := db.GetConvIndex(convID); row != nil && row.CustomTitle != "" {
		name = row.CustomTitle
	}
	if name == "" {
		return Label{}, false
	}
	label := Label{Name: name}
	groups, err := db.ListGroupsForAgent(agent.AgentID)
	if err != nil || len(groups) == 0 {
		return label, true
	}
	label.Group = groups[0].Name
	if snap, err := db.GetAgentStartupSnapshot(agent.AgentID); err == nil && snap != nil && snap.SpawnGroupID != 0 {
		for _, g := range groups {
			if g.ID == snap.SpawnGroupID {
				label.Group = g.Name
				break
			}
		}
	}
	return label, true
}

// dropped are the characters TitleText leaves out.
const dropped = "#%:'\"`\u2018\u2019\u201a\u201b\u201c\u201d\u201e"

// maxTitleRunes bounds a label in a window title.
const maxTitleRunes = 80

// TitleText is text safe to put in a window title: one line of printable
// characters at most maxTitleRunes long, without
//   - '#', which starts every tmux format sequence in set-titles-string
//     ("#(...)" runs a command), and '%', which tmux expands as strftime;
//   - ':', so a name cannot pass for another session's "tclaude:<id>",
//     which window focus and tiling find windows by;
//   - quote characters, ASCII and the typographic single quotes PowerShell
//     also reads as quotes: the WSL focus path hands a matched title back
//     to a PowerShell script.
func TitleText(text string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range text {
		switch {
		case strings.ContainsRune(dropped, r):
			continue
		case unicode.IsSpace(r) || !unicode.IsPrint(r):
			space = b.Len() > 0
			continue
		}
		if n >= maxTitleRunes {
			break
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
