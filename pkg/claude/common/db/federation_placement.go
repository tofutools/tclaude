package db

import (
	"database/sql"
	"errors"
)

func FederationSpawnRequestByEnvelope(peer, envelope string) (*FederationSpawnRequest, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	r, err := scanFedSpawn(d.QueryRow(`SELECT `+fedSpawnColumns+` FROM federation_spawn_requests WHERE from_instance=? AND envelope_id=?`, peer, envelope).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// Include every undecided request and uncertain launch across all peers and
// approval kinds; an acknowledged human request holds capacity until decided.
func ListFederationSpawnReservations() ([]*FederationSpawnRequest, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+fedSpawnColumns+` FROM federation_spawn_requests WHERE status IN (?,?)`, FedSpawnPending, FedSpawnLaunching)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*FederationSpawnRequest{}
	for rows.Next() {
		r, err := scanFedSpawn(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Session status can say exited after a premature attach while a delayed pane
// is still starting. The caller intersects these actor bindings with actual
// tmux liveness, rather than trusting that cached status as termination proof.
type FederationCapacityActorSession struct {
	AgentID     string
	TmuxSession string
}

func FederationCapacityActorSessions() ([]FederationCapacityActorSession, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT a.agent_id,s.tmux_session FROM agents a JOIN sessions s ON s.conv_id=a.current_conv_id WHERE a.retired_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationCapacityActorSession{}
	for rows.Next() {
		var r FederationCapacityActorSession
		if err := rows.Scan(&r.AgentID, &r.TmuxSession); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
