package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermSelfTeleport = "self.teleport"
const PermAgentsTeleportReceive = "agents.teleport.receive"

type teleportSourceContextKey struct{}
type teleportRequest struct {
	Peer        string `json:"peer,omitempty"`
	Node        string `json:"node,omitempty"`
	Group       string `json:"group,omitempty"`
	Require     string `json:"require,omitempty"`
	Prefer      string `json:"prefer,omitempty"`
	Clone       bool   `json:"clone,omitempty"`
	Home        bool   `json:"home,omitempty"`
	Note        string `json:"note,omitempty"`
	Credentials string `json:"credentials,omitempty"`
	GitRef      string `json:"git_ref,omitempty"`
}

func teleportFrozen() bool {
	cfg, err := config.Load()
	return err != nil || cfg.Federation != nil && cfg.Federation.Teleport != nil && cfg.Federation.Teleport.Disabled
}
func teleportLocalLimits() config.TeleportLimits {
	cfg, err := config.Load()
	if err == nil && cfg.Federation != nil && cfg.Federation.Teleport != nil {
		return cfg.Federation.Teleport.Limits.Effective()
	}
	return (config.TeleportLimits{}).Effective()
}
func validateTeleportLimits(intent *bundletransfer.TeleportIntent, target string, limits config.TeleportLimits) error {
	limits = limits.Effective()
	if limits.Hour < 1 || limits.Day < 1 || limits.Chain < 1 || limits.Chain > 128 || limits.RevisitMinutes < 0 || limits.RevisitMinutes > 525600 {
		return errors.New("operator teleport limits are invalid")
	}
	if len(intent.Hops) > limits.Chain {
		return errors.New("teleport chain hop limit reached")
	}
	if intent.Home && target != intent.OriginInstance {
		return errors.New("home return must target the recorded origin")
	}
	if intent.Home && limits.AllowReturn {
		return nil
	}
	cutoff := time.Now().Add(-time.Duration(limits.RevisitMinutes) * time.Minute)
	for i, h := range intent.Hops {
		if h.At.Before(cutoff) {
			continue
		}
		if h.FromInstance == target || i < len(intent.Hops)-1 && h.ToInstance == target {
			return errors.New("teleport revisit refused by ping-pong policy")
		}
	}
	return nil
}
func teleportMoveAuthority(m db.FederationAgentMove) error {
	if teleportFrozen() {
		return errors.New("teleports are frozen by the operator")
	}
	if m.Initiator != m.SourceAgent {
		return errors.New("teleport initiator must be the source identity")
	}
	r := httpRequestAsAgent(m.SourceConv)
	allowed, _, err := permissionAllowsAction(r, m.SourceConv, PermSelfTeleport, ActionContext{RemotePeer: m.Peer, RemoteGroup: m.Group})
	if err != nil || !allowed {
		return errors.New("self.teleport authority was revoked")
	}
	return nil
}
func httpRequestAsAgent(conv string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://tclaude/internal/teleport", nil)
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: -1, ConvID: conv, HasClaudeAncestor: true}))
}
func teleportGroupAllowed(r *http.Request, caller, peer string, g proto.CatalogGroup) bool {
	if !g.HasCap(proto.CapAgentsReceive) && !g.HasCap(proto.CapTeleportReceive) {
		return false
	}
	yes, _, err := permissionAllowsAction(r, caller, PermSelfTeleport, ActionContext{RemotePeer: peer, RemoteGroup: g.Name})
	return err == nil && yes
}
func handleFederationTeleport(w http.ResponseWriter, r *http.Request) {
	caller, ok := requireAgent(w, r)
	if !ok {
		return
	}
	if teleportFrozen() {
		writeError(w, 409, "teleport_off", "teleports are frozen by the operator")
		return
	}
	var in teleportRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil {
		writeError(w, 400, "json", "invalid teleport request")
		return
	}
	if len(in.Note) > 4096 || !bundletransfer.ValidCredentials(in.Credentials) {
		writeError(w, 400, "invalid_arg", "invalid note or credentials; use local or proxy:<name>@<peer>")
		return
	}
	if len(in.GitRef) > 512 {
		writeError(w, 400, "invalid_arg", "git ref exceeds 512 bytes")
		return
	}
	actor, err := db.GetAgentByConv(caller)
	if err != nil || actor == nil || !actor.Active() || actor.CurrentConvID != caller {
		writeError(w, 409, "source", "teleport requires your current active agent generation")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation is disconnected")
		return
	}
	intent := bundletransfer.TeleportIntent{Version: 1, Chain: proto.NewEnvelopeID(), OriginInstance: rt.id.ID(), OriginAgent: actor.AgentID, SourceAgent: actor.AgentID, SourceConv: caller, Clone: in.Clone, Home: in.Home, Note: in.Note, Credentials: in.Credentials, Require: in.Require, GitRef: in.GitRef}
	previous, err := db.FederationTeleportForAgent(actor.AgentID)
	if err != nil {
		writeError(w, 503, "provenance", "could not read teleport provenance")
		return
	}
	if previous != nil {
		intent.Chain = previous.Intent.Chain
		intent.OriginInstance = previous.Intent.OriginInstance
		intent.OriginAgent = previous.Intent.OriginAgent
		intent.Hops = append([]bundletransfer.TeleportHop(nil), previous.Intent.Hops...)
	}
	if in.Home {
		if in.Peer != "" || in.Node != "" {
			writeError(w, 400, "invalid_arg", "--home cannot be combined with a destination")
			return
		}
		if previous == nil {
			writeError(w, 409, "no_home", "this agent has no teleport origin")
			return
		}
		in.Peer = intent.OriginInstance
	}
	explanation := fedPlacementExplanation{Selector: in.Node, Require: in.Require, Prefer: in.Prefer, Candidates: []fedPlacementCandidate{}}
	if in.Node != "" {
		if in.Peer != "" {
			writeError(w, 400, "invalid_arg", "peer and node are mutually exclusive")
			return
		}
		if in.Prefer == "" {
			in.Prefer = "least-loaded"
		}
		explanation.Prefer = in.Prefer
		if in.Prefer != "least-loaded" && in.Prefer != "most-free-ram" {
			writeError(w, 400, "invalid_arg", "prefer must be least-loaded or most-free-ram")
			return
		}
		match, err := proto.ParseNodeMatch(in.Require)
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		peers, pool, err := placementPeers(in.Node)
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		req := fedSpawnSendReq{Group: in.Group, Require: in.Require, Prefer: in.Prefer}
		authority := placementAuthority{Supported: func(cat *proto.CatalogPayload) bool { return cat.AgentTeleports == 1 }, GroupAllowed: teleportGroupAllowed}
		for _, p := range peers {
			row, visible := placementCandidateWithAuthority(r, p, req, caller, match, authority)
			if visible {
				explanation.Candidates = append(explanation.Candidates, row)
			}
		}
		order := placementOrder(explanation.Candidates, in.Prefer)
		if len(order) == 0 {
			writeJSON(w, 409, map[string]any{"code": "no_candidate", "error": "no eligible teleport destination; source remains active", "placement": explanation})
			return
		}
		selected := &explanation.Candidates[order[0]]
		if pool != "" {
			yes, err := db.FederationNodeGroupContainsID(pool, selected.Instance)
			if err != nil || !yes {
				writeError(w, 409, "pool_changed", "selected node left the pool; retry selection")
				return
			}
		}
		p, err := db.GetFederationPeer(selected.Instance)
		if err != nil || p == nil {
			writeError(w, 409, "peer_changed", "selected peer is no longer trusted")
			return
		}
		checked, visible := placementCandidateWithAuthority(r, *p, req, caller, match, authority)
		if !visible || !checked.Eligible {
			writeError(w, 409, "candidate_changed", "selected node is no longer eligible; source remains active")
			return
		}
		in.Peer, in.Group = selected.Instance, checked.Group
		explanation.Selected = selected.Instance
		selected.Attempt = "selected"
	} else if in.Require != "" || in.Prefer != "" {
		writeError(w, 400, "invalid_arg", "require/prefer need --node")
		return
	}
	p, err := resolveFederationPeer(in.Peer)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	cat, _, err := fedCatalogFor(p.InstanceID)
	if err != nil || cat == nil || cat.AgentTeleports != 1 {
		writeError(w, 409, "unsupported_peer", "peer has not advertised teleport support")
		return
	}
	if in.Group == "" {
		groups := []string{}
		for _, g := range cat.Groups {
			if teleportGroupAllowed(r, caller, p.InstanceID, g) {
				groups = append(groups, g.Name)
			}
		}
		if len(groups) != 1 {
			writeError(w, 409, "group_required", "specify --group unless exactly one authorized receiving group exists")
			return
		}
		in.Group = groups[0]
	}
	if _, ok := requirePermission(w, r, PermSelfTeleport, ActionContext{RemotePeer: p.InstanceID, RemoteGroup: in.Group}); !ok {
		return
	}
	if err := validateTeleportLimits(&intent, p.InstanceID, teleportLocalLimits()); err != nil {
		writeError(w, 409, "teleport_limit", err.Error())
		return
	}
	body, _ := json.Marshal(fedShareAgentRequest{Agent: "self", Peer: p.InstanceID, Group: in.Group, History: true})
	inner := r.Clone(context.WithValue(r.Context(), teleportSourceContextKey{}, &intent))
	path := "/v1/federation/move-agent"
	if in.Clone {
		path = "/v1/federation/share-agent"
	}
	inner.URL = &url.URL{Path: path}
	inner.Body = ioNop(body)
	inner.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	handleFederationShareAgent(rec, inner)
	var out map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		writeError(w, 500, "teleport", "invalid transfer response")
		return
	}
	if in.Node != "" {
		out["placement"] = explanation
	}
	out["source_active"] = true
	if rec.Code == 200 {
		out["teleport_state"] = "offered"
		if !in.Clone {
			out["teleport_state"] = "awaiting_running_confirmation"
		}
	}
	writeJSON(w, rec.Code, out)
}
func ioNop(body []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(body)) }
func handleFederationTeleportSwitch(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "freeze or enable teleports") {
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil {
		writeError(w, 400, "json", "invalid teleport switch")
		return
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		if cfg.Federation.Teleport == nil {
			cfg.Federation.Teleport = &config.FederationTeleportConfig{}
		}
		cfg.Federation.Teleport.Disabled = in.Disabled
		return nil
	})
	if err != nil {
		writeError(w, 500, "config", err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"disabled": in.Disabled})
}

func teleportPredecessor(agent string) *db.FederationMoveLink {
	t, err := db.FederationTeleportForAgent(agent)
	if err != nil || t == nil {
		return nil
	}
	return &db.FederationMoveLink{Instance: t.Peer, Agent: t.Intent.SourceAgent, Offer: t.Offer}
}
func handleSelfTeleports(w http.ResponseWriter, r *http.Request) {
	conv, ok := requireAgent(w, r)
	if !ok {
		return
	}
	id, err := db.AgentIDForConv(conv)
	if err != nil {
		writeError(w, 503, "identity", "could not resolve identity")
		return
	}
	rows, err := db.ListFederationTeleports()
	if err != nil {
		writeError(w, 503, "teleports", "could not list teleports")
		return
	}
	out := []map[string]any{}
	for _, t := range rows {
		if t.Intent.SourceAgent == id || t.TargetAgent == id {
			if t.Direction == "out" {
				if m, err := db.GetFederationAgentMove("out", t.Peer, t.Offer); err == nil && m != nil {
					t.State = m.State
					if m.MovedTo != nil {
						t.TargetAgent = m.MovedTo.Agent
					}
				} else if o, err := db.GetFederationBundleOffer("out", t.Peer, t.Offer); err == nil && o != nil {
					t.State = o.State
				}
			}
			out = append(out, map[string]any{"offer": t.Offer, "peer": t.Peer, "direction": t.Direction, "state": t.State, "target_agent": t.TargetAgent, "intent": t.Intent, "credentials": t.Credentials})
		}
	}
	writeJSON(w, 200, out)
}
