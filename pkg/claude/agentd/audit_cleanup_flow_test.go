package agentd_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The retention sweep deletes audit rows older than the configured
// window (default 30 days) and leaves recent ones — so the command trail
// stays bounded without losing the useful recent history.
func TestAudit_RetentionSweepPrunesOldRows(t *testing.T) {
	newFlow(t) // sets HOME + a fresh DB; no config file → default 30-day retention

	old := time.Now().Add(-90 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)
	_, err := db.InsertAuditLog(db.AuditLogEntry{At: old, Verb: "spawn", Source: db.AuditSourceCLI})
	require.NoError(t, err)
	_, err = db.InsertAuditLog(db.AuditLogEntry{At: recent, Verb: "message", Source: db.AuditSourceCLI})
	require.NoError(t, err)

	agentd.RunAuditLogCleanupForTest(time.Now())

	rows, err := db.ListAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the 90-day-old row should be pruned, the 1-hour-old one kept")
	assert.Equal(t, "message", rows[0].Verb)
}

// A genuine config-load failure (corrupt config.json) must SKIP the
// prune, not fall back to the default 30-day window and delete rows the
// operator may have meant to keep — deleting against a guessed policy is
// unrecoverable.
func TestAudit_RetentionSweepSkipsOnBrokenConfig(t *testing.T) {
	newFlow(t)

	// Write a corrupt config.json so config.Load returns an error.
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte("{ not valid json"), 0o600))

	old := time.Now().Add(-365 * 24 * time.Hour) // far past any default window
	_, err := db.InsertAuditLog(db.AuditLogEntry{At: old, Verb: "spawn", Source: db.AuditSourceCLI})
	require.NoError(t, err)

	agentd.RunAuditLogCleanupForTest(time.Now())

	n, err := db.CountAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a broken config must skip pruning, not delete against a guessed policy")
}
