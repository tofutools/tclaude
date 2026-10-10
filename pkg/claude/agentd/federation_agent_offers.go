package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermAgentShare = "agent.share"
const PermAgentMove = "agent.move"

type fedShareAgentRequest struct {
	CarryPermissions bool   `json:"carry_permissions"`
	DirectIfAllowed  bool   `json:"direct_if_allowed"`
	Cwd              string `json:"cwd"`
	Landing          string `json:"landing"`
	Agent            string `json:"agent"`
	Peer             string `json:"peer"`
	Group            string `json:"group"`
	History          bool   `json:"history"`
	AllowFlagged     bool   `json:"allow_flagged"`
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
	if in.CarryPermissions && !human {
		if _, ok := requirePermission(w, r, PermSelfTeleportPermissions, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: in.Group}); !ok {
			return
		}
	}
	b, err := collectAgentBundle(source, in.History)
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	defer func() { _ = b.Close() }()
	b.Manifest.Agent.CarryPermissions = in.CarryPermissions
	if (moving || teleport != nil) && b.Manifest.History == nil {
		writeError(w, 400, "history_required", "moves and teleports require native conversation history")
		return
	}
	if len(b.Manifest.Findings) > 0 && !in.AllowFlagged {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: use --allow-flagged or share without --history", "code": "flagged_credentials", "findings": b.Manifest.Findings})
		return
	}
	archive, err := archiveAgentBundle(b)
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	if err := r.Context().Err(); err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	summary := "Agent bundle: configuration only"
	if b.Manifest.History != nil {
		summary = "Agent bundle: configuration and conversation history"
	}
	d, err := bundletransfer.NewFile(agentTransferType(), archive, summary, time.Now().Add(bundletransfer.DefaultTTL))
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
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
		mode, lease, err := prepareRequesterLease(r, caller, peer, d.ID, "teleport", teleport.Credentials)
		if err != nil {
			writeError(w, 403, "requester_pays", err.Error())
			return
		}
		teleport.Credentials, teleport.ModelLease = mode, lease
		teleport.Hops = append(teleport.Hops, bundletransfer.TeleportHop{Offer: d.ID, FromInstance: rt.id.ID(), FromAgent: teleport.SourceAgent, ToInstance: peer.InstanceID, ToGroup: in.Group, At: time.Now().UTC()})
		if err := validateTeleportLimits(teleport, peer.InstanceID, teleportLocalLimits()); err != nil {
			writeError(w, 409, "teleport_limit", err.Error())
			return
		}
		d.Teleport = teleport
		if teleport.KeepPausedBackup {
			if err := reserveTeleportBackup(peer.InstanceID, d.ID, teleport.SourceAgent, source, d.ExpiresAt, teleport.BackupRenewSeconds); err != nil {
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
		if teleport == nil {
			d.Move.DirectIfAllowed, d.Move.Cwd, d.Move.Landing = in.DirectIfAllowed && peerSupportsDirectAgentMoves(peer.InstanceID), in.Cwd, in.Landing
		}
	}
	o := db.FederationBundleOffer{Descriptor: d, Peer: peer.InstanceID, Direction: "out", State: "pending"}
	fromConv := ""
	if !human {
		fromConv = caller
		o.SenderAgent, _ = db.AgentIDForConv(caller)
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if _, err = db.InsertFederationBundleOffer(o, agentTransferType()); err != nil {
		writeError(w, 409, "quota", err.Error())
		return
	}
	cleanup := func() {
		_ = fedBundleSpool().Remove("out", peer.InstanceID, d.ID)
		_ = db.DeleteFederationBundleOffer("out", peer.InstanceID, d.ID)
		_ = db.DeleteFederationAgentMove("out", peer.InstanceID, d.ID)
	}
	if moving {
		m := db.FederationAgentMove{Disposition: "pending_acceptance", Teleport: teleport != nil, Direction: "out", Peer: peer.InstanceID, ID: d.ID, State: "awaiting_confirmation", SourceAgent: d.Move.SourceAgent, SourceConv: source, SHA256: d.SHA256, Human: human, Group: in.Group, ExpiresAt: d.ExpiresAt}
		if d.Move.DirectIfAllowed {
			m.Disposition = "checking"
		}
		if !human {
			m.Initiator, _ = db.AgentIDForConv(caller)
			if a := peerActionFromRequest(r); a != nil {
				m.Initiator = "peer:" + a.peer
				m.Human = false
			}
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
	if err = fedBundleSpool().Receive("out", peer.InstanceID, d, archive); err != nil {
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
	disposition := "pending_acceptance"
	if d.Move != nil && d.Move.DirectIfAllowed {
		disposition = "checking"
	}
	writeJSON(w, 200, map[string]any{"move_id": d.ID, "disposition": disposition, "offer": o, "envelope_id": row.EnvelopeID, "state": row.State, "findings": b.Manifest.Findings, "warnings": b.Manifest.Warnings, "receiver_decides": true, "source_repo": b.Manifest.Agent.Paths.RepoURL})
}

type fedBundleImportRequest struct {
	DropPermissions           bool `json:"drop_permissions"`
	AllowSensitivePermissions bool `json:"allow_sensitive_permissions"`
	configBundleRequest
	Cwd         string   `json:"cwd"`
	Worktree    string   `json:"worktree"`
	Group       string   `json:"group"`
	Name        string   `json:"name"`
	SkipHistory bool     `json:"skip_history"`
	Landing     string   `json:"landing"`
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
	archive, err := fedBundleSpool().Open("in", o.Peer, o.Descriptor)
	if err != nil {
		writeError(w, 500, "spool", err.Error())
		return
	}
	defer archive.Close()
	if err = o.Descriptor.VerifyReader(archive); err != nil {
		writeError(w, 400, "bundle", err.Error())
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

	}
	if teleportRow != nil {
		if err := checkRequesterPays(o.Peer, g.ID, teleportRow.Credentials, teleportRow.Intent.ModelLease, true); err != nil {
			writeError(w, 409, "requester_pays", err.Error())
			return
		}
	}
	bundle, err := agentbundle.DecodeFile(archive, agentTransferLimit(), "")
	if err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	defer func() { _ = bundle.Close() }()
	landing, err := resolveFederationLanding(r.Context(), o, g, bundle.Manifest.Agent.Paths, in, teleportRow)
	if err != nil || landing.Preview.Cwd == "" || !landing.Preview.Exists && !landing.Preview.CheckoutRequired {
		code, status, message := "landing_unresolved", 409, "no receiving working directory resolved"
		if err != nil {
			code, message = "landing_candidate_changed", err.Error()
			if in.Cwd != "" {
				code, status = "landing_unowned", 403
			}
		} else if landing.Preview.Cwd != "" {
			code, message = "landing_missing", "selected receiving directory does not exist"
		}
		writeJSON(w, status, map[string]any{"code": code, "error": federationLandingError(message), "landing": landing.Preview, "offer": federationOfferProvenance(o)})
		return
	}
	// Preview is side-effect free. The inner importer validates the existing
	// receiver clone while the outer response names the planned isolated tree.
	innerCwd := landing.Preview.Cwd
	if landing.repo != nil {
		innerCwd = landing.repo.Definition.Clone
	}
	q := url.Values{"group": {g.Name}, "cwd": {innerCwd}, "worktree": {in.Worktree}, "name": {in.Name}}
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
		inner := r.Clone(context.WithValue(r.Context(), permissionCarryContextKey{}, permissionCarryPolicy{Peer: o.Peer, GroupID: g.ID, Group: g.Name, Enabled: bundle.Manifest.Agent.CarryPermissions && !in.DropPermissions, AllowSensitive: in.AllowSensitivePermissions && teleportLandingFromRequest(r) == nil}))
		if o.Descriptor.Teleport != nil {
			inner = inner.WithContext(context.WithValue(inner.Context(), teleportOfferContextKey{}, o))
		}
		inner.URL = &url.URL{Path: "/v1/agent-bundle/import", RawQuery: query.Encode()}
		inner = inner.WithContext(context.WithValue(inner.Context(), agentBundleDataContextKey{}, bundle))
		inner.Body = http.NoBody
		inner.ContentLength = 0
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
		if teleportRow != nil && teleportRow.Intent.ModelLease != "" {
			proxy := strings.Split(strings.TrimPrefix(teleportRow.Credentials, "proxy:"), "@")[0]
			if err := db.RecordModelProxyWorkerLease(db.ModelProxyWorkerLease{Worker: reserved, Gateway: o.Peer, Lease: teleportRow.Intent.ModelLease, Request: o.Descriptor.ID, Kind: "teleport", Proxy: proxy}); err != nil {
				writeError(w, 503, "requester_pays", err.Error())
				return
			}
		}
		if err = db.ReserveFederationBundleImport(o.Peer, o.Descriptor.ID, reserved); err != nil {
			writeError(w, 409, "launch_reserved", err.Error())
			return
		}
		o.ImportAgent = reserved
		releaseUnlaunched := func() {
			if released, err := db.ReleaseUnlaunchedFederationBundleImport(o.Peer, o.Descriptor.ID, reserved); err == nil && released {
				o.ImportAgent = ""
				_ = db.DeleteFederationAgentMove("in", o.Peer, o.Descriptor.ID)
				cleanupReleasedFederationLanding(o)
			}
		}

		// Persist ownership before creating the isolated tree. A crash at any
		// later point retains the existing reserved-launch inspection contract.
		if err := landing.prepare(r.Context(), g.ID); err != nil {
			releaseUnlaunched()
			writeJSON(w, 409, map[string]any{"code": "landing_candidate_changed", "error": err.Error(), "landing": landing.Preview})
			return
		}
		q.Set("cwd", landing.Preview.Cwd)
		defer func() {
			if landing.repo != nil && o.ImportAgent == "" {
				_ = os.RemoveAll(landing.root)
			}
		}()

		if o.Descriptor.Teleport != nil && teleportLandingFromRequest(r) == nil {
			row, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
			if err != nil || row == nil {
				releaseUnlaunched()
				writeError(w, 503, "teleport_reservation", "teleport provenance unavailable; reserved launch was not started")
				return
			}
			old := row.State
			row.TargetAgent, row.State = reserved, "admitting"
			if won, err := db.TransitionFederationTeleport(*row, old); err != nil || !won {
				releaseUnlaunched()
				writeError(w, 409, "teleport_reservation", "teleport changed; reserved launch was not started")
				return
			}
		}
		if o.Descriptor.Move != nil {
			if err = reserveIncomingAgentMove(o, bundle, reserved); err != nil {
				releaseUnlaunched()
				writeError(w, 400, "move", err.Error())
				return
			}
		}

		if teleportRow != nil && landing.checkout != nil {
			row, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID)
			if err != nil || row == nil {
				releaseUnlaunched()
				writeError(w, 503, "teleport_provenance", "cannot persist landing checkout; reserved launch was not started")
				return
			}
			row.Checkout = landing.checkout
			if won, err := db.TransitionFederationTeleport(*row, row.State); err != nil || !won {
				releaseUnlaunched()
				writeError(w, 409, "teleport_provenance", "teleport changed before dispatch")
				return
			}
			teleportRow = row
			if authority := teleportLandingFromRequest(r); authority != nil {
				authority.record.Checkout = landing.checkout
			}
		}
		liveGroup, groupErr := db.GetAgentGroupByID(g.ID)
		if groupErr != nil || liveGroup == nil || liveGroup.IsArchived() || !fedPeerAllows(o.Peer, g.ID, PermAgentsReceive) && (o.Descriptor.Teleport == nil || !fedPeerAllows(o.Peer, g.ID, PermAgentsTeleportReceive)) {
			releaseUnlaunched()
			writeError(w, 403, "admission", "receiving group admission changed before dispatch")
			return
		}
		q.Set("group", liveGroup.Name)
		if _, exists, err := federationLandingDirectory(landing.Preview.Cwd); err != nil || !exists {
			releaseUnlaunched()
			writeJSON(w, 409, map[string]any{"code": "landing_candidate_changed", "error": "landing directory changed before dispatch", "landing": landing.Preview})
			return
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
	response["landing"] = landing.Preview
	response["cwd"] = landing.Preview.Cwd
	if in.Apply {
		recordFederationAudit("agent.landing", o.Peer, o.ImportAgent, g.Name, fmt.Sprintf("offer=%s cwd=%q reason=%s", o.Descriptor.ID, landing.Preview.Cwd, landing.Preview.Reason), rec.Code)
	}
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
