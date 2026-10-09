package agentd

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// fedErr is a client-facing error with an HTTP status and code.
type fedErr struct {
	status int
	code   string
	msg    string
}

func (e *fedErr) Error() string { return e.msg }

func newFedErr(status int, code, format string, a ...any) *fedErr {
	return &fedErr{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

func writeFedErr(w http.ResponseWriter, err error) {
	var fe *fedErr
	if errors.As(err, &fe) {
		writeError(w, fe.status, fe.code, fe.msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "io", err.Error())
}

// resolveFederationPeer resolves a peer reference: label, hub-reported
// name, full instance id, or an instance-id prefix of 8+ characters.
func resolveFederationPeer(ref string) (*db.FederationPeer, error) {
	return resolveFederationPeerOpt(ref, true)
}

// resolveFederationPeerOpt is resolveFederationPeer with the hub-reported
// name optionally excluded. Message addressing excludes it: that name is
// chosen by the peer (or the hub), so it must not be able to capture an
// address meant for a local agent.
func resolveFederationPeerOpt(ref string, allowName bool) (*db.FederationPeer, error) {
	ref = strings.TrimSpace(ref)
	peers, err := db.ListFederationPeers()
	if err != nil {
		return nil, err
	}
	var hits []db.FederationPeer
	for _, p := range peers {
		switch {
		case p.InstanceID == ref, p.Label != "" && p.Label == ref:
			return &p, nil
		case allowName && p.Name != "" && strings.EqualFold(p.Name, ref),
			len(ref) >= 8 && strings.HasPrefix(p.InstanceID, ref),
			len(ref) >= 8 && strings.HasPrefix(p.InstanceID, proto.InstanceIDPrefix+ref):
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return nil, newFedErr(http.StatusNotFound, "not_found", "no trusted peer matches %q", ref)
	case 1:
		return &hits[0], nil
	}
	return nil, newFedErr(http.StatusConflict, "ambiguous", "%q matches %d trusted peers; use the label or instance id", ref, len(hits))
}

// splitFederatedAddress splits "member@peer" at the LAST '@'.
func splitFederatedAddress(addr string) (member, peer string, ok bool) {
	i := strings.LastIndex(addr, "@")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// isFederatedAddress reports whether to should be routed to a remote
// instance: it does not resolve locally, has the member@peer shape, and the
// peer part is a trusted peer's operator-chosen label or instance id. A local
// agent whose title contains '@' therefore always wins.
func isFederatedAddress(to string) bool {
	if strings.HasPrefix(to, multicastPrefix) {
		return false
	}
	_, peerRef, ok := splitFederatedAddress(to)
	if !ok {
		return false
	}
	p, err := resolveFederationPeerOpt(peerRef, false)
	if err != nil || p == nil {
		return false
	}
	if _, _, lerr := agent.ResolveSelector(to); lerr == nil || errors.Is(lerr, agent.ErrAmbiguous) {
		return false
	}
	return true
}

// fedTarget is a resolved remote recipient.
type fedTarget struct {
	peer    *db.FederationPeer
	agentID string
	name    string
	// attachments: some matched remote group accepts files from us.
	attachments bool
	// operator addresses the peer's human operator instead of an agent.
	operator    bool
	remoteGroup []string
}

func (t *fedTarget) label() string { return t.name + "@" + peerDisplay(t.peer) }

// resolveFederatedTarget resolves member@peer for a sender. fromConv is ""
// for the human operator. Authority is checked
// by the caller.
func resolveFederatedTarget(fromConv, addr string) (*fedTarget, error) {
	member, peerRef, ok := splitFederatedAddress(addr)
	if !ok {
		return nil, newFedErr(http.StatusBadRequest, "invalid_arg", "remote address must be member@peer")
	}
	peer, err := resolveFederationPeerOpt(peerRef, false)
	if err != nil {
		return nil, err
	}
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil {
		return nil, err
	}
	if cat == nil {
		return nil, newFedErr(http.StatusNotFound, "no_catalog", "no catalog received from %s yet; is it online and does it export anything to you?", peerDisplay(peer))
	}
	// Prefer candidates covered by standing grants before resolving ambiguous
	// names. When none is covered, retain catalog resolution for ask-human.
	grantedGroups := map[string]bool{}
	if fromConv != "" {
		verdict := resolvePermissionVerdictForAction(nil, fromConv, PermMessageDirect, ActionContext{RemotePeer: peer.InstanceID})
		for _, g := range cat.Groups {
			if !g.HasCap(proto.CapMail) {
				continue
			}
			allowed, _ := permissionVerdictAllowsAction(verdict, fromConv, PermMessageDirect, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: g.Name})
			if !allowed {
				continue
			}
			for _, m := range g.Members {
				if fedMemberMatches(m, member) {
					grantedGroups[g.Name] = true
				}
			}
		}
	}
	t := &fedTarget{peer: peer}
	for _, g := range cat.Groups {
		if !g.HasCap(proto.CapMail) || (len(grantedGroups) != 0 && !grantedGroups[g.Name]) {
			continue
		}
		for _, m := range g.Members {
			if fedMemberMatches(m, member) {
				if t.agentID != "" && t.agentID != m.Agent {
					return nil, newFedErr(http.StatusConflict, "ambiguous", "%q matches several members of %s; use the agent id", member, peerDisplay(peer))
				}
				t.agentID, t.name = m.Agent, m.Name
				t.remoteGroup = append(t.remoteGroup, g.Name)
				t.attachments = t.attachments || g.HasCap(proto.CapAttachments)
			}
		}
	}
	if t.agentID == "" {
		return nil, newFedErr(http.StatusNotFound, "not_found", "%s exports no mail-capable member matching %q", peerDisplay(peer), member)
	}
	return t, nil
}

func fedMemberMatches(m proto.CatalogMember, member string) bool {
	return m.Agent == member || strings.EqualFold(m.Name, member) || (len(member) >= 8 && strings.HasPrefix(m.Agent, member))
}

// queueFederatedMail seals mail from fromConv ("" = human operator) to the
// target and writes the durable outbox row.
func queueFederatedMail(fromConv string, t *fedTarget, subject, body, inReplyTo string, atts []proto.AttachmentPayload) (*db.FederationOutboxRow, error) {
	if strings.TrimSpace(body) == "" {
		return nil, newFedErr(http.StatusBadRequest, "invalid_arg", "body is empty")
	}
	if len(body) > proto.MaxMailBody {
		return nil, newFedErr(http.StatusRequestEntityTooLarge, "too_large", "remote messages are limited to %d bytes", proto.MaxMailBody)
	}
	if len(subject) > fedMaxSubject {
		return nil, newFedErr(http.StatusBadRequest, "invalid_arg", "remote message subjects are limited to %d bytes", fedMaxSubject)
	}
	if len(atts) > 0 {
		if t.operator {
			return nil, newFedErr(http.StatusBadRequest, "invalid_arg", "operator mail does not carry attachments")
		}
		if err := validateFedAttachments(atts); err != nil {
			return nil, newFedErr(http.StatusBadRequest, "invalid_arg", "%v", err)
		}
		if !t.attachments {
			return nil, newFedErr(http.StatusForbidden, "not_exported", "%s does not accept attachments from this instance", t.label())
		}
	}
	kind := proto.KindMail
	if t.operator {
		kind = proto.KindOperatorMail
	}
	return queueFederatedEnvelope(fedOutgoing{
		fromConv: fromConv, peer: t.peer, kind: kind, toAgent: t.agentID, toLabel: t.label(),
		subject: subject, preview: body, inReplyTo: inReplyTo, ttl: fedMailTTL,
		payload: proto.MailPayload{Subject: subject, Body: body, Attachments: atts},
	})
}

// fedMaxSubject matches what receivers accept.
const fedMaxSubject = 512

// fedOutgoing describes one envelope for the outbox.
type fedOutgoing struct {
	envelopeID string
	fromConv   string // "" = the human operator
	peer       *db.FederationPeer
	kind       string
	toAgent    string
	toLabel    string
	subject    string
	preview    string
	inReplyTo  string
	ttl        time.Duration
	payload    any
}

// queueFederatedEnvelope seals o for its peer and writes the durable outbox
// row; the outbox loop sends and retries it until acknowledged.
func queueFederatedEnvelope(o fedOutgoing) (*db.FederationOutboxRow, error) {
	identitySealMu.RLock()
	defer identitySealMu.RUnlock()
	id, err := federationIdentity()
	if err != nil {
		return nil, err
	}
	from := proto.Endpoint{Name: "human operator"}
	fromAgent := ""
	if o.fromConv != "" {
		fromAgent, _ = db.AgentIDForConv(o.fromConv)
		from = proto.Endpoint{Agent: fromAgent, Name: agent.TitleFor(o.fromConv)}
	}
	env, err := proto.NewEnvelope(id, o.kind, from, proto.Endpoint{Instance: o.peer.InstanceID, Agent: o.toAgent}, o.ttl, o.payload)
	if err != nil {
		return nil, err
	}
	if o.envelopeID != "" {
		env.ID = o.envelopeID
	}
	env.InReplyTo = o.inReplyTo
	sealed, err := proto.Seal(id, env, ed25519.PublicKey(o.peer.PubKey))
	if errors.Is(err, proto.ErrTooLarge) {
		return nil, newFedErr(http.StatusRequestEntityTooLarge, "too_large", "message and attachments are too large once encoded; send fewer or smaller files")
	}
	if err != nil {
		return nil, err
	}
	row := db.FederationOutboxRow{
		EnvelopeID: env.ID, Kind: o.kind, ToInstance: o.peer.InstanceID, ToAgent: o.toAgent, ToLabel: o.toLabel,
		FromConv: o.fromConv, FromAgent: fromAgent, InReplyTo: o.inReplyTo, Subject: o.subject, BodyPreview: preview(o.preview),
		Sealed: packSealed(sealed), ExpiresAt: env.ExpiresAt,
	}
	if err := db.InsertFederationOutbox(row); err != nil {
		return nil, err
	}
	if rt := currentFederation(); rt != nil {
		rt.kickOutbox()
	}
	row.State = db.FedOutboxQueued
	return &row, nil
}

type fedSendResp struct {
	EnvelopeID string `json:"envelope_id"`
	To         string `json:"to"`
	ToAgent    string `json:"to_agent"`
	State      string `json:"state"`
	ViaGroup   string `json:"via_group,omitempty"`
	Connected  bool   `json:"hub_connected"`
	// Cc lists the copies queued for remote cc recipients.
	Cc []fedSendResp `json:"cc,omitempty"`
}

func fedConnected() bool {
	rt := currentFederation()
	return rt != nil && rt.cl.Status().State == client.StateConnected
}

// handleFederatedAgentSend is the agent path of `tclaude agent message
// member@peer`, reached from handleMessages.
func handleFederatedAgentSend(w http.ResponseWriter, r *http.Request, fromConv string, req *sendReq) {
	if req.Role != "" || len(req.Members) > 0 || req.Gen != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "role, members and gen are not supported for remote recipients")
		return
	}
	t, err := resolveFederatedTarget(fromConv, req.To)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	ccTargets, err := resolveFederatedCC(fromConv, req.Cc, map[string]bool{t.peer.InstanceID + "/" + t.agentID: true})
	if err == nil {
		err = validateFederatedCopies(append([]*fedTarget{t}, ccTargets...), req.Subject, req.Body, req.Attachments)
	}
	if err != nil {
		writeFedErr(w, err)
		return
	}
	auth, ok := authorizeFederatedTargets(w, r, fromConv, append([]*fedTarget{t}, ccTargets...))
	if !ok {
		return
	}
	via, ccs := auth[0].via, auth[1:]
	row, err := queueFederatedMail(fromConv, t, req.Subject, req.Body, "", req.Attachments)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, t.label())
	resp := fedSendResp{EnvelopeID: row.EnvelopeID, To: t.label(), ToAgent: t.agentID + "@" + t.peer.InstanceID,
		State: row.State, ViaGroup: via, Connected: fedConnected()}
	for _, c := range ccs {
		cr := fedSendResp{To: c.target.label(), ToAgent: c.target.agentID + "@" + c.target.peer.InstanceID, ViaGroup: c.via}
		if row, err := queueFederatedMail(fromConv, c.target, req.Subject, req.Body, "", req.Attachments); err != nil {
			cr.State = "failed: " + err.Error()
		} else {
			cr.EnvelopeID, cr.State = row.EnvelopeID, row.State
		}
		resp.Cc = append(resp.Cc, cr)
	}
	writeJSON(w, http.StatusOK, resp)
}

// authorizeFederatedTarget requires mail authority for a catalog group
// containing the recipient. One-shot approval describes one concrete group.
func authorizeFederatedTarget(w http.ResponseWriter, r *http.Request, fromConv string, t *fedTarget) (string, bool) {
	for _, group := range t.remoteGroup {
		if allowed, _, err := permissionAllowsAction(r, fromConv, PermMessageDirect, ActionContext{RemotePeer: t.peer.InstanceID, RemoteGroup: group}); err == nil && allowed {
			return group, true
		}
	}
	if _, ok := requirePermission(w, r, PermMessageDirect, ActionContext{RemotePeer: t.peer.InstanceID, RemoteGroup: t.remoteGroup[0]}); !ok {
		return "", false
	}
	return t.remoteGroup[0], true
}

// handleFederatedReply answers an inbound remote message. Reply authority
// comes from having received the message: no import or slug is needed, and
// the remote side accepts it because it matches mail it sent.
func handleFederatedReply(w http.ResponseWriter, r *http.Request, fromConv string, in *db.FederationInbound, subject, body string) {
	peer, err := db.GetFederationPeer(in.FromInstance)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if peer == nil {
		writeError(w, http.StatusForbidden, "not_trusted", "the sending instance is no longer trusted")
		return
	}
	if in.FromAgent == "" {
		writeError(w, http.StatusBadRequest, "not_replyable", "the remote sender was the remote human operator; there is no agent to reply to")
		return
	}
	t := &fedTarget{peer: peer, agentID: in.FromAgent, name: in.FromName}
	row, err := queueFederatedMail(fromConv, t, subject, body, in.EnvelopeID, nil)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, t.label())
	writeJSON(w, http.StatusOK, fedSendResp{EnvelopeID: row.EnvelopeID, To: t.label(), ToAgent: t.agentID + "@" + t.peer.InstanceID,
		State: row.State, Connected: fedConnected()})
}

// --- human operator API: /v1/federation/* ---

type fedStatusResp struct {
	Enabled     bool                     `json:"enabled"`
	InstanceID  string                   `json:"instance_id"`
	Fingerprint string                   `json:"fingerprint"`
	Name        string                   `json:"name"`
	HubURL      string                   `json:"hub_url,omitempty"`
	Hub         *fedHubStatus            `json:"hub,omitempty"`
	Peers       []fedPeerJSON            `json:"peers"`
	PeerGrants  []db.FederationPeerGrant `json:"peer_grants"`
	Outbox      map[string]int           `json:"outbox"`
	Remote      []fedRemoteSystem        `json:"remote"`
}

// fedStatusSummaryResp is the local Fleet chip-row projection. It avoids
// loading grants, the outbox, or remote catalogs.
type fedStatusSummaryResp struct {
	Enabled     bool          `json:"enabled"`
	InstanceID  string        `json:"instance_id"`
	Fingerprint string        `json:"fingerprint"`
	Name        string        `json:"name"`
	HubURL      string        `json:"hub_url,omitempty"`
	Hub         *fedHubStatus `json:"hub,omitempty"`
	Peers       []fedPeerJSON `json:"peers"`
}

type fedHubStatus struct {
	State     string    `json:"state"`
	HubID     string    `json:"hub_id,omitempty"`
	Spaces    []string  `json:"spaces,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

type fedPeerJSON struct {
	Level       string    `json:"level,omitempty"`
	InstanceID  string    `json:"instance_id"`
	Fingerprint string    `json:"fingerprint"`
	Label       string    `json:"label,omitempty"`
	Name        string    `json:"name,omitempty"`
	Trusted     bool      `json:"trusted"`
	Online      bool      `json:"online"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
	Version     string    `json:"version,omitempty"`
	TrustedAt   time.Time `json:"trusted_at,omitempty"`
}

// fedRemoteSystem is what one trusted peer exports to us (discovery).
type fedRemoteSystem struct {
	Peer       string               `json:"peer"`
	Label      string               `json:"label"`
	Online     bool                 `json:"online"`
	ReceivedAt time.Time            `json:"catalog_received_at,omitempty"`
	Groups     []proto.CatalogGroup `json:"groups"`
}

func handleFederationStatus(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read federation status") {
		return
	}
	id, err := federationIdentity()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	cfg, _ := config.Load()
	resp := fedStatusResp{
		InstanceID: id.ID(), Fingerprint: proto.Fingerprint(id.Pub), Name: defaultFederationName(),
		Outbox: map[string]int{},
	}
	if cfg != nil && cfg.Federation != nil {
		resp.Enabled = cfg.Federation.Enabled
		resp.HubURL = cfg.Federation.HubURL
		if cfg.Federation.Name != "" {
			resp.Name = cfg.Federation.Name
		}
	}
	rt := currentFederation()
	dir := map[string]proto.DirectoryEntry{}
	if rt != nil {
		st := rt.cl.Status()
		resp.Hub = &fedHubStatus{State: string(st.State), HubID: st.HubID, Spaces: st.Spaces, LastError: st.LastError, Since: st.Since}
		for _, e := range rt.cl.Directory() {
			dir[e.InstanceID] = e
		}
	}
	trusted, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	labels := map[string]string{}
	seen := map[string]bool{}
	for _, p := range trusted {
		labels[p.InstanceID] = peerDisplay(&p)
		e := dir[p.InstanceID]
		resp.Peers = append(resp.Peers, fedPeerJSON{
			InstanceID: p.InstanceID, Fingerprint: proto.Fingerprint(p.PubKey), Label: p.Label, Name: fedFirst(e.Name, p.Name),
			Level: p.TrustLevel, Trusted: true, Online: e.Online, LastSeen: e.LastSeen, Version: e.Version, TrustedAt: p.TrustedAt,
		})
		seen[p.InstanceID] = true
	}
	summary := r.URL.Query().Get("summary") == "1"
	for _, e := range dir {
		if summary || seen[e.InstanceID] {
			continue
		}
		resp.Peers = append(resp.Peers, fedPeerJSON{
			InstanceID: e.InstanceID, Fingerprint: proto.Fingerprint(e.PubKey), Name: e.Name,
			Online: e.Online, LastSeen: e.LastSeen, Version: e.Version,
		})
	}
	sort.Slice(resp.Peers, func(i, j int) bool {
		if resp.Peers[i].Trusted != resp.Peers[j].Trusted {
			return resp.Peers[i].Trusted
		}
		return resp.Peers[i].InstanceID < resp.Peers[j].InstanceID
	})
	if resp.Peers == nil {
		resp.Peers = []fedPeerJSON{}
	}
	if summary {
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, fedStatusSummaryResp{
			Enabled: resp.Enabled, InstanceID: resp.InstanceID, Fingerprint: resp.Fingerprint,
			Name: resp.Name, HubURL: resp.HubURL, Hub: resp.Hub, Peers: resp.Peers,
		})
		return
	}
	grants, _ := db.ListFederationPeerGrants("")
	for _, grant := range grants {
		resp.PeerGrants = append(resp.PeerGrants, fedDisplayPeerGrant(grant))
	}
	if rows, err := db.ListFederationOutbox(500); err == nil {
		for _, row := range rows {
			resp.Outbox[row.State]++
		}
	}
	for _, p := range trusted {
		cat, at, err := fedCatalogFor(p.InstanceID)
		rs := fedRemoteSystem{Peer: p.InstanceID, Label: peerDisplay(&p), Online: dir[p.InstanceID].Online, ReceivedAt: at, Groups: []proto.CatalogGroup{}}
		if err == nil && cat != nil {
			rs.Groups = cat.Groups
		}
		resp.Remote = append(resp.Remote, rs)
	}
	if resp.Peers == nil {
		resp.Peers = []fedPeerJSON{}
	}
	writeJSON(w, http.StatusOK, resp)
}

type fedConfigReq struct {
	Enabled   *bool   `json:"enabled,omitempty"`
	HubURL    *string `json:"hub_url,omitempty"`
	Name      *string `json:"name,omitempty"`
	Invite    *string `json:"invite,omitempty"`
	HubCAFile *string `json:"hub_ca_file,omitempty"`
}

// handleFederationConfig updates the federation config block and restarts
// the hub client.
func handleFederationConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !requireHuman(w, r, "configure federation") {
		return
	}
	var req fedConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if req.HubURL != nil && *req.HubURL != "" {
		if _, err := client.ValidateURL(*req.HubURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		f := cfg.Federation
		if req.Enabled != nil {
			f.Enabled = *req.Enabled
		}
		if req.HubURL != nil {
			f.HubURL = strings.TrimSpace(*req.HubURL)
		}
		if req.Name != nil {
			f.Name = strings.TrimSpace(*req.Name)
		}
		if req.Invite != nil {
			f.Invite = strings.TrimSpace(*req.Invite)
		}
		if req.HubCAFile != nil {
			f.HubCAFile = strings.TrimSpace(*req.HubCAFile)
		}
		if f.Enabled && f.HubURL == "" {
			return newFedErr(http.StatusBadRequest, "invalid_arg", "set hub_url before enabling federation")
		}
		return nil
	})
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if err := reloadFederation(); err != nil {
		writeError(w, http.StatusBadRequest, "federation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type fedTrustReq struct {
	Profile            string `json:"profile,omitempty"`
	NoDefaultProfile   bool   `json:"no_default_profile,omitempty"`
	Preview            bool   `json:"preview,omitempty"`
	PreviewToken       string `json:"preview_token,omitempty"`
	Level              string `json:"level,omitempty"`
	ConfirmFingerprint string `json:"confirm_fingerprint,omitempty"`
	Instance           string `json:"instance"`
	Label              string `json:"label,omitempty"`
}

// handleFederationTrust trusts a peer the hub directory lists, pinning the
// key the directory presented. The instance id is derived from that key,
// so a hub cannot swap keys under an id the operator checked.
func handleFederationTrust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !requireHuman(w, r, "trust a federation peer") {
		return
	}
	var req fedTrustReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if req.Label != "" && !validFedLabel(req.Label) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "label must be 1-32 chars of [a-z0-9-_.]")
		return
	}
	if req.Level != "" && req.Level != db.FederationTrustRestricted && req.Level != db.FederationTrustUnrestricted {
		writeError(w, http.StatusBadRequest, "invalid_arg", "level must be restricted or unrestricted")
		return
	}
	rt := currentFederation()
	var entry *proto.DirectoryEntry
	var matches []proto.DirectoryEntry
	var directory []proto.DirectoryEntry
	if rt != nil {
		directory = rt.cl.Directory()
	}
	// A pinned peer can have its local level changed while disconnected.
	existing, _ := resolveFederationPeer(req.Instance)
	if existing != nil {
		directory = []proto.DirectoryEntry{{InstanceID: existing.InstanceID, PubKey: existing.PubKey, Name: existing.Name}}
		req.Instance = existing.InstanceID
		if req.Label == "" {
			req.Label = existing.Label
		}
	}
	for _, e := range directory {
		if e.InstanceID == req.Instance {
			matches = []proto.DirectoryEntry{e}
			break
		}
		if len(req.Instance) >= 8 && (strings.HasPrefix(e.InstanceID, req.Instance) || strings.HasPrefix(e.InstanceID, proto.InstanceIDPrefix+req.Instance)) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		writeError(w, http.StatusNotFound, "not_found", "no visible instance matches "+req.Instance+" (see tclaude federation peers)")
		return
	case 1:
		entry = &matches[0]
	default:
		writeError(w, http.StatusConflict, "ambiguous", "instance prefix matches several instances")
		return
	}
	if proto.InstanceID(entry.PubKey) != entry.InstanceID {
		writeError(w, http.StatusBadGateway, "bad_directory", "hub directory key does not match the instance id; refusing to trust")
		return
	}
	if req.Profile != "" && req.NoDefaultProfile {
		writeError(w, 400, "profile", "--profile and --no-default-profile are mutually exclusive")
		return
	}
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	finish := func() {}
	if !req.Preview {
		finish = lockAwayMutation(entry.InstanceID, true)
	}
	defer finish()
	existing, err := db.GetFederationPeer(entry.InstanceID)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	var profile *db.FederationNodeProfile
	if req.Profile != "" {
		profile, err = db.GetFederationNodeProfile(req.Profile)
	} else if existing == nil && !req.NoDefaultProfile {
		profile, err = db.DefaultFederationNodeProfile()
	}
	if err != nil {
		writeError(w, 400, "profile", err.Error())
		return
	}
	if profile != nil {
		if req.Level != "" && req.Level != profile.Definition.TrustLevel {
			writeError(w, 400, "profile", "explicit trust level conflicts with selected profile")
			return
		}
		req.Level = profile.Definition.TrustLevel
	}
	if req.Level == "" {
		req.Level = db.FederationTrustRestricted
	}
	var plan *db.FederationNodeProfilePlan
	newPeer := &db.FederationPeer{InstanceID: entry.InstanceID, PubKey: entry.PubKey, Label: req.Label, Name: proto.SafeName(entry.Name, true)}
	if profile != nil {
		plan, err = db.PlanFederationNodeProfile(profile.ID, entry.InstanceID, "", newPeer)
		if err != nil {
			writeError(w, 409, "profile", err.Error())
			return
		}
	}
	if req.Preview {
		writeJSON(w, 200, map[string]any{"instance_id": entry.InstanceID, "fingerprint": proto.Fingerprint(entry.PubKey), "level": req.Level, "profile": profile, "plan": plan, "applied": false})
		return
	}
	if profile == nil && req.PreviewToken != "" {
		writeError(w, 409, "stale_preview", "selected default profile changed; preview trust again")
		return
	}
	if profile != nil && req.PreviewToken == "" {
		writeError(w, 400, "preview_required", "preview the selected peer profile before trusting")
		return
	}
	if req.Level == db.FederationTrustUnrestricted && (existing == nil || existing.TrustLevel != db.FederationTrustUnrestricted) && req.ConfirmFingerprint != proto.Fingerprint(entry.PubKey) {
		writeError(w, http.StatusBadRequest, "confirmation_required", "confirm fingerprint "+proto.Fingerprint(entry.PubKey)+": unrestricted grants all peer permissions on all live groups, automatic spawn, and local unscoped grants towards this peer; interactive terminal attach includes harness approval answers")
		return
	}
	if profile != nil {
		plan, err = db.PlanFederationNodeProfile(profile.ID, entry.InstanceID, req.PreviewToken, newPeer)
	} else {
		err = db.TrustFederationPeer(db.FederationPeer{TrustLevel: req.Level, InstanceID: entry.InstanceID, PubKey: entry.PubKey, Label: req.Label, Name: proto.SafeName(entry.Name, true)})
	}
	if err != nil {
		writeError(w, 409, "conflict", err.Error())
		return
	}
	setAuditTargetLabel(r, entry.InstanceID)
	if rt != nil {
		go func() {
			rt.sendCatalog(entry.InstanceID)
			rt.sendControl(entry.InstanceID, proto.KindCatalogReq, "", struct{}{})
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": entry.InstanceID, "fingerprint": proto.Fingerprint(entry.PubKey), "level": req.Level, "profile": profile, "plan": plan})
}

func validFedLabel(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func handleFederationUntrust(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !requireHuman(w, r, "untrust a federation peer") {
		return
	}
	var req fedTrustReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	p, err := resolveFederationPeer(req.Instance)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	finish := lockAwayAuthorityMutation(p.InstanceID)
	defer finish()
	if _, err := db.UntrustFederationPeer(p.InstanceID); err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, p.InstanceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": p.InstanceID})
}

type fedOutboxJSON struct {
	EnvelopeID string    `json:"envelope_id"`
	To         string    `json:"to"`
	From       string    `json:"from"`
	Subject    string    `json:"subject,omitempty"`
	Preview    string    `json:"preview"`
	State      string    `json:"state"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func handleFederationOutbox(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read the federation outbox") {
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	rows, err := db.ListFederationOutbox(limit)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	out := []fedOutboxJSON{}
	for _, row := range rows {
		if row.Kind == proto.KindCatalog || row.Kind == proto.KindCatalogReq || row.Kind == proto.KindAck {
			continue
		}
		from := "human operator"
		if row.FromConv != "" {
			from = agent.TitleFor(row.FromConv)
		}
		out = append(out, fedOutboxJSON{
			EnvelopeID: row.EnvelopeID, To: row.ToLabel, From: from, Subject: row.Subject, Preview: row.BodyPreview,
			State: row.State, Attempts: row.Attempts, LastError: row.LastError, InReplyTo: row.InReplyTo,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type fedHumanSendReq struct {
	To          string                    `json:"to"`
	Role        string                    `json:"role,omitempty"`
	Subject     string                    `json:"subject,omitempty"`
	Body        string                    `json:"body"`
	Attachments []proto.AttachmentPayload `json:"attachments,omitempty"`
}

// handleFederationSend is the human operator's remote send.
func handleFederationSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !requireHuman(w, r, "send remote mail as the operator") {
		return
	}
	var req fedHumanSendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if group, peer, ok := splitFederatedGroup(req.To); ok {
		handleFederatedGroupSend(w, r, "", &sendReq{To: req.To, Role: req.Role, Subject: req.Subject, Body: req.Body, Attachments: req.Attachments}, group, peer)
		return
	}
	if req.Role != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "role is only valid with a group:<group>@<peer> target")
		return
	}
	t, err := resolveFederatedTarget("", req.To)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	row, err := queueFederatedMail("", t, req.Subject, req.Body, "", req.Attachments)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, t.label())
	writeJSON(w, http.StatusOK, fedSendResp{EnvelopeID: row.EnvelopeID, To: t.label(), ToAgent: t.agentID + "@" + t.peer.InstanceID,
		State: row.State, Connected: fedConnected()})
}

type fedNotifyReq struct {
	Peer    string `json:"peer"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body"`
}

// handleFederationNotify sends mail from the local operator to a trusted
// peer's operator. No export or import is involved: trusting each other is
// what lets two operators talk.
func handleFederationNotify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if !requireHuman(w, r, "message a remote operator") {
		return
	}
	var req fedNotifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	peer, err := resolveFederationPeer(req.Peer)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	t := &fedTarget{peer: peer, name: "operator", operator: true}
	row, err := queueFederatedMail("", t, req.Subject, req.Body, "", nil)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, t.label())
	writeJSON(w, http.StatusOK, fedSendResp{EnvelopeID: row.EnvelopeID, To: t.label(), ToAgent: "@" + peer.InstanceID,
		State: row.State, Connected: fedConnected()})
}

type fedInboxJSON struct {
	ID         int64     `json:"id"`
	OfferID    string    `json:"offer_id,omitempty"`
	OfferState string    `json:"offer_state,omitempty"`
	From       string    `json:"from"`
	Instance   string    `json:"instance"`
	Subject    string    `json:"subject,omitempty"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
	Read       bool      `json:"read"`
}

// handleFederationInbox lists messages remote operators sent the local
// operator. They also appear in the dashboard Messages tab.
func handleFederationInbox(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read remote operator mail") {
		return
	}
	msgs, err := db.ListHumanMessages()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	prefix := db.FederationHumanGroup("")
	out := []fedInboxJSON{}
	for _, m := range msgs {
		// Remote operator rows have no local sender; a local group that
		// happens to be named federation:… cannot pose as one.
		if m.FromConv != "" || !strings.HasPrefix(m.GroupName, prefix) {
			continue
		}
		out = append(out, fedInboxJSON{ID: m.ID, From: m.FromTitle, Instance: strings.TrimPrefix(m.GroupName, prefix),
			Subject: m.Subject, Body: m.Body, CreatedAt: m.CreatedAt, Read: m.IsRead()})
	}
	reconcileFederationBundleOffers()
	offers, err := db.ListFederationBundleOffers("in")
	if err != nil {
		writeFedErr(w, err)
		return
	}
	for _, o := range offers {
		label := o.Peer
		if p, _ := db.GetFederationPeer(o.Peer); p != nil {
			label = peerDisplay(p)
		}
		out = append(out, fedInboxJSON{OfferID: o.Descriptor.ID, OfferState: o.State, From: label, Instance: o.Peer, Subject: o.Descriptor.Type + " bundle offer", Body: proto.StripControls(o.Descriptor.Summary) + "\nPreview: tclaude federation offers import " + o.Descriptor.ID + " --peer " + o.Peer, CreatedAt: o.CreatedAt, Read: o.State != "pending" && o.State != "ready"})
	}
	writeJSON(w, http.StatusOK, out)
}

// fedStaleAfter is how old a catalog may get before its presence data is
// reported as stale. Catalogs refresh every fedCatalogRefresh while the peer
// is online.
const fedStaleAfter = 3 * fedCatalogRefresh

// fedRemoteMember is one remote member reachable through an import.
type fedRemoteMember struct {
	State            *proto.AgentStatus `json:"state,omitempty"`
	StatusObservedAt time.Time          `json:"status_observed_at,omitempty"`
	StatusReceivedAt time.Time          `json:"status_received_at,omitempty"`
	StatusStale      bool               `json:"status_stale,omitempty"`
	IdleSeconds      *int64             `json:"idle_seconds,omitempty"`
	// Address is what `tclaude agent message` accepts: name@label, or
	// name@instance-id for a peer without a label.
	Address     string    `json:"address"`
	Agent       string    `json:"agent"`
	Name        string    `json:"name"`
	Role        string    `json:"role,omitempty"`
	Harness     string    `json:"harness,omitempty"`
	Presence    string    `json:"presence,omitempty"`
	Peer        string    `json:"peer"`
	Instance    string    `json:"instance"`
	RemoteGroup string    `json:"remote_group"`
	Mail        bool      `json:"mail"`
	PeerOnline  bool      `json:"peer_online"`
	CatalogAt   time.Time `json:"catalog_received_at"`
	// Stale marks presence that may be out of date: the peer is offline or
	// its catalog has not been refreshed recently.
	Stale bool `json:"stale"`
}

// handleFederationReachable intersects the peer catalog with the caller's
// peer-scoped mail, spawn and route grants. The operator sees every catalog.
func handleFederationReachable(w http.ResponseWriter, r *http.Request) {
	myID, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	filter := strings.TrimSpace(r.URL.Query().Get("group"))
	if filter != "" && !isHuman {
		groups, err := db.ListGroupsForConv(myID)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		member := false
		for _, g := range groups {
			if !g.IsArchived() && (g.Name == filter || strconv.FormatInt(g.ID, 10) == filter) {
				member = true
			}
		}
		if !member {
			writeJSON(w, http.StatusOK, []*fedRemoteMember{})
			return
		}
	}
	rt, now := currentFederation(), time.Now()
	out := []*fedRemoteMember{}
	for _, peer := range peers {
		cat, at, err := fedCatalogFor(peer.InstanceID)
		if err != nil || cat == nil {
			continue
		}
		online := rt != nil && rt.isOnline(peer.InstanceID)
		addrPeer := fedFirst(peer.Label, peer.InstanceID)
		for _, g := range cat.Groups {
			actx := ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: g.Name}
			statusAllowed := isHuman
			if !isHuman {
				statusAllowed, _, _ = permissionAllowsAction(r, myID, PermAgentsStatusRead, actx)
			}
			statusAllowed = statusAllowed && g.HasCap(proto.CapAgentStatus)
			mail := isHuman && g.HasCap(proto.CapMail)
			visible := isHuman
			if !isHuman {
				for _, slug := range []string{PermMessageDirect, PermGroupsMembersSpawn, PermAgentSpawn, PermRoutesConsume} {
					allowed, _, err := permissionAllowsAction(r, myID, slug, actx)
					if err != nil || !allowed {
						continue
					}
					// Spawn requests can target any visible catalog group; lack of a spawn
					// capability means approval is required on the receiving instance.
					if slug == PermMessageDirect && !g.HasCap(proto.CapMail) {
						continue
					}
					if slug == PermRoutesConsume && !g.HasCap(proto.CapRoutes) {
						continue
					}
					visible = true
					if slug == PermMessageDirect {
						mail = true
					}
				}
			}
			if !visible && !statusAllowed {
				continue
			}
			indices := map[string]*fedRemoteMember{}
			if visible {
				for _, m := range g.Members {
					row := &fedRemoteMember{
						Address: m.Name + "@" + addrPeer, Agent: m.Agent, Name: m.Name, Role: m.Role, Harness: m.Harness,
						Presence: m.Presence, Peer: peerDisplay(&peer), Instance: peer.InstanceID, RemoteGroup: g.Name,
						Mail: mail, PeerOnline: online, CatalogAt: at, Stale: !online || now.Sub(at) > fedStaleAfter,
					}
					out = append(out, row)
					indices[m.Agent] = row
				}
			}
			if statusAllowed {
				for _, s := range g.AgentStatuses {
					row := indices[s.Agent]
					if row == nil {
						row = &fedRemoteMember{Address: s.Agent + "@" + addrPeer, Agent: s.Agent, Name: s.Name, Role: s.Role, Harness: s.Harness, Peer: peerDisplay(&peer), Instance: peer.InstanceID, RemoteGroup: g.Name, PeerOnline: online, CatalogAt: at, Stale: !online || now.Sub(at) > fedStaleAfter}
						out = append(out, row)
					}
					status := s
					row.State = &status
					row.StatusObservedAt = g.AgentStatusesAt
					row.StatusReceivedAt = g.AgentStatusesReceivedAt
					row.StatusStale = remoteAgentStatusStale(rt, peer.InstanceID, g)
					if s.LastActivity != nil && s.Online && s.Status != "working" && s.Status != "running" && s.Status != "main_agent_idle" {
						end := now
						if row.StatusStale {
							end = g.AgentStatusesAt
						}
						idle := max(int64(0), int64(end.Sub(*s.LastActivity)/time.Second))
						row.IdleSeconds = &idle
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Peer != out[j].Peer {
			return out[i].Peer < out[j].Peer
		}
		return out[i].Address < out[j].Address
	})
	writeJSON(w, http.StatusOK, out)
}

// remoteSenderLabel names the sender of an inbound remote message for
// nudges and inbox views, or "" for local mail.
func remoteSenderLabel(messageID int64) (label, addr string) {
	in, err := db.FederationInboundForMessage(messageID)
	if err != nil || in == nil {
		return "", ""
	}
	peer := in.FromInstance
	if p, _ := db.GetFederationPeer(in.FromInstance); p != nil {
		peer = peerDisplay(p)
	}
	// Stored values were gated on receipt; gate again on render (these
	// labels go into pane nudges).
	name := proto.SafeName(fedFirst(in.FromName, in.FromAgent), false)
	peer = proto.SafeName(peer, true)
	addr = in.FromInstance
	if in.FromAgent != "" {
		addr = in.FromAgent + "@" + in.FromInstance
	}
	return name + "@" + peer + " (remote)", addr
}

func fedFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func registerFederationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/node/run", handleLocalNodeRun)
	mux.HandleFunc("POST /v1/node/run", handleLocalNodeRun)
	mux.HandleFunc("GET /v1/node/run/jobs/{id}", handleLocalNodeRun)
	mux.HandleFunc("GET /v1/node/run/jobs/{id}/logs", handleLocalNodeRun)
	mux.HandleFunc("GET /v1/node/run/settings", handleLocalNodeRunSettings)
	mux.HandleFunc("PUT /v1/node/run/settings", handleLocalNodeRunSettings)
	mux.HandleFunc("GET /v1/node/update", handleLocalNodeUpdate)
	mux.HandleFunc("POST /v1/node/update", handleLocalNodeUpdate)
	mux.HandleFunc("GET /v1/node/update/jobs/{id}", handleLocalNodeUpdate)
	mux.HandleFunc("GET /v1/harnesses/availability", handleLocalHarnessAvailability)
	mux.HandleFunc("GET /v1/harnesses/operations", handleLocalHarnessOperations)
	mux.HandleFunc("POST /v1/harnesses/operations", handleLocalHarnessOperations)
	mux.HandleFunc("POST /v1/harnesses/credentials", handleLocalHarnessOperations)
	mux.HandleFunc("GET /v1/harnesses/credentials/backups", handleLocalHarnessCredentials)
	mux.HandleFunc("POST /v1/harnesses/credentials/push", handleLocalHarnessCredentials)
	mux.HandleFunc("POST /v1/harnesses/credentials/backup", handleLocalHarnessCredentials)
	mux.HandleFunc("POST /v1/harnesses/credentials/restore", handleLocalHarnessCredentials)
	mux.HandleFunc("GET /v1/harnesses/operations/jobs/{id}", handleLocalHarnessOperations)

	registerFederationPeerViewRoutes(mux)
	registerFederationNodeGroupRoutes(mux)
	registerFederationNodeProfileRoutes(mux)
	registerFederationRepoRoutes(mux)
	registerFederationJobRoutes(mux)
	registerFederationEnrollmentRoutes(mux)
	registerFederationBundleRoutes(mux)
	mux.HandleFunc("GET /v1/federation/away", handleFederationAway)
	mux.HandleFunc("POST /v1/federation/away", handleFederationAway)
	mux.HandleFunc("POST /v1/federation/return", handleFederationReturn)
	mux.HandleFunc("POST /v1/federation/answer", handleFederationAwayAnswer)
	mux.HandleFunc("GET /v1/federation/status", handleFederationStatus)
	mux.HandleFunc("GET /v1/federation/audit", handleFederationAudit)
	mux.HandleFunc("/v1/federation/notify", handleFederationNotify)
	mux.HandleFunc("GET /v1/federation/inbox", handleFederationInbox)
	mux.HandleFunc("GET /v1/federation/reachable", handleFederationReachable)
	mux.HandleFunc("GET /v1/federation/sessions", handleFederationSessions)
	mux.HandleFunc("GET /v1/federation/nodes", handleFederationNodes)
	mux.HandleFunc("GET /v1/federation/nodes/watch", handleFleetWatch)
	mux.HandleFunc("GET /v1/federation/nodes/health", handleFleetHealthConfig)
	mux.HandleFunc("POST /v1/federation/nodes/health", handleFleetHealthConfig)
	mux.HandleFunc("GET /v1/federation/node-labels", handleFederationNodeLabels)
	mux.HandleFunc("POST /v1/federation/node-labels", handleFederationNodeLabels)
	mux.HandleFunc("GET /v1/federation/attach", handleFederationAttach)
	mux.HandleFunc("GET /v1/federation/viewers", handleFederationViewers)
	mux.HandleFunc("POST /v1/federation/viewers/{id}/kick", handleFederationKick)
	mux.HandleFunc("POST /v1/federation/spawn-requests", handleFederationSpawnRequestSend)
	mux.HandleFunc("GET /v1/federation/spawn-requests", handleFederationSpawnRequestList)
	mux.HandleFunc("POST /v1/federation/spawn-requests/{id}/approve", handleFederationSpawnRequestApprove)
	mux.HandleFunc("POST /v1/federation/spawn-requests/{id}/deny", handleFederationSpawnRequestDeny)
	mux.HandleFunc("POST /v1/federation/spawn-requests/{id}/abandon", handleFederationSpawnRequestAbandon)
	mux.HandleFunc("/v1/federation/config", handleFederationConfig)
	mux.HandleFunc("/v1/federation/peers/trust", handleFederationTrust)
	mux.HandleFunc("/v1/federation/peers/untrust", handleFederationUntrust)
	mux.HandleFunc("/v1/federation/identity/rotate", handleFederationIdentityRotate)
	mux.HandleFunc("/v1/federation/identity/rotations", handleFederationIdentityRotations)
	mux.HandleFunc("/v1/federation/identity/recover", handleFederationIdentityRecover)
	mux.HandleFunc("/v1/federation/identity/recover-local", handleFederationIdentityRecoverLocal)
	mux.HandleFunc("/v1/federation/identity/revoke", handleFederationIdentityRevoke)
	mux.HandleFunc("/v1/federation/grants", handleFederationPeerGrants)
	mux.HandleFunc("GET /v1/federation/outbox", handleFederationOutbox)
	mux.HandleFunc("/v1/federation/send", handleFederationSend)
	mux.HandleFunc("POST /v1/federation/routes/open", handleFederatedRouteOpen)
}
