//go:build linux

package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestLocalAccessSpawnRefusesCloudModelWithoutExplicitEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		harnessName string
		sandboxMode string
		endpoint    string
	}{
		{
			name: "claude", harnessName: harness.DefaultName,
			sandboxMode: harness.ClaudeSandboxOff,
			endpoint:    "api.anthropic.com:443",
		},
		{
			name: "codex", harnessName: harness.CodexName,
			sandboxMode: harness.SandboxReadOnly,
			endpoint:    "api.openai.com:443",
		},
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
			profile := &db.SandboxProfile{
				Name: "local-access",
				Network: &sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeList,
					Allow: []sandboxpolicy.NetworkAllowEntry{{
						Loopback: true,
					}},
				},
			}
			if tc.harnessName == harness.CodexName {
				codexHome := t.TempDir()
				require.NoError(t, os.WriteFile(
					filepath.Join(codexHome, "auth.json"),
					[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
					0o600))
				t.Setenv("CODEX_HOME", codexHome)
			}
			_, err := db.CreateSandboxProfile(profile)
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   "worker",
				"harness":                tc.harnessName,
				"sandbox":                tc.sandboxMode,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        "local-access",
			})
			require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
				"spawn body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Equal(t, harness.SandboxCapabilityModelTransport, failure.Code)
			assert.Contains(t, failure.Error, tc.endpoint)
			assert.Contains(t, failure.Error, "no hidden model-traffic bypass")
			assert.Contains(t, failure.Error, "use network open")
			assert.Empty(t, resp.ConvID, "refusal must occur before spawning")
		})
	}
}

func TestLocalAccessSpawnAllowsConcreteHostLoopbackProvider(t *testing.T) {
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
		Name: "local-provider",
		Environment: []sandboxpolicy.EnvironmentEntry{{
			Name: "ANTHROPIC_BASE_URL", Value: "http://host.tclaude.internal:11434/v1",
		}},
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true,
			}},
		},
	})
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "local-worker",
		"harness":                harness.DefaultName,
		"sandbox":                harness.ClaudeSandboxOff,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		"sandbox_profile":        "local-provider",
	})
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	require.NotEmpty(t, resp.ConvID)
}

func TestLocalModelAPIsSpawnAllowsFirstPartyCloudProvider(t *testing.T) {
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
		Name: "local-model-apis",
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.anthropic.com", Ports: []int{443}},
				{Domain: "api.openai.com", Ports: []int{443}},
				{Loopback: true},
			},
		},
	})
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "cloud-worker",
		"harness":                harness.DefaultName,
		"sandbox":                harness.ClaudeSandboxOff,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		"sandbox_profile":        "local-model-apis",
	})
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	require.NotEmpty(t, resp.ConvID)
}

func TestLocalPresetsOpenCodeRefuseAtNamedModelTransportSeam(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []sandboxpolicy.NetworkAllowEntry
	}{
		{
			name:  "local-access",
			allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		},
		{
			name: "local-model-apis",
			allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.anthropic.com", Ports: []int{443}},
				{Domain: "api.openai.com", Ports: []int{443}},
				{Loopback: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			profileName := "opencode-" + tc.name
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: profileName,
				Network: &sandboxpolicy.NetworkRules{
					Mode:  sandboxpolicy.AccessModeList,
					Allow: tc.allow,
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   profileName + "-worker",
				"harness":                harness.OpenCodeName,
				"sandbox":                harness.OpenCodeSandboxTclaudeLayer,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        profileName,
			})
			require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
				"spawn body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Equal(t, harness.SandboxCapabilityModelTransport, failure.Code)
			assert.Contains(t, failure.Error, "OpenCode")
			assert.Contains(t, failure.Error, "TCL-826")
			assert.Contains(t, failure.Error, "network open")
			assert.Empty(t, resp.ConvID)
		})
	}
}
