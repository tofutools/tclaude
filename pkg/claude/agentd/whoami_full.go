package agentd

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
)

// GET /v1/whoami/full: the calling agent's own full setup, for tclaude agent
// whoami --full (tk_361933d6). Behind self.introspect, of the caller only.
// Read only.

type whoamiFullResp struct {
	IsHuman bool   `json:"is_human"`
	AgentID string `json:"agent_id,omitempty"`
	ConvID  string `json:"conv_id,omitempty"`
	Title   string `json:"title,omitempty"`
	TaskRef *struct {
		URL   string `json:"url"`
		Label string `json:"label,omitempty"`
	} `json:"task_ref,omitempty"`
	Spawn       whoamiFullSpawn       `json:"spawn"`
	Groups      []whoamiFullGroup     `json:"groups"`
	Startup     whoamiFullStartup     `json:"startup"`
	Reinject    *whoamiFullReinject   `json:"reinject,omitempty"`
	Permissions whoamiFullPermissions `json:"permissions"`
}

// whoamiFullSpawn is how the agent was launched and where it works.
type whoamiFullSpawn struct {
	// Profile and SandboxProfile are the spawn and sandbox profile the spawn
	// named; empty when it named none.
	Profile        string `json:"profile,omitempty"`
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	agentLocationView
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
}

// whoamiFullGroup is one group the agent is in or owns, and the links that
// reach it.
type whoamiFullGroup struct {
	Name     string     `json:"name"`
	Member   bool       `json:"member"`
	Owner    bool       `json:"owner,omitempty"`
	Archived bool       `json:"archived,omitempty"`
	Role     string     `json:"role,omitempty"`
	Descr    string     `json:"descr,omitempty"`
	LinksIn  []linkJSON `json:"links_in,omitempty"`
}

// whoamiFullStartup is the startup briefing the agent was given.
type whoamiFullStartup struct {
	SpawnGroup string `json:"spawn_group,omitempty"`
	SpawnedBy  string `json:"spawned_by,omitempty"`
	// IncludeGroupContext is false when the spawn opted out of the group's
	// startup context (spawn --no-group-context, or the profile's
	// include_group_default_context); it is stored on the agent's startup
	// snapshot.
	IncludeGroupContext bool   `json:"include_group_context"`
	ProfileContext      string `json:"profile_context,omitempty"`
	// BriefMessageID and Brief are the startup briefing as it was delivered:
	// the group context (unless opted out), the profile context and the
	// spawn's initial message, in one inbox message.
	BriefMessageID int64  `json:"brief_message_id,omitempty"`
	Brief          string `json:"brief,omitempty"`
	// Recorded is false for an agent with no startup snapshot (one spawned
	// before snapshots, or enrolled without a spawn).
	Recorded bool `json:"recorded"`
}

// whoamiFullReinject is what tclaude re-injects after a compaction or
// /clear, decided by the primary group.
type whoamiFullReinject struct {
	Group  string `json:"group"`
	Policy string `json:"policy"`
	// GroupContext is the primary group's startup context, read live, as a
	// compaction re-injects it; empty when the spawn opted out or the policy
	// re-injects identity only.
	GroupContext string `json:"group_context,omitempty"`
}

// whoamiFullPermissions is the agent's effective permissions as
// permissions ls . computes them, with member-conferred slugs named by group.
type whoamiFullPermissions struct {
	Effective    []string          `json:"effective"`
	Provenance   map[string]string `json:"provenance"`
	Source       string            `json:"source"`
	OwnerImplied []string          `json:"owner_implied"`
	OwnedGroups  []string          `json:"owned_groups,omitempty"`
	// MemberGroups names, per effective slug membership confers, the groups
	// whose membership confers it, whichever source Provenance names.
	MemberGroups map[string][]string `json:"member_groups,omitempty"`
}

func handleWhoamiFull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if classify(peerFromContext(r.Context())) == classHuman {
		writeJSON(w, http.StatusOK, whoamiFullResp{IsHuman: true})
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	if _, ok := requirePermission(w, r, PermSelfIntrospect); !ok {
		return
	}
	out, err := whoamiFullFor(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func whoamiFullFor(convID string) (whoamiFullResp, error) {
	out := whoamiFullResp{ConvID: convID, Groups: []whoamiFullGroup{}}
	out.Title = agent.FreshTitle(convID)
	if out.Title == agent.UnknownTitle {
		out.Title = "(unnamed)"
	}
	agentID, _ := db.AgentIDForConv(convID)
	out.AgentID = agentID
	if agentID != "" {
		if ref, err := db.GetAgentTaskRef(agentID); err == nil && ref.URL != "" {
			out.TaskRef = &struct {
				URL   string `json:"url"`
				Label string `json:"label,omitempty"`
			}{ref.URL, ref.Label}
		}
	}

	out.Spawn.agentLocationView = locationView(convID)
	if raw, err := db.AgentInitialSpawnConfigForConv(convID); err == nil && strings.TrimSpace(raw) != "" {
		var spawn struct {
			Profile        string `json:"profile"`
			SandboxProfile string `json:"sandbox_profile"`
		}
		if json.Unmarshal([]byte(raw), &spawn) == nil {
			out.Spawn.Profile, out.Spawn.SandboxProfile = spawn.Profile, spawn.SandboxProfile
		}
	}

	var snap *db.AgentStartupSnapshot
	if agentID != "" {
		var err error
		if snap, err = db.GetAgentStartupSnapshot(agentID); err != nil {
			return out, err
		}
	}
	if snap != nil {
		out.Spawn.WorktreePath, out.Spawn.WorktreeBranch = snap.WorktreePath, snap.WorktreeBranch
		out.Startup = whoamiFullStartup{
			Recorded: true, IncludeGroupContext: snap.IncludeGroupContext,
			ProfileContext: snap.ProfileContext, BriefMessageID: snap.BriefMessageID,
			SpawnedBy: snap.SpawnedByAgent,
		}
		if snap.SpawnGroupID > 0 {
			if g, err := db.GetAgentGroupByID(snap.SpawnGroupID); err == nil && g != nil {
				out.Startup.SpawnGroup = g.Name
			}
		}
		if snap.BriefMessageID > 0 {
			if m, err := db.GetAgentMessage(snap.BriefMessageID); err == nil && m != nil {
				out.Startup.Brief = m.Body
			}
		}
	}

	groups, err := whoamiFullGroups(convID)
	if err != nil {
		return out, err
	}
	out.Groups = groups

	if agentID != "" {
		primary, err := startupctx.PrimaryGroup(agentID)
		if err != nil {
			return out, err
		}
		if primary != nil {
			policy := primary.Group.EffectiveReinjectAfterCompact()
			out.Reinject = &whoamiFullReinject{Group: primary.Group.Name, Policy: policy}
			if policy == db.ReinjectContexts && (snap == nil || snap.IncludeGroupContext) {
				out.Reinject.GroupContext = primary.Group.DefaultContext
			}
		}
	}

	state, err := snapshotPermissions()
	if err != nil {
		return out, err
	}
	effective, ownerAdded, provenance, source := effectivePermsFor(state, convID, ownerImpliedSlugsFor(convID))
	sort.Strings(effective)
	sort.Strings(ownerAdded)
	perms := whoamiFullPermissions{
		Effective: nonNilStrings(effective), OwnerImplied: nonNilStrings(ownerAdded),
		Provenance: provenance, Source: source, OwnedGroups: ownedGroupNamesFor(convID),
	}
	if perms.Provenance == nil {
		perms.Provenance = map[string]string{}
	}
	// Name the groups whose membership confers each effective slug, also
	// where another source (ownership, a grant) is the one permissions ls
	// names.
	for _, slug := range effective {
		if !IsMemberImpliedSlug(slug) {
			continue
		}
		for _, g := range groups {
			if g.Member && !g.Archived && memberPermissionPermitted(convID, slug, ActionContext{Group: g.Name, structuralGroup: g.Name}) {
				if perms.MemberGroups == nil {
					perms.MemberGroups = map[string][]string{}
				}
				perms.MemberGroups[slug] = append(perms.MemberGroups[slug], g.Name)
			}
		}
	}
	out.Permissions = perms
	return out, nil
}

// whoamiFullGroups is every group the agent is in or owns, with its role
// there and the links into it.
func whoamiFullGroups(convID string) ([]whoamiFullGroup, error) {
	byName := map[string]*whoamiFullGroup{}
	var order []string
	add := func(g *db.AgentGroup) *whoamiFullGroup {
		if entry, ok := byName[g.Name]; ok {
			return entry
		}
		entry := &whoamiFullGroup{Name: g.Name, Archived: g.IsArchived()}
		if links, err := groupLinksView(g, db.LinkIn); err == nil {
			entry.LinksIn = links
		}
		byName[g.Name] = entry
		order = append(order, g.Name)
		return entry
	}
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		entry := add(g)
		entry.Member = true
		if m, err := db.FindMemberInGroup(g.ID, convID); err == nil && m != nil {
			entry.Role, entry.Descr = m.Role, m.Descr
		}
	}
	for _, name := range ownedGroupNamesFor(convID) {
		g, err := db.GetAgentGroupByName(name)
		if err != nil || g == nil {
			continue
		}
		add(g).Owner = true
	}
	out := make([]whoamiFullGroup, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return slices.Clone(in)
}
