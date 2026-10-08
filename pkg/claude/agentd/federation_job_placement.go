package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func placementJobAllowed(r *http.Request, caller, peer, group string) bool {
	if caller == "" {
		return true
	}
	allowed, _, e := permissionAllowsAction(r, caller, PermJobsRun, ActionContext{RemotePeer: peer, RemoteGroup: group})
	return e == nil && allowed
}

// Reuse interactive placement's discovery, freshness, pool membership and rank.
// Select once. Admission on that receiver is authoritative; never fail over.
func selectJobPeer(r *http.Request, selector string, q proto.JobRequest, prefer string) (string, error) {
	caller, human, ok := authedCaller(discardJobResponse{}, r)
	if !ok {
		return "", fmt.Errorf("caller unavailable")
	}
	if human {
		caller = ""
	}
	if prefer == "" {
		prefer = "least-loaded"
	}
	if prefer != "least-loaded" && prefer != "most-free-ram" {
		return "", fmt.Errorf("--prefer must be least-loaded or most-free-ram")
	}
	peers, pool, e := placementPeers(selector)
	if e != nil {
		return "", e
	}
	match, e := proto.ParseNodeMatch(q.Require)
	if e != nil {
		return "", e
	}
	rows := []fedPlacementCandidate{}
	for _, p := range peers {
		row, visible := placementCandidateWithAuthority(r, p, fedSpawnSendReq{Group: q.Group, Prefer: prefer}, caller, match, jobPlacementAuthority)
		if visible {
			rows = append(rows, row)
		}
	}
	order := placementOrder(rows, prefer)
	if len(order) == 0 {
		reasons := []string{}
		for _, r := range rows {
			reasons = append(reasons, r.Peer+": "+r.Reason)
		}
		return "", fmt.Errorf("no eligible job node for %s (%s); inspect federation nodes or select --node explicitly", selector, strings.Join(reasons, "; "))
	}
	chosen := rows[order[0]].Instance
	contains := true
	if pool != "" {
		contains, e = db.FederationNodeGroupContainsID(pool, chosen)
	}
	if e != nil || !contains {
		return "", fmt.Errorf("selected node left the pool")
	}
	return chosen, nil
}

// jobPlacementAuthority keeps spawn's admission-version check and admits only
// exported groups advertising job support that the caller may run jobs on.
var jobPlacementAuthority = placementAuthority{
	Supported: func(cat *proto.CatalogPayload) bool { return cat.Node.SpawnPlacementVersion == fedPlacementVersion },
	GroupAllowed: func(r *http.Request, caller, peer string, g proto.CatalogGroup) bool {
		return g.HasCap(proto.CapJobs) && placementJobAllowed(r, caller, peer, g.Name)
	},
}
