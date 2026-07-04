package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Re-brief a deployed force (JOH-247). A task force deployed from a template
// carries provenance — the source template + the mission it was deployed
// against (JOH-245). Over the life of a mission the roster drifts (agents
// reincarnate, waves settle) and the original work-pattern briefing scrolls out
// of everyone's context. Re-brief re-delivers the source template's CURRENT
// work pattern to the force's live members, with the group's recorded mission
// interpolated — a pure comms act over the SAME machinery a deploy's final wave
// uses (deliverWorkPattern), no new delivery mechanism and no new state.
//
// Wire surface (group-scoped, under the existing /v1/groups/{name} family):
//
//	POST /v1/groups/{name}/rebrief → re-deliver the work pattern to live members
//
// Gating: this reuses requireGroupPermission(templates.instantiate) — the human
// always passes, a group OWNER passes structurally (the owner bypass fills the
// undecided gap for ANY group-scoped slug, regardless of the slug's registry
// OwnerImplied flag), and any other agent needs the templates.instantiate slug.
// That is deliberately the SAME shape as process-advance (human + owner + slug)
// but a DIFFERENT slug: re-brief is a template-use act (it re-runs the
// template's work pattern against the group), so it borrows the existing
// templates.instantiate bar
// rather than minting a new slug or overloading process.advance (a process act,
// not a comms one).

// handleGroupRebrief serves POST /v1/groups/{name}/rebrief: re-deliver the
// source template's work pattern to the force's live members, interpolating the
// group's recorded mission. Degrades with a clear 4xx (never a partial write):
//   - the group is not a deployed force (no source_template)          → 400
//   - the source template has been deleted since deploy               → 422
//   - the (current) template carries no work pattern to re-deliver    → 422
//
// A template edited since deploy re-delivers its CURRENT pattern — that is the
// point of a re-brief (the operator changed the plan and wants the force told).
func handleGroupRebrief(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	caller, ok := requireGroupPermission(w, r, PermTemplatesUse, g)
	if !ok {
		return
	}

	if g.SourceTemplate == "" {
		writeError(w, http.StatusBadRequest, "not_a_force",
			"this group was not deployed from a template (no source template) — nothing to re-brief")
		return
	}

	tmpl, err := db.GetGroupTemplate(g.SourceTemplate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if tmpl == nil {
		writeError(w, http.StatusUnprocessableEntity, "template_gone",
			"the source template "+g.SourceTemplate+" no longer exists — cannot re-brief")
		return
	}
	if len(tmpl.WorkPattern) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_work_pattern",
			"the source template "+g.SourceTemplate+" has no work pattern to re-deliver")
		return
	}

	// Reconstruct the deliverWorkPattern inputs from the current roster. Targeted
	// steps (send_to: <agent>) route by the template-agent name → conv map;
	// broadcast steps (send_to: all) reach every current member. A failed member
	// read is a hard 500 — otherwise an empty roster would report a false
	// "delivered nothing, no errors" success.
	spawnedConvs, spawnedOrder, rosterNames, err := rebriefRoster(g, tmpl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}

	assignment := normalizeAssignment(g.Mission)
	delivered, patErrs := deliverWorkPattern(g, tmpl.WorkPattern, tmpl.Name, assignment, caller,
		spawnedConvs, spawnedOrder, rosterNames)

	writeJSON(w, http.StatusOK, map[string]any{
		"group":             g.Name,
		"template":          tmpl.Name,
		"mission":           g.Mission,
		"pattern_delivered": delivered,
		"pattern_errors":    patErrs,
	})
}

// rebriefRoster reconstructs deliverWorkPattern's (spawnedConvs, spawnedOrder,
// rosterNames) inputs for a re-brief from a force's CURRENT membership — the
// re-brief analogue of the (name→conv) map a deploy builds as it spawns. It
// reads the roster ONCE and feeds both maps from it (a failed read is returned,
// not swallowed — otherwise an empty roster would look like a clean "nothing to
// deliver" success).
//
//   - spawnedConvs maps each template-agent name to its conv, matched by the
//     member's effective title ("<group>-<agent>", the same restart-idempotency
//     key the wave runner dedupes against via agent.CachedTitle). A member the
//     human has RENAMED away from that title won't match — its targeted steps
//     then report "did not spawn" rather than mis-routing. That is an accepted,
//     honest degradation: the agent NAME is the work pattern's routing key, so a
//     renamed member is genuinely unaddressable by it.
//   - spawnedOrder (the "all" broadcast target) is every current member (offline
//     ones included — they read the re-brief on resume), so a broadcast re-brief
//     reaches the whole force regardless of renames.
//   - rosterNames is the template's full agent-name set, so deliverWorkPattern
//     can tell "target dropped out of the roster" from "stale work-pattern step
//     naming an agent the template never had".
func rebriefRoster(g *db.AgentGroup, tmpl *db.GroupTemplate) (map[string]string, []string, map[string]bool, error) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	byTitle := make(map[string]string, len(members))
	spawnedOrder := make([]string, 0, len(members))
	for _, m := range members {
		spawnedOrder = append(spawnedOrder, m.ConvID)
		if title := agent.CachedTitle(m.ConvID); title != "" {
			byTitle[title] = m.ConvID
		}
	}
	spawnedConvs := make(map[string]string, len(tmpl.Agents))
	rosterNames := make(map[string]bool, len(tmpl.Agents))
	for _, a := range tmpl.Agents {
		rosterNames[a.Name] = true
		if conv, ok := byTitle[g.Name+"-"+a.Name]; ok {
			spawnedConvs[a.Name] = conv
		}
	}
	return spawnedConvs, spawnedOrder, rosterNames, nil
}
