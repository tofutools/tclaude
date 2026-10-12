package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	if ag, ok := arch.Agent(arch.Manifest.Agents[0]); !ok || (ag.ID != "" && ag.ID != h.SourceAgent) {
		return &boardHTTPError{422, "provenance", "the held snapshot is not of agent " + h.SourceAgent}
	}
	summary, err := privacy.SummarizeForConsent(arch, pafContextRecordLimit())
	if err != nil {
		return &boardHTTPError{422, "archive", err.Error()}
	}
	from := h.Peer
	if p, err := resolveFederationPeerOpt(h.Peer, false); err == nil && p != nil {
		from = peerDisplay(p)
	}
	var newer []string
	for _, x := range listHeld("") {
		if x.SourceAgent == h.SourceAgent && x.TakenAt.After(h.TakenAt) {
			newer = append(newer, fmt.Sprintf("a newer snapshot of agent %s is held here (taken %s, offer %s)", h.SourceAgent, x.TakenAt.UTC().Format(time.RFC3339), x.Offer))
			break
		}
	}
	keep, conflicts := recoveryIdentityFor(h.SourceAgent, in, newer)
	snapshot := map[string]any{"peer": h.Peer, "offer": h.Offer, "source_agent": h.SourceAgent, "agent_name": h.AgentName, "taken_at": h.TakenAt, "summary": summary}
	if askRecoveryIdentity(w, h.SourceAgent, in, conflicts, snapshot) {
		return nil
	}
	note := fmt.Sprintf("Recovered from a resilience snapshot of agent %s that %s sent to this node, taken %s. ", h.SourceAgent, from, h.TakenAt.UTC().Format(time.RFC3339))
	if keep != nil {
		note += "You keep your agent id; this node is now your home."
	} else {
		note += "You are a new agent with a new identity; the original agent's identity is not kept."
	}
	arrival := &arrivalContext{Operation: "resilience recover", Peer: h.Peer, Reason: "--cwd or recorded path", UntrustedHistory: true, Note: note}
	key := struct{ Peer, Offer, SHA256 string }{h.Peer, h.Offer, h.SHA256}
	landed, err := landSnapshotAsAgent(w, r, op, plain, size, in.boardSnapshotImportInput, key, keep, summary, arrival, snapshot)
	if err == nil && landed {
		recordFederationAudit("resilience.recover", h.Peer, "", "", "agent="+h.SourceAgent+" offer="+h.Offer, 200)
	}
	return err
}

// recoverInput is a recovery's landing options and its identity choice.
// By default a recovery keeps the agent id, as a teleport arrival does.
type recoverInput struct {
	boardSnapshotImportInput
	// NewID recovers as a new agent with a fresh id.
	NewID bool `json:"new_id,omitempty"`
	// ForceKeepID keeps the id despite the conflicts a preview showed.
	ForceKeepID bool `json:"force_keep_id,omitempty"`
}

// recoveryIdentity is the agent id a recovery keeps.
type recoveryIdentity struct {
	Agent string
	Force bool
}

// recoveryIdentityFor decides whether a recovery keeps agentID and lists
// what speaks against it on this node: a local record of the agent (a
// running one is never taken over), presence records placing it
// elsewhere, and newer snapshots the caller found.
func recoveryIdentityFor(agentID string, in recoverInput, newer []string) (*recoveryIdentity, []string) {
	if in.NewID {
		return nil, nil
	}
	conflicts := append([]string{}, newer...)
	if a, err := db.GetAgent(agentID); err == nil && a != nil {
		switch {
		case !a.Active():
			conflicts = append(conflicts, "agent "+agentID+" was retired on this node")
		case agentHasLiveSession(a):
			conflicts = append(conflicts, "agent "+agentID+" is running on this node; stop it first, or recover with a new id")
		default:
			conflicts = append(conflicts, "agent "+agentID+" exists on this node; keeping the id makes the recovered conversation its current one")
		}
	}
	if p, err := db.GetAgentFederationPresence(agentID); err == nil && p != nil {
		switch {
		case p.State == "away":
			conflicts = append(conflicts, fmt.Sprintf("this node's records say agent %s is away at %s", agentID, p.CurrentInstance))
		case p.State == "reserved":
			conflicts = append(conflicts, "agent "+agentID+" has a launch in progress on this node")
		case p.State == "terminal":
			conflicts = append(conflicts, "agent "+agentID+" was explicitly retired or deleted on this node")
		case p.HomeInstance != "" && p.HomeInstance != arrivalNode():
			conflicts = append(conflicts, fmt.Sprintf("this node's records say agent %s's home is %s", agentID, p.HomeInstance))
		}
	}
	return &recoveryIdentity{Agent: agentID, Force: in.ForceKeepID}, conflicts
}

// askRecoveryIdentity answers a recovery whose kept id conflicts with this
// node's records, unless the operator chose: it never refuses outright or
// silently forks. A running agent is never taken over, even with force.
func askRecoveryIdentity(w http.ResponseWriter, agentID string, in recoverInput, conflicts []string, snapshot map[string]any) bool {
	snapshot["identity"] = "keep"
	if in.NewID {
		snapshot["identity"] = "new"
	}
	if len(conflicts) == 0 {
		return false
	}
	snapshot["identity_conflicts"] = conflicts
	running := false
	for _, c := range conflicts {
		running = running || strings.Contains(c, "is running on this node")
	}
	if in.ForceKeepID && !running {
		return false
	}
	writeJSON(w, 409, map[string]any{"code": "identity_conflict", "agent": agentID, "conflicts": conflicts, "snapshot": snapshot,
		"error": "keeping agent id " + agentID + " conflicts with this node's records; recover with new_id, or with force_keep_id to keep it anyway"})
	return true
}

func agentHasLiveSession(a *db.Agent) bool {
	if a.CurrentConvID == "" {
		return false
	}
	rows, err := db.FindSessionsByConvID(a.CurrentConvID)
	if err != nil {
		return true
	}
	for _, s := range rows {
		if s.TmuxSession != "" && session.IsTmuxSessionAlive(s.TmuxSession) {
			return true
		}
	}
	return false
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
	sweepRecoverPlaintext(dir, time.Now())
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

// sweepRecoverPlaintext removes decrypted copies a killed daemon left
// behind; a live preview or import holds its copy for minutes at most.
func sweepRecoverPlaintext(dir string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), ".plain-") && now.Sub(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
