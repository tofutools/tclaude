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
