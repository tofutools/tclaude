package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV212toV213PreservesTriggerWorkersAndDefaultsCronToMessage(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v212.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `
		CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version VALUES (212);
		CREATE TABLE trigger_rules (id INTEGER PRIMARY KEY);
		CREATE TABLE trigger_firings (id INTEGER PRIMARY KEY);
		CREATE TABLE agent_cron_jobs (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '');
		CREATE TABLE agent_cron_runs (id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL REFERENCES agent_cron_jobs(id) ON DELETE CASCADE, fired_at INTEGER NOT NULL, status TEXT NOT NULL DEFAULT '', error_msg TEXT NOT NULL DEFAULT '');
		CREATE TABLE trigger_workers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id INTEGER REFERENCES trigger_rules(id) ON DELETE SET NULL,
			firing_id INTEGER NOT NULL REFERENCES trigger_firings(id) ON DELETE CASCADE,
			action_index INTEGER NOT NULL, agent_id TEXT NOT NULL UNIQUE,
			conv_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'reserved',
			pending_label TEXT NOT NULL DEFAULT '', deadline_at INTEGER, created_at INTEGER NOT NULL,
			completed_at INTEGER, detail TEXT NOT NULL DEFAULT '') STRICT;
		INSERT INTO trigger_rules VALUES (1); INSERT INTO trigger_firings VALUES (2);
		INSERT INTO agent_cron_jobs VALUES (3,'legacy-message');
		INSERT INTO trigger_workers(rule_id,firing_id,action_index,agent_id,state,created_at) VALUES(1,2,0,'agt_legacy','live',10);
	`)
	require.NoError(t, migrateV212toV213(d))
	assert.Equal(t, 213, schemaVersion(d))
	var action, policy string
	var maxLive int
	require.NoError(t, d.QueryRow(`SELECT action_kind,spawn_concurrency_policy,spawn_max_live_workers FROM agent_cron_jobs WHERE id=3`).Scan(&action, &policy, &maxLive))
	assert.Equal(t, CronActionMessage, action)
	assert.Equal(t, CronConcurrencyForbid, policy)
	assert.Equal(t, 1, maxLive)
	var ruleID, firingID int64
	require.NoError(t, d.QueryRow(`SELECT rule_id,firing_id FROM trigger_workers WHERE agent_id='agt_legacy'`).Scan(&ruleID, &firingID))
	assert.Equal(t, int64(1), ruleID)
	assert.Equal(t, int64(2), firingID)
	_, err = d.Exec(`INSERT INTO agent_cron_runs(id,job_id,fired_at,status) VALUES(4,3,20,'running')`)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO trigger_workers(cron_job_id,cron_run_id,action_index,agent_id,state,created_at) VALUES(3,4,0,'agt_cron','reserved',20)`)
	require.NoError(t, err)
}
