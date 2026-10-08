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
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermAgentShare = "agent.share"

type fedShareAgentRequest struct {
	Agent        string `json:"agent"`
	Peer         string `json:"peer"`
	Group        string `json:"group"`
	History      bool   `json:"history"`
	AllowFlagged bool   `json:"allow_flagged"`
}

func handleFederationShareAgent(w http.ResponseWriter, r *http.Request) {
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
	if !human {
		if _, ok := requirePermission(w, r, PermAgentShare, ActionContext{RemotePeer: peer.InstanceID, RemoteGroup: in.Group}); !ok {
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
	b, err := collectAgentBundle(source, in.History)
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
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
	o.Descriptor.Inline = nil
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
	return map[string]any{"id": o.Descriptor.ID, "peer": o.Peer, "sender_agent": o.SenderAgent, "type": o.Descriptor.Type, "sha256": o.Descriptor.SHA256, "expires_at": o.Descriptor.ExpiresAt, "requested_group": o.Descriptor.Group, "group_id": o.GroupID}
}

// Caller holds fedBundleMu across validation, reservation and the normal
// bundle importer. A durable reserved identity prevents an interrupted apply
// from silently spawning another agent when the operator retries.
func importFederationAgentOffer(w http.ResponseWriter, r *http.Request, o *db.FederationBundleOffer, in *fedBundleImportRequest) {
	if len(in.Only) > 0 || len(in.Skip) > 0 || in.Replace {
		writeError(w, 400, "selector", "agent offers use --skip-history; config selectors and --replace do not apply")
		return
	}
	g, err := db.GetAgentGroupByID(o.GroupID)
	if in.Group != "" {
		g, err = db.GetAgentGroupByName(in.Group)
	}
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(o.Peer, g.ID, PermAgentsReceive) {
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
		if err = db.ReserveFederationBundleImport(o.Peer, o.Descriptor.ID, reserved); err != nil {
			writeError(w, 409, "launch_reserved", err.Error())
			return
		}
		o.ImportAgent = reserved
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
		} else {
			// Keep the reserved ID on uncertain failures. Discarding an offer never
			// stops a possibly late agent; the operator can inspect it first.
			_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "ready", "launch attempt reserved as "+reserved+"; inspect it before declining or requesting another offer")
		}
	}
	var response map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		writeError(w, 500, "import", err.Error())
		return
	}
	response["offer"] = federationOfferProvenance(o)
	if o.ImportAgent != "" {
		response["reserved_agent_id"] = o.ImportAgent
	}
	writeJSON(w, rec.Code, response)
}
