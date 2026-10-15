package agentd

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// POST /v1/process/resolve-preview — "who it will use now" (tcl-52p3cq).
//
// Given a template (saved or not) and a group, it answers for every agent
// work step who would take it if the run started now: a member, a spawn of
// some profile, a wait, or a failure. It is a dry run of the group rules in
// resolveProcessPerformer and has no effects: no spawn is authorized and no
// rate budget is spent, so a previewed spawn can still be refused at run
// time. Each step is previewed on its own; two steps may name the same idle
// member.

type processResolvePreviewRequest struct {
	processTemplateEditView
	Group string `json:"group"`
}

type processResolvePreviewNode struct {
	AgentID      string `json:"agentId,omitempty"`
	Name         string `json:"name,omitempty"`
	Reason       string `json:"reason"`
	SpawnProfile string `json:"spawnProfile,omitempty"`
	Waits        bool   `json:"waits,omitempty"`
	Fails        bool   `json:"fails,omitempty"`
}

func handleProcessResolvePreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessTemplatesRead); !ok {
		return
	}
	visible, ok := processContextVisibility(w, r)
	if !ok {
		return
	}
	body := &processResolvePreviewRequest{}
	present, err := decodeProcessEditBody(w, r, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	body.layoutPresent = present
	if strings.TrimSpace(body.Group) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "group is required")
		return
	}
	group, err := db.GetAgentGroupByName(strings.TrimSpace(body.Group))
	if err != nil || group == nil || group.IsArchived() || (!visible.all && !visible.groups[group.ID]) {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	if body.Template == nil && strings.TrimSpace(body.Source) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template or source is required")
		return
	}
	parsed, err := parseProcessEditView(&body.processTemplateEditView)
	if err != nil {
		writeProcessEditParseError(w, err)
		return
	}
	nodes := map[string]processResolvePreviewNode{}
	if parsed.Template != nil {
		members := processGroupCandidates(group.ID)
		ids := make([]string, 0, len(parsed.Template.Nodes))
		for id := range parsed.Template.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			node := parsed.Template.Nodes[id]
			if node.Type != model.NodeTypeTask || node.Performer == nil || node.Performer.Kind != model.PerformerAgent {
				continue
			}
			spec := engine.AgentCommandFor(*node.Performer, parsed.Template.Fallback)
			nodes[id] = previewProcessPerformer(group, members, spec)
		}
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"group": group.Name, "nodes": nodes})
}

// previewProcessPerformer is the effect-free mirror of the group branch of
// resolveProcessPerformer for a first attempt.
func previewProcessPerformer(group *db.AgentGroup, members []processPerformerCandidate, spec *engine.AgentCommand) processResolvePreviewNode {
	if spec.Assignee != "" {
		for i := range members {
			if processCandidateMatches(members[i], spec.Assignee) {
				return processResolvePreviewNode{AgentID: members[i].AgentID, Name: members[i].Name, Reason: "the named agent"}
			}
		}
	}
	matching, _ := processMatchingMembers(members, spec)
	if len(matching) > 0 {
		sortProcessCandidates(matching)
		reason := "a member with the step's role"
		switch {
		case spec.Anyone:
			reason = "anyone in the group"
		case spec.Role != "":
			reason = fmt.Sprintf("a member with role %q", spec.Role)
		}
		if matching[0].rank == 0 {
			reason += ", idle now"
		}
		return processResolvePreviewNode{AgentID: matching[0].AgentID, Name: matching[0].Name, Reason: reason}
	}
	nobody := processNoMemberReason(spec)
	if spec.Assignee != "" {
		nobody = fmt.Sprintf("assignee %q is not a member of the group", spec.Assignee)
	}
	switch action, profile := spec.Fallback(); action {
	case model.FallbackSpawn:
		if profile == "" {
			profile = group.DefaultProfile
		}
		if profile == "" {
			return processResolvePreviewNode{Waits: true,
				Reason: nobody + "; the fallback spawns the group's default spawn profile, and the group has none"}
		}
		return processResolvePreviewNode{SpawnProfile: profile, Reason: nobody + "; spawns one with profile " + profile}
	case model.FallbackFail:
		return processResolvePreviewNode{Fails: true, Reason: nobody + "; the step's fallback fails it"}
	default:
		return processResolvePreviewNode{Waits: true, Reason: nobody}
	}
}
