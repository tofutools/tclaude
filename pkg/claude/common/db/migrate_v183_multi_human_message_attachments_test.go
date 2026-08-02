package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func v182FixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	setupTestDB(t)
	d, err := sql.Open("sqlite", t.TempDir()+"/v182.sqlite?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	d.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, createSchema(d))
	for _, step := range migrationSteps {
		if step.version > 182 {
			break
		}
		require.NoErrorf(t, step.apply(d), "migration v%d", step.version)
	}
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 182, version, "fixture must prove the v183 step has not run")
	return d
}

// A v116 attachment survives the rebuild as its message's first file, and the
// table now accepts several attachments per message.
func TestMigrateV182toV183_PreservesLegacyAttachmentAndAllowsSeveral(t *testing.T) {
	d := v182FixtureDB(t)
	_, err := d.Exec(`INSERT INTO human_messages (id, from_conv, from_agent, from_title, group_name,
		subject, body, created_at) VALUES (7, 'c1', '', '', '', 'art', 'ready', 1)`)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO human_message_attachments
		(message_id, filename, content_type, size_bytes, storage_path)
		VALUES (7, 'export.zip', 'application/zip', 12, '/private/export.zip')`)
	require.NoError(t, err)

	require.NoError(t, migrateV182toV183(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 183, version)

	var id, messageID, seq int64
	var filename string
	require.NoError(t, d.QueryRow(`SELECT id, message_id, seq, filename
		FROM human_message_attachments`).Scan(&id, &messageID, &seq, &filename))
	assert.Positive(t, id)
	assert.Equal(t, int64(7), messageID)
	assert.Equal(t, int64(0), seq, "the migrated row is its message's first file")
	assert.Equal(t, "export.zip", filename)

	for _, name := range []string{"world.png", "guild.png"} {
		_, err = d.Exec(`INSERT INTO human_message_attachments
			(message_id, seq, filename, content_type, size_bytes, storage_path)
			VALUES (7, 1, ?, 'image/png', 3, '/private/'||?)`, name, name)
		require.NoError(t, err, "a message may now carry several files")
	}
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM human_message_attachments WHERE message_id = 7`).Scan(&count))
	assert.Equal(t, 3, count)

	// The cascade still reclaims every attachment of a deleted message.
	_, err = d.Exec(`DELETE FROM human_messages WHERE id = 7`)
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM human_message_attachments`).Scan(&count))
	assert.Zero(t, count)
}

// The upgraded schema must match what a fresh database gets.
func TestMigrateV182toV183_MatchesFreshSchema(t *testing.T) {
	d := v182FixtureDB(t)
	require.NoError(t, migrateV182toV183(d))
	upgraded, err := SchemaSQL(d)
	require.NoError(t, err)
	fresh, err := SchemaSQL(freshMigratedDB(t))
	require.NoError(t, err)
	assert.Equal(t, fresh, upgraded)
}
