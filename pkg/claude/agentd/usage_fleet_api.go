package agentd

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usage_fleet_api.go — the HTTP surface for fleet usage limits and the fleet
// usage view. These routes stay with this node (a peer never reads another
// peer's shared usage through it); /api/usage/node-summary and
// /api/usage/fleet-enforce are the peer-facing ones (usage_fleet.go,
// usage_fleet_receive.go).

type usageFleetLimitView struct {
	*db.UsageFleetLimit
	TargetLabel string `json:"target_label"`
}

// usageFleetNode is one trusted peer in the fleet view: what it shares with
// this node and its latest summary.
type usageFleetNode struct {
	Node    string            `json:"node"`
	Label   string            `json:"label"`
	Pools   []string          `json:"pools"`
	Summary *usageNodeSummary `json:"summary,omitempty"`
	ReadAt  string            `json:"read_at,omitempty"`
	Error   string            `json:"error,omitempty"`
	// LastSend is the peer's answer to the latest fleet enforcement.
	LastSend *usageFleetEnforceReply `json:"last_send,omitempty"`
	SendErr  string                  `json:"send_error,omitempty"`
}

type usageFleetPool struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"` // instance ids
}

type usageFleetOverview struct {
	Limits   []usageFleetLimitView `json:"limits"`
	Statuses []usageFleetStatus    `json:"statuses"`
	// Self is this node's own summary, Nodes every trusted peer's.
	Self  *usageNodeSummary `json:"self,omitempty"`
	Nodes []usageFleetNode  `json:"nodes"`
	Pools []usageFleetPool  `json:"pools"`
	// Controls are peers' fleet limits in force on this node.
	Controls    []usageFleetControlView `json:"controls"`
	EvaluatedAt string                  `json:"evaluated_at,omitempty"`
}

func handleUsageFleet(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRead); !ok {
		return
	}
	now := time.Now()
	human := classify(peerFromContext(r.Context())) == classHuman
	out, err := collectUsageFleetOverview(r.Context(), human, r.URL.Query().Get("fresh") == "1", now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// collectUsageFleetOverview reads every trusted peer's summary (reusing one
// read under a minute old, or under a few seconds when fresh) for the
// operator; an agent caller sees the fleet limits and their readings only.
func collectUsageFleetOverview(ctx context.Context, withNodes, fresh bool, now time.Time) (usageFleetOverview, error) {
	out := usageFleetOverview{Limits: []usageFleetLimitView{}, Statuses: []usageFleetStatus{}, Nodes: []usageFleetNode{}, Pools: []usageFleetPool{}, Controls: []usageFleetControlView{}}
	trusted, err := db.ListFederationPeers()
	if err != nil {
		return out, err
	}
	limits, err := db.ListUsageFleetLimits()
	if err != nil {
		return out, err
	}
	for _, l := range limits {
		label, _, _ := usageFleetTargetMembers(l.Target, trusted)
		out.Limits = append(out.Limits, usageFleetLimitView{UsageFleetLimit: l, TargetLabel: label})
	}
	statuses, at := latestUsageFleetStatus()
	out.Statuses = append(out.Statuses, statuses...)
	if !at.IsZero() {
		out.EvaluatedAt = at.UTC().Format(time.RFC3339)
	}
	if !withNodes {
		return out, nil
	}
	out.Controls = usageFleetControlViews(liveUsageFleetControls("", now))
	if self, err := collectUsageNodeSummary("", now); err == nil {
		out.Self = &self
	}
	pools, err := db.ListFederationNodeGroups()
	if err != nil {
		return out, err
	}
	poolsOf := map[string][]string{}
	for _, p := range pools {
		members, err := db.ListFederationNodeGroupMembers(p.ID)
		if err != nil {
			return out, err
		}
		view := usageFleetPool{ID: p.ID, Name: p.Name, Members: []string{}}
		for _, m := range members {
			view.Members = append(view.Members, m.InstanceID)
			poolsOf[m.InstanceID] = append(poolsOf[m.InstanceID], p.Name)
		}
		out.Pools = append(out.Pools, view)
	}
	maxAge := usageFleetPollInterval
	if fresh {
		maxAge = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, fedPeerViewTimeout+5*time.Second)
	defer cancel()
	reads := readUsageFleetPeers(ctx, trusted, maxAge, now)
	for _, p := range trusted {
		node := usageFleetNode{Node: p.InstanceID, Label: peerDisplay(&p), Pools: poolsOf[p.InstanceID]}
		if node.Pools == nil {
			node.Pools = []string{}
		}
		if rd, ok := reads[p.InstanceID]; ok {
			if !rd.readAt.IsZero() {
				s := rd.summary
				node.Summary, node.ReadAt = &s, rd.readAt.UTC().Format(time.RFC3339)
			}
			node.Error, node.LastSend, node.SendErr = rd.err, rd.lastSend, rd.sendErr
		}
		out.Nodes = append(out.Nodes, node)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return strings.ToLower(out.Nodes[i].Label) < strings.ToLower(out.Nodes[j].Label) })
	return out, nil
}

type usageFleetLimitBody struct {
	Target      string          `json:"target"`
	IncludeSelf *bool           `json:"include_self"`
	Rule        usagerules.Rule `json:"rule"`
	Enabled     *bool           `json:"enabled"`
}

// resolveUsageFleetTarget accepts "all", "pool:<id or name>" or
// "peer:<instance id or label>" and returns the stored form.
func resolveUsageFleetTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "" || raw == db.UsageFleetTargetAll:
		return db.UsageFleetTargetAll, nil
	case strings.HasPrefix(raw, db.UsageFleetTargetPoolPrefix):
		ref := strings.TrimSpace(strings.TrimPrefix(raw, db.UsageFleetTargetPoolPrefix))
		pool, err := db.GetFederationNodeGroupByID(ref)
		if err != nil {
			return "", err
		}
		if pool == nil {
			if pool, err = db.GetFederationNodeGroup(ref); err != nil {
				return "", err
			}
		}
		if pool == nil {
			return "", fmt.Errorf("no node pool %q", ref)
		}
		return db.UsageFleetTargetPoolPrefix + pool.ID, nil
	case strings.HasPrefix(raw, db.UsageFleetTargetPeerPrefix):
		ref := strings.TrimSpace(strings.TrimPrefix(raw, db.UsageFleetTargetPeerPrefix))
		peers, err := db.ListFederationPeers()
		if err != nil {
			return "", err
		}
		for _, p := range peers {
			if p.InstanceID == ref || strings.EqualFold(peerDisplay(&p), ref) {
				return db.UsageFleetTargetPeerPrefix + p.InstanceID, nil
			}
		}
		return "", fmt.Errorf("no trusted peer %q", ref)
	}
	return "", fmt.Errorf("target must be \"all\", \"pool:<pool>\" or \"peer:<peer>\"")
}

func usageFleetLimitFromBody(w http.ResponseWriter, l *db.UsageFleetLimit, body usageFleetLimitBody, create bool) bool {
	if create || strings.TrimSpace(body.Target) != "" {
		target, err := resolveUsageFleetTarget(body.Target)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_target", err.Error())
			return false
		}
		l.Target = target
	}
	if body.IncludeSelf != nil {
		l.IncludeSelf = *body.IncludeSelf
	} else if create {
		l.IncludeSelf = true
	}
	if body.Enabled != nil {
		l.Enabled = *body.Enabled
	} else if create {
		l.Enabled = true
	}
	if create || body.Rule.Name != "" || len(body.Rule.Steps) > 0 {
		l.Rule = body.Rule
	}
	l.Rule.Normalize()
	if err := l.Rule.Validate(usagerules.ScopeFleet); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return false
	}
	return true
}

func handleUsageFleetLimitCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	var body usageFleetLimitBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	l := &db.UsageFleetLimit{}
	if !usageFleetLimitFromBody(w, l, body, true) {
		return
	}
	if err := db.CreateUsageFleetLimit(l); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageFleet()
	writeJSON(w, http.StatusOK, l)
}

func usageFleetLimitFromPath(w http.ResponseWriter, r *http.Request) *db.UsageFleetLimit {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "fleet limit id must be a number")
		return nil
	}
	l, err := db.GetUsageFleetLimit(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such fleet usage limit")
	}
	return l
}

func handleUsageFleetLimitUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	l := usageFleetLimitFromPath(w, r)
	if l == nil {
		return
	}
	var body usageFleetLimitBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	if !usageFleetLimitFromBody(w, l, body, false) {
		return
	}
	if _, err := db.UpdateUsageFleetLimit(l); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageFleet()
	writeJSON(w, http.StatusOK, l)
}

func handleUsageFleetLimitDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	l := usageFleetLimitFromPath(w, r)
	if l == nil {
		return
	}
	if _, err := db.DeleteUsageFleetLimit(l.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageFleet()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": l.ID})
}

// addPeerUsageRules classifies the usage routes for the peer view: the
// summary answers any trusted peer (its totals only under usage.read), the
// rules are managed under usage-rules.manage, a fleet limit acts here under
// usage-rules.enforce, and the fleet view stays with this node.
func addPeerUsageRules(rules map[string]peerViewRule) {
	wrap := dashboardSandboxProfilesRoute
	rules["GET /api/usage/node-summary"] = peerViewRule{feature: "usage.summary", publicRead: true, serve: servePeerUsageSummary}
	rules["POST /api/usage/fleet-enforce"] = peerViewRule{feature: "usage.enforce", requires: PermUsageRulesEnforce, group: true, write: servePeerUsageFleetEnforce}
	rules["GET /api/usage/rules"] = peerViewRule{feature: "usage.manage", requires: PermUsageRulesManage, serve: servePeerGrantedRead(wrap(handleUsageRulesOverview))}
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /api/usage/profiles":                 handleUsageProfileCreate,
		"PUT /api/usage/profiles/{ref}":            handleUsageProfileUpdate,
		"DELETE /api/usage/profiles/{ref}":         handleUsageProfileDelete,
		"POST /api/usage/limits":                   handleUsageLimitCreate,
		"PUT /api/usage/limits/{id}":               handleUsageLimitUpdate,
		"DELETE /api/usage/limits/{id}":            handleUsageLimitDelete,
		"PUT /api/usage/assignments":               handleUsageAssignmentsSet,
		"POST /api/usage/agents/{selector}/adjust": handleUsageAgentAdjust,
	} {
		rules[pattern] = peerViewRule{feature: "usage.manage", requires: PermUsageRulesManage, write: servePeerGrantedWrite(wrap(h))}
	}
	for _, pattern := range []string{"GET /api/usage/fleet", "POST /api/usage/fleet-limits", "PUT /api/usage/fleet-limits/{id}", "DELETE /api/usage/fleet-limits/{id}"} {
		rules[pattern] = peerViewRule{feature: "own_node", requires: "local_only"}
	}
}
