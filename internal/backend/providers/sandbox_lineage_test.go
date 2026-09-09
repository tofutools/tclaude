package providers_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
)

func TestSandboxLineageRequiresRecordedOuterWall(t *testing.T) {
	providers := map[string]ports.SandboxLineageProvider{"claude": &claude.Provider{}, "codex": &codex.Provider{}, "copilot": &copilot.Provider{}, "opencode": &opencode.Provider{}}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			mode := model.SandboxUnconfined
			if name == "claude" {
				mode = model.SandboxWorkspaceWrite
			}
			spec := model.ResolvedExecutionSpec{ExecutionID: "parent", Harness: name, Sandbox: mode, HostSandbox: &model.SandboxSelection{PolicyHash: strings.Repeat("a", 64)}}
			expected := provider.RequestedSandboxPosture(spec)
			require.True(t, expected.Known())
			version := uint32(1)
			if name == "opencode" {
				version = 2
			}
			record := map[string]any{"execution_id": "parent", "host_sandbox_policy_hash": spec.HostSandbox.PolicyHash, "host_sandbox": map[string]string{"Path": "/disposable-fixture/descriptor", "Digest": strings.Repeat("b", 64)}}
			execution := func() model.Execution {
				raw, err := json.Marshal(record)
				require.NoError(t, err)
				return model.Execution{ID: "parent", Spec: spec, Evidence: model.ProviderEvidence{Provider: name, Version: version, Payload: raw}}
			}
			require.Equal(t, expected, provider.RecordedSandboxPosture(execution()))
			bad := execution()
			bad.Evidence.Version = 999
			require.False(t, provider.RecordedSandboxPosture(bad).Known())
			bad = execution()
			bad.Evidence.Provider = "foreign"
			require.False(t, provider.RecordedSandboxPosture(bad).Known())
			bad = execution()
			bad.ID = "other"
			require.False(t, provider.RecordedSandboxPosture(bad).Known())
			bad = execution()
			bad.Spec.HostSandbox = nil
			require.False(t, provider.RecordedSandboxPosture(bad).Known())
			record["host_sandbox_policy_hash"] = strings.Repeat("c", 64)
			require.False(t, provider.RecordedSandboxPosture(execution()).Known())
			record["host_sandbox_policy_hash"] = spec.HostSandbox.PolicyHash
			delete(record, "host_sandbox")
			require.False(t, provider.RecordedSandboxPosture(execution()).Known(), "selected profile alone does not prove the wrapper was prepared")
			spec.HostSandbox = &model.SandboxSelection{}
			require.False(t, provider.RequestedSandboxPosture(spec).Known(), "saved profile choices are not resolved launch evidence")
		})
	}
}

func TestSandboxLineagePreservesNativeAndHostDistinction(t *testing.T) {
	codexProvider := &codex.Provider{}
	spec := model.ResolvedExecutionSpec{Harness: "codex", Sandbox: model.SandboxUnconfined}
	open := codexProvider.RequestedSandboxPosture(spec)
	spec.HostSandbox = &model.SandboxSelection{PolicyHash: strings.Repeat("a", 64)}
	managed := codexProvider.RequestedSandboxPosture(spec)
	require.Equal(t, model.SandboxPostureUnconfined, open)
	require.Equal(t, model.SandboxPostureCodexManaged, managed)
	require.False(t, managed.Allows(open), "native off under an outer wall is not unconfined authority")
	spec.Sandbox = model.SandboxReadOnly
	require.Equal(t, model.SandboxPostureCodexReadOnly, codexProvider.RequestedSandboxPosture(spec), "an additional wrapper does not disable native read-only mode")
	spec.Sandbox = model.SandboxWorkspaceWrite
	require.Equal(t, model.SandboxPostureCodexWorkspace, codexProvider.RequestedSandboxPosture(spec))
	require.False(t, model.SandboxPostureCodexWorkspace.Allows(managed))
	require.True(t, managed.Allows(model.SandboxPostureClaudeConfined))
	require.False(t, model.SandboxPostureCodexWorkspace.Allows(model.SandboxPostureClaudeConfined))
	copilotProvider := &copilot.Provider{}
	require.False(t, copilotProvider.RequestedSandboxPosture(model.ResolvedExecutionSpec{Harness: "copilot", Sandbox: model.SandboxUnconfined}).Known())
	require.False(t, model.SandboxPostureCopilotHost.Allows(model.SandboxPostureOpenCodeHost), "v1 does not equate terminal and server containment")
	require.True(t, model.SandboxPostureOpenCodeHost.Allows(model.SandboxPostureCopilotHost))
	require.False(t, model.SandboxPostureClaudeConfined.Allows(model.SandboxPostureClaudeInherited))
	require.True(t, model.SandboxPostureClaudeInherited.Allows(model.SandboxPostureClaudeConfined))
	require.False(t, model.SandboxPostureUnconfined.Allows(model.SandboxPostureUnknown))
	require.False(t, model.SandboxPosture("future").Allows(model.SandboxPostureCodexReadOnly))
}
