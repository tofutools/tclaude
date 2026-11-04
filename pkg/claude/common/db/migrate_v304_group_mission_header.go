package db

import (
	"database/sql"
	"strings"
)

// migrateV303toV304 adds agent_groups.mission_header, which keeps a deployed
// group's mission apart from its default_context (see AgentGroup.MissionHeader).
//
// Before it, a deploy folded the mission into default_context as a trailing
// "## Mission" (or "## Task") section. Where a group's default_context still
// ends with exactly that section, the section is moved out: stripped from
// default_context, with the header recorded. A context the operator has edited
// since no longer ends that way and is left as it is, header empty, so the
// mission is never delivered twice.
func migrateV303toV304(d *sql.DB) error {
	// default_context is operator-authored text; keep the same best-effort
	// recovery point the earlier data-rewriting migrations take.
	vacuumBackup(d, ".pre-v304-mission.bak")
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Partial test fixtures (and only those) can reach here without the table
	// or its two source columns; there is nothing to add or move out then.
	var columns, sources, existing int
	if err := tx.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(name IN ('default_context', 'mission')), 0),
		COALESCE(SUM(name = 'mission_header'), 0)
		FROM pragma_table_info('agent_groups')`).Scan(&columns, &sources, &existing); err != nil {
		return err
	}
	if columns > 0 && existing == 0 {
		if _, err := tx.Exec(`ALTER TABLE agent_groups ADD COLUMN mission_header TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if sources < 2 {
		if _, err := tx.Exec(`UPDATE schema_version SET version = 304`); err != nil {
			return err
		}
		return tx.Commit()
	}
	rows, err := tx.Query(`SELECT id, default_context, mission FROM agent_groups WHERE mission <> ''`)
	if err != nil {
		return err
	}
	type split struct {
		id             int64
		context, title string
	}
	var splits []split
	for rows.Next() {
		var id int64
		var context, mission string
		if err := rows.Scan(&id, &context, &mission); err != nil {
			_ = rows.Close()
			return err
		}
		if rest, header, ok := splitFoldedMission(context, mission); ok {
			splits = append(splits, split{id: id, context: rest, title: header})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, sp := range splits {
		if _, err := tx.Exec(`UPDATE agent_groups SET default_context = ?, mission_header = ? WHERE id = ?`,
			sp.context, sp.title, sp.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 304`); err != nil {
		return err
	}
	return tx.Commit()
}

// splitFoldedMission reports whether context ends with the mission section a
// pre-v304 deploy folded into it, and if so returns the context without it and
// the section's header.
func splitFoldedMission(context, mission string) (string, string, bool) {
	mission = normalizeMissionText(mission)
	if mission == "" {
		return "", "", false
	}
	for _, header := range []string{"Mission", "Task"} {
		section := "## " + header + "\n\n" + mission
		if context == section {
			return "", header, true
		}
		if rest, ok := strings.CutSuffix(context, "\n\n"+section); ok {
			return rest, header, true
		}
	}
	return "", "", false
}
