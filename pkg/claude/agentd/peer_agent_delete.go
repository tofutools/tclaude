package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An agent's permanent delete and its debug export over the peer view, each
// under its own group grant held in every group of the agent: the dashboard
// handlers behind them trust their caller as this node's operator, so the
// peer layer settles what a restricted peer may ask for first.

// servePeerAgentDelete deletes an agent for a peer holding groups.members.delete
// in every group the agent is in or owns (a delete purges its owner seats).
// Deleting its git worktree as well destroys whatever is uncommitted there,
// on this machine and past undoing: only unrestricted trust may ask for it.
func servePeerAgentDelete(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAgentControl(w, r, v, rule)
		return
	}
	target, ok := v.visibleAgent(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if q := r.URL.Query(); q.Has("delete_worktree") || q.Has("expected_worktree") {
		writeError(w, http.StatusForbidden, "permission", "deleting an agent's worktree requires unrestricted trust")
		return
	} else if len(q) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a remote delete takes no options")
		return
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, rule.requires) ||
		!peerArchivedFootprintHeld(v.peer.InstanceID, target.CurrentConvID, rule.requires) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every affected group", rule.requires))
		return
	}
	servePeerAgentAsLocal(w, r, v, target)
}

// peerArchivedFootprintHeld reports whether the peer holds slug, scoped to
// the group, in every archived group the agent is in or owns. A delete
// purges those memberships and seats too, and an archived group can be
// unarchived; no peer grant answers in an archived group otherwise
// (fedPeerAllows), so the grant must name it.
func peerArchivedFootprintHeld(peer, conv, slug string) bool {
	groups, err := db.ListGroupsForConv(conv)
	if err != nil {
		return false
	}
	ids := map[int64]bool{}
	for _, g := range groups {
		if g.IsArchived() {
			ids[g.ID] = true
		}
	}
	owned, err := db.ListGroupsOwnedBy(conv)
	if err != nil {
		return false
	}
	for _, id := range owned {
		g, err := db.GetAgentGroupByID(id)
		if err != nil {
			return false
		}
		if g != nil && g.IsArchived() {
			ids[id] = true
		}
	}
	for id := range ids {
		if fedPeerExplicitGroupGrant(peer, id, slug) == nil {
			return false
		}
	}
	return true
}

// servePeerAgentDebugExport serves an agent's debug export for a peer
// holding groups.members.debug-export in every group of the agent. A
// restricted peer's copy carries no environment value: the export embeds the
// agent's spawn request and sandbox snapshot, whose environments come from
// this node's groups, spawn profiles and sandbox profiles, and no peer grant
// reads a sandbox profile's. The names stay, so the export still shows what
// the launch set.
func servePeerAgentDebugExport(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAgentControl(w, r, v, rule)
		return
	}
	target, ok := v.visibleAgent(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, rule.requires) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every affected group", rule.requires))
		return
	}
	v.targetConv = target.CurrentConvID
	payload, err := buildAgentDebugExport(target.CurrentConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_export", err.Error())
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "debug_export", err.Error())
		return
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusInternalServerError, "debug_export", err.Error())
		return
	}
	var redacted []string
	redactPeerEnvironmentValues(doc, "", false, &redacted)
	if obj, ok := doc.(map[string]any); ok && len(redacted) > 0 {
		list, _ := obj["redactions"].([]any)
		for _, p := range redacted {
			list = append(list, p)
		}
		obj["redactions"] = list
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tclaude-agent-debug-%s.json"`, short8(target.AgentID)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(doc)
}

// redactPeerEnvironmentValues replaces every value under an environment
// field (any key naming an environment, at any depth, a recorded process
// PATH and a pre-launch script) with a redaction
// marker, recording each field's path once. Entries keep their names; a
// name-to-value map keeps its keys.
func redactPeerEnvironmentValues(node any, path string, underEnv bool, redacted *[]string) {
	switch n := node.(type) {
	case map[string]any:
		for key, child := range n {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			// Provenance names where each variable came from, not its value.
			// A process's recorded PATH is an environment value under another
			// name (live_process.path, execution_boundary.path).
			// A sandbox profile's pre-launch script sets what its environment
			// field may not hold, secrets included: the script is a value too.
			env := underEnv || peerEnvironmentKey(key) && !strings.HasSuffix(strings.ToLower(path), "provenance") ||
				key == "path" && (strings.HasSuffix(path, "live_process") || strings.HasSuffix(path, "execution_boundary")) ||
				key == "script" && strings.HasSuffix(path, "pre_launch")
			if env && !underEnv {
				if redactPeerEnvironmentField(n, key) && !slices.Contains(*redacted, childPath) {
					*redacted = append(*redacted, childPath)
				}
				continue
			}
			redactPeerEnvironmentValues(child, childPath, env, redacted)
		}
	case []any:
		for _, child := range n {
			redactPeerEnvironmentValues(child, path, underEnv, redacted)
		}
	}
}

// peerEnvironmentKey reports whether a JSON key names an environment.
func peerEnvironmentKey(key string) bool {
	k := strings.ToLower(key)
	return k == "env" || strings.Contains(k, "environment")
}

// redactPeerEnvironmentField redacts the string values in parent[key], an
// environment field, reporting whether it held any. Entry names stay, and so
// does a field that is a number or flag (an environment group's ID, say).
func redactPeerEnvironmentField(parent map[string]any, key string) bool {
	found := false
	var walk func(v any) any
	walk = func(v any) any {
		switch n := v.(type) {
		case string:
			found = true
			return "<redacted>"
		case []any:
			for i, c := range n {
				n[i] = walk(c)
			}
		case map[string]any:
			for k, c := range n {
				if k != "name" {
					n[k] = walk(c)
				}
			}
		}
		return v
	}
	parent[key] = walk(parent[key])
	return found
}
