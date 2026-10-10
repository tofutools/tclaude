package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// This peer-only grant authorizes issued leases, never ordinary gateway opens.
const PermModelsProxyLeased = "models.proxy.leased"

func fedPeerLeasedModelAllows(peer, name string) bool {
	instance, err := modelProxyPolicy(name)
	if err != nil {
		return false
	}
	for _, blocked := range instance.ModelPolicy.BlockedPeers {
		successor, resolveErr := db.ResolveFederationIdentitySuccessor(blocked)
		if resolveErr != nil || blocked == peer || successor == peer {
			return false
		}
	}
	p, err := db.GetFederationPeer(peer)
	if err != nil || p == nil {
		return false
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if (g.Slug == PermModelsProxyLeased || g.Slug == PermModelsProxy) && (g.Scope == "" || g.Scope == "http_proxy="+name) {
			return true
		}
	}
	return false
}
func modelOpenAllowed(peer string, p proto.ModelOpenPayload, touch bool) bool {
	if p.Lease == "" {
		return fedPeerModelAllows(peer, p.Proxy)
	}
	return fedPeerLeasedModelAllows(peer, p.Proxy) && db.CheckModelProxyLease(p.Lease, peer, p.Proxy, p.Session, p.Generation, touch) == nil
}
func modelLeaseWorker(row *db.SessionRow) string {
	if row.ConvID != "" {
		if actor, _ := db.GetAgentByConv(row.ConvID); actor != nil {
			return actor.AgentID
		}
	}
	if pending, _ := db.GetPendingSpawn(row.ID); pending != nil {
		return pending.AgentID
	}
	return ""
}

// Creation spends the requester's gateway account and requires its ordinary
// models.proxy authority for this receiving peer and named proxy.
func prepareRequesterLease(r *http.Request, caller string, peer *db.FederationPeer, request, kind, mode string) (string, string, error) {
	if mode == "" || mode == "local" {
		return mode, "", nil
	}
	ref, ok := strings.CutPrefix(mode, "proxy:")
	if !ok {
		return "", "", errors.New("credentials must be local or proxy:<name>@self")
	}
	name, selector, ok := strings.Cut(ref, "@")
	if !ok || !validModelProxyName(name) {
		return "", "", errors.New("invalid requester gateway reference")
	}
	rt := currentFederation()
	if rt == nil {
		return "", "", errors.New("requester gateway federation is offline")
	}
	if selector != "self" && selector != rt.id.ID() && selector != rt.name {
		return mode, "", nil
	} // Existing third-party teleport mode.
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil || cat == nil || cat.RequesterPays != 1 {
		return "", "", errors.New("receiving peer does not support requester-pays; upgrade it before requesting this account mode")
	}
	instance, err := modelProxyPolicy(name)
	if err != nil {
		return "", "", err
	}
	if caller != "" {
		allowed, _, err := permissionAllowsAction(r, caller, PermModelsProxy, ActionContext{RemotePeer: peer.InstanceID, HTTPProxy: name})
		if err != nil || !allowed {
			return "", "", errors.New("models.proxy does not cover the receiving peer and requester gateway")
		}
	}
	if !fedPeerLeasedModelAllows(peer.InstanceID, name) {
		return "", "", errors.New("grant the receiving peer models.proxy.leased for this named gateway")
	}
	hours := instance.ModelPolicy.LeaseIdleHours
	if hours == 0 {
		hours = 24
	}
	if hours < 1 || hours > 8760 {
		return "", "", errors.New("lease_idle_hours must be between 1 and 8760")
	}
	id := proto.NewEnvelopeID()
	err = db.IssueModelProxyLease(db.ModelProxyLease{ID: id, Peer: peer.InstanceID, Request: request, Kind: kind, Proxy: name, IdleSeconds: int64(hours) * 3600})
	if err != nil {
		return "", "", err
	}
	recordFederationAudit("models.lease.issue", peer.InstanceID, "", name, "request="+request+" lease="+id+" payer="+rt.id.ID(), 200)
	return "proxy:" + name + "@" + rt.id.ID(), id, nil
}

func (rt *fedRuntime) activateModelLease(ctx context.Context, peer *db.FederationPeer, l db.ModelProxyWorkerLease, row *db.SessionRow) error {
	p := proto.ModelLeasePayload{Lease: l.Lease, Request: l.Request, Kind: l.Kind, Proxy: l.Proxy, Worker: l.Worker, Session: row.ID, Generation: row.ExitLaunchGeneration}
	id := proto.NewEnvelopeID()
	ch := make(chan bool, 1)
	rt.modelsMu.Lock()
	rt.modelsLocked().leaseWaiters[id] = ch
	rt.modelsLocked().leasePeers[id] = peer.InstanceID
	rt.modelsMu.Unlock()
	defer func() {
		rt.modelsMu.Lock()
		delete(rt.modelsLocked().leaseWaiters, id)
		delete(rt.modelsLocked().leasePeers, id)
		rt.modelsMu.Unlock()
	}()
	rt.sendControl(peer.InstanceID, proto.KindModelLease, id, p)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rt.ctx.Done():
		return rt.ctx.Err()
	case <-timer.C:
		return errors.New("requester gateway lease activation timed out")
	case ok := <-ch:
		if !ok {
			return errors.New("requester gateway lease refused")
		}
		return nil
	}
}
func (rt *fedRuntime) acceptModelLease(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.ModelLeasePayload
	if env.From.Agent != "" || env.To.Agent != "" || env.DecodePayload(&p) != nil || !proto.ValidStreamID(p.Lease) || !proto.ValidStreamID(p.Request) || !validModelProxyName(p.Proxy) || !proto.ValidAgentRef(p.Worker) || len(p.Session) > 128 || len(p.Generation) > 128 || proto.StripControls(p.Session) != p.Session || proto.StripControls(p.Generation) != p.Generation {
		return
	}
	l, err := db.GetModelProxyLease(p.Lease)
	ok := err == nil && l != nil && l.Peer == peer.InstanceID && l.Request == p.Request && l.Kind == p.Kind && l.Proxy == p.Proxy
	if ok && !p.Revoke && l.Worker == "" {
		switch l.Kind {
		case "spawn":
			o, e := db.GetFederationOutbox(l.Request)
			ok = e == nil && o != nil && o.ToInstance == peer.InstanceID && o.Kind == proto.KindSpawnReq && o.State != db.FedOutboxRefused && o.ExpiresAt.After(time.Now())
		case "teleport":
			o, e := db.GetFederationBundleOffer("out", peer.InstanceID, l.Request)
			ok = e == nil && o != nil && o.Descriptor.Teleport != nil && o.Descriptor.ExpiresAt.After(time.Now()) && o.State != "abandoned"
		default:
			ok = false
		}
	}
	if ok && p.Revoke {
		ok = db.RevokeModelProxyLease(p.Lease, peer.InstanceID) == nil
	} else if ok {
		ok = fedPeerLeasedModelAllows(peer.InstanceID, p.Proxy)
		if ok {
			ok = db.ActivateModelProxyLease(db.ModelProxyLease{ID: p.Lease, Peer: peer.InstanceID, Request: p.Request, Kind: p.Kind, Proxy: p.Proxy, Worker: p.Worker, Session: p.Session, Generation: p.Generation}) == nil
		}
	}
	if ok {
		recordFederationAudit("models.lease.bind", peer.InstanceID, p.Worker, p.Proxy, "request="+p.Request+" lease="+p.Lease+" generation="+p.Generation, 200)
	}
	rt.sendControl(peer.InstanceID, proto.KindModelLeaseAnswer, env.InReplyTo, proto.ModelLeaseAnswerPayload{OK: ok})
}
func (rt *fedRuntime) acceptModelLeaseAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.ModelLeaseAnswerPayload
	if env.From.Agent != "" || env.DecodePayload(&p) != nil {
		return
	}
	if p.OK {
		_ = db.ForgetClosedModelProxyWorkerLease(env.InReplyTo, peer.InstanceID)
	}
	rt.modelsMu.Lock()
	waiter := rt.modelsLocked().leaseWaiters[env.InReplyTo]
	expected := rt.modelsLocked().leasePeers[env.InReplyTo]
	rt.modelsMu.Unlock()
	if waiter != nil && expected == peer.InstanceID {
		select {
		case waiter <- p.OK:
		default:
		}
	}
}

func queueRequesterSpawn(r *http.Request, caller string, peer *db.FederationPeer, group string, req fedSpawnSendReq, placement int) (*db.FederationOutboxRow, error) {
	id := proto.NewEnvelopeID()
	mode, lease, err := prepareRequesterLease(r, caller, peer, id, "spawn", req.Credentials)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(mode, "proxy:") && lease == "" {
		return nil, errors.New("remote spawn requester gateway must use proxy:<name>@self")
	}
	return queueFederatedEnvelope(fedOutgoing{envelopeID: id, fromConv: caller, peer: peer, kind: proto.KindSpawnReq, toLabel: group + "@" + peerDisplay(peer), subject: "spawn request", preview: req.Brief, ttl: fedSpawnTTL, payload: proto.SpawnRequestPayload{Profile: req.Profile, Credentials: mode, ModelLease: lease, Group: group, Name: req.Name, Role: req.Role, Brief: req.Brief, Require: req.Require, PlacementVersion: placement}})
}

func (rt *fedRuntime) revokeStaleModelLeases() {
	rows, err := db.StaleModelProxyWorkerLeases()
	if err != nil {
		return
	}
	for _, l := range rows {
		rt.sendControl(l.Gateway, proto.KindModelLease, l.Lease, proto.ModelLeasePayload{Lease: l.Lease, Request: l.Request, Kind: l.Kind, Proxy: l.Proxy, Worker: l.Worker, Revoke: true})
	}
}

func handleModelProxyLeases(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect or revoke requester gateway leases") {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		rows, err := db.ListModelProxyLeases()
		if err != nil {
			modelError(w, 503, "model gateway leases unavailable")
			return
		}
		writeJSON(w, 200, rows)
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || !proto.ValidStreamID(in.ID) {
		modelError(w, 400, "select a valid lease id")
		return
	}
	l, err := db.GetModelProxyLease(in.ID)
	if err != nil || l == nil {
		modelError(w, 404, "model gateway lease not found")
		return
	}
	if err = db.RevokeModelProxyLease(l.ID, l.Peer); err != nil {
		modelError(w, 503, "model gateway lease revocation unavailable")
		return
	}
	recordFederationAudit("models.lease.revoke", l.Peer, l.Worker, l.Proxy, "request="+l.Request+" lease="+l.ID, 200)
	writeJSON(w, 200, map[string]bool{"revoked": true})
}
