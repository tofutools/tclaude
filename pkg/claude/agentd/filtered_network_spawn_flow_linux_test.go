//go:build linux

package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestFilteredNetworkSpawnRefusesUnresolvedModelTransport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		harnessName string
		sandboxMode string
	}{
		{name: "claude", harnessName: harness.DefaultName, sandboxMode: harness.ClaudeSandboxOff},
		{name: "codex", harnessName: harness.CodexName, sandboxMode: harness.SandboxReadOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(
				func() session.FilteredNetworkPrerequisite {
					return session.FilteredNetworkPrerequisite{
						Detected: true,
						Detail:   "test namespace, pasta, and nft readiness",
					}
				},
			))
			t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
				func(_ string, posture sandboxpolicy.NetworkPosture) (harness.LaunchOSSandbox, error) {
					require.Equal(t, sandboxpolicy.NetworkFiltered, posture)
					return harness.LaunchOSSandbox{
						State:           "on",
						Source:          "test filtered gateway",
						FilteredNetwork: true,
					}, nil
				},
			))
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: "filtered",
				Network: &sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeList,
					Allow: []sandboxpolicy.NetworkAllowEntry{{
						CIDR: "192.0.2.0/24", Ports: []int{443},
					}},
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   "worker",
				"harness":                tc.harnessName,
				"sandbox":                tc.sandboxMode,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        "filtered",
			})
			require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
				"spawn body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Equal(t, harness.SandboxCapabilityModelTransport, failure.Code)
			assert.Contains(t, failure.Error, "model provider configuration was not resolved")
			assert.Contains(t, failure.Error, "use network open")
			assert.Empty(t, resp.ConvID, "refusal must occur before spawning")
		})
	}
}
