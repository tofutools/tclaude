package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func registerResilienceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/resilience/policies", handleResiliencePolicies)
	mux.HandleFunc("POST /v1/resilience/policies", handleResiliencePolicySet)
	mux.HandleFunc("DELETE /v1/resilience/policies/{id}", handleResiliencePolicyRemove)
	mux.HandleFunc("POST /v1/resilience/policies/{id}/run", handleResiliencePolicyRun)
	mux.HandleFunc("GET /v1/resilience/held", handleResilienceHeld)
}

type resiliencePolicySetRequest struct {
	// Target is an agent selector or group:<name>.
	Target string `json:"target"`
	// Every is a duration such as 30m or 6h; at least 10m.
	Every string `json:"every"`
	// To is peer:<instance id|label>.
	To         string `json:"to"`
	Keep       int    `json:"keep"`
	MaxAgeDays int    `json:"max_age_days,omitempty"`
}

type resiliencePolicyView struct {
	resiliencePolicy
	PeerLabel string             `json:"peer_label,omitempty"`
	Agents    []*resilienceState `json:"agents"`
}

// handleResiliencePolicySet adds a resilience policy. Human-only: a policy
// sends conversations off this node, so setting one is the operator's
// consent to that destination.
//
//	POST /v1/resilience/policies {target, every, to, keep, max_age_days?}
func handleResiliencePolicySet(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "set a resilience snapshot policy") {
		return
	}
	var req resiliencePolicySetRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, 400, "body", err.Error())
		return
	}
	every, err := time.ParseDuration(req.Every)
	if err != nil || every < resilienceMinEvery {
		writeError(w, 400, "every", fmt.Sprintf("every must be a duration of at least %s", resilienceMinEvery))
		return
	}
	if req.Keep < 1 || req.Keep > bundletransfer.MaxResilienceKeep {
		writeError(w, 400, "keep", fmt.Sprintf("keep must be 1 to %d", bundletransfer.MaxResilienceKeep))
		return
	}
	if req.MaxAgeDays < 0 || req.MaxAgeDays > 365 {
		writeError(w, 400, "max_age_days", "max_age_days must be 0 (default 30) to 365")
		return
	}
	kind, ref, ok := cutDestination(req.To)
	if !ok || kind != "peer" {
		writeError(w, 400, "to", "to must be peer:<instance id or label>; Board destinations are not available yet")
		return
	}
	peer, err := resolveFederationPeerOpt(ref, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	target := req.Target
	if len(target) < 6 || target[:6] != "group:" {
		res, _, err := resolveAgentSelectorForResilience(target)
		if err != nil {
			writeError(w, 404, "target", err.Error())
			return
		}
		target = res
	}
	if _, err := resilienceTargets(target); err != nil {
		writeError(w, 404, "target", err.Error())
		return
	}
	p := resiliencePolicy{ID: newResiliencePolicyID(), Target: target, EverySeconds: int64(every / time.Second), Dest: resilienceDest{Kind: "peer", Peer: peer.InstanceID}, Keep: req.Keep, MaxAgeDays: req.MaxAgeDays, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	resilienceMu.Lock()
	f, err := loadResilience()
	if err == nil {
		f.Policies = append(f.Policies, p)
		err = saveResilience(f)
	}
	resilienceMu.Unlock()
	if err != nil {
		writeError(w, 500, "policy", err.Error())
		return
	}
	recordFederationAudit("resilience.policy.set", peerDisplay(peer), "", "", fmt.Sprintf("policy=%s target=%s every=%s keep=%d", p.ID, p.Target, every, p.Keep), 200)
	writeJSON(w, 200, p)
}

func cutDestination(to string) (kind, ref string, ok bool) {
	for i := 0; i < len(to); i++ {
		if to[i] == ':' {
			return to[:i], to[i+1:], i > 0 && i < len(to)-1
		}
	}
	return "", "", false
}

// resolveAgentSelectorForResilience maps a selector to a stable agent id.
func resolveAgentSelectorForResilience(sel string) (string, string, error) {
	if proto.ValidAgentRef(sel) {
		if a, err := db.GetAgent(sel); err == nil && a.Active() {
			return a.AgentID, a.CurrentConvID, nil
		}
	}
	res, _, err := agent.ResolveSelector(sel)
	if err != nil {
		return "", "", err
	}
	id, err := db.AgentIDForConv(res.ConvID)
	if err != nil || id == "" {
		return "", "", fmt.Errorf("%q is not an agent", sel)
	}
	return id, res.ConvID, nil
}

// handleResiliencePolicies lists the policies with each agent's status:
// the last good snapshot, a deferral, failures and credential findings.
//
//	GET /v1/resilience/policies
func handleResiliencePolicies(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read resilience snapshot policies") {
		return
	}
	resilienceMu.Lock()
	f, err := loadResilience()
	resilienceMu.Unlock()
	if err != nil {
		writeError(w, 500, "policy", err.Error())
		return
	}
	views := []resiliencePolicyView{}
	for _, p := range f.Policies {
		v := resiliencePolicyView{resiliencePolicy: p, Agents: []*resilienceState{}}
		if peer, err := db.GetFederationPeer(p.Dest.Peer); err == nil && peer != nil {
			v.PeerLabel = peerDisplay(peer)
		}
		for _, s := range f.State[p.ID] {
			v.Agents = append(v.Agents, s)
		}
		views = append(views, v)
	}
	writeJSON(w, 200, map[string]any{"policies": views})
}

// handleResiliencePolicyRemove removes a policy. Snapshots already sent
// stay with the peer until its retention removes them.
//
//	DELETE /v1/resilience/policies/{id}
func handleResiliencePolicyRemove(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "remove a resilience snapshot policy") {
		return
	}
	id := r.PathValue("id")
	resilienceMu.Lock()
	defer resilienceMu.Unlock()
	f, err := loadResilience()
	if err != nil {
		writeError(w, 500, "policy", err.Error())
		return
	}
	n := len(f.Policies)
	kept := f.Policies[:0]
	for _, p := range f.Policies {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	if len(kept) == n {
		writeError(w, 404, "policy", "no policy "+id)
		return
	}
	f.Policies = kept
	delete(f.State, id)
	delete(f.FindingsNotified, id)
	if err := saveResilience(f); err != nil {
		writeError(w, 500, "policy", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"removed": id})
}

// handleResiliencePolicyRun runs a policy now for its idle agents.
//
//	POST /v1/resilience/policies/{id}/run
func handleResiliencePolicyRun(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "run a resilience snapshot policy") {
		return
	}
	id := r.PathValue("id")
	resilienceMu.Lock()
	f, err := loadResilience()
	resilienceMu.Unlock()
	if err != nil {
		writeError(w, 500, "policy", err.Error())
		return
	}
	found := false
	for _, p := range f.Policies {
		found = found || p.ID == id
	}
	if !found {
		writeError(w, 404, "policy", "no policy "+id)
		return
	}
	resilienceTickOnce(time.Now(), id)
	handleResiliencePolicies(w, r)
}

// handleResilienceHeld lists the snapshots this node keeps for peers.
//
//	GET /v1/resilience/held
func handleResilienceHeld(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read held resilience snapshots") {
		return
	}
	held := listHeld("")
	if held == nil {
		held = []heldSnapshot{}
	}
	writeJSON(w, 200, map[string]any{"held": held})
}
