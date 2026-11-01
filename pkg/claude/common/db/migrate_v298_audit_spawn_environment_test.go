package db

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactAuditEnvironmentKeepsNames(t *testing.T) {
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"name": "worker",
		"Environment": [{"name": "API_TOKEN", "value": "s3cret"}, {"Name": "not a name=s3cret", "Value": "s3cret"}, "BARE_NAME", "BARE=s3cret", 7],
		"nested": {"environment": {"API_TOKEN": "s3cret"}},
		"other": [{"environment": "s3cret"}]
	}`), &input))
	assert.Equal(t, 6, RedactAuditEnvironment(input))
	out, err := json.Marshal(input)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "s3cret")
	assert.Contains(t, string(out), `{"name":"API_TOKEN","value":"[redacted]"}`)
	assert.Contains(t, string(out), "BARE_NAME")
	assert.Equal(t, "worker", input["name"])
	assert.Zero(t, RedactAuditEnvironment(input), "a second pass changes nothing")
}

func TestMigrateV297toV298RedactsStoredSpawnRows(t *testing.T) {
	d, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	d.SetMaxOpenConns(1)
	_, err = d.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (297);
		CREATE TABLE audit_log (id INTEGER PRIMARY KEY, verb TEXT NOT NULL, actor_label TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)

	const clear = `{"kind":"tclaude.spawn.audit.v1","summary":"role: dev","input":{"context_window":1000000000000000001,"environment":[{"name":"API_TOKEN","value":"s3cret"}]},"resolved":{"launch":{"sandbox_policy":{"environment":["API_TOKEN"]}}}}`
	const refused = `{"kind":"tclaude.spawn.audit.v1","input":{"environment":[{"name":"API_TOKEN=s3cret"}]},"response":{"code":"invalid_environment","error":"environment[0].name \"API_TOKEN=s3cret\" is invalid (want an ASCII identifier up to 128 bytes)"}}`
	const message = `{"environment":"not a spawn row, s3cret"}`
	const notJSON = `role: "environment" s3cret`
	for _, row := range [][]string{{"spawn", clear}, {"send", message}, {"spawn", notJSON}, {"spawn", refused}} {
		_, err := d.Exec(`INSERT INTO audit_log (verb, actor_label, detail) VALUES (?, 'operator', ?)`, row[0], row[1])
		require.NoError(t, err)
	}

	require.NoError(t, migrateV297toV298(d))

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 298, version)
	detail := func(id int) string {
		var out string
		require.NoError(t, d.QueryRow(`SELECT detail FROM audit_log WHERE id = ?`, id).Scan(&out))
		return out
	}
	spawn := detail(1)
	assert.NotContains(t, spawn, "s3cret")
	assert.Contains(t, spawn, `{"name":"API_TOKEN","value":"[redacted]"}`)
	assert.Contains(t, spawn, "1000000000000000001", "a number stays as written")
	assert.Contains(t, spawn, `"summary":"role: dev"`)
	assert.Contains(t, spawn, `"resolved":{"launch":{"sandbox_policy":{"environment":["API_TOKEN"]}}}`)
	assert.NotContains(t, detail(4), "s3cret", "a refusal quoted what was typed as a name")
	assert.Contains(t, detail(4), `"code":"invalid_environment"`)
	assert.Equal(t, message, detail(2), "other verbs are not touched")
	assert.Equal(t, notJSON, detail(3), "a row that cannot be read is left")

	again, changed := redactSpawnAuditDetail(spawn)
	assert.False(t, changed, again)
}
