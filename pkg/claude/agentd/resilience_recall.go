package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

// A recall asks a custodian for a resilience snapshot it holds for this
// node, for example after this node was rebuilt with its federation
// identity. The returned snapshot is recovered as a new agent: possession
// of a snapshot never restores the original agent's identity.
//
// A recall is bound to the peer asked, the exact source agent, the offer
// asked for (when named) and an expiry. The first return offer that
// matches binds it, with its digest; any other or later return is refused,
// and a recall lands at most one agent.

const (
	resilienceRecallTTL = time.Hour
	// resilienceRecallKeep is how long finished recalls stay listed.
	resilienceRecallKeep = 7 * 24 * time.Hour
	resilienceMaxRecalls = 50
	// resilienceRecoveringStale is how long an import may hold a recall.
	resilienceRecoveringStale = 30 * time.Minute
)

type resilienceRecall struct {
	ID          string `json:"id"`
	Peer        string `json:"peer"`
	SourceAgent string `json:"source_agent"`
	Offer       string `json:"offer,omitempty"`
	// State is requested, offered (a return offer is bound), ready (the
	// returned snapshot is here), recovering, recovered, interrupted (a
	// recovery that did not finish; recall again) or expired.
	State     string    `json:"state"`
	ClaimedAt time.Time `json:"claimed_at,omitempty"`
	Bound     string    `json:"bound_offer,omitempty"`
	HeldOffer string    `json:"held_offer,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	TakenAt   time.Time `json:"taken_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Error     string    `json:"error,omitempty"`
}

var resilienceRecallMu sync.Mutex

func resilienceRecallPath() string {
	return filepath.Join(config.DataDir(), "resilience", "recalls.json")
}

func resilienceReturnedPath(recall string) string {
	return filepath.Join(config.DataDir(), "resilience", "returned", recall+".paf.age")
}

// loadRecalls reads the recalls, expiring unanswered ones and dropping
// old finished ones. The caller holds resilienceRecallMu.
func loadRecalls(now time.Time) ([]resilienceRecall, error) {
	raw, err := os.ReadFile(resilienceRecallPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []resilienceRecall
	if err = json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("read %s: %w", resilienceRecallPath(), err)
	}
	kept := all[:0]
	for _, r := range all {
		if (r.State == "requested" || r.State == "offered") && !now.Before(r.ExpiresAt) {
			r.State = "expired"
		}
		// A returned snapshot waits a while for its recovery, not forever.
		if r.State == "ready" && now.Sub(r.CreatedAt) > resilienceRecallKeep {
			r.State = "expired"
			_ = os.Remove(resilienceReturnedPath(r.ID))
		}
		// A recovery cut short (the daemon stopped mid-import) may or may
		// not have landed; it is never retried from this copy, so it can
		// never land twice. Recall again to recover.
		if r.State == "recovering" && now.Sub(r.ClaimedAt) > resilienceRecoveringStale {
			r.State = "interrupted"
			_ = os.Remove(resilienceReturnedPath(r.ID))
		}
		if (r.State == "expired" || r.State == "recovered" || r.State == "interrupted") && now.Sub(r.ExpiresAt) > resilienceRecallKeep {
			_ = os.Remove(resilienceReturnedPath(r.ID))
			continue
		}
		kept = append(kept, r)
	}
	return kept, nil
}

func saveRecalls(all []resilienceRecall) error {
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(resilienceRecallPath()), 0o700); err != nil {
		return err
	}
	return writeSpoolFile(resilienceRecallPath(), raw)
}

// updateRecall applies fn to one recall under the lock and saves.
func updateRecall(id string, fn func(*resilienceRecall) error) (resilienceRecall, error) {
	resilienceRecallMu.Lock()
	defer resilienceRecallMu.Unlock()
	all, err := loadRecalls(time.Now())
	if err != nil {
		return resilienceRecall{}, err
	}
	for i := range all {
		if all[i].ID == id {
			if err := fn(&all[i]); err != nil {
				return all[i], err
			}
			return all[i], saveRecalls(all)
		}
	}
	return resilienceRecall{}, &boardHTTPError{404, "recall", "no recall " + id}
}

// resilienceRecallAdmits reports whether a return offer answers a recall
// this node made of that peer: the same source agent and offer asked for,
// unexpired, and either unanswered or already bound to this very offer.
func resilienceRecallAdmits(peer string, d bundletransfer.Descriptor) bool {
	if d.Return == nil {
		return false
	}
	resilienceRecallMu.Lock()
	defer resilienceRecallMu.Unlock()
	all, err := loadRecalls(time.Now())
	if err != nil {
		return false
	}
	for _, r := range all {
		if r.ID == d.Return.Recall {
			return recallMatches(r, peer, d)
		}
	}
	return false
}

func recallMatches(r resilienceRecall, peer string, d bundletransfer.Descriptor) bool {
	if r.Peer != peer || r.SourceAgent != d.Return.SourceAgent || (r.Offer != "" && r.Offer != d.Return.Offer) {
		return false
	}
	switch r.State {
	case "requested":
		return time.Now().Before(r.ExpiresAt)
	case "offered", "ready":
		return r.Bound == d.ID && r.SHA256 == d.SHA256
	}
	return false
}

// bindResilienceRecall binds a recall to the first return offer that
// answers it.
func bindResilienceRecall(peer string, d bundletransfer.Descriptor) error {
	_, err := updateRecall(d.Return.Recall, func(r *resilienceRecall) error {
		if !recallMatches(*r, peer, d) {
			return errors.New("no matching recall")
		}
		if r.State == "requested" {
			r.State, r.Bound, r.SHA256, r.HeldOffer, r.TakenAt = "offered", d.ID, d.SHA256, d.Return.Offer, d.Return.TakenAt
		}
		return nil
	})
	return err
}

// resilienceReturnReceive fetches a bound return offer if needed and keeps
// the returned ciphertext for the operator to recover.
func (rt *fedRuntime) resilienceReturnReceive(o *db.FederationBundleOffer) {
	if o.State == "pending" {
		if err := rt.fetchBundle(rt.ctx, o); err != nil {
			_ = db.SetFederationBundleOfferState("in", o.Peer, o.Descriptor.ID, "pending", err.Error())
			return
		}
		o.State = "ready"
	}
	if err := resilienceReturnCommit(o); err != nil {
		slog.Warn("resilience: keep returned snapshot", "peer", o.Peer, "offer", o.Descriptor.ID, "error", err)
	}
}

func resilienceReturnCommit(o *db.FederationBundleOffer) error {
	d := o.Descriptor
	if d.Return == nil || !proto.ValidStreamID(d.Return.Recall) {
		return errors.New("invalid return identity")
	}
	fedBundleMu.Lock()
	defer fedBundleMu.Unlock()
	current, err := db.GetFederationBundleOffer("in", o.Peer, d.ID)
	if err != nil {
		return err
	}
	if current == nil || current.State != "ready" || !fedBundleOfferAdmitted(current, bundletransfer.SnapshotReturn) {
		return errors.New("return offer is not ready or no longer matches its recall")
	}
	raw, err := fedBundleSpool().Read("in", o.Peer, d)
	if err != nil {
		return err
	}
	if err = validResilienceSnapshot(raw); err != nil {
		if db.SetFederationBundleOfferState("in", o.Peer, d.ID, "declined", err.Error()) == nil {
			queueBundleResult(current, "declined")
		}
		return err
	}
	path := resilienceReturnedPath(d.Return.Recall)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err = writeSpoolFile(path, raw); err != nil {
		return err
	}
	if _, err = updateRecall(d.Return.Recall, func(r *resilienceRecall) error {
		if r.State == "offered" && r.Bound == d.ID {
			r.State = "ready"
		}
		return nil
	}); err != nil {
		return err
	}
	_ = fedBundleSpool().Remove("in", o.Peer, d.ID)
	if err = db.SetFederationBundleOfferState("in", o.Peer, d.ID, "applied", ""); err != nil {
		return err
	}
	queueBundleResult(current, "applied")
	recordFederationAudit("resilience.recall.returned", o.Peer, "", "", "agent="+d.Return.SourceAgent+" recall="+d.Return.Recall+" offer="+d.ID, 200)
	return nil
}

// serveResilienceRecall answers a recall from the instance a snapshot is
// held for: only that authenticated instance gets its own snapshots back.
func (rt *fedRuntime) serveResilienceRecall(peer *db.FederationPeer, env *proto.Envelope) {
	refuse := func(code, msg string) {
		rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckRefused, Code: code, Reason: msg})
	}
	var req proto.ResilienceRecallPayload
	if env.DecodePayload(&req) != nil || env.From.Agent != "" || !proto.ValidStreamID(req.Recall) || !proto.ValidAgentRef(req.SourceAgent) || (req.Offer != "" && !proto.ValidStreamID(req.Offer)) {
		refuse(fedCodeMalformed, "a recall comes from the remote operator and names a source agent")
		return
	}
	if !rt.allowInbound(peer.InstanceID) {
		refuse(fedCodeRateLimited, "recall rate exceeded")
		return
	}
	now := time.Now()
	resilienceHeldMu.Lock()
	var h heldSnapshot
	found := false
	for _, x := range listHeld(peer.InstanceID) {
		if x.SourceAgent == req.SourceAgent && now.Before(x.ExpiresAt) && (req.Offer == "" || x.Offer == req.Offer) {
			h, found = x, true // newest first
			break
		}
	}
	var raw []byte
	var err error
	if found {
		raw, err = os.ReadFile(resilienceHeldPath(h.Peer, h.SourceAgent, h.Offer))
	}
	resilienceHeldMu.Unlock()
	if !found || err != nil {
		refuse("not_held", "no snapshot of that agent is held for this instance")
		return
	}
	d := bundletransfer.New(bundletransfer.SnapshotReturn, raw, "Returned resilience snapshot of "+h.SourceAgent, now.Add(resilienceOfferTTL))
	d.Return = &bundletransfer.ReturnIntent{Recall: req.Recall, SourceAgent: h.SourceAgent, Offer: h.Offer, TakenAt: h.TakenAt}
	if err := offerResilienceSnapshot(peer, d, raw); err != nil {
		refuse(fedCodeInternal, "could not return the snapshot")
		return
	}
	recordFederationAudit("resilience.recall.served", peerDisplay(peer), "", "", "agent="+h.SourceAgent+" held="+h.Offer+" offer="+d.ID, 200)
	rt.sendControl(peer.InstanceID, proto.KindAck, env.ID, proto.AckPayload{Status: proto.AckAccepted})
}

// --- operator routes ---

type resilienceRecallRequest struct {
	// From is peer:<instance id|label>.
	From        string `json:"from"`
	SourceAgent string `json:"source_agent"`
	Offer       string `json:"offer,omitempty"`
}

// handleResilienceRecallCreate asks a custodian for a held snapshot back.
//
//	POST /v1/resilience/recalls {from, source_agent, offer?}
func handleResilienceRecallCreate(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "recall a resilience snapshot") {
		return
	}
	var req resilienceRecallRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, 400, "body", err.Error())
		return
	}
	ref, ok := strings.CutPrefix(req.From, "peer:")
	if !ok || !proto.ValidAgentRef(req.SourceAgent) || (req.Offer != "" && !proto.ValidStreamID(req.Offer)) {
		writeError(w, 400, "recall", "name from peer:<instance id or label> and the source agent's id (agt_…)")
		return
	}
	peer, err := resolveFederationPeerOpt(ref, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	var catalog proto.CatalogPayload
	if raw, _, err := db.GetFederationCatalog(peer.InstanceID); err == nil {
		_ = json.Unmarshal([]byte(raw), &catalog)
	}
	if catalog.ResilienceSnapshots < 2 {
		writeError(w, 409, "peer_unsupported", "peer "+peerDisplay(peer)+" cannot return snapshots (it needs a newer tclaude); its operator can recover the agent there with tclaude agent recover --held")
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	rec := resilienceRecall{ID: proto.NewEnvelopeID(), Peer: peer.InstanceID, SourceAgent: req.SourceAgent, Offer: req.Offer, State: "requested", CreatedAt: now, ExpiresAt: now.Add(resilienceRecallTTL)}
	resilienceRecallMu.Lock()
	all, err := loadRecalls(now)
	if err == nil {
		active := 0
		for _, x := range all {
			if x.State == "requested" || x.State == "offered" || x.State == "ready" || x.State == "recovering" {
				active++
			}
		}
		if active >= resilienceMaxRecalls {
			err = &boardHTTPError{429, "recall_limit", "too many open recalls"}
		} else {
			all = append(all, rec)
			err = saveRecalls(all)
		}
	}
	resilienceRecallMu.Unlock()
	if err == nil {
		payload := proto.ResilienceRecallPayload{Recall: rec.ID, SourceAgent: rec.SourceAgent, Offer: rec.Offer}
		_, err = queueFederatedEnvelope(fedOutgoing{envelopeID: rec.ID, peer: peer, kind: proto.KindResilienceRecall, toLabel: peerDisplay(peer), subject: "resilience recall " + rec.SourceAgent, preview: rec.ID, ttl: resilienceRecallTTL, payload: payload})
	}
	if err != nil {
		var be *boardHTTPError
		if errors.As(err, &be) {
			writeError(w, be.Status, be.Code, be.Message)
		} else {
			writeError(w, 500, "recall", err.Error())
		}
		return
	}
	recordFederationAudit("resilience.recall", peerDisplay(peer), "", "", "agent="+rec.SourceAgent+" recall="+rec.ID, 200)
	writeJSON(w, 200, rec)
}

// handleResilienceRecalls lists this node's recalls.
//
//	GET /v1/resilience/recalls
func handleResilienceRecalls(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read resilience recalls") {
		return
	}
	resilienceRecallMu.Lock()
	all, err := loadRecalls(time.Now())
	resilienceRecallMu.Unlock()
	if err != nil {
		writeError(w, 500, "recall", err.Error())
		return
	}
	if all == nil {
		all = []resilienceRecall{}
	}
	slices.SortFunc(all, func(a, b resilienceRecall) int { return b.CreatedAt.Compare(a.CreatedAt) })
	writeJSON(w, 200, map[string]any{"recalls": all})
}

// handleResilienceRecallRecover previews or recovers a returned snapshot
// as a new agent.
//
//	POST /v1/resilience/recalls/{id}/preview {group, name?, cwd?, …}
//	POST /v1/resilience/recalls/{id}/import  {…, preview_token}
func handleResilienceRecallRecover(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "recover an agent from a recalled resilience snapshot") {
			return
		}
		if err := resilienceRecallRecover(w, r, op); err != nil {
			var be *boardHTTPError
			if errors.As(err, &be) {
				writeError(w, be.Status, be.Code, be.Message)
			} else {
				writeError(w, 500, "recover", err.Error())
			}
		}
	}
}

func resilienceRecallRecover(w http.ResponseWriter, r *http.Request, op string) error {
	id := r.PathValue("id")
	if !proto.ValidStreamID(id) {
		return &boardHTTPError{400, "recall", "invalid recall id"}
	}
	var in recoverInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		return &boardHTTPError{400, "json", err.Error()}
	}
	if in.Group == "" {
		return &boardHTTPError{400, "group", "group is required: the receiving group"}
	}
	if in.NewID && in.ForceKeepID {
		return &boardHTTPError{400, "identity", "choose one of new_id and force_keep_id"}
	}
	// An import claims the recall, so two previews cannot land two agents.
	rec, err := updateRecall(id, func(x *resilienceRecall) error {
		if x.State != "ready" {
			return &boardHTTPError{409, "recall_state", "recall is " + x.State + "; a returned snapshot is recovered once, after it arrives"}
		}
		if op == "import" {
			x.State, x.ClaimedAt = "recovering", time.Now().UTC()
		}
		return nil
	})
	if err != nil {
		return err
	}
	landed := false
	if op == "import" {
		defer func() {
			_, _ = updateRecall(id, func(x *resilienceRecall) error {
				if x.State == "recovering" {
					x.State = "ready"
					if landed {
						x.State = "recovered"
						_ = os.Remove(resilienceReturnedPath(id))
					}
				}
				return nil
			})
		}()
	}
	self, err := federationIdentity()
	if err != nil {
		return err
	}
	plain, size, err := decryptSealedSnapshot(resilienceReturnedPath(id))
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close(); _ = os.Remove(plain.Name()) }()
	arch, err := paf.Open(plain, size, paf.Options{Limits: paf.DefaultLimits()})
	if err != nil {
		return &boardHTTPError{422, "archive", "the returned snapshot is not a PAF this node can import: " + err.Error()}
	}
	if len(arch.Manifest.Agents) != 1 {
		return &boardHTTPError{422, "archive", "a resilience snapshot holds exactly one agent"}
	}
	// The custodian's word is not trusted: the snapshot must be this
	// node's own, of the agent recalled.
	if !signedByInstance(arch.Signatures, self.ID()) {
		return &boardHTTPError{422, "provenance", "the returned snapshot is not signed by this node"}
	}
	ag, ok := arch.Agent(arch.Manifest.Agents[0])
	if !ok || (ag.ID != "" && ag.ID != rec.SourceAgent) {
		return &boardHTTPError{422, "provenance", "the returned snapshot is not of agent " + rec.SourceAgent}
	}
	// When it was taken comes from the signed snapshot, not the custodian.
	if ag.Snapshot == nil || ag.Snapshot.TakenAt.IsZero() {
		return &boardHTTPError{422, "provenance", "the returned snapshot records no time it was taken"}
	}
	rec.TakenAt = ag.Snapshot.TakenAt
	summary, err := privacy.SummarizeForConsent(arch, pafContextRecordLimit())
	if err != nil {
		return &boardHTTPError{422, "archive", err.Error()}
	}
	from := rec.Peer
	if p, err := db.GetFederationPeer(rec.Peer); err == nil && p != nil {
		from = peerDisplay(p)
	}
	keep, conflicts := recoveryIdentityFor(rec.SourceAgent, in, newerPolicySnapshot(rec.SourceAgent, rec.TakenAt))
	snapshot := map[string]any{"recall": rec.ID, "peer": rec.Peer, "offer": rec.HeldOffer, "source_agent": rec.SourceAgent, "taken_at": rec.TakenAt, "summary": summary}
	if askRecoveryIdentity(w, rec.SourceAgent, in, conflicts, snapshot) {
		return nil
	}
	note := fmt.Sprintf("Recovered from this node's own resilience snapshot of agent %s, returned by %s, taken %s. ", rec.SourceAgent, from, rec.TakenAt.UTC().Format(time.RFC3339))
	if keep != nil {
		note += "You keep your agent id."
	} else {
		note += "You are a new agent with a new identity; the original agent's identity is not kept."
	}
	// Signed by this node: its own history, not received context.
	arrival := &arrivalContext{Operation: "resilience recover", Peer: rec.Peer, Reason: "--cwd or recorded path", Note: note}
	key := struct{ Recall, SHA256 string }{rec.ID, rec.SHA256}
	landed, err = landSnapshotAsAgent(w, r, op, plain, size, in.boardSnapshotImportInput, key, keep, summary, arrival, snapshot)
	if err == nil && landed {
		recordFederationAudit("resilience.recover", rec.Peer, "", "", "agent="+rec.SourceAgent+" recall="+rec.ID, 200)
	}
	return err
}

// newerPolicySnapshot reports a snapshot of agentID this node's policies
// sent after takenAt: keeping the id of an older one is the operator's call.
func newerPolicySnapshot(agentID string, takenAt time.Time) []string {
	resilienceMu.Lock()
	f, err := loadResilience()
	resilienceMu.Unlock()
	if err != nil {
		return nil
	}
	for _, states := range f.State {
		if s := states[agentID]; s != nil && s.LastGood.Truncate(time.Second).After(takenAt) {
			return []string{fmt.Sprintf("this node sent a newer snapshot of agent %s (taken %s)", agentID, s.LastGood.UTC().Format(time.RFC3339))}
		}
	}
	return nil
}

// decryptSealedSnapshot decrypts an age-sealed snapshot with this node's
// key into a private temporary file.
func decryptSealedSnapshot(path string) (*os.File, int64, error) {
	sealed, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, &boardHTTPError{404, "snapshot", "the snapshot is gone"}
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = sealed.Close() }()
	id, err := federationIdentity()
	if err != nil {
		return nil, 0, err
	}
	ageID, err := seal.Identity(id.Priv)
	if err != nil {
		return nil, 0, err
	}
	plain, err := seal.Decrypt(sealed, ageID)
	if err != nil {
		return nil, 0, &boardHTTPError{403, "not_recipient", "this node cannot decrypt the snapshot: " + err.Error()}
	}
	dir := filepath.Join(config.DataDir(), "resilience", "recover")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	sweepRecoverPlaintext(dir, time.Now())
	out, err := os.CreateTemp(dir, ".plain-")
	if err != nil {
		return nil, 0, err
	}
	limits := paf.DefaultLimits()
	n, err := io.Copy(out, io.LimitReader(plain, limits.MaxArchiveBytes+1))
	if err == nil && n > limits.MaxArchiveBytes {
		err = &boardHTTPError{413, "too_large", "the decrypted snapshot exceeds the PAF size limit"}
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return nil, 0, err
	}
	return out, n, nil
}

// isResilienceType reports bundle types the generic offer routes refuse.
func isResilienceType(t string) bool {
	return t == bundletransfer.Snapshot.Name || t == bundletransfer.SnapshotReturn.Name
}
