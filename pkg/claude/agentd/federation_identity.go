package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"io"
	"maps"
	"net/http"
)

func peerSupportsStableIdentity(peer string) bool {
	raw, _, e := db.GetFederationCatalog(peer)
	var c proto.CatalogPayload
	return e == nil && json.Unmarshal([]byte(raw), &c) == nil && c.StableAgentIdentity
}
func peerSupportsHomeMail(peer string) bool {
	raw, _, err := db.GetFederationCatalog(peer)
	var c proto.CatalogPayload
	return err == nil && json.Unmarshal([]byte(raw), &c) == nil && c.HomeRoutedMail
}
func departingFederationIdentity(conv string) (*db.FederationIdentity, error) {
	a, e := db.GetAgentByConv(conv)
	if e != nil {
		return nil, e
	}
	if a == nil || !a.Active() || a.CurrentConvID != conv {
		return nil, errors.New("departure requires the active source generation")
	}
	identity := db.FederationIdentity{Agent: a.AgentID, Home: arrivalNode(), Proofs: map[string]string{}}
	p, e := db.GetAgentFederationPresence(a.AgentID)
	if e != nil {
		return nil, e
	}
	if p != nil {
		if p.State != "here" {
			return nil, errors.New("agent already has a pending departure or arrival")
		}
		identity = p.Transfer
		identity.Proofs = maps.Clone(identity.Proofs)
	}
	if identity.Proofs == nil {
		identity.Proofs = map[string]string{}
	}
	identity.Proofs[arrivalNode()] = db.NewAgentID()
	identity.Hops++
	if len(identity.Proofs) > 256 {
		return nil, errors.New("stable identity supports at most 256 distinct visited nodes")
	}
	return &identity, nil
}
func stableOfferIdentity(o *db.FederationBundleOffer, b *agentbundle.Bundle) *db.FederationIdentity {
	if o.Descriptor.Move == nil || o.Descriptor.Teleport != nil && o.Descriptor.Teleport.Clone {
		return nil
	}
	return b.Manifest.Agent.Identity
}

func catalogFederationPresence(p *db.AgentFederationPresence) *proto.FederationAgentPresence {
	if p == nil {
		return nil
	}
	host, hops := p.CurrentInstance, p.HopCount
	if p.State == "away" && p.HomeInstance == arrivalNode() {
		if l, e := db.GetFederationAgentLocation(p.AgentID); e == nil && l != nil && l.Epoch == p.ContinuationNonceHash {
			host, hops = l.CurrentInstance, l.HopCount
		}
	}
	return &proto.FederationAgentPresence{HomeInstance: p.HomeInstance, State: p.State, CurrentInstance: host, HopCount: hops}
}

type federationIdentityLaunchKey struct{}
type federationIdentityLaunch struct{ Agent, Offer string }

// terminal notification is informational: hosting-node authority remains local.
func notifyAwayAgentTerminal(conv string) {
	id, e := db.AgentIDForConv(conv)
	if e != nil || id == "" {
		return
	}
	p, e := db.GetAgentFederationPresence(id)
	if e != nil || p == nil || p.HomeInstance != arrivalNode() || p.CurrentPeer == "" {
		return
	}
	if rt := currentFederation(); rt != nil {
		rt.sendControl(p.CurrentPeer, proto.KindAgentPresence, "", struct{ Agent, Offer, State string }{id, p.DepartureOffer, "terminal"})
	}
}
func (rt *fedRuntime) acceptAgentPresence(peer *db.FederationPeer, env *proto.Envelope) {
	var in struct{ Agent, Offer, State string }
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&in) != nil || in.State != "terminal" || !proto.ValidAgentRef(in.Agent) {
		return
	}
	p, e := db.GetAgentFederationPresence(in.Agent)
	if e != nil || p == nil || p.HomeInstance != peer.InstanceID || p.State != "here" || p.ArrivalOffer != in.Offer {
		return
	}
	a, e := db.GetAgent(in.Agent)
	if e != nil || a == nil || !a.Active() {
		return
	}
	_, _ = db.InsertAgentMessage(&db.AgentMessage{ToAgent: in.Agent, ToConv: a.CurrentConvID, Subject: "Home identity retired or deleted", Body: "The home operator retired or deleted your home identity. Returning under this agent ID is no longer allowed; coordinate with the local operator before departing."})
	recordFederationAudit("agent.home.terminal", peer.InstanceID, in.Agent, "", "home refuses later return", 200)
}

// Recall uses the existing peer move admission. Home identity possession never
// substitutes for the visiting node's grants or the receiver's landing policy.
func handleFederationAgentRecall(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "recall an away agent") {
		return
	}
	id := r.PathValue("id")
	p, err := db.GetAgentFederationPresence(id)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if p == nil {
		writeError(w, 404, "not_away", "agent has no away record")
		return
	}
	if p.State == "terminal" {
		writeError(w, 409, "agent_terminal", "home identity was explicitly retired or deleted; recall is unavailable")
		return
	}
	if p.State != "away" || p.HomeInstance != arrivalNode() {
		writeError(w, 409, "not_away", "recall requires this node's away home identity")
		return
	}
	var in struct {
		Group string `json:"group"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
		writeError(w, 400, "invalid_arg", "invalid recall body")
		return
	}
	if in.Group == "" {
		a, err := db.GetAgent(id)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if a != nil {
			groups, err := db.ListGroupsForConv(a.CurrentConvID)
			if err != nil {
				writeFedErr(w, err)
				return
			}
			if len(groups) > 0 {
				in.Group = groups[0].Name
			}
		}
	}
	if in.Group == "" {
		writeError(w, 409, "group_required", "choose a receiving home group for recall")
		return
	}
	raw, _ := json.Marshal(map[string]any{"group": in.Group, "direct_if_allowed": true})
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(raw))
	inner.ContentLength = int64(len(raw))
	destination, err := db.FederationMailDestination(id, arrivalNode())
	if err != nil || destination == "" {
		writeError(w, 409, "location_unavailable", "away host is not known")
		return
	}
	inner.SetPathValue("node", destination)
	inner.SetPathValue("tail", "agents/"+id+"/move")
	inner.URL.RawQuery = ""
	servePeerViewProxy(w, inner)
}
