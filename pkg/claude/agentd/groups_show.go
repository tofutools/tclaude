package agentd

import (
	"net/http"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// GET /v1/groups/{name}: everything the groups set-* commands write, for
// tclaude agent groups show (tk_361933d6). The summary is as open as the
// group reads it gathers (ls, members, owners, links, attachment). The
// startup context and the settings are sections of their own, each behind a
// read slug: a caller without one gets the rest and is told which section was
// withheld and the slug it needs. Read only.

// groupShowView is the reply.
type groupShowView struct {
	Name           string              `json:"name"`
	Descr          string              `json:"descr,omitempty"`
	Archived       bool                `json:"archived,omitempty"`
	Parent         string              `json:"parent,omitempty"`
	Attachment     groupAttachmentView `json:"attachment"`
	Mission        string              `json:"mission,omitempty"`
	SourceTemplate string              `json:"source_template,omitempty"`
	Owners         []ownerJSON         `json:"owners"`
	Members        []memberJSON        `json:"members"`
	Links          []linkJSON          `json:"links"`
	Context        *groupShowContext   `json:"context,omitempty"`
	Settings       *groupShowSettings  `json:"settings,omitempty"`
	// Withheld names each section left out and the slug it needs.
	Withheld []groupShowWithheld `json:"withheld,omitempty"`
}

// groupShowContext is the group's startup context, behind groups.context.read.
type groupShowContext struct {
	Text string `json:"text"`
	// Hash is db.GroupContextHash of Text.
	Hash           string   `json:"hash"`
	DefaultPrompts []string `json:"default_prompts,omitempty"`
	// ReinjectAfterCompact is what members get again after a compaction or
	// /clear: contexts, identity or off.
	ReinjectAfterCompact string `json:"reinject_after_compact"`
}

// groupShowSettings is the group's settings, behind groups.settings.read.
type groupShowSettings struct {
	DefaultCwd     string `json:"default_cwd,omitempty"`
	DefaultProfile string `json:"default_profile,omitempty"`
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	// EnvironmentKeys names the group's spawn environment. Values are never
	// shown here.
	EnvironmentKeys     []string                   `json:"environment_keys,omitempty"`
	MaxMembers          int                        `json:"max_members,omitempty"`
	NotifyEnabled       bool                       `json:"notify_enabled"`
	RemoteControlPolicy string                     `json:"remote_control_policy"`
	OwnerScopes         map[string]PermissionScope `json:"owner_scopes,omitempty"`
	DefaultSpawnGroup   bool                       `json:"default_spawn_group,omitempty"`
	TaskboardLocked     bool                       `json:"taskboard_locked,omitempty"`
	TaskboardWriters    []string                   `json:"taskboard_writers,omitempty"`
	Permissions         []string                   `json:"permissions,omitempty"`
}

type groupShowWithheld struct {
	Section string `json:"section"`
	Slug    string `json:"slug"`
}

func handleGroupShow(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	members, err := groupMembersView(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	owners, err := groupOwnersView(g)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	links, err := groupLinksView(g, db.LinkBoth)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := groupShowView{
		Name: g.Name, Descr: g.Descr, Archived: g.IsArchived(),
		Attachment: groupAttachmentViewFor(g), Mission: g.Mission, SourceTemplate: g.SourceTemplate,
		Owners: owners, Members: members, Links: links,
	}
	if g.ParentGroupID != nil {
		if parent, err := db.GetAgentGroupByID(*g.ParentGroupID); err == nil && parent != nil {
			out.Parent = parent.Name
		}
	}

	allowed, err := groupSectionAllowed(r, g, PermGroupsContextRead)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if allowed {
		out.Context = &groupShowContext{
			Text: g.DefaultContext, Hash: db.GroupContextHash(g.DefaultContext),
			DefaultPrompts: slices.Clone(g.DefaultPrompts), ReinjectAfterCompact: g.EffectiveReinjectAfterCompact(),
		}
	} else {
		out.Withheld = append(out.Withheld, groupShowWithheld{Section: "context", Slug: PermGroupsContextRead})
	}

	allowed, err = groupSectionAllowed(r, g, PermGroupsSettingsRead)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if allowed {
		settings, err := groupShowSettingsFor(g)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out.Settings = settings
	} else {
		out.Withheld = append(out.Withheld, groupShowWithheld{Section: "settings", Slug: PermGroupsSettingsRead})
	}
	writeJSON(w, http.StatusOK, out)
}

func groupShowSettingsFor(g *db.AgentGroup) (*groupShowSettings, error) {
	grants, err := db.ListAgentGroupPermissions(g.ID)
	if err != nil {
		return nil, err
	}
	lock, err := db.GetTaskboardLock(g.ID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(g.Environment))
	for _, entry := range g.Environment {
		keys = append(keys, entry.Name)
	}
	return &groupShowSettings{
		DefaultCwd: g.DefaultCwd, DefaultProfile: g.DefaultProfile, SandboxProfile: g.SandboxProfile,
		EnvironmentKeys: keys, MaxMembers: g.MaxMembers, NotifyEnabled: g.NotifyEnabled,
		RemoteControlPolicy: remoteControlPolicyToWire(g.RemoteControl),
		OwnerScopes:         ownerScopesWireOmitEmpty(g.OwnerScopesJSON),
		DefaultSpawnGroup:   g.DefaultSpawnGroup,
		TaskboardLocked:     lock.Locked, TaskboardWriters: slices.Clone(lock.Writers),
		Permissions: grants,
	}, nil
}

// groupSectionAllowed reports whether the caller may read a section of a
// group behind slug, as requireGroupPermission would decide it but without
// refusing the request: the human always, a live agent by its grants and by
// what membership or ownership of g confers. Nothing else reads a section.
func groupSectionAllowed(r *http.Request, g *db.AgentGroup, slug string) (bool, error) {
	if peerActionFromRequest(r) != nil || peerGrantFromRequest(r) != nil {
		return false, nil
	}
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classHuman:
		return true, nil
	case classAgent:
	default:
		return false, nil
	}
	state, err := db.AgentState(p.ConvID)
	if err != nil || state == db.AgentStateRetired {
		return false, nil
	}
	allowed, _, err := permissionAllowsAction(r, p.ConvID, slug, ActionContext{Group: g.Name, structuralGroup: g.Name})
	return allowed, err
}
