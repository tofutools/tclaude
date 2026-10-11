package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FederationIdentity travels only inside the authenticated, scanned bundle.
// Proofs are private continuation credentials for hosts visited by this actor;
// they never appear in dashboard projections, receipts or audit details.
type FederationIdentity struct {
	Mail   bool              `json:"mail,omitempty"`
	Agent  string            `json:"agent"`
	Home   string            `json:"home"`
	Hops   int               `json:"hops"`
	Proofs map[string]string `json:"proofs"`
}
type AgentFederationPresence struct {
	AgentID               string             `json:"agent_id"`
	HomeInstance          string             `json:"home_instance"`
	State                 string             `json:"state"`
	CurrentInstance       string             `json:"current_instance"`
	CurrentPeer           string             `json:"current_peer,omitempty"`
	PredecessorInstance   string             `json:"predecessor_instance,omitempty"`
	DepartureOffer        string             `json:"departure_offer,omitempty"`
	ArrivalOffer          string             `json:"arrival_offer,omitempty"`
	HopCount              int                `json:"hop_count"`
	VisitEpoch            int                `json:"visit_epoch"`
	IndependentClone      string             `json:"independent_clone,omitempty"`
	ArrivalRollbackJSON   string             `json:"-"`
	ContinuationNonceHash string             `json:"-"`
	Transfer              FederationIdentity `json:"-"`
	DepartedAt            time.Time          `json:"departed_at,omitempty"`
	ArrivedAt             time.Time          `json:"arrived_at,omitempty"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

func continuationHash(nonce string) string {
	h := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(h[:])
}

const federationPresenceColumns = `agent_id,home_instance,state,current_instance,current_peer,predecessor_instance,departure_offer,arrival_offer,hop_count,visit_epoch,independent_clone,arrival_rollback_json,continuation_nonce_hash,transfer_json,departed_at,arrived_at,updated_at`

func scanFederationPresence(row rowScanner) (*AgentFederationPresence, error) {
	var p AgentFederationPresence
	var raw string
	var departed, arrived, updated dbTimestamp
	err := row.Scan(&p.AgentID, &p.HomeInstance, &p.State, &p.CurrentInstance, &p.CurrentPeer, &p.PredecessorInstance, &p.DepartureOffer, &p.ArrivalOffer, &p.HopCount, &p.VisitEpoch, &p.IndependentClone, &p.ArrivalRollbackJSON, &p.ContinuationNonceHash, &raw, &departed, &arrived, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &p.Transfer); err != nil {
		return nil, err
	}
	p.DepartedAt = departed.Time()
	p.ArrivedAt = arrived.Time()
	p.UpdatedAt = updated.Time()
	return &p, nil
}
func GetAgentFederationPresence(id string) (*AgentFederationPresence, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanFederationPresence(d.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
}
func AgentAway(id string) bool {
	p, e := GetAgentFederationPresence(id)
	return e == nil && p != nil && p.State == "away"
}
func AgentConvAway(conv string) bool {
	id, e := AgentIDForConv(conv)
	return e == nil && id != "" && AgentAway(id)
}
func putFederationPresenceTx(tx *sql.Tx, p AgentFederationPresence) error {
	raw, e := json.Marshal(p.Transfer)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`INSERT INTO agent_federation_presence (`+federationPresenceColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_id) DO UPDATE SET home_instance=excluded.home_instance,state=excluded.state,current_instance=excluded.current_instance,current_peer=excluded.current_peer,predecessor_instance=excluded.predecessor_instance,departure_offer=excluded.departure_offer,arrival_offer=excluded.arrival_offer,hop_count=excluded.hop_count,visit_epoch=excluded.visit_epoch,independent_clone=excluded.independent_clone,arrival_rollback_json=excluded.arrival_rollback_json,continuation_nonce_hash=excluded.continuation_nonce_hash,transfer_json=excluded.transfer_json,departed_at=excluded.departed_at,arrived_at=excluded.arrived_at,updated_at=excluded.updated_at`, p.AgentID, p.HomeInstance, p.State, p.CurrentInstance, p.CurrentPeer, p.PredecessorInstance, p.DepartureOffer, p.ArrivalOffer, p.HopCount, p.VisitEpoch, p.IndependentClone, p.ArrivalRollbackJSON, p.ContinuationNonceHash, string(raw), nullableDBTime(p.DepartedAt), nullableDBTime(p.ArrivedAt), dbTime(time.Now()))
	return e
}

// ReserveFederationIdentity pins the actor before any launch. Existing local
// identities require the proof from their own most recent departure. A terminal
// record is permanent, even after the actor's conversations have been deleted.
func ReserveFederationIdentity(identity FederationIdentity, local, peer, offer string) (bool, error) {
	if identity.Agent == "" || identity.Home == "" || len(identity.Proofs) > 256 || identity.Hops < 1 {
		return false, errors.New("invalid stable identity continuation")
	}
	d, e := Open()
	if e != nil {
		return false, e
	}
	tx, e := d.Begin()
	if e != nil {
		return false, e
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the SQLite writer lock before testing the reservation/collision.
	if _, e = tx.Exec(`UPDATE agent_federation_presence SET updated_at=updated_at WHERE agent_id=?`, identity.Agent); e != nil {
		return false, e
	}
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, identity.Agent))
	if e != nil {
		return false, e
	}
	returning := false
	if p != nil {
		if p.State == "reserved" && p.ArrivalOffer == offer && p.PredecessorInstance == peer {
			return p.HomeInstance == local, tx.Commit()
		}
		if p.State == "terminal" {
			return false, errors.New("agent was explicitly retired or deleted on this node; return refused")
		}
		nonce := identity.Proofs[local]
		if p.State != "away" || p.HomeInstance != identity.Home || nonce == "" || p.ContinuationNonceHash != continuationHash(nonce) {
			return false, errors.New("agent identity is live, unrelated, or continuation is no longer valid")
		}
		returning = p.HomeInstance == local
		var previousConv string
		if e = tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id=?`, identity.Agent).Scan(&previousConv); e != nil {
			return false, e
		}
		rollback, err := json.Marshal(federationArrivalRollback{Presence: *p, Transfer: p.Transfer, NonceHash: p.ContinuationNonceHash, Conv: previousConv})
		if err != nil {
			return false, err
		}
		p.ArrivalRollbackJSON = string(rollback)
	} else {
		var n int
		if e = tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE agent_id=?`, identity.Agent).Scan(&n); e != nil {
			return false, e
		}
		if n != 0 {
			return false, errors.New("agent identity belongs to an unrelated local record")
		}
		p = &AgentFederationPresence{AgentID: identity.Agent, HomeInstance: identity.Home}
	}
	p.CurrentInstance = local
	p.State = "reserved"
	p.PredecessorInstance = peer
	p.ArrivalOffer = offer
	p.HopCount = identity.Hops
	p.Transfer = identity
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return false, e
	}
	return returning, tx.Commit()
}

// ReleaseFederationIdentity only releases an undispatched reservation. A bound
// generation is retained for inspection, as with bundle import reservations.
func ReleaseFederationIdentity(id, offer string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return e
	}
	if p == nil || p.State != "reserved" || p.ArrivalOffer != offer {
		return nil
	}
	if p.ArrivalRollbackJSON != "" {
		var old federationArrivalRollback
		if e = json.Unmarshal([]byte(p.ArrivalRollbackJSON), &old); e != nil {
			return e
		}
		old.Presence.ContinuationNonceHash = old.NonceHash
		old.Presence.Transfer = old.Transfer
		if e = putFederationPresenceTx(tx, old.Presence); e != nil {
			return e
		}
	} else if _, e = tx.Exec(`DELETE FROM agent_federation_presence WHERE agent_id=? AND state='reserved' AND arrival_offer=?`, id, offer); e != nil {
		return e
	}
	return tx.Commit()
}

// DepartFederationIdentity makes the pinned source inactive. Home state and
// explicitly retained paused-backup state stay dormant; ordinary visiting
// departures revoke their local authority before calling this transition.
func DepartFederationIdentity(conv, local, peer, offer string, identity FederationIdentity) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if e = departFederationIdentityTx(tx, conv, local, peer, offer, identity); e != nil {
		return e
	}
	return tx.Commit()
}

func departFederationIdentityTx(tx *sql.Tx, conv, local, peer, offer string, identity FederationIdentity) error {
	var e error
	var id, current string
	var retired sql.NullString
	e = tx.QueryRow(`SELECT a.agent_id,a.current_conv_id,a.retired_at FROM agents a JOIN agent_conversations c ON c.agent_id=a.agent_id WHERE c.conv_id=?`, conv).Scan(&id, &current, &retired)
	if e != nil {
		return e
	}
	if id != identity.Agent || current != conv {
		return errors.New("departure generation changed")
	}
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return e
	}
	if p != nil && p.State == "terminal" {
		return errors.New("agent was explicitly retired or deleted")
	}
	if p != nil && p.State == "away" && p.DepartureOffer == offer {
		return nil
	}
	if identity.Home == local && retired.Valid {
		return errors.New("home agent is no longer active")
	}
	if identity.Proofs[local] == "" {
		return errors.New("departure continuation missing")
	}
	if !retired.Valid {
		if _, e = tx.Exec(`UPDATE agents SET retired_at=?,retired_by='system:federation-away',retire_reason=? WHERE agent_id=? AND current_conv_id=? AND retired_at IS NULL`, dbTime(time.Now()), "away on "+peer, id, conv); e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE agent_sudo_grants SET revoked_at=? WHERE agent_id=? AND revoked_at IS NULL`, dbTime(time.Now()), id); e != nil {
			return e
		}
	}
	epoch := 0
	if p != nil {
		epoch = p.VisitEpoch
	}
	p = &AgentFederationPresence{AgentID: id, HomeInstance: identity.Home, State: "away", CurrentInstance: peer, CurrentPeer: peer, DepartureOffer: offer, HopCount: identity.Hops, VisitEpoch: epoch, ContinuationNonceHash: continuationHash(identity.Proofs[local]), Transfer: identity, DepartedAt: time.Now()}
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return e
	}
	return nil
}

func terminalFederationPresenceTx(tx *sql.Tx, id string) error {
	_, e := tx.Exec(`UPDATE agent_federation_presence SET state='terminal',continuation_nonce_hash='',updated_at=? WHERE agent_id=?`, dbTime(time.Now()), id)
	return e
}

// bindFederationArrivalTx is only reachable through a reserved stable ID.
func bindFederationArrivalTx(tx *sql.Tx, id, conv, via string) (bool, error) {
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return false, e
	}
	if p == nil || p.State != "reserved" {
		return false, nil
	}
	if _, e = tx.Exec(`UPDATE agent_conversations SET role=? WHERE agent_id=?`, ConvRoleGeneration, id); e != nil {
		return false, e
	}
	if e = linkConvTx(tx, conv, id, ConvRoleHead, via, time.Now()); e != nil {
		return false, e
	}
	_, e = tx.Exec(`UPDATE agents SET current_conv_id=?,retired_at=NULL,retired_by='',retire_reason='',retired_by_agent='' WHERE agent_id=?`, conv, id)
	if e != nil {
		return false, e
	}
	p.State = "here"
	// Keep the local instance set at reservation.
	p.CurrentPeer = ""
	p.ContinuationNonceHash = ""
	p.VisitEpoch++
	p.ArrivedAt = time.Now()
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return false, e
	}
	return true, nil
}
func (p *AgentFederationPresence) String() string {
	return fmt.Sprintf("%s %s home=%s current=%s", p.AgentID, p.State, p.HomeInstance, p.CurrentInstance)
}

// ValidateFederationArrival guards deferred dispatch against explicit retirement
// or deletion after reservation/enrollment.
func ValidateFederationArrival(id, offer string) error {
	p, e := GetAgentFederationPresence(id)
	if e != nil {
		return e
	}
	if p == nil || p.ArrivalOffer != offer || (p.State != "reserved" && p.State != "here") {
		return errors.New("stable arrival was cancelled or explicitly retired")
	}
	return nil
}
func RestoreFederationBackup(id, offer, local string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return e
	}
	if p == nil {
		return nil
	}
	if p.State == "here" && p.DepartureOffer == offer {
		return tx.Commit()
	}
	if p.State != "away" || p.DepartureOffer != offer {
		return errors.New("backup was retired, deleted, or superseded")
	}
	if _, e = tx.Exec(`UPDATE agents SET retired_at=NULL,retired_by='',retire_reason='',retired_by_agent='' WHERE agent_id=?`, id); e != nil {
		return e
	}
	p.State = "here"
	p.CurrentInstance = local
	p.CurrentPeer = ""
	p.ContinuationNonceHash = ""
	p.ArrivalOffer = ""
	p.VisitEpoch++
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return e
	}
	return tx.Commit()
}

// AcceptFederationBackupReturnProof retains the returning host's continuation
// before resuming the home backup. The authenticated lease pins host and offer.
func AcceptFederationBackupReturnProof(id, offer, peer, proof string) error {
	if proof == "" || len(proof) > 128 {
		return errors.New("return continuation missing")
	}
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return e
	}
	if p == nil || (p.State != "away" && p.State != "here") || p.DepartureOffer != offer || (p.State == "away" && p.CurrentPeer != peer) {
		return errors.New("backup return no longer current")
	}
	if p.Transfer.Proofs == nil {
		p.Transfer.Proofs = map[string]string{}
	}
	p.Transfer.Proofs[peer] = proof
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return e
	}
	return tx.Commit()
}

// The pre-launch rollback is private durable state, not an import credential.
type federationArrivalRollback struct {
	Presence  AgentFederationPresence
	Transfer  FederationIdentity
	NonceHash string
	Conv      string
}

// RollbackFederationArrival restores a pre-existing away actor after a proven
// undispatched launch failure. It must run before generic birth cleanup, which
// would revoke the home's standing grants and membership.
func RollbackFederationArrival(conv string) (bool, error) {
	d, e := Open()
	if e != nil {
		return false, e
	}
	tx, e := d.Begin()
	if e != nil {
		return false, e
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	if e = tx.QueryRow(`SELECT agent_id FROM agents WHERE current_conv_id=?`, conv).Scan(&id); errors.Is(e, sql.ErrNoRows) {
		return false, nil
	} else if e != nil {
		return false, e
	}
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return false, e
	}
	if p == nil || p.State != "here" || p.ArrivalRollbackJSON == "" {
		return false, nil
	}
	var old federationArrivalRollback
	if e = json.Unmarshal([]byte(p.ArrivalRollbackJSON), &old); e != nil {
		return false, e
	}
	if old.Presence.HomeInstance != p.CurrentInstance {
		// A visiting actor had no remaining local authority at departure. Remove
		// only the new birth grants; home grants never enter this branch.
		for _, table := range []string{"agent_permissions", "agent_group_members", "agent_group_owners"} {
			if _, e = tx.Exec(`DELETE FROM `+table+` WHERE agent_id=?`, id); e != nil {
				return false, e
			}
		}
	}
	if _, e = tx.Exec(`DELETE FROM agent_conversations WHERE conv_id=? AND agent_id=?`, conv, id); e != nil {
		return false, e
	}
	if _, e = tx.Exec(`UPDATE agent_conversations SET role=? WHERE conv_id=? AND agent_id=?`, ConvRoleHead, old.Conv, id); e != nil {
		return false, e
	}
	if _, e = tx.Exec(`UPDATE agents SET current_conv_id=?,retired_at=?,retired_by='system:federation-away',retire_reason='away',retired_by_agent='' WHERE agent_id=? AND current_conv_id=?`, old.Conv, dbTime(time.Now()), id, conv); e != nil {
		return false, e
	}
	old.Presence.ContinuationNonceHash = old.NonceHash
	old.Presence.Transfer = old.Transfer
	if e = putFederationPresenceTx(tx, old.Presence); e != nil {
		return false, e
	}
	return true, tx.Commit()
}

// RemintFederationClone converts a superseded roaming copy into a genuinely
// independent local actor. The running native conversation stays unchanged;
// only its local actor binding changes. Temporary sudo never follows a clone.
func RemintFederationClone(id, conv string) (string, error) {
	d, e := Open()
	if e != nil {
		return "", e
	}
	tx, e := d.Begin()
	if e != nil {
		return "", e
	}
	defer func() { _ = tx.Rollback() }()
	p, e := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, id))
	if e != nil {
		return "", e
	}
	if p == nil {
		return id, nil
	}
	if p.State == "terminal" && p.IndependentClone != "" {
		return p.IndependentClone, tx.Commit()
	}
	if p.State != "here" || p.HomeInstance == p.CurrentInstance {
		return "", errors.New("only a live visiting identity can become a clone")
	}
	var current string
	if e = tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id=? AND retired_at IS NULL`, id).Scan(&current); e != nil {
		return "", e
	}
	if current != conv {
		return "", errors.New("clone generation changed")
	}
	clone := NewAgentID()
	if _, e = tx.Exec(`UPDATE agents SET current_conv_id=? WHERE agent_id=? AND current_conv_id=?`, "superseded:"+id, id, conv); e != nil {
		return "", e
	}
	if e = insertAgentTx(tx, clone, conv, "clone", time.Now()); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE agents SET (pending_name,initial_spawn_config,relaunch_profile,effective_sandbox_config,task_ref_url,task_ref_label)=(SELECT pending_name,initial_spawn_config,relaunch_profile,effective_sandbox_config,task_ref_url,task_ref_label FROM agents WHERE agent_id=?) WHERE agent_id=?`, id, clone); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE agent_conversations SET agent_id=?,reason='teleport-superseded-clone' WHERE conv_id=? AND agent_id=?`, clone, conv, id); e != nil {
		return "", e
	}
	for _, table := range []string{"agent_group_members", "agent_group_owners", "agent_permissions"} {
		if _, e = tx.Exec(`UPDATE `+table+` SET agent_id=? WHERE agent_id=?`, clone, id); e != nil {
			return "", e
		}
	}
	if _, e = tx.Exec(`UPDATE agent_sudo_grants SET revoked_at=? WHERE agent_id=? AND revoked_at IS NULL`, dbTime(time.Now()), id); e != nil {
		return "", e
	}
	if _, e = tx.Exec(`UPDATE agents SET retired_at=?,retired_by='system:teleport-clone',retire_reason='superseded by independent clone' WHERE agent_id=?`, dbTime(time.Now()), id); e != nil {
		return "", e
	}
	p.State = "terminal"
	p.ContinuationNonceHash = ""
	p.IndependentClone = clone
	if e = putFederationPresenceTx(tx, *p); e != nil {
		return "", e
	}
	return clone, tx.Commit()
}
