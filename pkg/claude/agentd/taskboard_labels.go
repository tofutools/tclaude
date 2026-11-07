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
// in the same listing, has the same name, and stands alone when it has none.
// Callers learn every agent of a listing before labelling any of it.
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

// taskboardAgentDetailKinds are the events whose detail tclaude writes with
// agent ids in it ("→ agt_…", "from agt_…"). Other details are free text a
// person wrote (a note, a reason) and are never rewritten.
var taskboardAgentDetailKinds = map[string]bool{
	db.GroupTaskEventAssigned: true, db.GroupTaskEventUnassigned: true,
	db.GroupTaskEventTaken: true, db.GroupTaskEventReleased: true,
}

// detailIDs is the agent ids in an event's detail, for the kinds that carry
// them.
func detailIDs(kind, detail string) []string {
	if !taskboardAgentDetailKinds[kind] {
		return nil
	}
	return taskboardAgentIDPattern.FindAllString(detail, -1)
}

// learnEvent learns an event's actor and the agents in its detail, so that
// labelling afterwards tells apart every name the history shares.
func (l *taskboardLabeler) learnEvent(actor, kind, detail string) {
	if id, ok := strings.CutPrefix(actor, "agent:"); ok {
		l.learn(id)
	}
	for _, id := range detailIDs(kind, detail) {
		l.learn(id)
	}
}

// detail names the agents in an event's detail, for the kinds tclaude writes
// with agent ids; any other detail is returned as is.
func (l *taskboardLabeler) detail(kind, detail string) string {
	if !taskboardAgentDetailKinds[kind] {
		return detail
	}
	return taskboardAgentIDPattern.ReplaceAllStringFunc(detail, l.label)
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
