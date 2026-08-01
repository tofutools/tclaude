package agentd

import (
	"os/exec"
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

func TestManagedOpenCodeExternalResourceCgroupRequiresExplicitDegradation(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv,
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	_, err := configureOpenCodeResourceCgroup(exec.Command("true"),
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-test")
	require.Error(t, err)
	assert.ErrorIs(t, err, errOpenCodeResourceCgroup)
	assert.ErrorContains(t, err, "cannot be placed across systemd units")
}

func TestManagedServerDropsStoredCgroupFromPreviousDelegationBeforeReprepare(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "managed-old-cgroup", ConvID: "ses_old_cgroup",
		ServerURL: "http://127.0.0.1:43210", Cwd: t.TempDir(),
		ResourceCgroupDir: "/sys/fs/cgroup/system.slice/tclaude-agentd.service/tclaude-old",
	}))
	t.Setenv(session.ResourceDelegationDirEnv,
		"/sys/fs/cgroup/system.slice/tclaude-tmux.service")
	previousPrepare := prepareResourceCgroup
	prepareResourceCgroup = func(sessionID string, limits sandboxpolicy.ResourceLimits) (string, func(), error) {
		assert.Equal(t, "managed-old-cgroup", sessionID)
		assert.Equal(t, "128MB", limits.Memory)
		return "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-new", func() {}, nil
	}
	t.Cleanup(func() { prepareResourceCgroup = previousPrepare })
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "128MB"},
	}}

	dir, _, err := prepareManagedServerResourceCgroup(
		"managed-old-cgroup", snapshot, false)
	require.NoError(t, err)
	assert.Equal(t, "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-new", dir)
	stored, lookupErr := db.GetOpenCodeRuntime("managed-old-cgroup")
	require.NoError(t, lookupErr)
	assert.Nil(t, stored, "the invalid old-root runtime must be stopped before a fresh cgroup is prepared")
}
