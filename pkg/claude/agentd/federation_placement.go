package agentd

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const fedPlacementVersion = 1
const fedCodeNodeBusy = "node_busy" // terminal: no request or launch was created

type fedPlacementCandidate struct {
	Peer         string   `json:"peer"`
	Instance     string   `json:"instance"`
	Group        string   `json:"group,omitempty"`
	Eligible     bool     `json:"eligible"`
	Reason       string   `json:"reason,omitempty"`
	LoadPerCore  *float64 `json:"load_per_core,omitempty"`
	RAMAvailable *uint64  `json:"ram_available_bytes,omitempty"`
	Attempt      string   `json:"attempt,omitempty"`
}
type fedPlacementExplanation struct {
	Selector   string                  `json:"selector"`
	Require    string                  `json:"require,omitempty"`
	Prefer     string                  `json:"prefer"`
	Selected   string                  `json:"selected,omitempty"`
	Candidates []fedPlacementCandidate `json:"candidates"`
}
type fedPlacementResponse struct {
	fedSendResp
	Placement fedPlacementExplanation `json:"placement"`
}

func placementPeers(selector string) ([]db.FederationPeer, string, error) {
	if selector == "auto" {
		p, e := db.ListFederationPeers()
		return p, "", e
	}
	if name, ok := strings.CutPrefix(selector, "group:"); ok && name != "" {
		// Pin the immutable pool ID so delete/recreate cannot change this request.
		g, e := db.GetFederationNodeGroup(name)
		if e != nil {
			return nil, "", e
		}
		if g == nil {
			return nil, "", fmt.Errorf("node pool %q not found", name)
		}
		p, e := db.ListFederationNodeGroupPeers(name)
		return p, g.ID, e
	}
	return nil, "", fmt.Errorf("node must be auto or group:<pool>")
}
func placementSpawnAllowed(r *http.Request, caller, peer, group string) bool {
	if caller == "" {
		return true
	}
	actx := ActionContext{RemotePeer: peer, RemoteGroup: group}
	for _, slug := range []string{PermAgentSpawn, PermGroupsMembersSpawn} {
		allowed, _, err := permissionAllowsAction(r, caller, slug, actx)
		if err == nil && allowed {
			return true
		}
	}
	return false
}

type placementAuthority struct {
	Supported    func(*proto.CatalogPayload) bool
	GroupAllowed func(*http.Request, string, string, proto.CatalogGroup) bool
}

func placementCandidate(r *http.Request, p db.FederationPeer, req fedSpawnSendReq, caller string, match proto.NodeMatch) (fedPlacementCandidate, bool) {
	return placementCandidateWithAuthority(r, p, req, caller, match, placementAuthority{Supported: func(cat *proto.CatalogPayload) bool { return cat.Node.SpawnPlacementVersion == fedPlacementVersion }, GroupAllowed: func(r *http.Request, caller, peer string, g proto.CatalogGroup) bool {
		return placementSpawnAllowed(r, caller, peer, g.Name) && (req.Profile == "" || catalogAllowsSpawnProfile(g, req.Profile))
	}})
}
func placementCandidateWithAuthority(r *http.Request, p db.FederationPeer, req fedSpawnSendReq, caller string, match proto.NodeMatch, authority placementAuthority) (fedPlacementCandidate, bool) {
	row := fedPlacementCandidate{Peer: peerDisplay(&p), Instance: p.InstanceID}
	if caller != "" {
		allowed, _, err := permissionAllowsAction(r, caller, PermNodeRead, ActionContext{RemotePeer: p.InstanceID})
		if err != nil || !allowed {
			return row, false
		} // hidden entirely, including rejection reasons
	}
	reject := func(reason string) (fedPlacementCandidate, bool) { row.Reason = reason; return row, true }
	rt := currentFederation()
	if rt == nil || !rt.sessionPeerOnline(p.InstanceID) {
		return reject("offline")
	}
	cat, _, err := fedCatalogFor(p.InstanceID)
	if err != nil || cat == nil || cat.Node == nil {
		return reject("node metadata unavailable")
	}
	n := cat.Node
	now := time.Now()
	at := n.Resources.ObservedAt
	if cat.NodeReceivedAt.IsZero() || now.Sub(cat.NodeReceivedAt) > fedNodeStaleAfter || cat.NodeReceivedAt.After(now.Add(2*time.Minute)) || at == nil || now.Sub(*at) > fedNodeStaleAfter || at.After(now.Add(2*time.Minute)) || n.Resources.Status != "current" {
		return reject("node metadata stale or warming")
	}
	if !authority.Supported(cat) {
		return reject("receiver does not advertise placement admission support")
	}
	if !match.Matches(n) {
		return reject("requirements do not match")
	}
	groups := []string{}
	for _, g := range cat.Groups {
		if (req.Group == "" || req.Group == g.Name) && authority.GroupAllowed(r, caller, p.InstanceID, g) {
			groups = append(groups, g.Name)
		}
	}
	if len(groups) == 0 {
		return reject("no authorized exported group")
	}
	if len(groups) != 1 {
		return reject("multiple authorized groups; specify --group")
	}
	row.Group = groups[0]
	if n.MaxLiveAgents > 0 {
		if n.Resources.Agents == nil {
			return reject("agent load unavailable")
		}
		if n.Resources.Agents.LiveAgents >= n.MaxLiveAgents {
			return reject("advertised capacity busy")
		}
	}
	switch req.Prefer {
	case "least-loaded":
		if n.Resources.CPU.LoadAverage == nil || n.Resources.CPU.LogicalCores <= 0 {
			return reject("CPU load unavailable")
		}
		score := n.Resources.CPU.LoadAverage[0] / float64(n.Resources.CPU.LogicalCores)
		row.LoadPerCore = &score
	case "most-free-ram":
		if n.Resources.RAM == nil {
			return reject("RAM available reading unavailable")
		}
		bytes := n.Resources.RAM.AvailableBytes
		row.RAMAvailable = &bytes
	}
	row.Eligible = true
	return row, true
}
func placementOrder(rows []fedPlacementCandidate, prefer string) []int {
	order := []int{}
	for i, row := range rows {
		if row.Eligible {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := rows[order[i]], rows[order[j]]
		if prefer == "most-free-ram" && *a.RAMAvailable != *b.RAMAvailable {
			return *a.RAMAvailable > *b.RAMAvailable
		}
		if prefer == "least-loaded" && *a.LoadPerCore != *b.LoadPerCore {
			return *a.LoadPerCore < *b.LoadPerCore
		}
		return a.Instance < b.Instance
	})
	return order
}

func handleFederationPlacement(w http.ResponseWriter, r *http.Request, req fedSpawnSendReq, caller string, human bool) {
	if req.Peer != "" {
		writeError(w, 400, "invalid_arg", "peer and node are mutually exclusive")
		return
	}
	if len(req.Require) > 1024 {
		writeError(w, 400, "invalid_arg", "requirements exceed 1024 bytes")
		return
	}
	match, err := proto.ParseNodeMatch(req.Require)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	if req.Prefer == "" {
		req.Prefer = "least-loaded"
	}
	if req.Prefer != "least-loaded" && req.Prefer != "most-free-ram" {
		writeError(w, 400, "invalid_arg", "prefer must be least-loaded or most-free-ram")
		return
	}
	peers, poolID, err := placementPeers(req.Node)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].InstanceID < peers[j].InstanceID })
	explanation := fedPlacementExplanation{Selector: req.Node, Require: req.Require, Prefer: req.Prefer, Candidates: []fedPlacementCandidate{}}
	for _, p := range peers {
		row, visible := placementCandidate(r, p, req, caller, match)
		if visible {
			explanation.Candidates = append(explanation.Candidates, row)
		}
	}
	for _, idx := range placementOrder(explanation.Candidates, req.Prefer) {
		if r.Context().Err() != nil {
			writeError(w, http.StatusRequestTimeout, "canceled", "placement canceled before the next send")
			return
		}
		row := &explanation.Candidates[idx]
		// Recheck trust, pool membership, grants and observations immediately before each send.
		p, err := db.GetFederationPeer(row.Instance)
		if err != nil || p == nil {
			row.Eligible = false
			row.Reason = "peer no longer trusted"
			continue
		}
		if poolID != "" {
			contains, err := db.FederationNodeGroupContainsID(poolID, p.InstanceID)
			if err != nil || !contains {
				row.Eligible = false
				row.Reason = "peer no longer in selected pool"
				continue
			}
		}
		checked, visible := placementCandidate(r, *p, req, caller, match)
		if !visible {
			row.Eligible = false
			row.Reason = "authority withdrawn"
			continue
		}
		if !checked.Eligible {
			row.Eligible = false
			row.Reason = checked.Reason
			continue
		}
		row.Group = checked.Group
		label := row.Group + "@" + peerDisplay(p)
		queued, err := queueRequesterSpawn(r, caller, p, row.Group, req, fedPlacementVersion)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		settled := waitPlacementReceipt(r.Context(), queued)
		row.Attempt = settled.State
		if settled.State == db.FedOutboxRefused && strings.HasPrefix(settled.LastError, fedCodeNodeBusy+":") {
			row.Eligible = false
			row.Reason = "receiver terminally refused: busy"
			continue
		}
		explanation.Selected = p.InstanceID
		via := ""
		if !human {
			via = row.Group
		}
		setAuditTargetLabel(r, label)
		writeJSON(w, 200, fedPlacementResponse{fedSendResp: fedSendResp{EnvelopeID: settled.EnvelopeID, To: label, State: settled.State, ViaGroup: via, Connected: fedConnected()}, Placement: projectPlacementExplanation(r, caller, explanation)})
		return // Accepted, refused for another reason, or uncertain: never launch elsewhere.
	}
	writeJSON(w, http.StatusConflict, map[string]any{"code": "no_candidate", "error": "no eligible node remains; see placement candidates", "placement": projectPlacementExplanation(r, caller, explanation)})
}

func waitPlacementReceipt(ctx context.Context, row *db.FederationOutboxRow) *db.FederationOutboxRow {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if current, err := db.GetFederationOutbox(row.EnvelopeID); err == nil && current != nil {
			row = current
			if row.State == db.FedOutboxAccepted || row.State == db.FedOutboxRefused {
				return row
			}
		}
		select {
		case <-ctx.Done():
			return row
		case <-timer.C:
			return row
		case <-ticker.C:
		}
	}
}

// Authority may be withdrawn while waiting for a receipt. Re-project the
// explanation against current scopes before returning private node readings.
func projectPlacementExplanation(r *http.Request, caller string, in fedPlacementExplanation) fedPlacementExplanation {
	if caller == "" {
		return in
	}
	out := in
	out.Candidates = []fedPlacementCandidate{}
	for _, row := range in.Candidates {
		allowed, _, err := permissionAllowsAction(r, caller, PermNodeRead, ActionContext{RemotePeer: row.Instance})
		if err != nil || !allowed {
			continue
		}
		if row.Group != "" && !placementSpawnAllowed(r, caller, row.Instance, row.Group) {
			row.Group = ""
		}
		out.Candidates = append(out.Candidates, row)
	}
	return out
}
