package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermAgentShare = "agent.share"
const PermAgentMove = "agent.move"

type fedShareAgentRequest struct {
	Agent        string `json:"agent"`
	Peer         string `json:"peer"`
	Group        string `json:"group"`
	History      bool   `json:"history"`
	AllowFlagged bool   `json:"allow_flagged"`
}

func handleFederationShareAgent(w http.ResponseWriter, r *http.Request) {
	moving := strings.HasSuffix(r.URL.Path, "/move-agent")
	teleport, _ := r.Context().Value(teleportSourceContextKey{}).(*bundletransfer.TeleportIntent)
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	var in fedShareAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	peer, err := resolveFederationPeerOpt(in.Peer, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if in.Group == "" || len(in.Group) > 256 {
		writeError(w, 400, "group", "--group must name a receiving group on the peer")
		return
	}
	permission := PermAgentShare
	if moving {
		permission = PermAgentMove
		in.History = true
		if !peerSupportsAgentMoves(peer.InstanceID) {
			writeError(w, 409, "unsupported_peer", "peer has not advertised agent move support")
			return
		}
	}
	if teleport != nil {
		permission = PermSelfTeleport
	}
	if !human {
		if _, ok := requirePermission(w, r, permission, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: in.Group}); !ok {
			return
		}
	}
	source := caller
	if in.Agent != "" && in.Agent != "self" {
		res, _, err := agent.ResolveSelector(in.Agent)
		if err != nil {
			writeError(w, 404, "agent", err.Error())
			return
		}
		source = res.ConvID
	}
	if source == "" {
		writeError(w, 400, "agent", "select a source agent")
		return
	}
	if !human && source != caller {
		if _, ok := requireCrossAgentPermission(w, r, PermAgentBundleExport, source); !ok {
			return
		}
	}
	if teleport != nil && source != caller {
		writeError(w, 403, "self_only", "teleport can only transfer the caller")
		return
	}
	if moving && teleport == nil {
		if _, ok := requireCrossAgentPermission(w, r, PermAgentRetire, source); !ok {
			return
		}
	}
	b, err := collectAgentBundle(source, in.History)
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	if (moving || teleport != nil) && b.Manifest.History == nil {
		writeError(w, 400, "history_required", "moves and teleports require native conversation history")
		return
	}
	if len(b.Manifest.Findings) > 0 && !in.AllowFlagged {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: use --allow-flagged or share without --history", "code": "flagged_credentials", "findings": b.Manifest.Findings})
		return
	}
	raw, err := b.Encode()
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	summary := "Agent bundle: configuration only"
	if b.Manifest.History != nil {
		summary = "Agent bundle: configuration and conversation history"
	}
	d := bundletransfer.New(bundletransfer.Agent, raw, summary, time.Now().Add(bundletransfer.DefaultTTL))
	d.Group = in.Group
	backupQueued := false
	defer func() {
		if teleport != nil && teleport.KeepPausedBackup && !backupQueued {
			teleportLeaseMu.Lock()
			defer teleportLeaseMu.Unlock()
			if l, e := db.GetFederationTeleportLease("out", peer.InstanceID, d.ID); e == nil && l != nil && l.State == "reserved" {
				l.State = "released"
				_, _ = db.TransitionFederationTeleportLease(*l, "")
			}
		}
	}()
	if teleport != nil {
		rt := currentFederation()
		if rt == nil {
			writeError(w, 503, "offline", "federation disconnected")
			return
		}
		teleport.Hops = append(teleport.Hops, bundletransfer.TeleportHop{Offer: d.ID, FromInstance: rt.id.ID(), FromAgent: teleport.SourceAgent, ToInstance: peer.InstanceID, ToGroup: in.Group, At: time.Now().UTC()})
		if err := validateTeleportLimits(teleport, peer.InstanceID, teleportLocalLimits()); err != nil {
			writeError(w, 409, "teleport_limit", err.Error())
			return
		}
		d.Teleport = teleport
		if teleport.KeepPausedBackup {
			if err := reserveTeleportBackup(peer.InstanceID, d.ID, teleport.SourceAgent, source, d.ExpiresAt); err != nil {
				writeError(w, 409, "dormant_quota", err.Error())
				return
			}
		}
		if err := db.RecordFederationTeleport(db.FederationTeleport{Direction: "out", Peer: peer.InstanceID, Offer: d.ID, State: "offered", Intent: *teleport}, teleportLocalLimits()); err != nil {
			writeError(w, 429, "teleport_limit", err.Error())
			return
		}
	}

	if moving {
		aid, err := db.AgentIDForConv(source)
		if err != nil || aid == "" {
			writeError(w, 409, "source", "source must be an active local agent")
			return
		}
		a, err := db.GetAgent(aid)
		if err != nil || a == nil || !a.Active() || a.CurrentConvID != source {
			writeError(w, 409, "source", "source generation changed")
			return
		}
		d.Move = &bundletransfer.MoveIntent{SourceAgent: aid, SourceConv: source}
	}
	o := db.FederationBundleOffer{Descriptor: d, Peer: peer.InstanceID, Direction: "out", State: "pending"}
	fromConv := ""
	if !human {
		fromConv = caller
		o.SenderAgent, _ = db.AgentIDForConv(caller)
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if _, err = db.InsertFederationBundleOffer(o, bundletransfer.Agent); err != nil {
		writeError(w, 409, "quota", err.Error())
		return
	}
	cleanup := func() {
		_ = fedBundleSpool().Remove("out", peer.InstanceID, d.ID)
		_ = db.DeleteFederationBundleOffer("out", peer.InstanceID, d.ID)
		_ = db.DeleteFederationAgentMove("out", peer.InstanceID, d.ID)
	}
	if moving {
		m := db.FederationAgentMove{Teleport: teleport != nil, Direction: "out", Peer: peer.InstanceID, ID: d.ID, State: "awaiting_confirmation", SourceAgent: d.Move.SourceAgent, SourceConv: source, SHA256: d.SHA256, Human: human, Group: in.Group, ExpiresAt: d.ExpiresAt}
		if !human {
			m.Initiator, _ = db.AgentIDForConv(caller)
		}
		groups, e := db.ListGroupsForConv(source)
		if e != nil {
			cleanup()
			writeError(w, 500, "groups", e.Error())
			return
		}
		for _, g := range groups {
			m.SourceGroups = append(m.SourceGroups, g.ID)
		}
		if e = db.InsertFederationAgentMove(m); e != nil {
			cleanup()
			writeError(w, 409, "move", e.Error())
			return
		}
	}
	if err = fedBundleSpool().Receive("out", peer.InstanceID, d, bytes.NewReader(raw)); err != nil {
		cleanup()
		writeError(w, 500, "spool", err.Error())
		return
	}
	row, err := queueFederatedEnvelope(fedOutgoing{envelopeID: d.ID, fromConv: fromConv, peer: peer, kind: proto.KindBundleOffer, toLabel: in.Group + "@" + peerDisplay(peer), subject: "agent offer " + d.ID, preview: summary, ttl: time.Until(d.ExpiresAt), payload: d})
	if err != nil {
		cleanup()
		writeFedErr(w, err)
		return
	}
	backupQueued = true
	o.Descriptor.Inline = nil
	if teleport != nil {
		recordFederationAudit("teleport.send", peer.InstanceID, teleport.SourceAgent, in.Group, fmt.Sprintf("offer=%s chain=%s hop=%d credentials=%s clone=%t", d.ID, teleport.Chain, len(teleport.Hops), teleport.Credentials, teleport.Clone), 200)
	}
	setAuditTargetLabel(r, in.Group+"@"+peerDisplay(peer))
	writeJSON(w, 200, map[string]any{"offer": o, "envelope_id": row.EnvelopeID, "state": row.State, "findings": b.Manifest.Findings, "warnings": b.Manifest.Warnings})
}

type fedBundleImportRequest struct {
	configBundleRequest
	Cwd         string   `json:"cwd"`
	Worktree    string   `json:"worktree"`
	Group       string   `json:"group"`
	Name        string   `json:"name"`
	SkipHistory bool     `json:"skip_history"`
	Set         []string `json:"set"`
}

func federationOfferProvenance(o *db.FederationBundleOffer) map[string]any {
	return map[string]any{"id": o.Descriptor.ID, "peer": o.Peer, "sender_agent": o.SenderAgent, "type": o.Descriptor.Type, "sha256": o.Descriptor.SHA256, "expires_at": o.Descriptor.ExpiresAt, "requested_group": o.Descriptor.Group, "group_id": o.GroupID, "move": o.Descriptor.Move, "teleport": o.Descriptor.Teleport}
}

// Caller holds fedBundleMu across validation, reservation and the normal
// bundle importer. A durable reserved identity prevents an interrupted apply
// from silently spawning another agent when the operator retries.
func importFederationAgentOffer(w http.ResponseWriter, r *http.Request, o *db.FederationBundleOffer, in *fedBundleImportRequest) {
	if o.Descriptor.Teleport != nil {
		if teleportFrozen() {
			writeError(w, 409, "teleport_frozen", "teleports are frozen by the operator")
			return
		}
		if _, err := pendingTeleportCredentials(o.Peer, o.Descriptor.Teleport.Credentials); err != nil {
			writeError(w, 409, "teleport_credentials", err.Error())
			return
		}
	}
	if o.Descriptor.Teleport != nil && in.SkipHistory {
		writeError(w, 400, "history_required", "teleports cannot skip history")
		return
	}
	if o.Descriptor.Move != nil && in.SkipHistory {
		writeError(w, 400, "history_required", "moves cannot skip history")
		return
	}
	if len(in.Only) > 0 || len(in.Skip) > 0 || in.Replace {
		writeError(w, 400, "selector", "agent offers use --skip-history; config selectors and --replace do not apply")
		return
	}
	g, err := db.GetAgentGroupByID(o.GroupID)
	if in.Group != "" {
		g, err = db.GetAgentGroupByName(in.Group)
	}
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(o.Peer, g.ID, PermAgentsReceive) && (o.Descriptor.Teleport == nil || !fedPeerAllows(o.Peer, g.ID, PermAgentsTeleportReceive)) {
		writeError(w, 403, "admission", "receiving group must grant this peer agents.receive")
		return
	}
	if in.Apply && o.ImportAgent != "" {
		writeJSON(w, 409, map[string]any{"code": "launch_reserved", "error": "an earlier apply may have launched an agent; inspect the reserved identity before declining this offer", "agent_id": o.ImportAgent, "offer": federationOfferProvenance(o)})
		return
	}
	raw, err := fedBundleSpool().Read("in", o.Peer, o.Descriptor)
	if err != nil {
		writeError(w, 500, "spool", err.Error())
		return
	}
	var teleportRow *db.FederationTeleport
	if o.Descriptor.Teleport != nil {
		teleportRow, err = db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
		if err != nil || teleportRow == nil {
			writeError(w, 503, "teleport_provenance", "teleport provenance unavailable")
			return
		}
		if err := checkTeleportRepo(teleportRow, g.ID); err != nil {
			writeError(w, 409, "teleport_repo", err.Error())
			return
		}
		if teleportRow.Intent.GitRef != "" && teleportLandingFromRequest(r) == nil {
			assignment, err := db.GetFederationNodeProfileAssignment(o.Peer)
			if err != nil || assignment == nil || assignment.Profile.Definition.TeleportLanding == nil {
				writeError(w, 409, "teleport_repo", "git-ref requires an applied landing policy")
				return
			}
			in.Cwd = assignment.Profile.Definition.TeleportLanding.Cwd
			in.Worktree = ""
		}
	}
	q := url.Values{"group": {g.Name}, "cwd": {in.Cwd}, "worktree": {in.Worktree}, "name": {in.Name}}
	if in.KeepPaths {
		q.Set("keep_paths", "true")
	}
	if in.SkipHistory {
		q.Set("skip_history", "true")
	}
	q["set"] = append([]string{}, in.Set...)
	for name, value := range in.Values {
		q.Add("set", name+"="+value)
	}
	invoke := func(apply bool, reserved string) *httptest.ResponseRecorder {
		query := maps.Clone(q)
		if apply {
			query.Set("apply", "true")
		}
		inner := r.Clone(r.Context())
		if o.Descriptor.Teleport != nil {
			inner = inner.WithContext(context.WithValue(inner.Context(), teleportOfferContextKey{}, o))
		}
		inner.URL = &url.URL{Path: "/v1/agent-bundle/import", RawQuery: query.Encode()}
		inner.Body = io.NopCloser(bytes.NewReader(raw))
		inner.ContentLength = int64(len(raw))
		if reserved != "" {
			inner = inner.WithContext(context.WithValue(inner.Context(), reservedAgentIDContextKey{}, reserved))
		}
		rec := httptest.NewRecorder()
		handleAgentBundleImport(rec, inner)
		return rec
	}
	rec := invoke(false, "")
	if rec.Code == 200 && in.Apply {
		// The existing importer checks paths before entering spawn. Preserve its
		// diagnostics without reserving a launch for an unresolved preview.
		var preview agentBundlePreview
		if err = json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
			writeError(w, 500, "preview", err.Error())
			return
		}
		if preview.Cwd == "" || len(preview.Unresolved) > 0 {
			writeJSON(w, 409, map[string]any{"code": "unresolved_paths", "error": "resolve paths before applying", "preview": preview, "offer": federationOfferProvenance(o)})
			return
		}
		reserved := db.NewAgentID()
		if authority := teleportLandingFromRequest(r); authority != nil {
			reserved = authority.record.TargetAgent
			if err := authority.check(); err != nil {
				writeError(w, 403, "teleport_revoked", err.Error())
				return
			}
			if err := db.RecordFederationWorkerDefaults(reserved, authority.record.WorkerDefaults); err != nil {
				writeError(w, 503, "worker_defaults", err.Error())
				return
			}
		}
		if err = db.ReserveFederationBundleImport(o.Peer, o.Descriptor.ID, reserved); err != nil {
			writeError(w, 409, "launch_reserved", err.Error())
			return
		}
		o.ImportAgent = reserved
		if o.Descriptor.Teleport != nil && teleportLandingFromRequest(r) == nil {
			row, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
			if err != nil || row == nil {
				_, _ = db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved)
				writeError(w, 503, "teleport_reservation", "teleport provenance unavailable; reserved launch was not started")
				return
			}
			old := row.State
			row.TargetAgent, row.State = reserved, "admitting"
			if won, err := db.TransitionFederationTeleport(*row, old); err != nil || !won {
				_, _ = db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved)
				writeError(w, 409, "teleport_reservation", "teleport changed; reserved launch was not started")
				return
			}
		}
		if o.Descriptor.Move != nil {
			if err = reserveIncomingAgentMove(o, raw, reserved); err != nil {
				_, _ = db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved)
				writeError(w, 400, "move", err.Error())
				return
			}
		}
		if teleportRow != nil && teleportRow.Intent.GitRef != "" && teleportLandingFromRequest(r) == nil {
			teleportRow, err = db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
			if err != nil || teleportRow == nil {
				_, _ = db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved)
				o.ImportAgent = ""
				_ = db.DeleteFederationAgentMove("in", o.Peer, o.Descriptor.ID)
				writeError(w, 503, "teleport_provenance", "teleport provenance unavailable; launch was not started")
				return
			}
			checkout, checkoutErr := prepareTeleportCheckout(r.Context(), teleportRow, g.ID)
			if checkoutErr != nil {
				_, _ = db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved)
				o.ImportAgent = ""
				_ = db.DeleteFederationAgentMove("in", o.Peer, o.Descriptor.ID)
				teleportRow.State, teleportRow.TargetAgent = "pending", ""
				_, _ = db.TransitionFederationTeleport(*teleportRow, "admitting")
				writeError(w, 409, "teleport_repo", checkoutErr.Error())
				return
			}
			q.Set("cwd", checkout.Path)
		}
		rec = invoke(true, reserved)
		if rec.Code == 200 {
			if err = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "applied", ""); err != nil {
				writeError(w, 500, "receipt", fmt.Sprintf("launch accepted as %s but storing receipt failed: %v", reserved, err))
				return
			}
			_ = fedBundleSpool().Remove("in", o.Peer, o.Descriptor.ID)
			queueBundleResult(o, "applied")
		} else if released, err := db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved); err == nil && released {
			o.ImportAgent = ""
			_ = db.DeleteFederationAgentMove("in", o.Peer, o.Descriptor.ID)
		} else {
			// Keep the reserved ID on uncertain failures. Discarding an offer never
			// stops a possibly late agent; the operator can inspect it first.
			_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "ready", "launch attempt reserved as "+reserved+"; inspect it before declining or requesting another offer")
		}
		if o.Descriptor.Teleport != nil && teleportLandingFromRequest(r) == nil {
			if row, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID); err == nil && row != nil {
				row.State = "uncertain"
				if rec.Code == 200 {
					row.State = "landed"
				} else if o.ImportAgent == "" {
					row.State, row.TargetAgent = "pending", ""
					cleanupUnlaunchedTeleportCheckout(row)
				}
				_, _ = db.TransitionFederationTeleport(*row, "admitting")
				recordFederationAudit("teleport.land", o.Peer, row.TargetAgent, g.Name, fmt.Sprintf("offer=%s predecessor=%s credentials=%s state=%s", o.Descriptor.ID, row.Intent.SourceAgent, row.Credentials, row.State), rec.Code)
			}
		}
	}
	var response map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		writeError(w, 500, "import", err.Error())
		return
	}
	response["offer"] = federationOfferProvenance(o)
	if o.Descriptor.Teleport != nil {
		response["credentials"], _ = pendingTeleportCredentials(o.Peer, o.Descriptor.Teleport.Credentials)
		response["git_ref"] = o.Descriptor.Teleport.GitRef
		if teleportRow != nil && teleportRow.Repo != nil {
			response["repo"] = teleportRow.Repo.Name
		}
		if teleportRow != nil && teleportRow.Checkout != nil {
			response["checkout_commit"] = teleportRow.Checkout.Commit
			response["checkout_resolution"] = teleportRow.Checkout.Resolution
		}
	}
	if o.ImportAgent != "" {
		response["reserved_agent_id"] = o.ImportAgent
	}
	writeJSON(w, rec.Code, response)
}
