package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// migrateV108toV109 gives every persisted reference to the mutable profile
// and template registries a stable row-id companion. The old text columns stay
// as portable/historical snapshots: exports and API payloads continue to use
// names, while runtime reads resolve the current name through the id column.
// Keeping the snapshot also lets older binaries read the database and gives a
// useful fallback for legacy dangling references.
func migrateV108toV109(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v108→v109 (stable registry refs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	columns := []struct {
		table, column, ddl string
	}{
		{"agent_groups", "default_profile_id", "INTEGER"},
		{"agent_groups", "source_template_id", "INTEGER"},
		{"group_template_agents", "spawn_profile_id", "INTEGER"},
		{"roles", "spawn_profile_id", "INTEGER"},
	}
	for _, c := range columns {
		haveTable, err := txTableExists(tx, c.table)
		if err != nil {
			return fmt.Errorf("migrate v108→v109: probe %s: %w", c.table, err)
		}
		if !haveTable {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column,
		).Scan(&haveColumn); err != nil {
			// SQLite does not parameterize pragma table-valued function names
			// consistently on older builds; use the already hard-coded table set.
			return fmt.Errorf("migrate v108→v109: probe %s.%s: %w", c.table, c.column, err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.ddl); err != nil {
				return fmt.Errorf("migrate v108→v109: add %s.%s: %w", c.table, c.column, err)
			}
		}
	}

	// Backfill only unresolved rows so a half-applied migration converges.
	backfills := []struct {
		tables []string
		stmt   string
	}{
		{[]string{"agent_groups", "spawn_profiles"}, `UPDATE agent_groups SET default_profile_id =
			(SELECT id FROM spawn_profiles WHERE name = agent_groups.default_profile)
		 WHERE default_profile_id IS NULL AND default_profile <> ''`},
		{[]string{"agent_groups", "group_templates"}, `UPDATE agent_groups SET source_template_id =
			(SELECT id FROM group_templates WHERE name = agent_groups.source_template)
		 WHERE source_template_id IS NULL AND source_template <> ''`},
		{[]string{"group_template_agents", "spawn_profiles"}, `UPDATE group_template_agents SET spawn_profile_id =
			(SELECT id FROM spawn_profiles WHERE name = group_template_agents.spawn_profile)
		 WHERE spawn_profile_id IS NULL AND spawn_profile <> ''`},
		{[]string{"roles", "spawn_profiles"}, `UPDATE roles SET spawn_profile_id =
			(SELECT id FROM spawn_profiles WHERE name = roles.spawn_profile)
		 WHERE spawn_profile_id IS NULL AND spawn_profile <> ''`},
	}
	for _, b := range backfills {
		ready := true
		for _, table := range b.tables {
			have, err := txTableExists(tx, table)
			if err != nil {
				return fmt.Errorf("migrate v108→v109: probe %s for backfill: %w", table, err)
			}
			ready = ready && have
		}
		if !ready {
			continue
		}
		if _, err := tx.Exec(b.stmt); err != nil {
			return fmt.Errorf("migrate v108→v109: backfill: %w", err)
		}
	}
	haveProfiles, err := txTableExists(tx, "spawn_profiles")
	if err != nil {
		return fmt.Errorf("migrate v108→v109: probe spawn_profiles: %w", err)
	}
	if havePrefs, err := txTableExists(tx, "dashboard_prefs"); err != nil {
		return fmt.Errorf("migrate v108→v109: probe dashboard_prefs: %w", err)
	} else if havePrefs && haveProfiles {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO dashboard_prefs (key, value, updated_at)
			SELECT 'tclaude.dash.default_profile_id', CAST(p.id AS TEXT), dp.updated_at
			  FROM dashboard_prefs dp
			  JOIN spawn_profiles p ON p.name = dp.value
			 WHERE dp.key = 'tclaude.dash.default_profile'`); err != nil {
			return fmt.Errorf("migrate v108→v109: backfill global default profile id: %w", err)
		}
	}
	haveTemplates, err := txTableExists(tx, "group_templates")
	if err != nil {
		return fmt.Errorf("migrate v108→v109: probe group_templates: %w", err)
	}
	if haveWaves, err := txTableExists(tx, "group_wave_choreography"); err != nil {
		return fmt.Errorf("migrate v108→v109: probe group_wave_choreography: %w", err)
	} else if haveWaves && haveTemplates {
		if err := backfillWaveRegistryIDs(tx, haveProfiles); err != nil {
			return fmt.Errorf("migrate v108→v109: backfill wave template ids: %w", err)
		}
	}

	for _, idx := range []struct{ table, stmt string }{
		{"agent_groups", `CREATE INDEX IF NOT EXISTS idx_agent_groups_default_profile_id ON agent_groups(default_profile_id)`},
		{"agent_groups", `CREATE INDEX IF NOT EXISTS idx_agent_groups_source_template_id ON agent_groups(source_template_id)`},
		{"group_template_agents", `CREATE INDEX IF NOT EXISTS idx_template_agents_spawn_profile_id ON group_template_agents(spawn_profile_id)`},
		{"roles", `CREATE INDEX IF NOT EXISTS idx_roles_spawn_profile_id ON roles(spawn_profile_id)`},
	} {
		have, err := txTableExists(tx, idx.table)
		if err != nil {
			return fmt.Errorf("migrate v108→v109: probe %s for index: %w", idx.table, err)
		}
		if !have {
			continue
		}
		if _, err := tx.Exec(idx.stmt); err != nil {
			return fmt.Errorf("migrate v108→v109: index: %w", err)
		}
	}

	// These triggers are a downgrade-write compatibility bridge. An older
	// binary can open a past-head database and still writes only the legacy name
	// columns. Keep the ID companions synchronized so the next v109 process
	// observes that intent instead of preferring a stale ID.
	triggers := []struct {
		table string
		deps  []string
		stmt  string
	}{
		{"agent_groups", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_group_profile_insert
			AFTER INSERT ON agent_groups BEGIN
				UPDATE agent_groups SET default_profile_id = COALESCE(NEW.default_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.default_profile))
				 WHERE id = NEW.id;
			END`},
		{"agent_groups", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_group_profile_update
			AFTER UPDATE OF default_profile ON agent_groups
			WHEN NEW.default_profile IS NOT OLD.default_profile BEGIN
				UPDATE agent_groups SET default_profile_id = CASE
					WHEN NEW.default_profile_id IS NOT OLD.default_profile_id THEN NEW.default_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.default_profile) END
				 WHERE id = NEW.id;
			END`},
		{"agent_groups", []string{"group_templates"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_group_template_insert
			AFTER INSERT ON agent_groups BEGIN
				UPDATE agent_groups SET source_template_id = COALESCE(NEW.source_template_id,
					(SELECT id FROM group_templates WHERE name = NEW.source_template))
				 WHERE id = NEW.id;
			END`},
		{"agent_groups", []string{"group_templates"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_group_template_update
			AFTER UPDATE OF source_template ON agent_groups
			WHEN NEW.source_template IS NOT OLD.source_template BEGIN
				UPDATE agent_groups SET source_template_id = CASE
					WHEN NEW.source_template_id IS NOT OLD.source_template_id THEN NEW.source_template_id
					ELSE (SELECT id FROM group_templates WHERE name = NEW.source_template) END
				 WHERE id = NEW.id;
			END`},
		{"group_template_agents", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_template_agent_profile_insert
			AFTER INSERT ON group_template_agents BEGIN
				UPDATE group_template_agents SET spawn_profile_id = COALESCE(NEW.spawn_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile))
				 WHERE id = NEW.id;
			END`},
		{"group_template_agents", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_template_agent_profile_update
			AFTER UPDATE OF spawn_profile ON group_template_agents
			WHEN NEW.spawn_profile IS NOT OLD.spawn_profile BEGIN
				UPDATE group_template_agents SET spawn_profile_id = CASE
					WHEN NEW.spawn_profile_id IS NOT OLD.spawn_profile_id THEN NEW.spawn_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile) END
				 WHERE id = NEW.id;
			END`},
		{"roles", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_role_profile_insert
			AFTER INSERT ON roles BEGIN
				UPDATE roles SET spawn_profile_id = COALESCE(NEW.spawn_profile_id,
					(SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile))
				 WHERE id = NEW.id;
			END`},
		{"roles", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_role_profile_update
			AFTER UPDATE OF spawn_profile ON roles
			WHEN NEW.spawn_profile IS NOT OLD.spawn_profile BEGIN
				UPDATE roles SET spawn_profile_id = CASE
					WHEN NEW.spawn_profile_id IS NOT OLD.spawn_profile_id THEN NEW.spawn_profile_id
					ELSE (SELECT id FROM spawn_profiles WHERE name = NEW.spawn_profile) END
				 WHERE id = NEW.id;
			END`},
		{"dashboard_prefs", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_global_profile_insert
			AFTER INSERT ON dashboard_prefs WHEN NEW.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
				INSERT INTO dashboard_prefs (key, value, updated_at)
					SELECT 'tclaude.dash.default_profile_id', CAST(id AS TEXT), NEW.updated_at
					  FROM spawn_profiles WHERE name = NEW.value;
			END`},
		{"dashboard_prefs", []string{"spawn_profiles"}, `CREATE TRIGGER IF NOT EXISTS stable_ref_global_profile_update
			AFTER UPDATE OF value ON dashboard_prefs WHEN NEW.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
				INSERT INTO dashboard_prefs (key, value, updated_at)
					SELECT 'tclaude.dash.default_profile_id', CAST(id AS TEXT), NEW.updated_at
					  FROM spawn_profiles WHERE name = NEW.value;
			END`},
		{"dashboard_prefs", nil, `CREATE TRIGGER IF NOT EXISTS stable_ref_global_profile_delete
			AFTER DELETE ON dashboard_prefs WHEN OLD.key = 'tclaude.dash.default_profile' BEGIN
				DELETE FROM dashboard_prefs WHERE key = 'tclaude.dash.default_profile_id';
			END`},
	}
	for _, trigger := range triggers {
		have, err := txTableExists(tx, trigger.table)
		if err != nil {
			return fmt.Errorf("migrate v108→v109: probe %s for trigger: %w", trigger.table, err)
		}
		ready := have
		for _, dep := range trigger.deps {
			haveDep, err := txTableExists(tx, dep)
			if err != nil {
				return fmt.Errorf("migrate v108→v109: probe %s for trigger: %w", dep, err)
			}
			ready = ready && haveDep
		}
		if !ready {
			continue
		}
		if _, err := tx.Exec(trigger.stmt); err != nil {
			return fmt.Errorf("migrate v108→v109: create trigger on %s: %w", trigger.table, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 109`); err != nil {
		return fmt.Errorf("migrate v108→v109: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v108→v109: commit: %w", err)
	}
	return nil
}

func backfillWaveRegistryIDs(tx *sql.Tx, haveProfiles bool) error {
	rows, err := tx.Query(`SELECT group_id, state FROM group_wave_choreography`)
	if err != nil {
		return err
	}
	type update struct {
		groupID int64
		state   string
	}
	updates := []update{}
	for rows.Next() {
		var groupID int64
		var state string
		if err := rows.Scan(&groupID, &state); err != nil {
			_ = rows.Close()
			return err
		}
		var c WaveChoreography
		if err := json.Unmarshal([]byte(state), &c); err != nil {
			_ = rows.Close()
			return err
		}
		changed := false
		if c.TemplateID == 0 && c.TemplateName != "" {
			if err := tx.QueryRow(`SELECT id FROM group_templates WHERE name = ?`, c.TemplateName).Scan(&c.TemplateID); err == nil {
				changed = true
			} else if err != sql.ErrNoRows {
				_ = rows.Close()
				return err
			}
		}
		if haveProfiles {
			for wi := range c.Waves {
				for ai := range c.Waves[wi].Agents {
					a := &c.Waves[wi].Agents[ai]
					if a.SpawnProfileID != 0 || a.SpawnProfile == "" {
						continue
					}
					if err := tx.QueryRow(`SELECT id FROM spawn_profiles WHERE name = ?`, a.SpawnProfile).Scan(&a.SpawnProfileID); err == nil {
						changed = true
					} else if err != sql.ErrNoRows {
						_ = rows.Close()
						return err
					}
				}
			}
		}
		if changed {
			blob, err := json.Marshal(&c)
			if err != nil {
				_ = rows.Close()
				return err
			}
			updates = append(updates, update{groupID: groupID, state: string(blob)})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE group_wave_choreography SET state = ? WHERE group_id = ?`, u.state, u.groupID); err != nil {
			return err
		}
	}
	return nil
}
