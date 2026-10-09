package startupctx

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Boundary names the context boundary a re-injection follows.
type Boundary string

const (
	// BoundaryCompact is a compaction (automatic or manual): the first turn
	// was summarized, so identity AND the durable guidance are re-injected.
	BoundaryCompact Boundary = "compact"
	// BoundaryClear is /clear: the conversation restarted empty on purpose,
	// so only identity is re-injected — the operator cleared the rest.
	BoundaryClear Boundary = "clear"
)

// Reinjection is a composed re-injection message.
type Reinjection struct {
	// GroupID is the group the message is addressed under (0 = none).
	GroupID int64
	Body    string
}

// ComposeReinjection builds the re-injection message for agentID after
// boundary, reading everything from the database. It returns a zero value
// (empty Body) when the agent has nothing worth re-injecting: no spawn
// snapshot and no group membership means tclaude never briefed it.
//
// What goes in, and what deliberately does not:
//
//   - Identity (name, role, groups, spawner, worktree) — always.
//   - The group's startup context, read LIVE from the group row so an edit
//     made after spawn reaches the agent, and only when the spawn did not opt
//     out of it. The group is the spawn group while the agent is still a
//     member, otherwise the oldest-joined group; other groups are listed by
//     name only.
//   - The profile/role startup context as it resolved at spawn.
//   - A POINTER to the original briefing and the task link, never the brief
//     itself: a long-running agent's brief is partly done, and the compaction
//     summary carries the current state better than the original wording.
//
// The harness reloads CLAUDE.md/AGENTS.md and its skill list on its own, and
// standing orders have their own compaction trigger, so none of those are
// repeated here. After /clear only the identity is re-injected.
//
// The primary group's reinject_after_compact setting can narrow this:
// "identity" re-injects identity only after a compaction too, and "off"
// re-injects nothing at either boundary.
func ComposeReinjection(agentID string, boundary Boundary) (Reinjection, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return Reinjection{}, nil
	}
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil || !a.Active() {
		return Reinjection{}, err
	}
	snap, err := db.GetAgentStartupSnapshot(agentID)
	if err != nil {
		return Reinjection{}, err
	}
	memberships, err := db.ListAgentGroupMembershipsByJoin(agentID)
	if err != nil {
		return Reinjection{}, err
	}
	if snap == nil && len(memberships) == 0 {
		return Reinjection{}, nil
	}
	identityOnly := false

	primary := primaryMembership(snap, memberships)
	// The primary group's reinject_after_compact setting decides how much is
	// re-injected: off suppresses the message, identity trims a compaction's
	// message down to what a /clear gets.
	if primary != nil {
		switch primary.Group.EffectiveReinjectAfterCompact() {
		case db.ReinjectOff:
			return Reinjection{}, nil
		case db.ReinjectIdentity:
			identityOnly = true
		}
	}
	var groupName, role, descr string
	var groupID int64
	if primary != nil {
		groupID = primary.Group.ID
		groupName = primary.Group.Name
		role = primary.Role
		descr = primary.Descr
	}

	attribution := "you are a tclaude agent"
	var worktreePath, worktreeBranch string
	if snap != nil {
		attribution = "spawned by the human"
		if spawner := actorName(snap.SpawnedByAgent); spawner != "" {
			attribution = "spawned by " + spawner
		} else if snap.SpawnedByAgent != "" {
			attribution = "spawned by another agent"
		}
		worktreePath, worktreeBranch = snap.WorktreePath, snap.WorktreeBranch
	}
	identity := IdentityPrefix(attribution, actorName(agentID), role, descr,
		groupName, worktreePath, worktreeBranch)
	if others := otherGroupNames(memberships, primary); len(others) > 0 {
		identity += " Also a member of: " + strings.Join(others, ", ") + "."
	}

	var lead string
	switch {
	case boundary == BoundaryClear:
		lead = "Your conversation was cleared. tclaude re-injected your agent identity so you keep your bearings; this is not a new request."
	case identityOnly:
		lead = "Your context was compacted. tclaude re-injected your agent identity so you keep your bearings; this is not a new request — continue your current work."
	default:
		lead = "Your context was compacted. tclaude re-injected the durable startup context you were spawned with, since the compaction summary may have dropped it. This is standing guidance, not a new request — continue your current work."
	}
	sections := []string{lead + "\n\n" + identity}

	if boundary != BoundaryClear && !identityOnly {
		groupContext := ""
		if primary != nil && (snap == nil || snap.IncludeGroupContext) {
			groupContext = primary.Group.DefaultContext
		}
		profileContext := ""
		if snap != nil {
			profileContext = snap.ProfileContext
		}
		if body := ContextBody(groupName, groupContext, profileContext, "", nil); body != "" {
			sections = append(sections, body)
		}
		if ref := briefReference(agentID, snap); ref != "" {
			sections = append(sections, ref)
		}
	}
	return Reinjection{GroupID: groupID, Body: strings.Join(sections, sectionDivider)}, nil
}

// primaryMembership picks the group whose context is re-injected: the spawn
// group while the agent is still in it, otherwise the oldest-joined group.
func primaryMembership(snap *db.AgentStartupSnapshot, memberships []db.AgentGroupMembership) *db.AgentGroupMembership {
	if len(memberships) == 0 {
		return nil
	}
	if snap != nil && snap.SpawnGroupID > 0 {
		for i := range memberships {
			if memberships[i].Group.ID == snap.SpawnGroupID {
				return &memberships[i]
			}
		}
	}
	return &memberships[0]
}

func otherGroupNames(memberships []db.AgentGroupMembership, primary *db.AgentGroupMembership) []string {
	var out []string
	for i := range memberships {
		if primary != nil && memberships[i].Group.ID == primary.Group.ID {
			continue
		}
		out = append(out, fmt.Sprintf("%q", memberships[i].Group.Name))
	}
	return out
}

// briefReference points at the original startup briefing and the task link.
func briefReference(agentID string, snap *db.AgentStartupSnapshot) string {
	var lines []string
	if snap != nil && snap.BriefMessageID > 0 {
		lines = append(lines, fmt.Sprintf(
			"Your original startup briefing is inbox message #%d. Re-read it with `tclaude agent inbox read %d` only if you need its exact wording; your compaction summary carries the current state of the task.",
			snap.BriefMessageID, snap.BriefMessageID))
	}
	if ref, err := db.GetAgentTaskRef(agentID); err == nil && ref.URL != "" {
		line := "Your task link: " + ref.URL
		if ref.Label != "" {
			line += " (" + ref.Label + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// actorName resolves an agent to the name a human gave it: the conversation's
// custom title, or the spawn-time pending name. It deliberately does not fall
// back to a summary or first prompt — those are not names, and a first
// prompt can be an entire briefing.
func actorName(agentID string) string {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return ""
	}
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil {
		return ""
	}
	if row, err := db.GetConvIndex(a.CurrentConvID); err == nil && row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	return a.PendingName
}
