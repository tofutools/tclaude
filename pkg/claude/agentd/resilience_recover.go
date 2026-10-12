package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

// handleResilienceRecover previews or recovers a held snapshot as a new
// agent on this node, for when the peer that sent it is gone for good.
//
//	POST /v1/resilience/held/{peer}/{offer}/preview {group, name?, cwd?, …}
//	POST /v1/resilience/held/{peer}/{offer}/import  {…, preview_token}
//
// The snapshot is sealed to this node's key as well as its sender's, so it
// is decrypted here, checked to be signed by the sending instance, and
// lands like a board snapshot import: a new identity, the receiver's
// profile and policy, no permissions from the source, and an arrival
// briefing that marks the history as received. Possession of a snapshot
// never carries the original agent's identity. Human-only.
func handleResilienceRecover(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "recover an agent from a held resilience snapshot") {
			return
		}
		if err := resilienceRecover(w, r, op); err != nil {
			var be *boardHTTPError
			if errors.As(err, &be) {
				writeError(w, be.Status, be.Code, be.Message)
			} else {
				writeError(w, 500, "recover", err.Error())
			}
		}
	}
}

func resilienceRecover(w http.ResponseWriter, r *http.Request, op string) error {
	peer, ok := resolveHeldPeer(r.PathValue("peer"))
	offer := r.PathValue("offer")
	if !ok || !proto.ValidStreamID(offer) {
		return &boardHTTPError{400, "held", "name a held snapshot: the peer (instance id or label) and the offer id from tclaude agent snapshot held"}
	}
	var in boardSnapshotImportInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		return &boardHTTPError{400, "json", err.Error()}
	}
	if in.Group == "" {
		return &boardHTTPError{400, "group", "group is required: the receiving group"}
	}
	plain, size, h, err := openHeldSnapshot(peer, offer)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close(); _ = os.Remove(plain.Name()) }()
	arch, err := paf.Open(plain, size, paf.Options{Limits: paf.DefaultLimits()})
	if err != nil {
		return &boardHTTPError{422, "archive", "the held snapshot is not a PAF this node can import: " + err.Error()}
	}
	if len(arch.Manifest.Agents) != 1 {
		return &boardHTTPError{422, "archive", "a resilience snapshot holds exactly one agent"}
	}
	if !signedByInstance(arch.Signatures, h.Peer) {
		return &boardHTTPError{422, "provenance", "the held snapshot is not signed by the instance that sent it"}
	}
	summary, err := privacy.SummarizeForConsent(arch, pafContextRecordLimit())
	if err != nil {
		return &boardHTTPError{422, "archive", err.Error()}
	}
	from := h.Peer
	if p, err := resolveFederationPeerOpt(h.Peer, false); err == nil && p != nil {
		from = peerDisplay(p)
	}
	arrival := &arrivalContext{Operation: "resilience recover", Peer: h.Peer, Reason: "--cwd or recorded path", UntrustedHistory: true,
		Note: fmt.Sprintf("Recovered from a resilience snapshot of agent %s that %s sent to this node, taken %s. You are a new agent with a new identity; the original agent's identity is not restored.", h.SourceAgent, from, h.TakenAt.UTC().Format(time.RFC3339))}
	key := struct{ Peer, Offer, SHA256 string }{h.Peer, h.Offer, h.SHA256}
	snapshot := map[string]any{"peer": h.Peer, "offer": h.Offer, "source_agent": h.SourceAgent, "agent_name": h.AgentName, "taken_at": h.TakenAt, "identity": "new", "summary": summary}
	landed, err := landSnapshotAsNewAgent(w, r, op, plain, size, in, key, summary, arrival, snapshot)
	if err == nil && landed {
		recordFederationAudit("resilience.recover", h.Peer, "", "", "agent="+h.SourceAgent+" offer="+h.Offer, 200)
	}
	return err
}

// resolveHeldPeer maps an instance id or a trusted peer's label to the
// instance id snapshots are held under. A peer that is no longer trusted
// is named by its instance id.
func resolveHeldPeer(ref string) (string, bool) {
	if proto.ValidInstanceID(ref) {
		return ref, true
	}
	if p, err := resolveFederationPeerOpt(ref, false); err == nil && p != nil {
		return p.InstanceID, true
	}
	return "", false
}

// signedByInstance reports whether a valid signature on the archive is by
// the instance key behind id.
func signedByInstance(results []paf.SignatureResult, id string) bool {
	for _, s := range results {
		if s.Status == paf.SignatureValid && len(s.Signature.PublicKey) > 0 && proto.InstanceID(s.Signature.PublicKey) == id {
			return true
		}
	}
	return false
}

// openHeldSnapshot decrypts a held snapshot with this node's key into a
// private temporary file.
func openHeldSnapshot(peer, offer string) (*os.File, int64, heldSnapshot, error) {
	var h heldSnapshot
	resilienceHeldMu.Lock()
	found := false
	for _, x := range listHeld(peer) {
		if x.Offer == offer {
			h, found = x, true
			break
		}
	}
	var sealed *os.File
	var err error
	if found {
		sealed, err = os.Open(resilienceHeldPath(h.Peer, h.SourceAgent, h.Offer))
	}
	resilienceHeldMu.Unlock()
	if !found || errors.Is(err, os.ErrNotExist) {
		return nil, 0, h, &boardHTTPError{404, "held", "no held snapshot " + offer + " from " + peer}
	}
	if err != nil {
		return nil, 0, h, err
	}
	defer func() { _ = sealed.Close() }()
	if !time.Now().Before(h.ExpiresAt) {
		return nil, 0, h, &boardHTTPError{410, "expired", "this held snapshot has expired"}
	}
	id, err := federationIdentity()
	if err != nil {
		return nil, 0, h, err
	}
	ageID, err := seal.Identity(id.Priv)
	if err != nil {
		return nil, 0, h, err
	}
	plain, err := seal.Decrypt(sealed, ageID)
	if err != nil {
		return nil, 0, h, &boardHTTPError{403, "not_recipient", "this node cannot decrypt the held snapshot: " + err.Error()}
	}
	dir := filepath.Join(config.DataDir(), "resilience", "recover")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, h, err
	}
	out, err := os.CreateTemp(dir, ".plain-")
	if err != nil {
		return nil, 0, h, err
	}
	limits := paf.DefaultLimits()
	n, err := io.Copy(out, io.LimitReader(plain, limits.MaxArchiveBytes+1))
	if err == nil && n > limits.MaxArchiveBytes {
		err = &boardHTTPError{413, "too_large", "the decrypted snapshot exceeds the PAF size limit"}
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(out.Name())
		return nil, 0, h, err
	}
	return out, n, h, nil
}
