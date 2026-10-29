package agentd_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Scenario: a group grants routes.publish-host and a fresh Codex agent is
// spawned into it. Codex enrolls only after its pane starts, so the launch
// cannot carry a host-route helper. The operator must see that at spawn time
// in the daemon log, not first as a failed `routes publish --host`.
func TestCodexSpawn_HostRouteGrantWarnsWithoutHelper(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host routes are Linux/macOS only")
	}
	f := newFlow(t)
	g := f.HaveGroup("crew")
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublishHost}, "test"))

	logs := &lockedLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "codex-publisher", "harness": "codex"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	agentd.WaitForBackgroundForTest()

	out := logs.String()
	assert.Contains(t, out, "this launch has no host-route helper")
	assert.Contains(t, out, "conv="+spawn.ConvID)
	assert.Contains(t, out, "codex harness enrolls after its pane starts")
}

// Scenario: the same group grant with a Claude Code spawn that is not in
// tclaude's pane sandbox. The spawn path reports why the helper was skipped.
// Without the grant, no warning appears at all.
func TestClaudeSpawn_HostRouteWarningOnlyWithGrant(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host routes are Linux/macOS only")
	}
	f := newFlow(t)
	f.HaveGroup("plain")
	g := f.HaveGroup("granted")
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermRoutesPublishHost}, "test"))

	logs := &lockedLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	plain := f.AsHuman().SpawnWith("plain", map[string]any{"name": "no-grant"})
	require.Equalf(t, http.StatusOK, plain.Code, "spawn body=%s", plain.Raw)
	agentd.WaitForBackgroundForTest()
	assert.NotContains(t, logs.String(), "host-route helper", "an agent without the grant must not warn")

	granted := f.AsHuman().SpawnWith("granted", map[string]any{"name": "with-grant"})
	require.Equalf(t, http.StatusOK, granted.Code, "spawn body=%s", granted.Raw)
	agentd.WaitForBackgroundForTest()
	out := logs.String()
	assert.Contains(t, out, "this launch has no host-route helper")
	assert.Contains(t, out, "conv="+granted.ConvID)
	assert.Contains(t, out, "surface=spawn")
}
