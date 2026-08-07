package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The assignment writes one POSTURE, not one field: an implementation recorded
// beside the mode it replaced would describe a launch that never happens.
func TestAssignAgentSandboxImplementationRewritesTheWholePosture(t *testing.T) {
	setupTestDB(t)
	const convID = "assign-posture-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	mode := "on"
	implementation := string(sandboxpolicy.ImplementationHarnessBuiltin)
	source := `group default profile "confined"`
	approval := "default"
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, HarnessBuiltinMode: &mode,
		SandboxImplementation:    &implementation,
		HarnessBuiltinModeSource: &source, ApprovalPolicy: &approval,
	}))

	require.NoError(t, AssignAgentSandboxImplementation(
		agentID, string(sandboxpolicy.ImplementationResourceOnly), "off",
		AssignedSandboxImplementationSource))

	got, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SandboxImplementation)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly), *got.SandboxImplementation)
	require.NotNil(t, got.HarnessBuiltinMode)
	assert.Equal(t, "off", *got.HarnessBuiltinMode,
		"the mode moves with the implementation that decides it")
	require.NotNil(t, got.HarnessBuiltinModeSource)
	assert.Equal(t, AssignedSandboxImplementationSource, *got.HarnessBuiltinModeSource,
		"no spawn tier chose this mode; crediting the old one would be a false attribution")
	require.NotNil(t, got.ApprovalPolicy)
	assert.Equal(t, approval, *got.ApprovalPolicy,
		"nothing outside the sandbox posture may change")
}

// The reversible unlock preserves the normal posture underneath so restore can
// put it back byte-for-byte. Writing that posture from under an active override
// would silently change what restore restores.
func TestAssignAgentSandboxImplementationRefusesUnderTemporaryOverride(t *testing.T) {
	setupTestDB(t)
	const convID = "assign-under-unlock-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	mode := "on"
	implementation := string(sandboxpolicy.ImplementationTclaudeLayer)
	approval := "default"
	require.NoError(t, SetAgentRelaunchProfile(agentID, AgentRelaunchProfile{
		Version: RelaunchProfileVersion, HarnessBuiltinMode: &mode,
		SandboxImplementation: &implementation, ApprovalPolicy: &approval,
	}))
	override := "off"
	require.NoError(t, SetTemporaryHarnessBuiltinMode(
		agentID, mode, implementation, "", &override))

	err = AssignAgentSandboxImplementation(
		agentID, string(sandboxpolicy.ImplementationOff), "off",
		AssignedSandboxImplementationSource)
	assert.ErrorIs(t, err, ErrTemporarySandboxOverrideActive)

	got, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SandboxImplementation)
	assert.Equal(t, implementation, *got.SandboxImplementation,
		"the refused write must leave the preserved normal posture intact")
}

// The historical repair that recovers an implementation lost by the temporary
// unlock must not fire on an operator's deliberate later choice. Its damage
// fingerprint — a temporary-attributed harness-builtin row at the history tail —
// survives a restore whose relaunch failed, which is exactly when an operator
// reassigns the agent to harness-builtin.
func TestAssignedHarnessBuiltinSurvivesTheHistoricalImplementationRepair(t *testing.T) {
	setupTestDB(t)
	const convID = "assign-over-repair-conv"
	agentID, _, err := EnsureAgentForConv(convID, "test")
	require.NoError(t, err)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "layered-before-unlock", ConvID: convID, Cwd: t.TempDir(),
		Harness: DefaultHarness, Status: "exited", HarnessBuiltinMode: "on",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ApprovalPolicy:        "default", CreatedAt: time.Now().Add(-time.Minute),
	}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "temporary-unlocked", ConvID: convID, Cwd: t.TempDir(),
		Harness: DefaultHarness, Status: "exited", HarnessBuiltinMode: "off",
		SandboxImplementation:    string(sandboxpolicy.ImplementationHarnessBuiltin),
		HarnessBuiltinModeSource: TemporaryHarnessBuiltinModeSource,
		ApprovalPolicy:           "default", CreatedAt: time.Now(),
	}))
	require.NoError(t, AssignAgentSandboxImplementation(
		agentID, string(sandboxpolicy.ImplementationHarnessBuiltin), "on",
		AssignedSandboxImplementationSource))

	posture, err := AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	resolved, err := NormalSandboxImplementationForConv(convID, posture)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.ImplementationHarnessBuiltin, resolved,
		"an assignment is a choice, not damage; the repair must leave it alone")

	// The repair still works for the shape it exists for: the same history with
	// no assignment attribution recovers the layered implementation.
	unassigned := "harness-builtin"
	recovered, err := NormalSandboxImplementationForConv(convID, &AgentRelaunchProfile{
		Version: RelaunchProfileVersion, SandboxImplementation: &unassigned,
	})
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.ImplementationTclaudeLayer, recovered)
}
