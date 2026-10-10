package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func validateOfferedConfig(raw []byte) error {
	var b configbundle.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	return b.Validate()
}

type fedConfigOfferSendReq struct {
	Peer         string               `json:"peer"`
	Bundle       *configbundle.Bundle `json:"bundle,omitempty"`
	Only         []string             `json:"only,omitempty"`
	Skip         []string             `json:"skip,omitempty"`
	AllowFlagged bool                 `json:"allow_flagged,omitempty"`
}

func handleFederationOfferConfig(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "offer a config bundle") {
		return
	}
	var in fedConfigOfferSendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 17<<20)).Decode(&in); err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	peer, err := resolveFederationPeerOpt(in.Peer, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	b := in.Bundle
	if b == nil {
		b, err = collectConfigBundle(r)
		if err != nil {
			writeError(w, 500, "export", err.Error())
			return
		}
	}
	if err = b.Validate(); err != nil {
		writeError(w, 400, "format", err.Error())
		return
	}
	if err = b.Select(in.Only, in.Skip); err != nil {
		writeError(w, 400, "selector", err.Error())
		return
	}
	// Re-scan the supplied content: flags in an edited file are not trusted.
	// Prepare omits structured credentials but preserves all free text.
	b.Flags = nil
	b.Omitted = nil
	if err = b.Prepare(); err != nil {
		writeError(w, 400, "export", err.Error())
		return
	}
	if len(b.Flags) > 0 && !in.AllowFlagged {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: exclude flagged items or use --allow-flagged", "code": "flagged_credentials", "flags": b.Flags})
		return
	}
	raw, err := json.Marshal(b)
	if err != nil {
		writeError(w, 400, "export", err.Error())
		return
	}
	if int64(len(raw)) > bundletransfer.Config.MaxBytes {
		writeError(w, 413, "too_large", "config bundle exceeds 16 MiB")
		return
	}
	count := 0
	for _, items := range b.Sections {
		count += len(items)
	}
	d := bundletransfer.New(bundletransfer.Config, raw, fmt.Sprintf("Config bundle: %d items", count), time.Now().Add(bundletransfer.DefaultTTL))
	o := db.FederationBundleOffer{Descriptor: d, Peer: peer.InstanceID, Direction: "out", State: "pending"}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	if _, err = db.InsertFederationBundleOffer(o, bundletransfer.Config); err != nil {
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
	row, err := queueFederatedEnvelope(fedOutgoing{envelopeID: d.ID, peer: peer, kind: proto.KindBundleOffer, toLabel: peerDisplay(peer), subject: "config offer " + d.ID, preview: d.Summary, ttl: time.Until(d.ExpiresAt), payload: d})
	if err != nil {
		cleanup()
		writeFedErr(w, err)
		return
	}
	o.Descriptor.Inline = nil
	writeJSON(w, 200, map[string]any{"offer": o, "envelope_id": row.EnvelopeID, "state": row.State, "flags": b.Flags, "omitted": b.Omitted})
}
func handleFederationBundleOffers(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "list bundle offers") {
		return
	}
	reconcileFederationBundleOffers()
	direction := r.URL.Query().Get("direction")
	if direction != "" && direction != "in" && direction != "out" {
		writeError(w, 400, "direction", "direction must be in or out")
		return
	}
	offers, err := db.ListFederationBundleOffers(direction)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	writeJSON(w, 200, offers)
}
func receivingBundleOffer(w http.ResponseWriter, r *http.Request, requireAdmission bool) *db.FederationBundleOffer {
	found := lookupReceivingBundleOffer(w, r)
	if found == nil {
		return nil
	}
	if found.State != "pending" && found.State != "ready" {
		writeError(w, 409, "offer_state", "offer is "+found.State)
		return nil
	}
	kind, ok := federationBundleKind(found.Descriptor.Type)
	peer, err := db.GetFederationPeer(found.Peer)
	if requireAdmission && (err != nil || peer == nil || !ok || !fedBundleOfferAdmitted(found, kind.Type)) {
		writeError(w, 403, "admission", "peer trust or bundle receive grant revoked")
		return nil
	}
	return found
}
func lookupReceivingBundleOffer(w http.ResponseWriter, r *http.Request) *db.FederationBundleOffer {
	reconcileFederationBundleOffers()
	id := r.PathValue("id")
	if !proto.ValidStreamID(id) {
		writeError(w, 400, "offer", "invalid offer id")
		return nil
	}
	peerRef := r.URL.Query().Get("peer")
	peerID := ""
	if proto.ValidInstanceID(peerRef) {
		// Stable source IDs remain usable to discard local offers after untrust.
		// Fetch and import still enforce current admission below.
		peerID = peerRef
	} else if peerRef != "" {
		peer, err := resolveFederationPeerOpt(peerRef, false)
		if err != nil {
			writeFedErr(w, err)
			return nil
		}
		peerID = peer.InstanceID
	}
	offers, err := db.ListFederationBundleOffers("in")
	if err != nil {
		writeFedErr(w, err)
		return nil
	}
	var found *db.FederationBundleOffer
	for i := range offers {
		o := &offers[i]
		if o.Descriptor.ID == id && (peerID == "" || o.Peer == peerID) {
			if found != nil {
				writeError(w, 409, "ambiguous", "offer id matches multiple peers; select --peer")
				return nil
			}
			found = o
		}
	}
	if found == nil {
		writeError(w, 404, "offer", "no such receiving offer")
		return nil
	}
	return found
}
func ensureFederationBundleReady(w http.ResponseWriter, r *http.Request, o *db.FederationBundleOffer) bool {
	if o.State == "ready" {
		return true
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 503, "offline", "federation must be connected to fetch this bundle; sender must be online")
		return false
	}
	if err := rt.fetchBundle(r.Context(), o); err != nil {
		_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "pending", err.Error())
		writeError(w, 502, "transfer", err.Error())
		return false
	}
	o.State = "ready"
	return true
}
func handleFederationBundleFetch(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "fetch a bundle offer") {
		return
	}
	o := receivingBundleOffer(w, r, true)
	if o == nil || !ensureFederationBundleReady(w, r, o) {
		return
	}
	writeJSON(w, 200, o)
}
func handleFederationBundleImport(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "preview or apply a bundle offer") {
		return
	}
	var in fedBundleImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, 400, "json", err.Error())
		return
	}
	o := receivingBundleOffer(w, r, true)
	if o == nil {
		return
	}

	if !ensureFederationBundleReady(w, r, o) {
		return
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	kind, _ := federationBundleKind(o.Descriptor.Type)
	current, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil || current == nil || current.State != "ready" || !current.Descriptor.ExpiresAt.After(time.Now()) || !fedBundleOfferAdmitted(current, kind.Type) {
		writeError(w, 409, "offer_state", "offer changed before import; preview again")
		return
	}
	if o.Descriptor.Type == bundletransfer.Agent.Name {
		importFederationAgentOffer(w, r, current, &in)
		return
	}
	raw, err := fedBundleSpool().Read("in", o.Peer, o.Descriptor)
	if err != nil {
		writeError(w, 500, "spool", err.Error())
		return
	}
	if err = json.Unmarshal(raw, &in.Bundle); err != nil {
		writeError(w, 400, "bundle", err.Error())
		return
	}
	request, err := json.Marshal(in.configBundleRequest)
	if err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(request))
	inner.ContentLength = int64(len(request))
	rec := httptest.NewRecorder()
	handleConfigBundleImport(rec, inner)
	if rec.Code == 200 && in.Apply {
		if err = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "applied", ""); err != nil {
			writeError(w, 500, "receipt", "import ran but storing offer receipt failed: "+err.Error())
			return
		}
		_ = fedBundleSpool().Remove("in", o.Peer, o.Descriptor.ID)
		queueBundleResult(o, "applied")
	}
	// Preserve the existing import's before/after diffs, security tags, selector
	// and placeholder diagnostics, including partial-apply failures.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Tclaude-Offer-Peer", o.Peer)
	w.Header().Set("X-Tclaude-Offer-ID", o.Descriptor.ID)
	w.WriteHeader(rec.Code)
	var response map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &response) == nil {
		response["offer"] = federationOfferProvenance(o)
		_ = json.NewEncoder(w).Encode(response)
	} else {
		_, _ = w.Write(rec.Body.Bytes())
	}
}

// A deterministic outbox ID makes a crash between queuing and recording the
// receipt idempotent. Ordinary federation outbox retries then handle delivery.
func queueBundleResult(o *db.FederationBundleOffer, state string) {
	peer, err := db.GetFederationPeer(o.Peer)
	if err != nil || peer == nil {
		return
	}
	sum := sha256.Sum256([]byte("bundle-result/" + o.Peer + "/" + o.Descriptor.ID))
	id := hex.EncodeToString(sum[:16])
	if row, err := db.GetFederationOutbox(id); err != nil {
		return
	} else if row != nil {
		_ = db.MarkFederationBundleResultQueued(o.Peer, o.Descriptor.ID)
		return
	}
	_, err = queueFederatedEnvelope(fedOutgoing{envelopeID: id, peer: peer, kind: proto.KindBundleResult, toLabel: peerDisplay(peer), inReplyTo: o.Descriptor.ID, subject: o.Descriptor.Type + " offer " + state, preview: o.Descriptor.ID, ttl: bundletransfer.DefaultTTL, payload: bundletransfer.Result{Offer: o.Descriptor.ID, State: state}})
	if err == nil {
		_ = db.MarkFederationBundleResultQueued(o.Peer, o.Descriptor.ID)
	}
}
func handleFederationBundleDecline(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "decline a bundle offer") {
		return
	}
	o := receivingBundleOffer(w, r, false)
	if o == nil {
		return
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	current, err := db.GetFederationBundleOffer("in", o.Peer, o.Descriptor.ID)
	if err != nil || current == nil || (current.State != "pending" && current.State != "ready") {
		writeError(w, 409, "offer_state", "offer changed before decline")
		return
	}
	if err := db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "declined", ""); err != nil {
		writeFedErr(w, err)
		return
	}
	_ = fedBundleSpool().Remove("in", o.Peer, o.Descriptor.ID)
	queueBundleResult(o, "declined")
	writeJSON(w, 200, map[string]string{"state": "declined", "id": o.Descriptor.ID})
}
func registerFederationBundleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/federation/share-agent", handleFederationShareAgent)
	mux.HandleFunc("POST /v1/federation/move-agent", handleFederationShareAgent)
	mux.HandleFunc("POST /v1/whoami/teleport", handleFederationTeleport)
	mux.HandleFunc("POST /v1/whoami/teleport/report", handleTeleportReport)
	mux.HandleFunc("POST /v1/teleport/recover", handleTeleportRecover)
	mux.HandleFunc("GET /v1/whoami/teleports", handleSelfTeleports)
	mux.HandleFunc("GET /v1/federation/teleport", handleFederationTeleportSwitch)
	mux.HandleFunc("PUT /v1/federation/teleport", handleFederationTeleportSwitch)
	mux.HandleFunc("GET /v1/federation/moves", handleFederationMoves)
	mux.HandleFunc("GET /v1/federation/moves/{id}", handleFederationMoves)
	mux.HandleFunc("POST /v1/federation/moves/{id}/abandon", handleFederationMoveAbandon)
	mux.HandleFunc("POST /v1/federation/offer-config", handleFederationOfferConfig)
	mux.HandleFunc("GET /v1/federation/bundle-offers", handleFederationBundleOffers)
	mux.HandleFunc("GET /v1/federation/bundle-offers/{id}/contents", handleFederationBundleContents)
	mux.HandleFunc("GET /v1/federation/bundle-offers/{id}/download", handleFederationBundleDownload)
	mux.HandleFunc("POST /v1/federation/bundle-offers/{id}/fetch", handleFederationBundleFetch)
	mux.HandleFunc("POST /v1/federation/bundle-offers/{id}/import", handleFederationBundleImport)
	mux.HandleFunc("POST /v1/federation/bundle-offers/{id}/decline", handleFederationBundleDecline)
}
