package db

import (
	"database/sql"
	"fmt"
)

// migrateV185toV186 normalizes the launch contract's slot ownership. The
// encoded slots on darwin_route_launches remain the durable contract returned
// to callers; this table is the agentd-owned uniqueness index that keeps a
// TCP slot exclusive across pending and active launch generations.
func migrateV185toV186(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS darwin_route_slot_claims (
			slot INTEGER NOT NULL CHECK(slot BETWEEN 1 AND 65535),
			agent_id TEXT NOT NULL,
			conv_id TEXT NOT NULL,
			launch_generation TEXT NOT NULL,
			state TEXT NOT NULL CHECK(state IN ('pending', 'active')),
			created_at INTEGER NOT NULL,
			PRIMARY KEY(slot),
			UNIQUE(agent_id, conv_id, launch_generation, slot)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_darwin_route_slot_claims_identity
			ON darwin_route_slot_claims(agent_id, conv_id, launch_generation, state);
		UPDATE schema_version SET version = 186;
	`); err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): %w", err)
	}
	rows, err := tx.Query(`SELECT agent_id, conv_id, launch_generation, slots, state, created_at
		FROM darwin_route_launches WHERE state IN ('pending', 'active')`)
	if err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): read launches: %w", err)
	}
	type launchRow struct {
		agentID, convID, generation, rawSlots, state string
		createdAt                                    int64
	}
	var launches []launchRow
	for rows.Next() {
		var agentID, convID, generation, rawSlots, state string
		var createdAt int64
		if err := rows.Scan(&agentID, &convID, &generation, &rawSlots, &state, &createdAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): scan launch: %w", err)
		}
		launches = append(launches, launchRow{agentID, convID, generation, rawSlots, state, createdAt})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): close launches: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): read launches: %w", err)
	}
	for _, launch := range launches {
		slots, err := decodeDarwinRouteLaunchSlots(launch.rawSlots)
		if err != nil {
			return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): decode launch %s/%s/%s: %w", launch.agentID, launch.convID, launch.generation, err)
		}
		for _, slot := range slots {
			if _, err := tx.Exec(`INSERT INTO darwin_route_slot_claims
				(slot, agent_id, conv_id, launch_generation, state, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`, slot, launch.agentID, launch.convID, launch.generation, launch.state, launch.createdAt); err != nil {
				return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): claim slot %d: %w", slot, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v185→v186 (Darwin route slot claims): commit: %w", err)
	}
	return nil
}
