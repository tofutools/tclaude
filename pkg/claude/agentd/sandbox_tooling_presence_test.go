package agentd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

// The reset contract that matters is not invalidateSandboxToolingPresence in
// isolation — it is that a REFUSED LAUNCH resumes checking. This drives the
// production refusal (sandboxImplementationHostFailure) rather than calling the
// invalidator directly, so a refusal path that forgot to invalidate fails here.
//
// It also describes the case the presence check deliberately cannot see: the
// tool is installed (presence succeeds) but the live probe refuses, which is
// exactly the "available means INSTALLED, not WORKING" gap.
func TestSandboxImplementationHostFailure_ResumesPresenceChecking(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	calls := 0
	present := func() error { calls++; return nil }

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, present))
	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, present))
	require.Equal(t, 1, calls, "the tool is installed, so the disclosure latches")

	previous := tclaudeLayerHostAvailability
	tclaudeLayerHostAvailability = func() error {
		return errors.New("unprivileged user namespaces are unavailable")
	}
	t.Cleanup(func() { tclaudeLayerHostAvailability = previous })

	failure := sandboxImplementationHostFailure(
		harness.DefaultName, string(sandboxpolicy.ImplementationTclaudeLayer))
	require.NotNil(t, failure, "the live probe refuses even though the tool is present")
	assert.Equal(t, sandboxImplementationUnavailableKind, failure.Kind)

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, present))
	assert.Equal(t, 2, calls,
		"the refused launch dropped the latch, so the next poll looks again")
}

// A probe already in flight when a launch refuses must not write its
// pre-invalidation observation afterwards — that would resurrect the latch the
// refusal just dropped, and with no TTL the disclosure would stay green until
// the next refused launch.
func TestSandboxToolPresence_InFlightProbeCannotResurrectLatch(t *testing.T) {
	resetSandboxImplHostProbeCache()
	t.Cleanup(resetSandboxImplHostProbeCache)

	calls := 0
	// The probe observes "present", but an invalidation lands while it runs.
	racy := func() error {
		calls++
		if calls == 1 {
			invalidateSandboxToolingPresence()
		}
		return nil
	}

	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, racy))
	require.NoError(t, sandboxToolPresence(sandboxToolLayerHost, racy))
	assert.Equal(t, 2, calls,
		"the stale observation must not latch; the next poll re-checks")
}
