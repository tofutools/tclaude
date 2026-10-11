package db

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrTeleportDormantQuota = errors.New("teleport dormant quota reached or source already has a backup")

type FederationTeleportLease struct {
	RenewSeconds         int       `json:"renew_seconds"`
	LeaseSeconds         int       `json:"lease_seconds,omitempty"`
	GraceSeconds         int       `json:"grace_seconds,omitempty"`
	ShutdownPID          int       `json:"shutdown_pid,omitempty"`
	ShutdownProcessStart string    `json:"shutdown_process_start,omitempty"`
	ShutdownConv         string    `json:"shutdown_conv,omitempty"`
	Direction            string    `json:"direction"`
	Peer                 string    `json:"peer"`
	Offer                string    `json:"offer"`
	SourceAgent          string    `json:"source_agent"`
	SourceConv           string    `json:"source_conv"`
	TargetAgent          string    `json:"target_agent,omitempty"`
	Epoch                int64     `json:"epoch"`
	State                string    `json:"state"`
	Revision             int64     `json:"revision"`
	ExpiresAt            time.Time `json:"expires_at"`
	LastRenewed          time.Time `json:"last_renewed,omitempty"`
	Sequence             int64     `json:"sequence,omitempty"`
	ReturnID             string    `json:"return_id,omitempty"`
	Findings             string    `json:"findings,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
}

// ReserveFederationTeleportLease charges dormant slots atomically, including
// pending landings. The partial unique index also prevents concurrent moves of
// one stable identity from minting two backups.
func ReserveFederationTeleportLease(l FederationTeleportLease, limit int) error {
	d, err := Open()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if limit < 1 {
		return ErrTeleportDormantQuota
	}
	r, err := d.Exec(`INSERT INTO federation_teleport_leases(direction,peer,offer,source_agent,target_agent,epoch,state,revision,snapshot)
 SELECT ?,?,?,?,?,?,?,?,? WHERE ?='in' OR (SELECT COUNT(*) FROM federation_teleport_leases WHERE direction='out' AND state NOT IN ('recovered','released'))<?`, l.Direction, l.Peer, l.Offer, l.SourceAgent, l.TargetAgent, l.Epoch, l.State, l.Revision, string(raw), l.Direction, limit)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return ErrTeleportDormantQuota
	}
	return err
}
func ListFederationTeleportLeases() ([]FederationTeleportLease, error) {
	return listFederationTeleportLeases(false)
}
func ListActiveFederationTeleportLeases() ([]FederationTeleportLease, error) {
	return listFederationTeleportLeases(true)
}
func listFederationTeleportLeases(active bool) ([]FederationTeleportLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := "SELECT snapshot FROM federation_teleport_leases"
	if active {
		query += " WHERE state NOT IN ('recovered','released','superseded','clone') OR (direction='in' AND state='superseded')"
	}
	query += " ORDER BY direction,peer,offer"
	rows, err := d.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationTeleportLease{}
	for rows.Next() {
		var raw string
		var l FederationTeleportLease
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func GetFederationTeleportLease(direction, peer, offer string) (*FederationTeleportLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT snapshot FROM federation_teleport_leases WHERE direction=? AND peer=? AND offer=?`, direction, peer, offer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var raw string
	if err = rows.Scan(&raw); err != nil {
		return nil, err
	}
	var l FederationTeleportLease
	err = json.Unmarshal([]byte(raw), &l)
	return &l, err
}

// TransitionFederationTeleportLease commits the state, its recovery briefing,
// and operator notice together. Losing a CAS rolls back all three; a restart
// cannot double-deliver findings or miss the partition warning.
func TransitionFederationTeleportLease(l FederationTeleportLease, briefing string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	previous := l.Revision
	l.Revision++
	raw, err := json.Marshal(l)
	if err != nil {
		return false, err
	}
	r, err := tx.Exec(`UPDATE federation_teleport_leases SET target_agent=?,epoch=?,state=?,revision=?,snapshot=? WHERE direction=? AND peer=? AND offer=? AND revision=?`, l.TargetAgent, l.Epoch, l.State, l.Revision, string(raw), l.Direction, l.Peer, l.Offer, previous)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if briefing != "" {
		if l.State != "recovery_needed" {
			if _, err = insertAgentMessage(tx, &AgentMessage{ToConv: l.SourceConv, Subject: "Teleport backup resumed", Body: briefing}); err != nil {
				return false, err
			}
		}
		if _, err = insertHumanMessage(tx, &HumanMessage{FromConv: l.SourceConv, FromTitle: "Teleport recovery", Subject: "Teleport backup recovery", Body: briefing}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// TeleportBackupForAgent uses only local durable authority. Callers must still
// project it through their normal agent visibility checks.

func TeleportBackupForAgent(agent string) (*FederationTeleportLease, error) {
	return teleportLeaseForAgent(agent, true)
}
func TeleportIncomingLeaseForAgent(agent string) (*FederationTeleportLease, error) {
	return teleportLeaseForAgent(agent, false)
}
func teleportLeaseForAgent(agent string, backup bool) (*FederationTeleportLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `SELECT snapshot FROM federation_teleport_leases WHERE direction='in' AND target_agent=?`
	if backup {
		query = `SELECT snapshot FROM federation_teleport_leases WHERE direction='out' AND source_agent=? AND state NOT IN ('recovered','released')`
	}
	rows, err := d.Query(query, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var raw string
	if err = rows.Scan(&raw); err != nil {
		return nil, err
	}
	var l FederationTeleportLease
	err = json.Unmarshal([]byte(raw), &l)
	return &l, err
}
