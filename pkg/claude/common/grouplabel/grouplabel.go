// Package grouplabel names an agent the way its terminal does: its name after
// its group's shortest unique prefix and a slash, "t/lead" for the lead of
// tclaude and "c/lead" for the lead of counterslop (tk_c501cda3). The
// dashboard draws the same label (dashboard/js/agent-group-prefix.js); this
// is its Go twin, for the tmux window title and the status bar.
//
// A group's prefix is the shortest start of its name that no other group's
// name on this node starts with, compared without case, so it stays the same
// whichever terminals are open. A name that another starts with ("web" beside
// "webshop") is given in full.
package grouplabel

import (
	"strings"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Prefixes maps each group name to its prefix.
func Prefixes(names []string) map[string]string {
	seen := make(map[string]bool, len(names))
	var unique []string
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	folded := make([][]rune, len(unique))
	for i, name := range unique {
		folded[i] = []rune(strings.ToLower(name))
	}
	out := make(map[string]string, len(unique))
	for i, name := range unique {
		runes := []rune(name)
		length := 1
		for ; length < len(runes); length++ {
			start := string(folded[i][:min(length, len(folded[i]))])
			shared := false
			for j, other := range folded {
				if j != i && strings.HasPrefix(string(other), start) {
					shared = true
					break
				}
			}
			if !shared {
				break
			}
		}
		out[name] = string(runes[:length])
	}
	return out
}

// Label is an agent's name and its group's prefix; Prefix is empty for an
// agent in no group.
type Label struct {
	Prefix string
	Name   string
}

// String is "t/lead", or the bare name.
func (l Label) String() string {
	if l.Prefix == "" {
		return l.Name
	}
	return l.Prefix + "/" + l.Name
}

// ForSession is the label of the agent a tclaude session runs, read from the
// database: its name as the dashboard shows it, and the prefix of the group
// it was spawned into, else its first group by name. ok is false for a
// session with no agent (a plain shell) or no name yet.
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
	row, _ := db.GetConvIndex(convID)
	name := db.AgentDisplayTitle(row, agent.PendingName)
	if name == "" {
		return Label{}, false
	}
	label := Label{Name: name}
	groups, err := db.ListGroupsForAgent(agent.AgentID)
	if err != nil || len(groups) == 0 {
		return label, true
	}
	group := groups[0].Name
	if spawn, err := db.AgentSpawnGroupIDs(); err == nil {
		for _, g := range groups {
			if g.ID == spawn[agent.AgentID] {
				group = g.Name
				break
			}
		}
	}
	all, err := db.ListAgentGroups()
	if err != nil {
		return label, true
	}
	names := make([]string, 0, len(all))
	for _, g := range all {
		names = append(names, g.Name)
	}
	label.Prefix = Prefixes(names)[group]
	if label.Prefix == "" {
		label.Prefix = group
	}
	return label, true
}

// maxTitleRunes bounds a label in a window title.
const maxTitleRunes = 80

// TitleText is text safe to put in a tmux option that tmux expands as a
// format (set-titles-string): one line of printable characters, without '#',
// which starts every tmux format sequence ("#(...)" runs a command), and at
// most maxTitleRunes long.
func TitleText(text string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range text {
		switch {
		case r == '#':
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
