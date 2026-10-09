package db

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
)

var ErrTeleportLimit = errors.New("teleport hourly or daily limit reached")

type FederationTeleport struct {
	Repo           *FederationRepo               `json:"repo,omitempty"`
	Checkout       *jobrepo.Checkout             `json:"checkout,omitempty"`
	Direction      string                        `json:"direction"`
	Peer           string                        `json:"peer"`
	Offer          string                        `json:"offer"`
	State          string                        `json:"state"`
	Intent         bundletransfer.TeleportIntent `json:"intent"`
	TargetAgent    string                        `json:"target_agent,omitempty"`
	Credentials    string                        `json:"credentials,omitempty"`
	Landing        *FederationTeleportLanding    `json:"landing,omitempty"`
	Profile        *SpawnProfile                 `json:"profile,omitempty"`
	WorkerDefaults *FederationWorkerDefaults     `json:"worker_defaults,omitempty"`
	CreatedAt      time.Time                     `json:"created_at"`
}

// RecordFederationTeleport charges an attempt before transfer or launch. The
// same stable origin is rate-limited across new landed identities and clones;
// the immediate peer is also limited independently of claimed provenance.
func RecordFederationTeleport(t FederationTeleport, limits config.TeleportLimits) error {
	limits = limits.Effective()
	if limits.Hour < 1 || limits.Day < 1 || limits.Chain < 1 {
		return ErrTeleportLimit
	}
	d, err := Open()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	now := time.Now()
	origin := t.Intent.OriginInstance + "/" + t.Intent.OriginAgent
	result, err := d.Exec(`INSERT INTO federation_teleports(direction,peer,offer,chain,source_agent,target_agent,created_at,state,snapshot)
 SELECT ?,?,?,?,?,?,?,?,? WHERE
 (SELECT COUNT(*) FROM federation_teleports WHERE direction=? AND source_agent=? AND created_at>?)<? AND
 (SELECT COUNT(*) FROM federation_teleports WHERE direction=? AND source_agent=? AND created_at>?)<? AND
 (SELECT COUNT(*) FROM federation_teleports WHERE direction=? AND peer=? AND created_at>?)<? AND
 (SELECT COUNT(*) FROM federation_teleports WHERE direction=? AND peer=? AND created_at>?)<?`,
		t.Direction, t.Peer, t.Offer, t.Intent.Chain, origin, t.TargetAgent, dbTime(now), t.State, string(raw),
		t.Direction, origin, dbTime(now.Add(-time.Hour)), limits.Hour, t.Direction, origin, dbTime(now.Add(-24*time.Hour)), limits.Day,
		t.Direction, t.Peer, dbTime(now.Add(-time.Hour)), limits.Hour, t.Direction, t.Peer, dbTime(now.Add(-24*time.Hour)), limits.Day)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTeleportLimit
	}
	return nil
}
func GetFederationTeleport(direction, peer, offer string) (*FederationTeleport, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT snapshot,state,target_agent,created_at FROM federation_teleports WHERE direction=? AND peer=? AND offer=?`, direction, peer, offer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanTeleport(rows)
}
func scanTeleport(rows rowScanner) (*FederationTeleport, error) {
	var raw, state, target string
	var at dbTimestamp
	if err := rows.Scan(&raw, &state, &target, &at); err != nil {
		return nil, err
	}
	var t FederationTeleport
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, err
	}
	t.State, t.TargetAgent, t.CreatedAt = state, target, at.Time()
	return &t, nil
}
func ListFederationTeleports() ([]FederationTeleport, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT snapshot,state,target_agent,created_at FROM federation_teleports ORDER BY created_at,offer`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationTeleport{}
	for rows.Next() {
		t, err := scanTeleport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}
func TransitionFederationTeleport(t FederationTeleport, from string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`UPDATE federation_teleports SET state=?,target_agent=?,snapshot=? WHERE direction=? AND peer=? AND offer=? AND state=?`, t.State, t.TargetAgent, string(raw), t.Direction, t.Peer, t.Offer, from)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func FederationTeleportForAgent(agent string) (*FederationTeleport, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT snapshot,state,target_agent,created_at FROM federation_teleports WHERE direction='in' AND target_agent=? ORDER BY created_at DESC LIMIT 1`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanTeleport(rows)
}
