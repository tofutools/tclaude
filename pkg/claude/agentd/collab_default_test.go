package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An upgrade stores features.collab once: on without Collab traces, off with
// them, since old builds deleted the key when Collab was turned off. A stored
// value is never changed.
func TestResolveCollabDefault(t *testing.T) {
	writeConfig := func(t *testing.T, raw string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o700))
		require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(raw), 0o600))
	}
	resolve := func(t *testing.T) (*config.Config, *config.Config) {
		t.Helper()
		cfg, err := config.Load()
		require.NoError(t, err)
		resolveCollabDefault(cfg)
		stored, err := config.Load()
		require.NoError(t, err)
		require.NotNil(t, stored.Features)
		require.NotNil(t, stored.Features.Collab, "the decision is stored")
		assert.Equal(t, *stored.Features.Collab, cfg.CollabEnabled(), "the caller's config matches what was stored")
		return cfg, stored
	}

	t.Run("no config file, no traces: on", func(t *testing.T) {
		setupTestDB(t)
		_, stored := resolve(t)
		assert.True(t, stored.CollabEnabled())
		// Only the key is written: a default notifications block would read to
		// tclaude setup as the operator's choice and skip its first-run offer.
		assert.False(t, config.NotificationsPresent(), "no defaults are written")
	})
	t.Run("config without the key, no traces: on", func(t *testing.T) {
		setupTestDB(t)
		writeConfig(t, `{"features":{"processes":true},"steward":{}}`)
		_, stored := resolve(t)
		assert.True(t, stored.CollabEnabled())
		assert.True(t, stored.ProcessesEnabled(), "the rest of the config is kept")
		raw, err := os.ReadFile(config.ConfigPath())
		require.NoError(t, err)
		assert.JSONEq(t, `{"features":{"processes":true,"collab":true},"steward":{}}`, string(raw), "only the key is added")
	})
	t.Run("steward settings: off", func(t *testing.T) {
		setupTestDB(t)
		writeConfig(t, `{"steward":{"external_enabled":true}}`)
		_, stored := resolve(t)
		assert.False(t, stored.CollabEnabled())
		assert.True(t, stored.ExternalStewardsEnabled(), "the steward settings are kept")
	})
	t.Run("a Steward group: off", func(t *testing.T) {
		setupTestDB(t)
		_, err := db.CreateAgentGroup(stewardGroupName, db.ScribeGroupDescr)
		require.NoError(t, err)
		_, stored := resolve(t)
		assert.False(t, stored.CollabEnabled())
	})
	t.Run("an operator's own group named steward: on", func(t *testing.T) {
		setupTestDB(t)
		_, err := db.CreateAgentGroup(stewardGroupName, "my team")
		require.NoError(t, err)
		_, stored := resolve(t)
		assert.True(t, stored.CollabEnabled())
	})
	t.Run("boards only: off", func(t *testing.T) {
		setupTestDB(t)
		_, err := db.CreateCollabBoard("sketch", "Sketch", "human", 10)
		require.NoError(t, err)
		_, stored := resolve(t)
		assert.False(t, stored.CollabEnabled())
	})
	t.Run("explicit values are kept", func(t *testing.T) {
		for _, raw := range []string{`{"features":{"collab":true}}`, `{"features":{"collab":false}}`} {
			setupTestDB(t)
			_, err := db.CreateCollabBoard("sketch", "Sketch", "human", 10)
			require.NoError(t, err)
			writeConfig(t, `{"steward":{"name":"Ada"},`+raw[1:])
			before, err := config.Load()
			require.NoError(t, err)
			_, stored := resolve(t)
			assert.Equal(t, before.CollabEnabled(), stored.CollabEnabled(), raw)
		}
	})
}
