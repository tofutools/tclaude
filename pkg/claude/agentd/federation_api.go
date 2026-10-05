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

// PermFederationMessage lets an agent mail members of a remote group that
// is imported into one of its groups. Scope dim group = the importing local
// group. Not default-granted, not ownership-contributed.
const PermFederationMessage = "federation.message"

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
	// localGroups are the importing local groups the sender may use, in
	// name order. Empty for the human sender without membership.
	localGroups []string
}

func (t *fedTarget) label() string { return t.name + "@" + peerDisplay(t.peer) }

// resolveFederatedTarget resolves member@peer for a sender. fromConv is ""
// for the human operator. Authority (beyond import reachability) is checked
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
	t := &fedTarget{peer: peer}
	for _, g := range cat.Groups {
		if !g.HasCap(proto.CapMail) {
			continue
		}
		for _, m := range g.Members {
			if m.Agent == member || strings.EqualFold(m.Name, member) || (len(member) >= 8 && strings.HasPrefix(m.Agent, member)) {
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
	imports, err := db.ListFederationImports()
	if err != nil {
		return nil, err
	}
	var senderGroups map[int64]bool
	if fromConv != "" {
		senderGroups = map[int64]bool{}
		groups, err := db.ListGroupsForConv(fromConv)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if !g.IsArchived() {
				senderGroups[g.ID] = true
			}
		}
	}
	seen := map[string]bool{}
	imported := false
	for _, im := range imports {
		if im.Peer != peer.InstanceID || !containsString(t.remoteGroup, im.RemoteGroup) {
			continue
		}
		if g, _ := db.GetAgentGroupByID(im.LocalGroupID); g == nil || g.IsArchived() {
			continue
		}
		imported = true
		if senderGroups != nil && !senderGroups[im.LocalGroupID] {
			continue
		}
		if !seen[im.LocalGroupName] {
			seen[im.LocalGroupName] = true
			t.localGroups = append(t.localGroups, im.LocalGroupName)
		}
	}
	sort.Strings(t.localGroups)
	if !imported {
		return nil, newFedErr(http.StatusForbidden, "not_imported",
			"%s's groups %v are not imported into any local group (tclaude federation import %s/<group> --into <local-group>)",
			t.label(), t.remoteGroup, peerDisplay(peer))
	}
	if fromConv != "" && len(t.localGroups) == 0 {
		return nil, newFedErr(http.StatusForbidden, "not_member",
			"you are not a member of any local group that imports %s's group", t.label())
	}
	return t, nil
}

func containsString(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
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
	fromConv  string // "" = the human operator
	peer      *db.FederationPeer
	kind      string
	toAgent   string
	toLabel   string
	subject   string
	preview   string
	inReplyTo string
	ttl       time.Duration
	payload   any
}

// queueFederatedEnvelope seals o for its peer and writes the durable outbox
// row; the outbox loop sends and retries it until acknowledged.
func queueFederatedEnvelope(o fedOutgoing) (*db.FederationOutboxRow, error) {
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

// authorizeFederatedTarget checks that fromConv holds federation.message
// for at least one importing group it belongs to and returns that group.
// It tries each silently and falls back to the full gate (which handles
// --ask-human and writes the 403) on the first.
func authorizeFederatedTarget(w http.ResponseWriter, r *http.Request, fromConv string, t *fedTarget) (string, bool) {
	for _, g := range t.localGroups {
		if ok, _, err := permissionAllowsAction(r, fromConv, PermFederationMessage, ActionContext{Group: g, Peer: t.peer.InstanceID}); err == nil && ok {
			return g, true
		}
	}
	if _, ok := requirePermission(w, r, PermFederationMessage, ActionContext{Group: t.localGroups[0], Peer: t.peer.InstanceID}); !ok {
		return "", false
	}
	return t.localGroups[0], true
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
	Enabled     bool              `json:"enabled"`
	InstanceID  string            `json:"instance_id"`
	Fingerprint string            `json:"fingerprint"`
	Name        string            `json:"name"`
	HubURL      string            `json:"hub_url,omitempty"`
	Hub         *fedHubStatus     `json:"hub,omitempty"`
	Peers       []fedPeerJSON     `json:"peers"`
	Exports     []fedExportJSON   `json:"exports"`
	Imports     []fedImportJSON   `json:"imports"`
	Outbox      map[string]int    `json:"outbox"`
	Remote      []fedRemoteSystem `json:"remote"`
}

type fedHubStatus struct {
	State     string    `json:"state"`
	HubID     string    `json:"hub_id,omitempty"`
	Spaces    []string  `json:"spaces,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

type fedPeerJSON struct {
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

type fedExportJSON struct {
	Group string   `json:"group"`
	Peer  string   `json:"peer"`
	Label string   `json:"peer_label,omitempty"`
	Caps  []string `json:"caps"`
}

type fedImportJSON struct {
	LocalGroup  string `json:"local_group"`
	Peer        string `json:"peer"`
	Label       string `json:"peer_label,omitempty"`
	RemoteGroup string `json:"remote_group"`
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
			Trusted: true, Online: e.Online, LastSeen: e.LastSeen, Version: e.Version, TrustedAt: p.TrustedAt,
		})
		seen[p.InstanceID] = true
	}
	for _, e := range dir {
		if seen[e.InstanceID] {
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
	exports, _ := db.ListFederationExports()
	for _, e := range exports {
		resp.Exports = append(resp.Exports, fedExportJSON{Group: e.GroupName, Peer: e.Peer, Label: labels[e.Peer], Caps: e.Caps})
	}
	imports, _ := db.ListFederationImports()
	for _, im := range imports {
		resp.Imports = append(resp.Imports, fedImportJSON{LocalGroup: im.LocalGroupName, Peer: im.Peer, Label: labels[im.Peer], RemoteGroup: im.RemoteGroup})
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
	Instance string `json:"instance"`
	Label    string `json:"label,omitempty"`
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
	rt := currentFederation()
	if rt == nil {
		writeError(w, http.StatusConflict, "not_connected", "federation is not running; configure and enable it first")
		return
	}
	var entry *proto.DirectoryEntry
	var matches []proto.DirectoryEntry
	for _, e := range rt.cl.Directory() {
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
	if err := db.TrustFederationPeer(db.FederationPeer{InstanceID: entry.InstanceID, PubKey: entry.PubKey, Label: req.Label, Name: proto.SafeName(entry.Name, true)}); err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	setAuditTargetLabel(r, entry.InstanceID)
	go func() {
		rt.sendCatalog(entry.InstanceID)
		rt.sendControl(entry.InstanceID, proto.KindCatalogReq, "", struct{}{})
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": entry.InstanceID, "fingerprint": proto.Fingerprint(entry.PubKey)})
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
	if _, err := db.UntrustFederationPeer(p.InstanceID); err != nil {
		writeFedErr(w, err)
		return
	}
	setAuditTargetLabel(r, p.InstanceID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "instance_id": p.InstanceID})
}

type fedExportReq struct {
	Group string   `json:"group"`
	Peer  string   `json:"peer"`
	Caps  []string `json:"caps,omitempty"`
}

func normalizeCaps(in []string) ([]string, error) {
	set := map[string]bool{}
	for _, c := range in {
		for _, part := range strings.Split(c, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !containsString(proto.AllCaps, part) {
				return nil, fmt.Errorf("unknown capability %q (known: %s)", part, strings.Join(proto.AllCaps, ", "))
			}
			set[part] = true
		}
	}
	var out []string
	for _, c := range proto.AllCaps {
		if set[c] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one capability is required (%s)", strings.Join(proto.AllCaps, ", "))
	}
	return out, nil
}

// handleFederationExports: POST upserts, DELETE removes (group, peer).
func handleFederationExports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST or DELETE")
		return
	}
	if !requireHuman(w, r, "change federation exports") {
		return
	}
	var req fedExportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(req.Group)
	if err != nil || g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group "+req.Group)
		return
	}
	peer := strings.TrimSpace(req.Peer)
	if peer != db.FederationExportAllPeers {
		p, err := resolveFederationPeer(peer)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		peer = p.InstanceID
	}
	setAuditTargetLabel(r, req.Group+" → "+peer)
	if r.Method == http.MethodDelete {
		ok, err := db.DeleteFederationExport(g.ID, peer)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such export")
			return
		}
	} else {
		caps, err := normalizeCaps(req.Caps)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if err := db.UpsertFederationExport(g.ID, peer, caps); err != nil {
			writeFedErr(w, err)
			return
		}
		setAuditDetail(r, strings.Join(caps, ","))
	}
	broadcastFederationCatalogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type fedImportReq struct {
	LocalGroup  string `json:"local_group"`
	Peer        string `json:"peer"`
	RemoteGroup string `json:"remote_group"`
}

// handleFederationImports: POST adds, DELETE removes an import.
func handleFederationImports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST or DELETE")
		return
	}
	if !requireHuman(w, r, "change federation imports") {
		return
	}
	var req fedImportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(req.LocalGroup)
	if err != nil || g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such local group "+req.LocalGroup)
		return
	}
	p, err := resolveFederationPeer(req.Peer)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if strings.TrimSpace(req.RemoteGroup) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "remote_group is required")
		return
	}
	setAuditTargetLabel(r, peerDisplay(p)+"/"+req.RemoteGroup+" → "+req.LocalGroup)
	if r.Method == http.MethodDelete {
		ok, err := db.DeleteFederationImport(g.ID, p.InstanceID, req.RemoteGroup)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such import")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	warning := ""
	if cat, _, _ := fedCatalogFor(p.InstanceID); cat == nil {
		warning = "no catalog from this peer yet; the import takes effect once it exports the group to you"
	} else {
		found := false
		for _, cg := range cat.Groups {
			if cg.Name == req.RemoteGroup {
				found = true
			}
		}
		if !found {
			warning = "the peer does not currently export " + req.RemoteGroup + " to you"
		}
	}
	if err := db.AddFederationImport(g.ID, p.InstanceID, req.RemoteGroup); err != nil {
		writeFedErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
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
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	Instance  string    `json:"instance"`
	Subject   string    `json:"subject,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Read      bool      `json:"read"`
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
	writeJSON(w, http.StatusOK, out)
}

// fedStaleAfter is how old a catalog may get before its presence data is
// reported as stale. Catalogs refresh every fedCatalogRefresh while the peer
// is online.
const fedStaleAfter = 3 * fedCatalogRefresh

// fedRemoteMember is one remote member reachable through an import.
type fedRemoteMember struct {
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
	LocalGroups []string  `json:"local_groups"`
	Mail        bool      `json:"mail"`
	PeerOnline  bool      `json:"peer_online"`
	CatalogAt   time.Time `json:"catalog_received_at"`
	// Stale marks presence that may be out of date: the peer is offline or
	// its catalog has not been refreshed recently.
	Stale bool `json:"stale"`
}

// handleFederationReachable lists the remote members imported into the
// caller's groups (every import for the operator): the remote half of
// `tclaude agent ls --remote`. It mirrors /v1/peers' visibility: an agent
// sees only what its own groups import.
func handleFederationReachable(w http.ResponseWriter, r *http.Request) {
	myID, isHuman, ok := authedCaller(w, r)
	if !ok {
		return
	}
	visible := map[int64]bool{}
	var groups []*db.AgentGroup
	var err error
	if isHuman {
		groups, err = db.ListAgentGroups()
	} else {
		groups, err = db.ListGroupsForConv(myID)
	}
	if err != nil {
		writeFedErr(w, err)
		return
	}
	filter := strings.TrimSpace(r.URL.Query().Get("group"))
	for _, g := range groups {
		if g.IsArchived() || (filter != "" && g.Name != filter && strconv.FormatInt(g.ID, 10) != filter) {
			continue
		}
		visible[g.ID] = true
	}
	imports, err := db.ListFederationImports()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	rt := currentFederation()
	now := time.Now()
	byKey := map[string]*fedRemoteMember{}
	var order []string
	for _, im := range imports {
		if !visible[im.LocalGroupID] {
			continue
		}
		peer, _ := db.GetFederationPeer(im.Peer)
		if peer == nil {
			continue
		}
		cat, at, err := fedCatalogFor(peer.InstanceID)
		if err != nil || cat == nil {
			continue
		}
		online := rt != nil && rt.isOnline(peer.InstanceID)
		addrPeer := peer.Label
		if addrPeer == "" {
			addrPeer = peer.InstanceID
		}
		for _, g := range cat.Groups {
			if g.Name != im.RemoteGroup {
				continue
			}
			for _, m := range g.Members {
				key := peer.InstanceID + "/" + m.Agent + "/" + g.Name
				rm := byKey[key]
				if rm == nil {
					rm = &fedRemoteMember{
						Address: m.Name + "@" + addrPeer, Agent: m.Agent, Name: m.Name, Role: m.Role, Harness: m.Harness,
						Presence: m.Presence, Peer: peerDisplay(peer), Instance: peer.InstanceID, RemoteGroup: g.Name,
						Mail: g.HasCap(proto.CapMail), PeerOnline: online, CatalogAt: at,
						Stale: !online || now.Sub(at) > fedStaleAfter,
					}
					byKey[key] = rm
					order = append(order, key)
				}
				if !containsString(rm.LocalGroups, im.LocalGroupName) {
					rm.LocalGroups = append(rm.LocalGroups, im.LocalGroupName)
				}
			}
		}
	}
	out := make([]*fedRemoteMember, 0, len(order))
	for _, k := range order {
		sort.Strings(byKey[k].LocalGroups)
		out = append(out, byKey[k])
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
	mux.HandleFunc("GET /v1/federation/status", handleFederationStatus)
	mux.HandleFunc("/v1/federation/notify", handleFederationNotify)
	mux.HandleFunc("GET /v1/federation/inbox", handleFederationInbox)
	mux.HandleFunc("GET /v1/federation/reachable", handleFederationReachable)
	mux.HandleFunc("POST /v1/federation/spawn-requests", handleFederationSpawnRequestSend)
	mux.HandleFunc("GET /v1/federation/spawn-requests", handleFederationSpawnRequestList)
	mux.HandleFunc("POST /v1/federation/spawn-requests/{id}/approve", handleFederationSpawnRequestApprove)
	mux.HandleFunc("POST /v1/federation/spawn-requests/{id}/deny", handleFederationSpawnRequestDeny)
	mux.HandleFunc("/v1/federation/config", handleFederationConfig)
	mux.HandleFunc("/v1/federation/peers/trust", handleFederationTrust)
	mux.HandleFunc("/v1/federation/peers/untrust", handleFederationUntrust)
	mux.HandleFunc("/v1/federation/exports", handleFederationExports)
	mux.HandleFunc("/v1/federation/imports", handleFederationImports)
	mux.HandleFunc("GET /v1/federation/outbox", handleFederationOutbox)
	mux.HandleFunc("/v1/federation/send", handleFederationSend)
	mux.HandleFunc("POST /v1/federation/routes/open", handleFederatedRouteOpen)
}
