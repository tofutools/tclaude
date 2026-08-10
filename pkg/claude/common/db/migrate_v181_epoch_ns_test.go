package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func v180FixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	setupTestDB(t)
	d, err := sql.Open("sqlite", t.TempDir()+"/v180.sqlite?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, createSchema(d))
	for _, step := range migrationSteps {
		if step.version > 180 {
			break
		}
		require.NoErrorf(t, step.apply(d), "migration v%d", step.version)
	}
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 180, version, "fixture must prove the v181 step has not run")
	return d
}

func TestMigrateV180toV181_ConvertsEveryTimestampAndPreservesSchemaGraph(t *testing.T) {
	d := v180FixtureDB(t)
	columns := seedEveryV180TimestampColumn(t, d)
	beforeTriggers := triggerDefinitions(t, d)
	childTables := []string{
		"agent_message_attachments", "agent_standing_order_hook_selectors",
		"agent_standing_order_messages", "agent_tags", "group_template_agents",
		"human_message_attachments", "operator_agent_messages",
		"sandbox_profile_global_assignment", "spawn_profile_aliases",
	}
	childCounts := map[string]int{}
	for _, table := range childTables {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM `+quoteIdentifier(table)).Scan(&count))
		childCounts[table] = count
		require.Positive(t, count, "%s fixture must contain a timestamp-less FK child", table)
	}
	sourceCounts := timestampNullabilityCounts(t, d)
	assert.Equal(t, timestampNullabilityCountsResult{
		total: 116, notNull: 114, emptyDefault: 38, zeroDefault: 1, alreadyNullable: 2,
	}, sourceCounts, "v180 classifier inputs")

	require.NoError(t, migrateV180toV181(d), "execute the exact v181 step")
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 181, version, "step execution control")

	strictTables := map[string]bool{}
	for _, column := range columns {
		query := `SELECT COUNT(*), COALESCE(SUM(typeof(` + quoteIdentifier(column.column) + `) = 'integer'), 0)
			FROM ` + quoteIdentifier(column.table) + ` WHERE ` + quoteIdentifier(column.column) + ` IS NOT NULL`
		var populated, integers int
		require.NoError(t, d.QueryRow(query).Scan(&populated, &integers), column.qualified())
		require.Positive(t, populated, "%s population control", column.qualified())
		require.Equal(t, populated, integers, "%s values use SQLite INTEGER storage", column.qualified())
		var declaredType string
		require.NoError(t, d.QueryRow(`SELECT type FROM pragma_table_info(?) WHERE name = ?`, column.table, column.column).Scan(&declaredType))
		assert.Equal(t, "INTEGER", strings.ToUpper(declaredType), "%s declared type", column.qualified())
		if !strictTables[column.table] {
			var strict int
			require.NoError(t, d.QueryRow(`SELECT [strict] FROM pragma_table_list WHERE name = ?`, column.table).Scan(&strict))
			assert.Equal(t, 1, strict, "%s is STRICT", column.table)
			strictTables[column.table] = true
		}
	}
	for _, table := range childTables {
		var after int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM `+quoteIdentifier(table)).Scan(&after))
		assert.Equal(t, childCounts[table], after, "%s rows survive parent rebuilds", table)
	}
	destinationCounts := timestampNullabilityCounts(t, d)
	assert.Equal(t, 116, destinationCounts.total)
	assert.Equal(t, 75, destinationCounts.notNull, "required timestamps retain NOT NULL")
	assert.Equal(t, 41, destinationCounts.alreadyNullable, "only genuine sentinel/nullable timestamps become nullable")

	rows, err := d.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	require.False(t, rows.Next(), "seeded FK children still reference rebuilt parents")
	require.NoError(t, rows.Err())
	require.Equal(t, beforeTriggers, triggerDefinitions(t, d), "all triggers survive the rebuild")

	fresh := freshMigratedDB(t)
	require.NoError(t, migrateV181toV182(d), "advance upgraded fixture through v182")
	require.NoError(t, migrateV182toV183(d), "advance upgraded fixture through v183")
	require.NoError(t, migrateV183toV184(d), "advance upgraded fixture through v184")
	require.NoError(t, migrateV184toV185(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV185toV186(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV186toV187(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV187toV188(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV188toV189(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV189toV190(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV190toV191(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV191toV192(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV192toV193(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV193toV194(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV194toV195(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV195toV196(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV196toV197(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV197toV198(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV198toV199(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV199toV200(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV200toV201(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV201toV202(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV202toV203(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV203toV204(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV204toV205(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV205toV206(d), "advance upgraded fixture to current schema")
	require.NoError(t, migrateV206toV207(d), "advance upgraded fixture to current schema")
	upgradedSchema, err := SchemaSQL(d)
	require.NoError(t, err)
	freshSchema, err := SchemaSQL(fresh)
	require.NoError(t, err)
	require.Equal(t, freshSchema, upgradedSchema, "upgraded schema matches a fresh current schema")
}

type qualifiedTimestampColumn struct{ table, column string }

func (c qualifiedTimestampColumn) qualified() string { return c.table + "." + c.column }

// seedEveryV180TimestampColumn derives the migration population from SQLite's
// schema, not from a grep list. One synthetic row is installed in every table
// (with uniform PK/FK values), then every timestamp column is populated. The
// result is returned to drive the per-column typeof assertions above.
func seedEveryV180TimestampColumn(t *testing.T, d *sql.DB) []qualifiedTimestampColumn {
	t.Helper()
	require.NoError(t, execPragma(d, `PRAGMA foreign_keys = OFF`))
	require.NoError(t, execPragma(d, `PRAGMA ignore_check_constraints = ON`))

	triggers := triggerStatementsInOrder(t, d)
	for _, trigger := range triggers {
		name := trigger.name
		_, err := d.Exec(`DROP TRIGGER ` + quoteIdentifier(name))
		require.NoError(t, err)
	}

	tables, err := d.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY rowid`)
	require.NoError(t, err)
	var names []string
	for tables.Next() {
		var name string
		require.NoError(t, tables.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, tables.Err())
	require.NoError(t, tables.Close())

	var timestampColumns []qualifiedTimestampColumn
	for _, table := range names {
		columns, err := d.Query(`SELECT name, type FROM pragma_table_info(?) ORDER BY cid`, table)
		require.NoError(t, err)
		var columnNames, placeholders []string
		var values []any
		for columns.Next() {
			var name, typ string
			require.NoError(t, columns.Scan(&name, &typ))
			columnNames = append(columnNames, quoteIdentifier(name))
			placeholders = append(placeholders, "?")
			upperType := strings.ToUpper(typ)
			switch {
			case isTimestampColumn(name) && upperType == "TEXT":
				values = append(values, "2024-01-02T03:04:05.123456789+02:00")
				timestampColumns = append(timestampColumns, qualifiedTimestampColumn{table, name})
			case isTimestampColumn(name) && upperType == "INTEGER":
				values = append(values, int64(1700000000))
				timestampColumns = append(timestampColumns, qualifiedTimestampColumn{table, name})
			case table == "agent_cron_jobs" && name == "target_kind":
				values = append(values, "conv")
			case table == "agent_permissions" && name == "effect":
				values = append(values, "grant")
			case table == "agent_notify_prefs" && name == "mode":
				values = append(values, "on")
			case table == "agentd_idempotency" && name == "state":
				values = append(values, "pending")
			case table == "spawn_harness_rules" && name == "source_harness":
				values = append(values, "claude")
			case table == "spawn_harness_rules" && name == "target_harness":
				values = append(values, "codex")
			case table == "spawn_harness_rules" && name == "decision":
				values = append(values, "allow")
			case table == "opencode_agent_state_allocations" && name == "mode":
				values = append(values, "legacy-shared")
			case table == "opencode_agent_state_allocations" && name == "state_root":
				values = append(values, "")
			case table == "process_runs" && name == "program_authorizations_json":
				values = append(values, "{}")
			case table == "agent_standing_order_turn_origins" && name == "state":
				values = append(values, "pending")
			case upperType == "INTEGER":
				values = append(values, int64(1))
			case upperType == "REAL":
				values = append(values, float64(1))
			case upperType == "BLOB":
				values = append(values, []byte{1})
			default:
				values = append(values, "x")
			}
		}
		require.NoError(t, columns.Err())
		require.NoError(t, columns.Close())
		_, err = d.Exec(`INSERT OR IGNORE INTO `+quoteIdentifier(table)+` (`+
			strings.Join(columnNames, ", ")+`) VALUES (`+strings.Join(placeholders, ", ")+`)`, values...)
		require.NoErrorf(t, err, "seed %s", table)
	}

	for _, column := range timestampColumns {
		value := any("2024-01-02T03:04:05.123456789+02:00")
		var declaredType string
		require.NoError(t, d.QueryRow(`SELECT type FROM pragma_table_info(?) WHERE name = ?`, column.table, column.column).
			Scan(&declaredType))
		if strings.EqualFold(declaredType, "INTEGER") {
			value = int64(1700000000)
		}
		_, err := d.Exec(`UPDATE `+quoteIdentifier(column.table)+` SET `+quoteIdentifier(column.column)+` = ?`, value)
		require.NoError(t, err, column.qualified())
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM `+quoteIdentifier(column.table)).Scan(&count))
		require.Positive(t, count, "%s table population control", column.table)
	}

	for _, trigger := range triggers {
		_, err := d.Exec(trigger.statement)
		require.NoError(t, err)
	}
	require.NoError(t, execPragma(d, `PRAGMA foreign_keys = ON`))
	require.NoError(t, execPragma(d, `PRAGMA ignore_check_constraints = OFF`))
	return timestampColumns
}

type timestampNullabilityCountsResult struct {
	total, notNull, emptyDefault, zeroDefault, alreadyNullable int
}

func timestampNullabilityCounts(t *testing.T, d *sql.DB) timestampNullabilityCountsResult {
	t.Helper()
	rows, err := d.Query(`SELECT s.name, p.name, p.type, p.[notnull], p.dflt_value
		FROM sqlite_master s JOIN pragma_table_info(s.name) p
		WHERE s.type = 'table' AND s.name NOT LIKE 'sqlite_%' ORDER BY s.rowid, p.cid`)
	require.NoError(t, err)
	defer rows.Close()
	var counts timestampNullabilityCountsResult
	for rows.Next() {
		var table, name, declaredType string
		var notNull int
		var defaultValue any
		require.NoError(t, rows.Scan(&table, &name, &declaredType, &notNull, &defaultValue))
		if !isTimestampColumn(name) {
			continue
		}
		counts.total++
		if notNull == 0 {
			counts.alreadyNullable++
			continue
		}
		counts.notNull++
		if value, ok := defaultValue.(string); ok {
			switch value {
			case "''":
				counts.emptyDefault++
			case "0":
				counts.zeroDefault++
			}
		}
	}
	require.NoError(t, rows.Err())
	return counts
}

type namedTrigger struct{ name, statement string }

func triggerStatementsInOrder(t *testing.T, d *sql.DB) []namedTrigger {
	t.Helper()
	rows, err := d.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' ORDER BY rowid`)
	require.NoError(t, err)
	defer rows.Close()
	var out []namedTrigger
	for rows.Next() {
		var trigger namedTrigger
		require.NoError(t, rows.Scan(&trigger.name, &trigger.statement))
		out = append(out, trigger)
	}
	require.NoError(t, rows.Err())
	return out
}

func execPragma(d *sql.DB, statement string) error {
	_, err := d.Exec(statement)
	return err
}

func triggerDefinitions(t *testing.T, d *sql.DB) map[string]string {
	t.Helper()
	rows, err := d.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'trigger' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, statement string
		require.NoError(t, rows.Scan(&name, &statement))
		out[name] = statement
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMigrateV180toV181_FileMtimeRepairsSecondsAndMilliseconds(t *testing.T) {
	d := v180FixtureDB(t)
	for _, row := range []struct {
		id    string
		mtime int64
	}{
		{"seconds", 1700000000},
		{"milliseconds", 1700000000123},
		{"absent", 0},
	} {
		_, err := d.Exec(`INSERT INTO conv_index
			(conv_id, project_dir, full_path, file_mtime, indexed_at) VALUES (?, '', '', ?, ?)`,
			row.id, row.mtime, "2024-01-02T03:04:05Z")
		require.NoError(t, err)
	}
	require.NoError(t, migrateV180toV181(d))

	var seconds, milliseconds int64
	var absent sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT file_mtime FROM conv_index WHERE conv_id = 'seconds'`).Scan(&seconds))
	require.NoError(t, d.QueryRow(`SELECT file_mtime FROM conv_index WHERE conv_id = 'milliseconds'`).Scan(&milliseconds))
	require.NoError(t, d.QueryRow(`SELECT file_mtime FROM conv_index WHERE conv_id = 'absent'`).Scan(&absent))
	assert.Equal(t, int64(1700000000)*int64(time.Second), seconds)
	assert.Equal(t, int64(1700000000123)*int64(time.Millisecond), milliseconds)
	assert.False(t, absent.Valid, "legacy zero sentinel becomes NULL")
}

func TestMigrateV180toV181_RoundTripsLegacySpellingsExactly(t *testing.T) {
	d := v180FixtureDB(t)
	fixtures := []struct {
		id, value string
	}{
		{"whole_z", "2024-01-02T03:04:05Z"},
		{"trimmed_fraction", "2024-01-02T03:04:05.1Z"},
		{"offset", "2024-01-02T05:04:05.123456789+02:00"},
	}
	for _, fixture := range fixtures {
		_, err := d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES (?, ?)`, fixture.id, fixture.value)
		require.NoError(t, err)
	}
	_, err := d.Exec(`INSERT INTO usage_cache (id, data, fetched_at, last_attempt_at) VALUES (99, '{}', '', '')`)
	require.NoError(t, err)

	require.NoError(t, migrateV180toV181(d))
	for _, fixture := range fixtures {
		var stored int64
		require.NoError(t, d.QueryRow(`SELECT notified_at FROM notify_state WHERE session_id = ?`, fixture.id).Scan(&stored))
		original, err := time.Parse(time.RFC3339Nano, fixture.value)
		require.NoError(t, err)
		assert.True(t, time.Unix(0, stored).Equal(original), "%s instant changed", fixture.id)
		var storageClass string
		require.NoError(t, d.QueryRow(`SELECT typeof(notified_at) FROM notify_state WHERE session_id = ?`, fixture.id).
			Scan(&storageClass))
		assert.Equal(t, "integer", storageClass, fixture.id)
	}

	var absent sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT fetched_at FROM usage_cache WHERE id = 99`).Scan(&absent))
	assert.False(t, absent.Valid, "a genuine optional timestamp preserves absence as NULL")
}

func TestMigrateV180toV181_RepairsOptionalGoZeroTimeAsAbsent(t *testing.T) {
	d := v180FixtureDB(t)
	_, err := d.Exec(`INSERT INTO sessions (id, created_at, updated_at, last_hook)
		VALUES ('zero-last-hook', '2024-01-02T03:04:05Z', '2024-01-02T03:04:06Z', ?)`,
		time.Time{}.Format(time.RFC3339Nano))
	require.NoError(t, err)

	require.NoError(t, migrateV180toV181(d))

	var lastHook sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT last_hook FROM sessions WHERE id = 'zero-last-hook'`).Scan(&lastHook))
	assert.False(t, lastHook.Valid, "legacy Go zero time is the optional column's absence sentinel")
}

func TestMigrateV180toV181_RepairsRequiredSessionGoZeroFromSibling(t *testing.T) {
	d := v180FixtureDB(t)
	zero := time.Time{}.Format(time.RFC3339Nano)
	created := "2024-01-02T03:04:05.123456789Z"
	updated := "2024-02-03T04:05:06.987654321Z"
	_, err := d.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES
		('repair-created', ?, ?),
		('repair-updated', ?, ?)`, zero, updated, created, zero)
	require.NoError(t, err)

	require.NoError(t, migrateV180toV181(d), "both sibling-repair arms must execute")
	for _, tc := range []struct {
		id, sibling string
	}{
		{"repair-created", updated},
		{"repair-updated", created},
	} {
		var createdNS, updatedNS int64
		require.NoError(t, d.QueryRow(`SELECT created_at, updated_at FROM sessions WHERE id = ?`, tc.id).
			Scan(&createdNS, &updatedNS))
		parsed, err := time.Parse(time.RFC3339Nano, tc.sibling)
		require.NoError(t, err)
		assert.Equal(t, parsed.UnixNano(), createdNS, tc.id+" created_at uses sibling provenance")
		assert.Equal(t, parsed.UnixNano(), updatedNS, tc.id+" updated_at uses sibling provenance")
	}
}

func TestMigrateV180toV181_ReportsAllUnrepairableRequiredZeros(t *testing.T) {
	d := v180FixtureDB(t)
	zero := time.Time{}.Format(time.RFC3339Nano)
	_, err := d.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES
		('both-zero-a', ?, ?),
		('both-zero-b', ?, ?),
		('repair-then-rollback', ?, '2024-01-02T03:04:05Z')`, zero, zero, zero, zero, zero)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES ('required-zero', ?)`, zero)
	require.NoError(t, err)

	err = migrateV180toV181(d)
	require.Error(t, err, "unrepairable required-zero census must execute")
	for _, want := range []string{
		`id "both-zero-a"`, `id "both-zero-b"`, "notify_state.notified_at", "in 3 row(s)",
		"restore the .pre-v181-epochns.bak backup", "replace all offending required values",
	} {
		assert.ErrorContains(t, err, want)
	}
	var rolledBack string
	require.NoError(t, d.QueryRow(`SELECT created_at FROM sessions WHERE id = 'repair-then-rollback'`).Scan(&rolledBack))
	assert.Equal(t, zero, rolledBack, "aggregate failure rolls back a sibling repair that actually ran")
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 180, version, "the aggregate failure rolls back all sibling repairs and conversions")
}

func TestMigrateV180toV181_RepairsRequiredZeroFromOptionalSiblingInHalfAppliedSchema(t *testing.T) {
	setupTestDB(t)
	d, err := sql.Open("sqlite", t.TempDir()+"/half-applied.sqlite?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	zero := time.Time{}.Format(time.RFC3339Nano)
	valid := "2024-01-02T03:04:05.123456789Z"
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (180);
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			updated_at TEXT DEFAULT ''
		);
		INSERT INTO sessions(id, created_at, updated_at) VALUES
			('repair-from-optional', ?, ?),
			('null-optional-control', ?, NULL);
	`, zero, valid, valid)
	require.NoError(t, err)

	require.NoError(t, migrateV180toV181(d))
	var createdNS, updatedNS int64
	require.NoError(t, d.QueryRow(`SELECT created_at, updated_at FROM sessions WHERE id = 'repair-from-optional'`).
		Scan(&createdNS, &updatedNS))
	parsed, err := time.Parse(time.RFC3339Nano, valid)
	require.NoError(t, err)
	assert.Equal(t, parsed.UnixNano(), createdNS)
	assert.Equal(t, parsed.UnixNano(), updatedNS)
	var optional sql.NullInt64
	require.NoError(t, d.QueryRow(`SELECT updated_at FROM sessions WHERE id = 'null-optional-control'`).Scan(&optional))
	assert.False(t, optional.Valid, "NULL in the half-applied optional sibling scans and remains absent")
}

func TestMigrateV180toV181_CapsRequiredTimestampErrorDetails(t *testing.T) {
	d := v180FixtureDB(t)
	for i := 0; i < 23; i++ {
		_, err := d.Exec(`INSERT INTO notify_state(session_id, notified_at) VALUES (?, 'not-a-time')`, fmt.Sprintf("bad-%02d", i))
		require.NoError(t, err)
	}

	err := migrateV180toV181(d)
	require.Error(t, err)
	assert.ErrorContains(t, err, "in 23 row(s)")
	assert.ErrorContains(t, err, "and 3 more")
}

func TestRequiredTimestampPreflightAggregatesSQLNull(t *testing.T) {
	setupTestDB(t)
	d, err := sql.Open("sqlite", t.TempDir()+"/required-null.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`CREATE TABLE probe(id INTEGER PRIMARY KEY, required_at TEXT); INSERT INTO probe(required_at) VALUES (NULL)`)
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	err = repairAndValidateRequiredZeroTimestamps(context.Background(), tx, []timestampTable{{
		name: "probe", columns: []timestampColumn{{name: "required_at", text: true}},
	}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "probe.required_at")
	assert.ErrorContains(t, err, "value NULL")
	assert.ErrorContains(t, err, "in 1 row(s)")
}

func TestRequiredTimestampStorageClasses_GenericCensus(t *testing.T) {
	valid := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	for _, tc := range []struct {
		name      string
		value     any
		wantError bool
	}{
		{"parseable_blob", []byte(valid.Format(time.RFC3339Nano)), false},
		{"parseable_integer", valid.UnixNano(), false},
		{"unparseable_blob", []byte("not-a-time"), true},
		{"unparseable_integer", int64(0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			d, err := sql.Open("sqlite", t.TempDir()+"/generic-storage.sqlite")
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			_, err = d.Exec(`CREATE TABLE probe(id INTEGER PRIMARY KEY, required_at); INSERT INTO probe(required_at) VALUES (?)`, tc.value)
			require.NoError(t, err)
			tx, err := d.Begin()
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			column := timestampColumn{name: "required_at", text: true}
			err = repairAndValidateRequiredZeroTimestamps(context.Background(), tx, []timestampTable{{
				name: "probe", columns: []timestampColumn{column},
			}})
			if tc.wantError {
				require.Error(t, err)
				assert.ErrorContains(t, err, "probe.required_at")
				return
			}
			require.NoError(t, err)
			require.NoError(t, convertTimestampColumn(context.Background(), tx, "probe", column))
			var stored int64
			require.NoError(t, tx.QueryRow(`SELECT required_at FROM probe`).Scan(&stored))
			assert.Equal(t, valid.UnixNano(), stored, "parseable storage class converts exactly")
		})
	}
}

func TestRequiredTimestampStorageClasses_SessionsCensus(t *testing.T) {
	created := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	updated := created.Add(time.Second)
	for _, tc := range []struct {
		name      string
		created   any
		wantError bool
	}{
		{"parseable_blob", []byte(created.Format(time.RFC3339Nano)), false},
		{"parseable_integer", created.UnixNano(), false},
		{"unparseable_blob", []byte("not-a-time"), true},
		{"unparseable_integer", int64(0), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			d, err := sql.Open("sqlite", t.TempDir()+"/sessions-storage.sqlite")
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			_, err = d.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, created_at, updated_at);
				INSERT INTO sessions(id, created_at, updated_at) VALUES ('real-session', ?, ?)`, tc.created, updated.UnixNano())
			require.NoError(t, err)
			tx, err := d.Begin()
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			createdColumn := timestampColumn{name: "created_at", text: true}
			updatedColumn := timestampColumn{name: "updated_at", text: true}
			err = repairAndValidateRequiredZeroTimestamps(context.Background(), tx, []timestampTable{{
				name: "sessions", columns: []timestampColumn{createdColumn, updatedColumn},
			}})
			if tc.wantError {
				require.Error(t, err)
				assert.ErrorContains(t, err, `id "real-session"`)
				return
			}
			require.NoError(t, err)
			require.NoError(t, convertTimestampColumn(context.Background(), tx, "sessions", createdColumn))
			require.NoError(t, convertTimestampColumn(context.Background(), tx, "sessions", updatedColumn))
			var createdNS, updatedNS int64
			require.NoError(t, tx.QueryRow(`SELECT created_at, updated_at FROM sessions WHERE id = 'real-session'`).Scan(&createdNS, &updatedNS))
			assert.Equal(t, created.UnixNano(), createdNS)
			assert.Equal(t, updated.UnixNano(), updatedNS)
		})
	}
}

func TestMigrateV180toV181_RejectsEmptyRequiredTimestamp(t *testing.T) {
	d := v180FixtureDB(t)
	_, err := d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES ('required-empty', '')`)
	require.NoError(t, err)

	err = migrateV180toV181(d)
	require.Error(t, err, "required-empty failure arm must execute")
	assert.ErrorContains(t, err, "notify_state.notified_at")
	assert.ErrorContains(t, err, "rowid")
	assert.ErrorContains(t, err, `value ""`)
	assert.ErrorContains(t, err, "required timestamp is empty")
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 180, version)
	var notNull int
	require.NoError(t, d.QueryRow(`SELECT [notnull] FROM pragma_table_info('notify_state') WHERE name = 'notified_at'`).Scan(&notNull))
	assert.Equal(t, 1, notNull, "failed migration leaves the required source constraint intact")
}

func TestMigrateV180toV181_RestoresAutoincrementHighWater(t *testing.T) {
	d := v180FixtureDB(t)
	_, err := d.Exec(`INSERT INTO human_messages (id, from_conv, body, created_at) VALUES
		(1, 'first', 'first', '2024-01-02T03:04:05Z'),
		(100, 'deleted', 'deleted', '2024-01-02T03:04:06Z')`)
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM human_messages WHERE id = 100`)
	require.NoError(t, err)

	var before int64
	require.NoError(t, d.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'human_messages'`).Scan(&before))
	require.Equal(t, int64(100), before, "fixture retains a deleted externally-addressed id")
	require.NoError(t, migrateV180toV181(d))

	var restored int64
	require.NoError(t, d.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'human_messages'`).Scan(&restored))
	assert.Equal(t, int64(100), restored, "migration restores the original sequence, not the copied max id")
	id, err := insertHumanMessage(d, &HumanMessage{FromConv: "next", Body: "next", CreatedAt: time.Unix(1704164647, 0)})
	require.NoError(t, err)
	assert.Equal(t, int64(101), id)
}

func TestMigrateV180toV181_ConvertsNamedEpochSecondColumns(t *testing.T) {
	d := v180FixtureDB(t)
	const seconds = int64(1700000000)
	_, err := d.Exec(`INSERT INTO dashboard_session_grace (token_hash, expires_at, created_at)
		VALUES ('token', ?, '2024-01-02T03:04:05Z')`, seconds)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO agentd_idempotency
		(request_key, fingerprint, owner_id, state, created_at, expires_at)
		VALUES ('request', 'fingerprint', 'owner', 'pending', ?, ?)`, seconds, seconds+1)
	require.NoError(t, err)

	require.NoError(t, migrateV180toV181(d))
	var grace, created, expires int64
	require.NoError(t, d.QueryRow(`SELECT expires_at FROM dashboard_session_grace WHERE token_hash = 'token'`).Scan(&grace))
	require.NoError(t, d.QueryRow(`SELECT created_at, expires_at FROM agentd_idempotency WHERE request_key = 'request'`).
		Scan(&created, &expires))
	assert.Equal(t, seconds*int64(time.Second), grace)
	assert.Equal(t, seconds*int64(time.Second), created)
	assert.Equal(t, (seconds+1)*int64(time.Second), expires)
}

func TestDBTimestampRejectsLegacyStorageClasses(t *testing.T) {
	for _, legacy := range []any{"2024-01-02T03:04:05Z", []byte("1704164645000000000")} {
		var stamp dbTimestamp
		require.Error(t, stamp.Scan(legacy), "storage boundary must reject %T", legacy)
	}
	var stamp dbTimestamp
	require.NoError(t, stamp.Scan(int64(1704164645000000000)))
	assert.Equal(t, time.Unix(1704164645, 0).UTC(), stamp.Time())

	epoch := time.Unix(0, 0).UTC()
	_, err := dbTime(epoch).Value()
	require.Error(t, err, "persisted integer zero remains reserved")
	boundary, err := dbTimeBoundary(epoch).Value()
	require.NoError(t, err)
	assert.Equal(t, int64(0), boundary, "read-only inclusive range may start at the Unix epoch")
}

func TestMigrateV180toV181_InvalidTimestampMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, value        string
		optionalBecomesNil bool
	}{
		{name: "malformed", value: "not-a-time"},
		{name: "go_zero_rfc3339", value: time.Time{}.Format(time.RFC3339Nano), optionalBecomesNil: true},
		{name: "go_zero_string", value: time.Time{}.String()},
		{name: "epoch_zero", value: "1970-01-01T00:00:00Z"},
		{name: "sqlite_datetime_epoch", value: "1970-01-01 00:00:00"},
		{name: "out_of_range", value: "9999-01-01T00:00:00Z"},
	} {
		for _, column := range []struct {
			name, qualified string
			optional        bool
		}{
			{name: "required", qualified: "notify_state.notified_at"},
			{name: "optional", qualified: "sessions.last_hook", optional: true},
		} {
			t.Run(column.name+"/"+tc.name, func(t *testing.T) {
				d := v180FixtureDB(t)
				var err error
				if column.optional {
					_, err = d.Exec(`INSERT INTO sessions (id, created_at, updated_at, last_hook)
						VALUES ('bad', '2024-01-02T03:04:05Z', '2024-01-02T03:04:06Z', ?)`, tc.value)
				} else {
					_, err = d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES ('bad', ?)`, tc.value)
				}
				require.NoError(t, err)
				err = migrateV180toV181(d)
				if column.optional && tc.optionalBecomesNil {
					require.NoError(t, err)
					var value sql.NullInt64
					require.NoError(t, d.QueryRow(`SELECT last_hook FROM sessions WHERE id = 'bad'`).Scan(&value))
					assert.False(t, value.Valid, "recognized optional Go-zero becomes NULL")
					return
				}
				require.Error(t, err, "failure arm must execute")
				assert.ErrorContains(t, err, column.qualified)
				assert.ErrorContains(t, err, "rowid")
				assert.ErrorContains(t, err, fmt.Sprintf("%q", tc.value))
				if !column.optional {
					assert.ErrorContains(t, err, "in 1 row(s)", "every required parse failure is caught by the aggregate preflight")
					assert.ErrorContains(t, err, "replace all offending required values")
				}
				var version int
				require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
				assert.Equal(t, 180, version)
				var storageType string
				query := `SELECT typeof(notified_at) FROM notify_state WHERE session_id = 'bad'`
				if column.optional {
					query = `SELECT typeof(last_hook) FROM sessions WHERE id = 'bad'`
				}
				require.NoError(t, d.QueryRow(query).Scan(&storageType))
				assert.Equal(t, "text", storageType, "failed conversion rolls back the source row")
			})
		}
	}
}

func TestMigrateV180toV181_RejectsUnrecognizedFileMtimeUnit(t *testing.T) {
	d := v180FixtureDB(t)
	_, err := d.Exec(`INSERT INTO conv_index
		(conv_id, project_dir, full_path, file_mtime, indexed_at) VALUES ('bad-mtime', '', '', ?, ?)`,
		int64(^uint64(0)>>1), "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	err = migrateV180toV181(d)
	require.Error(t, err, "failure arm must execute")
	assert.ErrorContains(t, err, "conv_index.file_mtime")
	assert.ErrorContains(t, err, "rowid")
}

func TestLegacyFileMtimeToUnixNano_ThresholdBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     int64
		want      int64
		wantError bool
	}{
		{"floor_minus_one", legacyFileMtimeModernMillisFloor - 1, 0, true},
		{"floor", legacyFileMtimeModernMillisFloor, legacyFileMtimeModernMillisFloor * int64(time.Millisecond), false},
		{"floor_plus_one", legacyFileMtimeModernMillisFloor + 1, (legacyFileMtimeModernMillisFloor + 1) * int64(time.Millisecond), false},
		{"max", maxUnixNanoMillis, maxUnixNanoMillis * int64(time.Millisecond), false},
		{"max_plus_one", maxUnixNanoMillis + 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := legacyFileMtimeToUnixNano(tc.value)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
