package agentd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
)

// Scenario: the Config tab guides remote-access setup off /api/snapshot's
// `remote_access` block (JOH-227). The dashboard reads it to warn "no
// certificates yet — run `tclaude remote-access setup`" before the operator
// flips the toggle (enabling without material is a silent no-op). So the
// snapshot must report whether the material exists — and it must track the
// filesystem live, not a boot-time cache.
//
// Running/RunningBind stay false/"" here: no test calls startRemoteServer, so
// the live listener is never up — exactly what a loopback-only dashboard sees.
func TestDashboardSnapshot_RemoteAccessState(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	_ = f // snapshot-only; no agents needed

	// Fresh temp $HOME (testharness) ⇒ no remote-access material generated yet.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.False(t, snap.RemoteAccess.MaterialExists,
		"no `tclaude remote-access setup` has run ⇒ material_exists is false")
	assert.False(t, snap.RemoteAccess.Running, "no listener started in tests")
	assert.Empty(t, snap.RemoteAccess.RunningBind, "no running bind without a listener")

	// Simulate `tclaude remote-access setup` having generated material:
	// remoteaccess.Exists() is a presence check on the CA cert (ca.crt) under
	// remoteaccess.Dir(). Drop a stub there and the next snapshot must flip.
	require.NoError(t, os.MkdirAll(remoteaccess.Dir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(remoteaccess.Dir(), "ca.crt"), []byte("stub"), 0o600))

	snap = fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.True(t, snap.RemoteAccess.MaterialExists,
		"material_exists must track the filesystem live so the UI clears its warning after setup")
}
