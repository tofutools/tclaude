package harness

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareCodexDangerFullAccessResumeReplacesOnlySandboxPolicy(t *testing.T) {
	configDir := t.TempDir()
	d := openCodexResumeSandboxTestDB(t, configDir)
	_, err := d.Exec(`INSERT INTO threads (id, sandbox_policy, approval_mode)
		VALUES (?, ?, ?)`, "conv-1", `{"type":"managed"}`, "on-request")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	require.NoError(t, PrepareCodexDangerFullAccessResume(configDir, "conv-1"))

	d = openCodexResumeSandboxTestDB(t, configDir)
	defer func() { _ = d.Close() }()
	var policy, approval string
	require.NoError(t, d.QueryRow(`SELECT sandbox_policy, approval_mode FROM threads WHERE id = ?`,
		"conv-1").Scan(&policy, &approval))
	assert.Equal(t, codexDisabledSandboxPolicy, policy)
	assert.Equal(t, "on-request", approval, "sandbox unlock must not widen approval policy")
}

func TestPrepareCodexDangerFullAccessResumeRefusesMissingThread(t *testing.T) {
	configDir := t.TempDir()
	d := openCodexResumeSandboxTestDB(t, configDir)
	require.NoError(t, d.Close())

	err := PrepareCodexDangerFullAccessResume(configDir, "missing")
	require.ErrorContains(t, err, "no threads row")
}

func openCodexResumeSandboxTestDB(t *testing.T, configDir string) *sql.DB {
	t.Helper()
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	d, err := sql.Open("sqlite", filepath.Join(configDir, "state_5.sqlite"))
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		sandbox_policy TEXT NOT NULL,
		approval_mode TEXT NOT NULL
	)`)
	require.NoError(t, err)
	return d
}
