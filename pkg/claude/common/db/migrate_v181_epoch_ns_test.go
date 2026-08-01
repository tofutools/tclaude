package db

import (
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

	require.NoError(t, migrateV180toV181(d), "execute the exact v181 step")
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 181, version, "step execution control")

	for _, column := range columns {
		query := `SELECT COUNT(*), COALESCE(SUM(typeof(` + quoteIdentifier(column.column) + `) = 'integer'), 0)
			FROM ` + quoteIdentifier(column.table) + ` WHERE ` + quoteIdentifier(column.column) + ` IS NOT NULL`
		var populated, integers int
		require.NoError(t, d.QueryRow(query).Scan(&populated, &integers), column.qualified())
		require.Positive(t, populated, "%s population control", column.qualified())
		require.Equal(t, populated, integers, "%s values use SQLite INTEGER storage", column.qualified())
	}

	rows, err := d.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	require.False(t, rows.Next(), "seeded FK children still reference rebuilt parents")
	require.Equal(t, beforeTriggers, triggerDefinitions(t, d), "all triggers survive the rebuild")

	fresh := freshMigratedDB(t)
	upgradedSchema, err := SchemaSQL(d)
	require.NoError(t, err)
	freshSchema, err := SchemaSQL(fresh)
	require.NoError(t, err)
	require.Equal(t, freshSchema, upgradedSchema, "upgraded schema matches a fresh v181 schema")
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
	return timestampColumns
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
		{"absent", ""},
	}
	for _, fixture := range fixtures {
		_, err := d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES (?, ?)`, fixture.id, fixture.value)
		require.NoError(t, err)
	}

	require.NoError(t, migrateV180toV181(d))
	for _, fixture := range fixtures {
		var stored sql.NullInt64
		require.NoError(t, d.QueryRow(`SELECT notified_at FROM notify_state WHERE session_id = ?`, fixture.id).Scan(&stored))
		if fixture.value == "" {
			assert.False(t, stored.Valid, "empty-string absence must migrate to NULL")
			continue
		}
		original, err := time.Parse(time.RFC3339Nano, fixture.value)
		require.NoError(t, err)
		require.True(t, stored.Valid)
		assert.True(t, time.Unix(0, stored.Int64).Equal(original), "%s instant changed", fixture.id)
		var storageClass string
		require.NoError(t, d.QueryRow(`SELECT typeof(notified_at) FROM notify_state WHERE session_id = ?`, fixture.id).
			Scan(&storageClass))
		assert.Equal(t, "integer", storageClass, fixture.id)
	}
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

func TestMigrateV180toV181_RejectsInvalidTimestampsLoudly(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{"malformed", "not-a-time"},
		{"zero", "1970-01-01T00:00:00Z"},
		{"out_of_range", "9999-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := v180FixtureDB(t)
			_, err := d.Exec(`INSERT INTO notify_state (session_id, notified_at) VALUES ('bad', ?)`, tc.value)
			require.NoError(t, err)
			err = migrateV180toV181(d)
			require.Error(t, err, "failure arm must execute")
			assert.ErrorContains(t, err, "notify_state.notified_at")
			assert.ErrorContains(t, err, "rowid")
			assert.ErrorContains(t, err, fmt.Sprintf("value %q", tc.value))
			var version int
			require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
			assert.Equal(t, 180, version)
			var storageType string
			require.NoError(t, d.QueryRow(`SELECT typeof(notified_at) FROM notify_state WHERE session_id = 'bad'`).Scan(&storageType))
			assert.Equal(t, "text", storageType, "failed conversion rolls back the source row")
		})
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
