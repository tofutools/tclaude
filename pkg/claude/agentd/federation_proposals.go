package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const peerProposalTTL = 24 * time.Hour
const maxProposalItems = 32

func proposalHash(p proto.PeerProposalPayload) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func proposalLocalID() (string, error) {
	id, err := federationIdentity()
	if err != nil {
		return "", err
	}
	return id.ID(), nil
}

func validatePeerProposal(p proto.PeerProposalPayload, local, peer string) error {
	deadline, err := time.Parse(time.RFC3339Nano, p.Deadline)
	if err != nil || !deadline.After(time.Now()) || deadline.After(time.Now().Add(peerProposalTTL+time.Minute)) {
		return errors.New("proposal expired or deadline too distant")
	}
	if !proto.ValidStreamID(p.ID) || p.Revision < 1 || p.Revision > 32 || len(p.Note) > 4096 || len(p.Items) < 1 || len(p.Items) > maxProposalItems || p.Origin != local && p.Origin != peer {
		return errors.New("invalid proposal")
	}
	ids := map[string]bool{}
	for _, item := range p.Items {
		if !proto.ValidStreamID(item.ID) || ids[item.ID] || item.Host != local && item.Host != peer || len(item.GroupName) > 128 || len(item.Description) > 4096 || item.MemberCount < 0 || !proto.ValidGroupIdentity(item.GroupID) {
			return errors.New("invalid proposal item")
		}
		ids[item.ID] = true
		switch item.Kind {
		case proto.ProposalShareVisibility:
			if item.GroupID == "" || item.Source != "" || item.SourceGroupID != "" || item.Mode != "" {
				return errors.New("invalid visibility share")
			}
		case proto.ProposalGroupLink:
			if item.Source != local && item.Source != peer || item.Source == item.Host || item.GroupID == "" || item.SourceGroupID == "" || !proto.ValidGroupIdentity(item.SourceGroupID) || len(item.SourceGroupName) > 128 || !db.ValidLinkMode(item.Mode) || item.IncludeMembers {
				return errors.New("invalid group edge")
			}
		default:
			return errors.New("unsupported proposal item")
		}
		if item.Status != "" && item.Status != "pending" {
			return errors.New("proposal items cannot assert approval")
		}
	}
	return nil
}

func proposalEvent(p proto.PeerProposalPayload, peer, kind string, payload any) (db.FederationProposalEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return db.FederationProposalEvent{}, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, p.Deadline)
	return db.FederationProposalEvent{ID: proto.NewEnvelopeID(), ProposalID: p.ID, Peer: peer, Kind: kind, Payload: string(raw), ExpiresAt: deadline}, err
}

// flushProposalEvents converts durable intents to the existing sealed outbox.
// A crash between queue insertion and marking is safe: the intent's fixed
// envelope ID makes insertion/delivery idempotent.
func (rt *fedRuntime) flushProposalEvents(now time.Time) {
	_ = db.ExpireFederationProposals(now)
	events, err := db.PendingFederationProposalEvents(now)
	if err != nil {
		return
	}
	for _, event := range events {
		peer, err := db.GetFederationPeer(event.Peer)
		if err != nil || peer == nil {
			continue
		}
		row, err := db.GetFederationOutbox(event.ID)
		if err != nil {
			continue
		}
		if row == nil {
			_, err = queueFederatedEnvelope(fedOutgoing{envelopeID: event.ID, peer: peer, kind: event.Kind, toLabel: peerDisplay(peer), subject: "Peer proposal", preview: "Group sharing and communication proposal", ttl: time.Until(event.ExpiresAt), payload: json.RawMessage(event.Payload)})
			if err != nil {
				continue
			}
		}
		_ = db.MarkFederationProposalEventQueued(event.ID)
	}
}

func handleFederationLinkTargets(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "choose cross-node group links") {
		return
	}
	type group struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Auto bool   `json:"auto"`
	}
	type peerRow struct {
		Peer        string  `json:"peer"`
		Label       string  `json:"label"`
		Trust       string  `json:"trust"`
		LinkCapable bool    `json:"link_capable"`
		Groups      []group `json:"groups"`
	}
	out := []peerRow{}
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	for _, peer := range peers {
		if !peerHasAccess(peer.InstanceID) {
			continue
		}
		cat, _, err := fedCatalogFor(peer.InstanceID)
		if err != nil || cat == nil {
			continue
		}
		level := "limited"
		if peer.TrustLevel == db.FederationTrustUnrestricted {
			level = "unrestricted"
		}
		row := peerRow{Peer: peer.InstanceID, Label: peerDisplay(&peer), Trust: level, LinkCapable: cat.PeerProposals, Groups: []group{}}
		for _, g := range cat.Groups {
			row.Groups = append(row.Groups, group{ID: g.ID, Name: g.Name, Auto: g.HasCap(proto.CapGroupLinkAccept)})
		}
		out = append(out, row)
	}
	result := map[string]any{"hub_connected": fedConnected(), "peers": out}
	if !fedConnected() {
		result["reason"] = "Connect to the hub to propose a cross-node link"
	}
	writeJSON(w, 200, result)
}

func registerFederationProposalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/federation/link-targets", handleFederationLinkTargets)
	mux.HandleFunc("POST /api/federation/proposals", handlePeerProposalCreate)
	mux.HandleFunc("POST /api/federation/proposals/{id}/decision", handlePeerProposalDecision)
	mux.HandleFunc("GET /api/federation/proposals", handlePeerProposalList)
	mux.HandleFunc("GET /api/federation/proposals/{id}", handlePeerProposalList)
}

// proposalOwnedGroups refreshes local descriptions/counts from authoritative
// storage. Proposal payloads never carry roster names.
func proposalOwnedGroups(p *proto.PeerProposalPayload, local string) error {
	for i := range p.Items {
		item := &p.Items[i]
		if item.Host == local {
			g, err := db.FederationGroupByIdentity(item.GroupID)
			if err != nil {
				return err
			}
			if g == nil || g.IsArchived() {
				return fmt.Errorf("local group no longer exists")
			}
			item.GroupName, item.Description = g.Name, g.Descr
			members, err := db.ListAgentGroupMembers(g.ID)
			if err != nil {
				return err
			}
			item.MemberCount = len(members)
		}
		if item.Source == local {
			g, err := db.FederationGroupByIdentity(item.SourceGroupID)
			if err != nil {
				return err
			}
			if g == nil || g.IsArchived() {
				return fmt.Errorf("source group no longer exists")
			}
			item.SourceGroupName = g.Name
		}
		item.Status = "pending"
	}
	return nil
}

func proposalPeerGroup(cat *proto.CatalogPayload, id string) *proto.CatalogGroup {
	if cat == nil {
		return nil
	}
	for _, g := range cat.Groups {
		if g.ID == strings.TrimSpace(id) && g.ID != "" {
			return &g
		}
	}
	return nil
}

func proposalItemHash(item proto.ProposalItem) string {
	b, _ := json.Marshal(item)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func proposalAuthority(p proto.PeerProposalPayload, local string) string {
	authority := map[string]string{}
	for _, item := range p.Items {
		if item.Host == local || item.Source == local {
			authority[item.ID] = proposalItemHash(item)
		}
	}
	raw, _ := json.Marshal(authority)
	return string(raw)
}
func proposalRecord(p proto.PeerProposalPayload, peer, authority, state string) db.FederationPeerProposal {
	raw, _ := json.Marshal(p)
	deadline, _ := time.Parse(time.RFC3339Nano, p.Deadline)
	now := time.Now()
	return db.FederationPeerProposal{ID: p.ID, Peer: peer, Origin: p.Origin, Revision: p.Revision, Body: string(raw), Authority: authority, State: state, CreatedAt: now, Deadline: deadline, UpdatedAt: now}
}
func normalizeProposalAPI(p *proto.PeerProposalPayload, local, peer string) error {
	cat, _, err := fedCatalogFor(peer)
	if err != nil {
		return err
	}
	if cat == nil || !cat.PeerProposals {
		return errors.New("peer does not support proposals")
	}
	for i := range p.Items {
		item := &p.Items[i]
		if item.ID == "" {
			item.ID = proto.NewEnvelopeID()
		}
		if item.Host == "us" {
			item.Host = local
		}
		if item.Host == "them" {
			item.Host = peer
		}
		if item.Source == "us" {
			item.Source = local
		}
		if item.Source == "them" {
			item.Source = peer
		}
		normalize := func(host string, id *string, name *string) error {
			if host == local {
				g, err := db.FederationGroupByIdentity(*id)
				if err != nil {
					return err
				}
				if g == nil {
					g, err = resolveGroupSelector(*id)
					if err != nil {
						return err
					}
				}
				if g.IsArchived() {
					return errors.New("group is archived")
				}
				*id, err = db.FederationGroupIdentity(g.ID)
				*name = g.Name
				return err
			}
			if host != peer {
				return errors.New("item host must be one of these two nodes")
			}
			g := proposalPeerGroup(cat, *id)
			if g == nil {
				return errors.New("peer group is not shared or was deleted")
			}
			*name = g.Name
			return nil
		}
		if err := normalize(item.Host, &item.GroupID, &item.GroupName); err != nil {
			return err
		}
		if item.Kind == proto.ProposalGroupLink {
			if err := normalize(item.Source, &item.SourceGroupID, &item.SourceGroupName); err != nil {
				return err
			}
		}
	}
	return proposalOwnedGroups(p, local)
}

func handlePeerProposalCreate(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "propose group sharing and links") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		Peer  string               `json:"peer"`
		Note  string               `json:"note"`
		Items []proto.ProposalItem `json:"items"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, "invalid_arg", "invalid proposal body")
		return
	}
	peer, err := resolveFederationPeerOpt(in.Peer, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	local, err := proposalLocalID()
	if err != nil {
		writeError(w, 503, "identity", err.Error())
		return
	}
	p := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: local, Revision: 1, Deadline: time.Now().Add(peerProposalTTL).UTC().Format(time.RFC3339Nano), Note: in.Note, Items: in.Items}
	if err = normalizeProposalAPI(&p, local, peer.InstanceID); err == nil {
		err = validatePeerProposal(p, local, peer.InstanceID)
	}
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	e, err := proposalEvent(p, peer.InstanceID, proto.KindPeerProposal, p)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	row := proposalRecord(p, peer.InstanceID, proposalAuthority(p, local), "sent")
	links, e2 := proposalLinks(p, local)
	if e2 != nil {
		writeError(w, 400, "invalid_arg", e2.Error())
		return
	}
	if _, err = db.SaveFederationProposal(row, 0, &e, links...); err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if rt := currentFederation(); rt != nil {
		rt.kickOutbox()
	}
	writeJSON(w, 202, proposalView(row, local))
}

func handlePeerProposalList(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read peer proposals") {
		return
	}
	local, err := proposalLocalID()
	if err != nil {
		writeError(w, 503, "identity", err.Error())
		return
	}
	if id := r.PathValue("id"); id != "" {
		row, err := db.GetFederationProposal(id)
		if err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
		if row == nil {
			writeError(w, 404, "not_found", "proposal not found")
			return
		}
		writeJSON(w, 200, proposalView(*row, local))
		return
	}
	rows, err := db.ListFederationProposals()
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	out := []map[string]any{}
	for _, row := range rows {
		if id := r.PathValue("id"); id != "" && row.ID != id {
			continue
		}
		out = append(out, proposalView(row, local))
	}
	if r.PathValue("id") != "" {
		if len(out) == 0 {
			writeError(w, 404, "not_found", "proposal not found")
		} else {
			writeJSON(w, 200, out[0])
		}
		return
	}
	writeJSON(w, 200, map[string]any{"proposals": out})
}
func proposalView(row db.FederationPeerProposal, local string) map[string]any {
	var p proto.PeerProposalPayload
	_ = json.Unmarshal([]byte(row.Body), &p)
	peer, _ := db.GetFederationPeer(row.Peer)
	label := row.Peer
	if peer != nil {
		label = peerDisplay(peer)
	}
	items := []map[string]any{}
	var decision proto.PeerProposalDecisionPayload
	raw, _ := db.FederationProposalDecision(row.ID)
	_ = json.Unmarshal([]byte(raw), &decision)
	selected, narrowed, _ := proposalSelection(p, decision.ItemIDs, decision.WithoutMembers)
	here, remote, _ := db.FederationProposalApplied(row.ID, row.Revision)
	contains := func(ids []string, id string) bool {
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		return false
	}
	for _, item := range p.Items {
		host := "them"
		if item.Host == local {
			host = "us"
		}
		status := "pending"
		if decision.Decision != "" && !selected[item.ID] {
			status = "declined"
		}
		if narrowed[item.ID] {
			item.IncludeMembers = false
		}
		if contains(here, item.ID) || contains(remote, item.ID) {
			status = "accepted"
		}
		v := map[string]any{"id": item.ID, "kind": item.Kind, "host": host, "group_id": item.GroupID, "group_name": item.GroupName, "description": item.Description, "member_count": item.MemberCount, "include_members": item.IncludeMembers, "status": status}
		if item.Kind == proto.ProposalGroupLink {
			direction := "out"
			if item.Host == local {
				direction = "in"
			}
			v["direction"], v["mode"], v["peer_group_id"], v["peer_group_name"] = direction, item.Mode, item.SourceGroupID, item.SourceGroupName
		}
		items = append(items, v)
	}
	return map[string]any{"id": row.ID, "peer": row.Peer, "peer_label": label, "revision": row.Revision, "state": row.State, "deadline": row.Deadline.UTC().Format(time.RFC3339Nano), "note": p.Note, "ours": row.Origin == local, "items": items}
}

func proposalSelection(p proto.PeerProposalPayload, ids, without []string) (map[string]bool, map[string]bool, error) {
	available := map[string]proto.ProposalItem{}
	for _, item := range p.Items {
		available[item.ID] = item
	}
	selected, narrowed := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		if _, ok := available[id]; !ok || selected[id] {
			return nil, nil, errors.New("unknown or duplicate selected item")
		}
		selected[id] = true
	}
	for _, id := range without {
		item, ok := available[id]
		if !ok || !selected[id] || item.Kind != proto.ProposalShareVisibility || !item.IncludeMembers || narrowed[id] {
			return nil, nil, errors.New("invalid roster narrowing")
		}
		narrowed[id] = true
	}
	return selected, narrowed, nil
}

// Local changes come only from a human decision, a previously authorized exact
// item, or a standing ingress grant. A peer receipt is never local consent.
func proposalChanges(p proto.PeerProposalPayload, row db.FederationPeerProposal, selected, narrowed map[string]bool, human bool) ([]db.ProposalLocalChange, error) {
	local, err := proposalLocalID()
	if err != nil {
		return nil, err
	}
	authority := map[string]string{}
	if err = json.Unmarshal([]byte(row.Authority), &authority); err != nil {
		return nil, err
	}
	changes := []db.ProposalLocalChange{}
	for _, item := range p.Items {
		if !selected[item.ID] || item.Host != local && item.Source != local {
			continue
		}
		groupID := item.GroupID
		direction := "in"
		remoteID, remoteName := item.SourceGroupID, item.SourceGroupName
		if item.Kind == proto.ProposalGroupLink && item.Source == local {
			groupID = item.SourceGroupID
			direction = "out"
			remoteID, remoteName = item.GroupID, item.GroupName
		}
		g, err := db.FederationGroupByIdentity(groupID)
		if err != nil {
			return nil, err
		}
		if g == nil || g.IsArchived() {
			return nil, errors.New("local group was deleted or archived")
		}
		authorized := human || authority[item.ID] == proposalItemHash(item)
		if !authorized && item.Kind == proto.ProposalGroupLink && item.Host == local {
			authorized = fedPeerGroupGrant(row.Peer, g.ID, PermGroupsLinkAccept) != nil
		}
		if !authorized {
			return nil, errors.New("local approval required for changed proposal item")
		}
		change := db.ProposalLocalChange{ItemID: item.ID, GroupID: g.ID}
		switch item.Kind {
		case proto.ProposalShareVisibility:
			change.Share = true
			change.IncludeMembers = item.IncludeMembers && !narrowed[item.ID]
		case proto.ProposalGroupLink:
			// Ingress remains constrained to groups currently shared with this peer.
			if direction == "in" && fedPeerGroupGrant(row.Peer, g.ID, "") == nil {
				return nil, errors.New("destination group is no longer shared")
			}
			change.Link = &db.FederationGroupLink{ID: "xlink_" + p.ID + "_" + item.ID, RemoteGroupID: remoteID, RemoteGroupName: remoteName, Direction: direction, Mode: item.Mode}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func applyProposalDecision(row db.FederationPeerProposal, p proto.PeerProposalPayload, d proto.PeerProposalDecisionPayload, human bool, future ...bool) error {
	if d.ID != p.ID || d.Revision != p.Revision || d.RevisionHash != proposalHash(p) || d.Decision != "accept" && d.Decision != "accept_items" && d.Decision != "decline" {
		return errors.New("invalid or stale proposal decision")
	}
	selected, narrowed, err := proposalSelection(p, d.ItemIDs, d.WithoutMembers)
	if err != nil {
		return err
	}
	if d.Decision == "decline" && len(selected) != 0 {
		return errors.New("decline cannot select items")
	}
	// Once a decision is pinned, only the same decision can be retried.
	priorRaw, err := db.FederationProposalDecision(p.ID)
	if err != nil {
		return err
	}
	var prior proto.PeerProposalDecisionPayload
	if priorRaw != "{}" && priorRaw != "" {
		if err = json.Unmarshal([]byte(priorRaw), &prior); err != nil {
			return err
		}
		if d.Decision == "accept_items" {
			approved, _, e := proposalSelection(p, prior.ItemIDs, prior.WithoutMembers)
			if e != nil {
				return e
			}
			for id := range selected {
				if !approved[id] {
					return errors.New("item was declined")
				}
			}
			return nil
		}
		a, b := prior, d
		a.AppliedIDs = nil
		b.AppliedIDs = nil
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			return errors.New("proposal already decided; counter before deciding")
		}
		return nil // A retry must not create another receipt or reapply revoked grants.
	}
	changes := []db.ProposalLocalChange{}
	if d.Decision == "accept" || d.Decision == "accept_items" {
		changes, err = proposalChanges(p, row, selected, narrowed, human)
		if err != nil {
			return err
		}
	}
	if len(future) > 0 && future[0] {
		if !human || len(p.Items) != 1 || p.Items[0].Kind != proto.ProposalGroupLink || len(changes) != 1 || changes[0].Link == nil || changes[0].Link.Direction != "in" {
			return errors.New("future links require one approved ingress edge")
		}
		changes[0].FutureLinks = true
	}
	if d.Decision == "accept_items" {
		here, _, e := db.FederationProposalApplied(p.ID, p.Revision)
		if e != nil {
			return e
		}
		allApplied := len(changes) > 0
		for _, change := range changes {
			found := false
			for _, id := range here {
				if id == change.ItemID {
					found = true
				}
			}
			if !found {
				allApplied = false
			}
		}
		if allApplied || len(changes) == 0 {
			return nil
		}
	}
	d.AppliedIDs = nil
	for _, change := range changes {
		d.AppliedIDs = append(d.AppliedIDs, change.ItemID)
	}
	state := "declined"
	if d.Decision == "accept" {
		state = "applying"
	}
	raw, _ := json.Marshal(d)
	storedDecision := string(raw)
	if d.Decision == "accept_items" {
		storedDecision = "{}"
		state = "pending_items"
	}
	event, err := proposalEvent(p, row.Peer, proto.KindPeerProposalDecision, d)
	if err != nil {
		return err
	}
	ok, err := db.ApplyFederationProposal(p.ID, row.Peer, p.Revision, storedDecision, state, changes, event)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("proposal expired, revoked, or no longer authorized")
	}
	if rt := currentFederation(); rt != nil {
		rt.kickOutbox()
	}
	broadcastFederationCatalogs()
	return nil
}

func handlePeerProposalDecision(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "decide peer proposal") {
		return
	}
	var in struct {
		Revision       int                  `json:"revision"`
		Decision       string               `json:"decision"`
		ItemIDs        []string             `json:"item_ids"`
		WithoutMembers []string             `json:"without_members"`
		Items          []proto.ProposalItem `json:"items"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, "invalid_arg", "invalid proposal decision")
		return
	}
	row, err := db.GetFederationProposal(r.PathValue("id"))
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if row == nil {
		writeError(w, 404, "not_found", "proposal not found")
		return
	}
	var p proto.PeerProposalPayload
	if err = json.Unmarshal([]byte(row.Body), &p); err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if in.Revision != p.Revision {
		writeError(w, 409, "stale_revision", "proposal changed; refresh before deciding")
		return
	}
	local, err := proposalLocalID()
	if err != nil {
		writeError(w, 503, "identity", err.Error())
		return
	}
	if in.Decision == "counter" {
		next := p
		next.Revision++
		next.PreviousHash = proposalHash(p)
		next.Items = in.Items
		if err = normalizeProposalAPI(&next, local, row.Peer); err == nil {
			err = validatePeerProposal(next, local, row.Peer)
		}
		if err == nil {
			err = proposalCounterKeepsApplied(p, next)
		}
		if err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		event, e := proposalEvent(next, row.Peer, proto.KindPeerProposal, next)
		if e != nil {
			writeError(w, 500, "io", e.Error())
			return
		}
		newRow := proposalRecord(next, row.Peer, proposalAuthority(next, local), "countered")
		newRow.CreatedAt = row.CreatedAt
		ok, e := db.SaveFederationProposal(newRow, row.Revision, &event)
		if e != nil {
			writeError(w, 500, "io", e.Error())
			return
		}
		if !ok {
			writeError(w, 409, "stale_revision", "proposal expired or already decided")
			return
		}
		if rt := currentFederation(); rt != nil {
			rt.kickOutbox()
		}
		writeJSON(w, 202, proposalView(newRow, local))
		return
	}
	d := proto.PeerProposalDecisionPayload{ID: p.ID, Revision: p.Revision, RevisionHash: proposalHash(p), Decision: in.Decision, ItemIDs: in.ItemIDs, WithoutMembers: in.WithoutMembers}
	if err = applyProposalDecision(*row, p, d, true); err != nil {
		writeError(w, 409, "proposal_refused", err.Error())
		return
	}
	row, err = db.GetFederationProposal(p.ID)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	writeJSON(w, 202, proposalView(*row, local))
}

func (rt *fedRuntime) acceptPeerProposal(peer *db.FederationPeer, env *proto.Envelope) {
	refuse := func(reason string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: fedCodeMalformed, Reason: reason})
	}
	if env.From.Agent != "" {
		refuse("peer proposals require an operator actor")
		return
	}
	var p proto.PeerProposalPayload
	if err := env.DecodePayload(&p); err != nil {
		refuse("invalid proposal")
		return
	}
	local, err := proposalLocalID()
	if err != nil {
		return
	}
	if err = validatePeerProposal(p, local, peer.InstanceID); err != nil {
		refuse(err.Error())
		return
	}
	existing, err := db.GetFederationProposal(p.ID)
	if err != nil {
		return
	}
	expected := 0
	authority := "{}"
	if existing != nil {
		var previous proto.PeerProposalPayload
		if json.Unmarshal([]byte(existing.Body), &previous) != nil {
			return
		}
		if p.Revision == existing.Revision && proposalHash(p) == proposalHash(previous) {
			rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
			return
		}
		if existing.Peer != peer.InstanceID || p.Origin != previous.Origin || p.Deadline != previous.Deadline || p.Revision != previous.Revision+1 || p.PreviousHash != proposalHash(previous) {
			refuse("stale or mismatched proposal revision")
			return
		}
		if err = proposalCounterKeepsApplied(previous, p); err != nil {
			refuse(err.Error())
			return
		}
		expected = existing.Revision
		oldAuth, kept := map[string]string{}, map[string]string{}
		_ = json.Unmarshal([]byte(existing.Authority), &oldAuth)
		for _, item := range p.Items {
			if oldAuth[item.ID] == proposalItemHash(item) {
				kept[item.ID] = oldAuth[item.ID]
			}
		}
		raw, _ := json.Marshal(kept)
		authority = string(raw)
	} else if p.Revision != 1 || p.Origin != peer.InstanceID || p.PreviousHash != "" {
		refuse("invalid initial proposal")
		return
	}
	state := "sent"
	if p.Revision > 1 {
		state = "countered"
	}
	row := proposalRecord(p, peer.InstanceID, authority, state)
	if existing != nil {
		row.CreatedAt = existing.CreatedAt
	}
	links, err := proposalLinks(p, local)
	if err != nil {
		refuse(err.Error())
		return
	}
	ok, err := db.SaveFederationProposal(row, expected, nil, links...)
	if err != nil {
		return
	}
	if !ok {
		refuse("proposal no longer pending")
		return
	}
	// Auto-accept each covered ingress independently; the other direction
	// of a bidirectional proposal can still await a human decision.
	auto := []string{}
	for _, item := range p.Items {
		if item.Kind != proto.ProposalGroupLink || item.Host != local {
			continue
		}
		if g, e := db.FederationGroupByIdentity(item.GroupID); e == nil && g != nil && fedPeerGroupGrant(peer.InstanceID, g.ID, PermGroupsLinkAccept) != nil {
			auto = append(auto, item.ID)
		}
	}
	if len(auto) > 0 {
		decision := "accept_items"
		if len(auto) == len(p.Items) {
			decision = "accept"
		}
		d := proto.PeerProposalDecisionPayload{ID: p.ID, Revision: p.Revision, RevisionHash: proposalHash(p), Decision: decision, ItemIDs: auto}
		_ = applyProposalDecision(row, p, d, false)
	}
	_, _ = db.MarkFederationEnvelopeSeen(peer.InstanceID, env.ID, env.ExpiresAt)
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

func (rt *fedRuntime) acceptPeerProposalDecision(peer *db.FederationPeer, env *proto.Envelope) {
	refuse := func(reason string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: fedCodeMalformed, Reason: reason})
	}
	if env.From.Agent != "" {
		refuse("proposal decisions require an operator actor")
		return
	}
	var d proto.PeerProposalDecisionPayload
	if env.DecodePayload(&d) != nil {
		refuse("invalid decision")
		return
	}
	row, err := db.GetFederationProposal(d.ID)
	if err != nil {
		return
	}
	if row == nil || row.Peer != peer.InstanceID {
		refuse("unknown proposal")
		return
	}
	var p proto.PeerProposalPayload
	if json.Unmarshal([]byte(row.Body), &p) != nil {
		return
	}
	if d.Revision != p.Revision || d.RevisionHash != proposalHash(p) {
		refuse("stale decision")
		return
	}
	selected, _, err := proposalSelection(p, d.ItemIDs, d.WithoutMembers)
	if err != nil {
		refuse(err.Error())
		return
	}
	applied := map[string]bool{}
	for _, id := range d.AppliedIDs {
		if !selected[id] || applied[id] {
			refuse("invalid receipt item")
			return
		}
		applied[id] = true
	}
	// Shares have a single authorizing host; group links require both halves.
	for _, item := range p.Items {
		if applied[item.ID] && item.Host != peer.InstanceID && item.Source != peer.InstanceID {
			refuse("peer cannot apply this item")
			return
		}
	}
	if err = applyProposalDecision(*row, p, d, false); err != nil {
		refuse(err.Error())
		return
	}
	if err = db.RecordFederationProposalReceipt(p.ID, peer.InstanceID, p.Revision, d.AppliedIDs, time.Now()); err != nil {
		return
	}
	local, _ := proposalLocalID()
	proposalComplete(p, d, local)
	_, _ = db.MarkFederationEnvelopeSeen(peer.InstanceID, env.ID, env.ExpiresAt)
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

func proposalComplete(p proto.PeerProposalPayload, d proto.PeerProposalDecisionPayload, local string) {
	if d.Decision != "accept" {
		return
	}
	here, remote, err := db.FederationProposalApplied(p.ID, p.Revision)
	if err != nil {
		return
	}
	has := func(ids []string, id string) bool {
		for _, v := range ids {
			if id == v {
				return true
			}
		}
		return false
	}
	selected, _, err := proposalSelection(p, d.ItemIDs, d.WithoutMembers)
	if err != nil {
		return
	}
	for _, item := range p.Items {
		if !selected[item.ID] {
			continue
		}
		if (item.Host == local || item.Source == local) && !has(here, item.ID) {
			return
		}
		if (item.Host != local || item.Kind == proto.ProposalGroupLink && item.Source != local) && !has(remote, item.ID) {
			return
		}
	}
	state := "accepted"
	if len(selected) < len(p.Items) {
		state = "partially_accepted"
	}
	_ = db.CompleteFederationProposal(p.ID, p.Revision, state)
}

func proposalLinks(p proto.PeerProposalPayload, local string) ([]db.FederationGroupLink, error) {
	links := []db.FederationGroupLink{}
	for _, item := range p.Items {
		if item.Kind != proto.ProposalGroupLink {
			continue
		}
		groupID, direction, remoteID, remoteName := item.GroupID, "in", item.SourceGroupID, item.SourceGroupName
		if item.Source == local {
			groupID, direction, remoteID, remoteName = item.SourceGroupID, "out", item.GroupID, item.GroupName
		}
		g, err := db.FederationGroupByIdentity(groupID)
		if err != nil {
			return nil, err
		}
		if g == nil || g.IsArchived() {
			return nil, errors.New("local group no longer exists")
		}
		links = append(links, db.FederationGroupLink{ID: "xlink_" + p.ID + "_" + item.ID, LocalGroupID: g.ID, RemoteGroupID: remoteID, RemoteGroupName: remoteName, Direction: direction, Mode: item.Mode, ItemID: item.ID})
	}
	return links, nil
}

func handleRemoteGroupLinkAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, in linkAddBody) {
	if !requireHuman(w, r, "link groups across nodes") {
		return
	}
	peer, err := resolveFederationPeerOpt(in.ToPeer, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	local, err := proposalLocalID()
	if err != nil {
		writeError(w, 503, "identity", err.Error())
		return
	}
	mode := in.Mode
	if mode == "" {
		mode = db.LinkModeMembersToMembers
	}
	gid, err := db.FederationGroupIdentity(g.ID)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	p := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: local, Revision: 1, Deadline: time.Now().Add(peerProposalTTL).UTC().Format(time.RFC3339Nano), Items: []proto.ProposalItem{{Kind: proto.ProposalGroupLink, Host: peer.InstanceID, GroupID: in.ToGroupID, Source: local, SourceGroupID: gid, Mode: mode}}}
	if in.Bidir {
		p.Items = append(p.Items, proto.ProposalItem{Kind: proto.ProposalGroupLink, Host: local, GroupID: gid, Source: peer.InstanceID, SourceGroupID: in.ToGroupID, Mode: mode})
	}
	if err = normalizeProposalAPI(&p, local, peer.InstanceID); err == nil {
		err = validatePeerProposal(p, local, peer.InstanceID)
	}
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	links, err := proposalLinks(p, local)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	event, err := proposalEvent(p, peer.InstanceID, proto.KindPeerProposal, p)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	row := proposalRecord(p, peer.InstanceID, proposalAuthority(p, local), "sent")
	if _, err = db.SaveFederationProposal(row, 0, &event, links...); err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	cat, _, _ := fedCatalogFor(peer.InstanceID)
	remoteGroup := proposalPeerGroup(cat, in.ToGroupID)
	edges := []map[string]any{}
	for _, l := range links {
		state := "pending"
		if l.Direction == "out" && remoteGroup != nil && remoteGroup.HasCap(proto.CapGroupLinkAccept) {
			state = "connecting"
		}
		edges = append(edges, map[string]any{"id": l.ID, "direction": l.Direction, "state": state, "request_id": p.ID, "deadline": p.Deadline})
	}
	if rt := currentFederation(); rt != nil {
		rt.kickOutbox()
	}
	writeJSON(w, 202, map[string]any{"edges": edges})
}

func handleRemoteGroupLinkRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, id string) {
	if !requireHuman(w, r, "revoke cross-node group link") {
		return
	}
	link, err := db.GetFederationGroupLink(id)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if link == nil || link.LocalGroupID != g.ID {
		writeError(w, 404, "not_found", "no link in this group")
		return
	}
	payload := proto.GroupLinkRevokePayload{ProposalID: link.ProposalID, ItemID: link.ItemID}
	raw, _ := json.Marshal(payload)
	event := db.FederationProposalEvent{ID: proto.NewEnvelopeID(), ProposalID: link.ProposalID, Peer: link.Peer, Kind: proto.KindGroupLinkRevoke, Payload: string(raw), ExpiresAt: time.Now().Add(peerProposalTTL)}
	if _, err = db.RevokeFederationGroupLink(id, &event); err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if rt := currentFederation(); rt != nil {
		rt.kickOutbox()
	}
	w.WriteHeader(204)
}

func (rt *fedRuntime) acceptGroupLinkRevoke(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.GroupLinkRevokePayload
	if env.From.Agent != "" || env.DecodePayload(&p) != nil {
		return
	}
	link, err := db.GetFederationGroupLink("xlink_" + p.ProposalID + "_" + p.ItemID)
	if err != nil || link == nil || link.Peer != peer.InstanceID {
		return
	}
	if err = db.RecordFederationGroupLinkRevoked(link.ID, peer.InstanceID); err != nil {
		return
	}
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

func proposalAccessRequests() []dashboardAccessRequest {
	out := []dashboardAccessRequest{}
	local, err := proposalLocalID()
	if err != nil {
		return out
	}
	_ = db.ExpireFederationProposals(time.Now())
	rows, err := db.ListFederationProposals()
	if err != nil {
		return out
	}
	for _, row := range rows {
		view := proposalView(row, local)
		purpose := "peer_proposal"
		var link map[string]any
		var p proto.PeerProposalPayload
		_ = json.Unmarshal([]byte(row.Body), &p)
		if len(p.Items) == 1 && p.Items[0].Kind == proto.ProposalGroupLink {
			purpose = "group_link"
			item := p.Items[0]
			direction := "out"
			if item.Host == local {
				direction = "in"
			}
			link = map[string]any{"source_group": item.SourceGroupName, "source_peer_label": view["peer_label"], "dest_group": item.GroupName, "mode": item.Mode, "direction": direction}
		}
		status := row.State
		if status == "sent" || status == "countered" {
			status = "pending"
		}
		out = append(out, dashboardAccessRequest{ID: row.ID, Purpose: purpose, Proposal: view, Link: link, OriginPeer: row.Peer, Perm: PermGroupsLinkAccept, Status: status, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339), Deadline: row.Deadline.UTC().Format(time.RFC3339), BodyLabel: p.Note})
	}
	return out
}

func serveProposalAccessDecision(w http.ResponseWriter, r *http.Request, row db.FederationPeerProposal) {
	if !requireHuman(w, r, "decide peer link request") {
		return
	}
	var in struct {
		Decision string `json:"decision"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil {
		writeError(w, 400, "invalid_arg", "invalid link decision")
		return
	}
	var p proto.PeerProposalPayload
	if json.Unmarshal([]byte(row.Body), &p) != nil {
		writeError(w, 500, "io", "invalid stored proposal")
		return
	}
	if len(p.Items) != 1 || p.Items[0].Kind != proto.ProposalGroupLink {
		writeError(w, 400, "invalid_arg", "use the proposal decision route to select items")
		return
	}
	d := proto.PeerProposalDecisionPayload{ID: p.ID, Revision: p.Revision, RevisionHash: proposalHash(p), Decision: "accept", ItemIDs: []string{p.Items[0].ID}}
	switch in.Decision {
	case "approve", "approve_future":
	case "deny":
		d.Decision = "decline"
		d.ItemIDs = nil
	default:
		writeError(w, 400, "invalid_arg", "use approve, approve_future, or deny")
		return
	}
	if err := applyProposalDecision(row, p, d, true, in.Decision == "approve_future"); err != nil {
		writeError(w, 409, "proposal_refused", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"decision": in.Decision})
}

func proposalCounterKeepsApplied(previous, next proto.PeerProposalPayload) error {
	here, remote, err := db.FederationProposalApplied(previous.ID, previous.Revision)
	if err != nil {
		return err
	}
	applied := map[string]bool{}
	for _, id := range append(here, remote...) {
		applied[id] = true
	}
	replacements := map[string]string{}
	for _, item := range next.Items {
		replacements[item.ID] = proposalItemHash(item)
	}
	for _, item := range previous.Items {
		if applied[item.ID] && replacements[item.ID] != proposalItemHash(item) {
			return errors.New("an applied item cannot be removed or changed; revoke its link or grant separately")
		}
	}
	return nil
}
