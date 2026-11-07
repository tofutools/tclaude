package agentd

import (
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboardLabeler names agents for task board text a person reads: the
// writer lock and a task's history. An agent is named by its display name;
// its short id follows only when another agent of the group, or another agent
// in the same text, has the same name, and stands alone when it has none.
type taskboardLabeler struct {
	names map[string]string // agent id → display name ("" unknown)
	count map[string]int    // display name → agents carrying it
}

func newTaskboardLabeler(g *db.AgentGroup) *taskboardLabeler {
	l := &taskboardLabeler{names: map[string]string{}, count: map[string]int{}}
	if g == nil {
		return l
	}
	if members, err := db.ListAgentGroupMembers(g.ID); err == nil {
		for _, m := range members {
			if id, err := db.AgentIDForConv(m.ConvID); err == nil && id != "" {
				l.learn(id)
			}
		}
	}
	return l
}

func (l *taskboardLabeler) learn(id string) string {
	if name, ok := l.names[id]; ok {
		return name
	}
	name := taskboardAgentName(id)
	l.names[id] = name
	if name != "" {
		l.count[name]++
	}
	return name
}

// label is how agent id reads.
func (l *taskboardLabeler) label(id string) string {
	name := l.learn(id)
	short := agent.ShortAgentID(id, "")
	switch {
	case name == "":
		return short
	case l.count[name] > 1:
		return name + " (" + short + ")"
	default:
		return name
	}
}

var taskboardAgentIDPattern = regexp.MustCompile(`agt_[0-9a-f]{8,32}`)

// text names every agent id in s.
func (l *taskboardLabeler) text(s string) string {
	// Learn them all first, so a name two of them share is told apart in
	// both places.
	for _, id := range taskboardAgentIDPattern.FindAllString(s, -1) {
		l.learn(id)
	}
	return taskboardAgentIDPattern.ReplaceAllStringFunc(s, l.label)
}

// actor names an event or record actor: "agent:<id>" by the agent,
// "human:operator" as the operator, anything else as stored.
func (l *taskboardLabeler) actor(actor string) string {
	if id, ok := strings.CutPrefix(actor, "agent:"); ok {
		return l.label(id)
	}
	if actor == "human:operator" {
		return "operator"
	}
	return actor
}
