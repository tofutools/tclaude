package agentd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The disclosure's whole point is that a found tool costs nothing on the next
// poll and a missing one keeps being looked for. Both halves are counted here
// rather than timed, so the assertions hold on any machine.

func TestSandboxToolPresence_LatchesOnceFound(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	calls := 0
	probe := func() error { calls++; return nil }

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	assert.Equal(t, 1, calls, "the first read looks for the tool")

	for range 20 {
		require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	}
	assert.Equal(t, 1, calls,
		"a found tool is latched: 20 further polls must not touch the filesystem again")
}

func TestSandboxToolPresence_KeepsLookingWhileMissing(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	calls := 0
	missing := errors.New("bwrap is not on PATH")
	probe := func() error {
		calls++
		if calls < 3 {
			return missing
		}
		return nil // the operator installs it before the third poll
	}

	assert.ErrorIs(t, sandboxToolPresence(sandboxToolLayerHost, probe), missing)
	assert.ErrorIs(t, sandboxToolPresence(sandboxToolLayerHost, probe), missing)
	assert.Equal(t, 2, calls, "a missing tool is re-checked on every poll")

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe),
		"installing the tool mid-session clears the disclosure on the next poll")
	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	assert.Equal(t, 3, calls, "and it latches from there")
}

// A launch refusing on host capability is the only evidence the daemon gets
// that a latched-present tool may be gone — or that mere presence disagrees
// with the live probe. It must resume checking rather than serve a stale green.
func TestSandboxToolPresence_SpawnFailureResumesChecking(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	calls := 0
	probe := func() error { calls++; return nil }

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	require.Equal(t, 1, calls)

	invalidateSandboxToolingPresence()

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, probe))
	assert.Equal(t, 2, calls, "the latch is dropped, so the next poll looks again")
}

// Independent keys must not share a latch — finding bwrap cannot be allowed to
// imply the Claude engine is installed.
func TestSandboxToolPresence_KeysAreIndependent(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	missing := errors.New("codex is not on PATH")
	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, func() error { return nil }))
	assert.ErrorIs(t,
		sandboxToolPresence(stackedEngineToolKey("codex"), func() error { return missing }),
		missing)
}

// OpenCode owns no sandbox, so "stacked" is not a capability it can lack: it
// must be ABSENT from the catalog rather than listed as unavailable, which
// would read as a missing dependency the operator could install.
func TestSandboxImplCatalog_OmitsOpenCodeFromStacked(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	catalog := buildSandboxImplCatalog()
	assert.NotContains(t, catalog.Stacked, "opencode",
		"OpenCode has no nested sandbox; stacking is meaningless for it")
}
