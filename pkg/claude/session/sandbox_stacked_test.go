package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestValidateStackedSandboxHarnessRefusesMissingContract(t *testing.T) {
	err := ValidateStackedSandboxHarness(harness.MustGet(harness.OpenCodeName))
	require.Error(t, err)
	assert.Equal(t,
		"stacked requested — refused: missing capability stacked_inner_harness_sandbox: "+
			"harness \"opencode\" has no reviewed nested OS-sandbox contract; "+
			"refusing rather than falling back to tclaude-layer or harness-builtin",
		err.Error())
}

func TestStackedSandboxLaunchModeRefusesUnknownHarness(t *testing.T) {
	_, _, err := stackedSandboxLaunchMode(&harness.Harness{Name: "future"})
	require.ErrorContains(t, err, "no reviewed inner sandbox forcing")
	require.ErrorContains(t, err, "refusing rather than falling back")
}

func TestWaitForStackedBindingReadinessRetriesPartialToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	require.NoError(t, os.WriteFile(path, []byte("rea"), 0o600))
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = os.WriteFile(path, []byte("ready\n"), 0o600)
	}()
	require.NoError(t, WaitForStackedBindingReadiness(path))
}

func TestStackedProbeOuterEnvironmentScrubsExportedFunctionCanary(t *testing.T) {
	t.Setenv("BASH_FUNC_grep%%", "() { return 0; }")
	t.Setenv("TCLAUDE_STACKED_CREDENTIAL_CANARY", "must-not-cross")

	assert.Equal(
		t,
		[]string{"PATH=" + stackedProbeOuterSystemPath},
		stackedProbeOuterEnvironment(),
	)
}

func TestStackedLaunchOSSandboxNamesBothMechanisms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		harness    string
		posture    sandboxpolicy.NetworkPosture
		wantSource string
		unverified bool
	}{
		{
			name: "claude host open", harness: harness.DefaultName,
			posture: sandboxpolicy.NetworkHostOpen,
			wantSource: "Stacked: tclaude bwrap (host-open; ambient host Unix sockets reachable) + " +
				"Claude SRT bwrap/seccomp",
			unverified: true,
		},
		{
			name: "claude isolated", harness: harness.DefaultName,
			posture: sandboxpolicy.NetworkIsolatedWithAgentd,
			wantSource: "Stacked: tclaude bwrap (isolated network/PIDs; constructed root) + " +
				"Claude SRT bwrap/seccomp",
		},
		{
			name: "codex host open", harness: harness.CodexName,
			posture: sandboxpolicy.NetworkHostOpen,
			wantSource: "Stacked: tclaude bwrap (host-open; ambient host Unix sockets reachable) + " +
				"Codex bwrap managed profile",
			unverified: true,
		},
		{
			name: "codex isolated", harness: harness.CodexName,
			posture: sandboxpolicy.NetworkIsolatedWithAgentd,
			wantSource: "Stacked: tclaude bwrap (isolated network/PIDs; constructed root) + " +
				"Codex bwrap managed profile",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StackedLaunchOSSandbox(harness.MustGet(tc.harness), tc.posture)
			assert.Equal(t, "on", got.State)
			assert.Equal(t, tc.wantSource, got.Source)
			assert.Equal(t, tc.unverified, got.Unverified)
		})
	}
}
