package agentd

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The Steward is a scribe with a place in the fleet: the operator sets its
// access in config, and any agent can reach it by that name.

const (
	stewardScopeKind = "steward"
	stewardGroupName = "steward"
	// stewardAddress is what agents write to reach the Steward, whatever its
	// generated name is.
	stewardAddress = "steward"
)

// stewardBaseSlugs are what talking to the operator takes, at every level:
// seeing what they see, pointing at it, drawing with them and asking them.
var stewardBaseSlugs = []string{
	PermFleetRead, PermDashboardViewRead, PermDashboardGuide, PermBoardsRead, PermBoardsWrite, PermHumanNotify,
}

// stewardOperatorReadSlugs bring a Steward to what the operator reads. Left out
// on purpose: reading other agents' mail (agent.inbox-watch), private setup text
// (config export), and the proxy.* reads, which spend the operator's GitHub,
// Linear and AWB credentials outside tclaude.
var stewardOperatorReadSlugs = []string{
	PermProcessTemplatesRead, PermProcessRunsRead, PermTriggersRead, PermUsageRead, PermHostRead, PermCostsRead,
	PermAgentContextInfo,
}

// stewardSlugs is the access a Steward is born with under this config. The
// daemon decides it, not the summoning client.
func stewardSlugs(cfg *config.Config) []string {
	slugs := append([]string{}, stewardBaseSlugs...)
	if cfg.StewardReads() == config.StewardReadsOperator {
		slugs = append(slugs, stewardOperatorReadSlugs...)
	}
	switch cfg.StewardWrites() {
	case config.StewardWritesMessaging:
		slugs = append(slugs, PermMessageDirect)
	case config.StewardWritesWorkers:
		slugs = append(slugs, PermMessageDirect, PermAgentSpawn, PermAgentTask)
	}
	return slugs
}

// stewardAccessNote tells a Steward what it was born with, for its brief.
func stewardAccessNote(cfg *config.Config) string {
	reads := "the fleet, boards and the dashboard view"
	if cfg.StewardReads() == config.StewardReadsOperator {
		reads = "what the operator reads across tclaude"
	}
	writes := "drawing on boards and notifying the operator; everything else needs approval"
	switch cfg.StewardWrites() {
	case config.StewardWritesMessaging:
		writes = "messaging any agent, drawing on boards and notifying the operator; everything else needs approval"
	case config.StewardWritesWorkers:
		writes = "messaging any agent, starting workers, drawing on boards and notifying the operator; everything else needs approval"
	}
	return "Your access, set by the operator: you can read " + reads + ". You can write: " + writes + "."
}

// liveStewardConvs lists the running Stewards' conversations, newest last.
func liveStewardConvs() []string {
	g, err := db.GetAgentGroupByName(stewardGroupName)
	if err != nil || !isScribeGroup(g) {
		return nil
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil
	}
	descr := scribeScopeDescr(stewardScopeKind + "/")
	var convs []string
	for _, member := range members {
		if member.Descr != descr || pickAliveSession(member.ConvID) == nil {
			continue
		}
		if state, err := db.AgentState(member.ConvID); err == nil && state == db.AgentStateActive {
			convs = append(convs, member.ConvID)
		}
	}
	return convs
}

// isStewardAddress reports whether a message target means "the Steward".
func isStewardAddress(to string) bool {
	return strings.EqualFold(strings.TrimSpace(to), stewardAddress) && collabRoutesEnabled()
}

// reachableAsSteward reports whether conv is a live Steward. Any agent may
// message one, so a worker can always answer it and ask it for help. What a
// Steward does with such mail stays bound by its own access.
func reachableAsSteward(conv string) bool {
	if !collabRoutesEnabled() {
		return false
	}
	for _, steward := range liveStewardConvs() {
		if steward == conv {
			return true
		}
	}
	return false
}
