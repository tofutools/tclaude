package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestResolveResourceDelegationDirPrecedence(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "/from-env")
	cfg := &config.Config{Agent: &config.AgentConfig{ResourceDelegationDir: "/from-config"}}

	got, source := resolveResourceDelegationDir("/from-flag", cfg)
	assert.Equal(t, "/from-flag", got)
	assert.Equal(t, "flag", source)

	got, source = resolveResourceDelegationDir("", cfg)
	assert.Equal(t, "/from-env", got)
	assert.Equal(t, "environment", source)

	t.Setenv(session.ResourceDelegationDirEnv, "")
	got, source = resolveResourceDelegationDir("", cfg)
	assert.Equal(t, "/from-config", got)
	assert.Equal(t, "config", source)

	got, source = resolveResourceDelegationDir("", nil)
	assert.Empty(t, got)
	assert.Equal(t, "legacy self-cgroup derivation", source)
}

func TestManagedServerDropsStoredCgroupFromPreviousDelegationBeforeReprepare(t *testing.T) {
	setupTestDB(t)
	old := filepath.Join(t.TempDir(), "tclaude-old")
	require.NoError(t, os.Mkdir(old, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "memory.max"), []byte("128000000"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(old, "cpu.max"), []byte("max 100000"), 0o644))
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "managed-old-cgroup", ConvID: "ses_old_cgroup",
		ServerURL: "http://127.0.0.1:43210", Cwd: t.TempDir(),
		ResourceCgroupDir: old,
	}))
	t.Setenv(session.ResourceDelegationDirEnv,
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "128MB"},
	}}

	_, _, err := prepareManagedServerResourceCgroup(
		"managed-old-cgroup", snapshot, false)
	require.Error(t, err, "the test host does not provide the configured external root")
	stored, lookupErr := db.GetOpenCodeRuntime("managed-old-cgroup")
	require.NoError(t, lookupErr)
	assert.Nil(t, stored, "the invalid old-root runtime must be stopped before a fresh cgroup is prepared")
}
