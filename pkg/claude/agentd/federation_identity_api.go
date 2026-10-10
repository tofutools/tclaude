package agentd

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type fedIdentityAction struct {
	Old         string `json:"old,omitempty"`
	New         string `json:"new,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Apply       bool   `json:"apply"`
}

// Fingerprints are display metadata; the embedded signed rotation is unchanged.
type fedIdentityRotationJSON struct {
	proto.Rotation
	OldFingerprint string `json:"old_fingerprint"`
	NewFingerprint string `json:"new_fingerprint"`
}

func identityRotationJSON(r proto.Rotation) fedIdentityRotationJSON {
	return fedIdentityRotationJSON{r, proto.Fingerprint(r.OldKey), proto.Fingerprint(r.NewKey)}
}

func readIdentityAction(w http.ResponseWriter, r *http.Request) (*fedIdentityAction, bool) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method", "POST only")
		return nil, false
	}
	if !requireHuman(w, r, "manage federation identity") {
		return nil, false
	}
	var req fedIdentityAction
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return nil, false
	}
	return &req, true
}
func handleFederationIdentityRotate(w http.ResponseWriter, r *http.Request) {
	req, ok := readIdentityAction(w, r)
	if !ok {
		return
	}
	old, err := federationIdentity()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if !req.Apply {
		journal, err := loadIdentityJournal()
		if err != nil {
			writeFedErr(w, err)
			return
		}
		// Preview never generates or stages a successor key. The dashboard can
		// explain the consequences before the operator confirms the apply.
		writeJSON(w, 200, map[string]any{
			"instance_id": old.ID(), "fingerprint": proto.Fingerprint(old.Pub),
			"window_seconds": rotationWindow().Seconds(), "hop_count": len(journal.Chain),
			"hop_limit": proto.MaxRotationHops, "pending": journal.Pending,
			"effects": map[string]bool{"successor_linked": true, "streams_reconnect": true,
				"pending_sealed_mail_requires_resend": true, "issued_model_credentials_revoked": true,
				"requester_paid_leases_revoked": true},
			"effect": "creates linked successor; closes sealed sessions/routes and revokes model capabilities; pending encrypted mail requires resending",
			"apply":  "federation identity rotate --apply",
		})
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 409, "offline", "connect federation before initiating rotation so hubs and peers can observe the transition")
		return
	}
	if rt.cl.Status().IdentityRotationVersion != 1 {
		writeError(w, 409, "hub_upgrade_required", "connect to a hub supporting identity rotation before applying; upgrade the hub or explicitly recover/re-pair instead")
		return
	}
	statement, err := prepareLocalIdentityRotation(time.Now())
	if err != nil {
		writeError(w, 409, "rotation", err.Error())
		return
	}
	rt = currentFederation()
	if rt != nil {
		peers, _ := db.ListFederationPeers()
		for _, p := range peers {
			rt.sendControl(p.InstanceID, proto.KindIdentityRotation, "", statement)
		}
	}
	if err = reloadFederation(); err != nil {
		writeError(w, 503, "rotation_pending", "rotation is staged; reconnect federation to publish it: "+err.Error())
		return
	}
	writeJSON(w, 200, identityRotationJSON(statement))
}
func handleFederationIdentityRotations(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read identity rotation state") {
		return
	}
	rows, err := db.ListFederationIdentityRotations()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	local, err := loadIdentityJournal()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	var chain []fedIdentityRotationJSON
	for _, rotation := range local.Chain {
		chain = append(chain, identityRotationJSON(rotation))
	}
	writeJSON(w, 200, map[string]any{"peers": rows, "local": struct {
		Chain   []fedIdentityRotationJSON `json:"chain"`
		Pending bool                      `json:"pending"`
	}{chain, local.Pending}})
}
func handleFederationIdentityRecover(w http.ResponseWriter, r *http.Request) {
	req, ok := readIdentityAction(w, r)
	if !ok {
		return
	}
	old, err := resolveFederationPeer(req.Old)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if !proto.ValidInstanceID(req.New) || req.New == old.InstanceID {
		writeError(w, 400, "invalid_arg", "replacement must be a different immutable instance ID")
		return
	}
	rt := currentFederation()
	if rt == nil {
		writeError(w, 409, "offline", "connect the replacement to the hub and admit it before recovering trust")
		return
	}
	key, _ := rt.cl.LookupKey(req.New)
	if len(key) != 32 || proto.InstanceID(key) != req.New {
		writeError(w, 404, "unknown_identity", "replacement key is not available in the hub directory; admit the replacement first")
		return
	}
	grants, err := db.ListFederationPeerGrants(old.InstanceID)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	pools, err := db.ListFederationNodeGroups()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	memberships := []string{}
	for _, p := range pools {
		yes, e := db.FederationNodeGroupContainsID(p.ID, old.InstanceID)
		if e != nil {
			writeFedErr(w, e)
			return
		}
		if yes {
			memberships = append(memberships, p.Name)
		}
	}
	assignment, err := db.GetFederationNodeProfileAssignment(old.InstanceID)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	profile := map[string]any{}
	if assignment != nil {
		profile = map[string]any{"id": assignment.Profile.ID, "name": assignment.Profile.Name, "revision": assignment.Profile.Revision}
	}
	preview := map[string]any{"old": old.InstanceID, "new": req.New, "old_fingerprint": proto.Fingerprint(old.PubKey), "new_fingerprint": proto.Fingerprint(key), "label": old.Label, "trust_level": old.TrustLevel, "grants": grants, "pools": memberships, "profile": profile, "rules": db.FederationIdentityColumns, "warning": "This explicitly transfers the old peer's authority to the replacement. Unrestricted trust permits every exported capability. History and consumed enrollment records remain under the old identity; model capabilities are revoked."}
	if !req.Apply {
		writeJSON(w, 200, preview)
		return
	}
	if req.Fingerprint != proto.Fingerprint(key) {
		writeError(w, 400, "fingerprint_required", "confirm the replacement fingerprint with --fingerprint "+proto.Fingerprint(key))
		return
	}
	finish := lockAwayAuthorityMutation(old.InstanceID)
	teleportLeaseMu.Lock()
	err = db.RebindFederationIdentity(old.InstanceID, req.New, key, time.Now())
	if err == nil {
		rt.teleportLeases = teleportLeaseObservations{}
	}
	teleportLeaseMu.Unlock()
	finish()
	if err != nil {
		writeError(w, 409, "recovery", err.Error())
		return
	}
	recordFederationAudit("federation.identity.recovered", req.New, "", "", "old="+old.InstanceID, 200)
	preview["applied"] = true
	writeJSON(w, 200, preview)
	_ = reloadFederation()
}
func handleFederationIdentityRevoke(w http.ResponseWriter, r *http.Request) {
	req, ok := readIdentityAction(w, r)
	if !ok {
		return
	}
	instance := req.Old
	if p, e := resolveFederationPeer(instance); e == nil {
		instance = p.InstanceID
	}
	if !proto.ValidInstanceID(instance) {
		writeError(w, 400, "invalid_arg", "use an immutable instance ID or trusted peer label")
		return
	}
	if !req.Apply {
		writeJSON(w, 200, map[string]any{"instance": instance, "effect": "block predecessor rotation/replay, revoke its remaining trust and model capabilities; an already accepted successor remains current", "apply": "federation identity revoke-old " + instance + " --apply"})
		return
	}
	finish := lockAwayAuthorityMutation(instance)
	teleportLeaseMu.Lock()
	err := db.RevokeFederationIdentity(instance, time.Now())
	teleportLeaseMu.Unlock()
	finish()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	recordFederationAudit("federation.identity.revoked", instance, "", "", "operator revoked old identity", 200)
	writeJSON(w, 200, map[string]any{"revoked": instance})
	_ = reloadFederation()
}

func handleFederationIdentityRecoverLocal(w http.ResponseWriter, r *http.Request) {
	req, ok := readIdentityAction(w, r)
	if !ok {
		return
	}
	var previous any
	if raw, err := os.ReadFile(identityReceiptPath()); err == nil {
		_ = json.Unmarshal(raw, &previous)
	}
	if !req.Apply {
		writeJSON(w, 200, map[string]any{"previous": previous, "effect": "create a fresh unlinked identity; preserve local peer grants and records; peers and hub must explicitly recover/rebind the old identity; existing model capabilities and queued encrypted mail are retired; any pending signed rotation is abandoned and its public journal archived", "apply": "federation identity recover-local --apply"})
		return
	}
	next, err := recoverLocalIdentity()
	if err != nil {
		writeError(w, 409, "identity_recovery", err.Error())
		return
	}
	recordFederationAudit("federation.identity.local_recovery", next.ID(), "", "", "operator created an unlinked replacement; re-pair required", 200)
	writeJSON(w, 200, map[string]any{"instance_id": next.ID(), "fingerprint": proto.Fingerprint(next.Pub), "previous": previous, "re_pair_required": true})
	_ = reloadFederation()
}
