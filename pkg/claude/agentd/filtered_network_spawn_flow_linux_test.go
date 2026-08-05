//go:build linux

package agentd_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDefaultAllowDenySpawnSelectsFilteredNetworkPosture(t *testing.T) {
	testharness.ClearModelTransportProxyEnv(t)
	for _, tc := range []struct {
		name               string
		harnessName        string
		harnessBuiltinMode string
	}{
		{
			name:               "claude",
			harnessName:        harness.DefaultName,
			harnessBuiltinMode: harness.ClaudeSandboxOff,
		},
		{
			name:               "codex",
			harnessName:        harness.CodexName,
			harnessBuiltinMode: harness.SandboxReadOnly,
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
			var resolvedPostures []sandboxpolicy.NetworkPosture
			t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
				func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
					_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
					resolvedPostures = append(resolvedPostures, posture)
					return harness.LaunchOSSandbox{
						State:           "on",
						Source:          "test filtered gateway",
						FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
					}, nil
				},
			))
			if tc.harnessName == harness.CodexName {
				codexHome := t.TempDir()
				require.NoError(t, os.WriteFile(
					filepath.Join(codexHome, "auth.json"),
					[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
					0o600))
				t.Setenv("CODEX_HOME", codexHome)
			}
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: "default-allow-deny",
				Network: &sandboxpolicy.NetworkRules{
					Baseline: sandboxpolicy.NetworkBaselineAllow,
					Deny: []sandboxpolicy.NetworkAllowEntry{{
						CIDR: "192.0.2.0/24", Ports: []int{443},
					}},
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				// An empty cwd makes Claude provider inspection read the
				// REPOSITORY's own .claude settings — host state this test
				// does not author, and which an agent sandbox may deny
				// reading outright. A temp dir keeps the launch judged on
				// the authored profile alone.
				"cwd":                    t.TempDir(),
				"name":                   tc.name + "-worker",
				"harness":                tc.harnessName,
				"sandbox":                tc.harnessBuiltinMode,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        "default-allow-deny",
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
			require.NotEmpty(t, resp.ConvID)
			require.NotEmpty(t, resolvedPostures)
			for _, posture := range resolvedPostures {
				require.Equal(t, sandboxpolicy.NetworkFiltered, posture,
					"open+deny must enter the filtered launch path, never host-open")
			}

			snapshot, ok := f.World.SpawnSandboxPolicy(resp.ConvID)
			require.True(t, ok)
			require.NotNil(t, snapshot)
			require.NotNil(t, snapshot.Effective.Network)
			require.Equal(t, sandboxpolicy.AccessModeOpen, snapshot.Effective.Network.Mode)
			require.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
				CIDR: "192.0.2.0/24", Ports: []int{443},
			}}, snapshot.Effective.Network.Deny)
			var prerequisite *sandboxpolicy.AccessNotice
			for i := range snapshot.Effective.AccessNotices {
				if snapshot.Effective.AccessNotices[i].Reason ==
					sandboxpolicy.AccessNoticeReasonFilteredPrerequisite {
					prerequisite = &snapshot.Effective.AccessNotices[i]
					break
				}
			}
			require.NotNil(t, prerequisite)
			assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated,
				prerequisite.Effect)
			assert.Contains(t, prerequisite.Detail, "atomic nft policy")
			assert.NotContains(t, prerequisite.Detail, "outbound remains open")
		})
	}
}

// TestOpenCodeDefaultAllowDenyRefusesWithoutExplicitProvider records the honest
// cost of that activation: an OpenCode deny can no longer be dropped to keep a
// launch alive, so a profile OpenCode cannot filter is refused rather than
// started with the deny silently omitted.
func TestOpenCodeDefaultAllowDenyRefusesWithoutExplicitProvider(t *testing.T) {
	testharness.ClearModelTransportProxyEnv(t)
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
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			return harness.LaunchOSSandbox{
				State:           "on",
				Source:          "test filtered gateway",
				FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
			}, nil
		},
	))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "opencode-default-allow-deny-no-provider",
		Network: &sandboxpolicy.NetworkRules{
			Baseline: sandboxpolicy.NetworkBaselineAllow,
			Deny: []sandboxpolicy.NetworkAllowEntry{{
				CIDR: "192.0.2.0/24", Ports: []int{443},
			}},
		},
	})
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "opencode-worker",
		"harness":                harness.OpenCodeName,
		"sandbox":                harness.OpenCodeSandboxTclaudeLayer,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		"sandbox_profile":        "opencode-default-allow-deny-no-provider",
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code)
	body := string(resp.Raw)
	assert.Contains(t, body, "unsupported_filtered_model_transport")
	// The refusal must name what forced the filtered gateway, not only the
	// model transport the operator never asked about.
	assert.Contains(t, body, "open apart from 1 enforced deny rule")
	assert.Contains(t, body, "remove the deny rules to launch with open network")
	assert.Contains(t, body, "explicit provider/model launch model")
}

func TestLocalAccessSpawnRefusesCloudModelWithoutExplicitEndpoint(t *testing.T) {
	testharness.ClearModelTransportProxyEnv(t)
	for _, tc := range []struct {
		name               string
		harnessName        string
		harnessBuiltinMode string
		endpoint           string
	}{
		{
			name: "claude", harnessName: harness.DefaultName,
			harnessBuiltinMode: harness.ClaudeSandboxOff,
			endpoint:           "api.anthropic.com:443",
		},
		{
			name: "codex", harnessName: harness.CodexName,
			harnessBuiltinMode: harness.SandboxReadOnly,
			endpoint:           "api.openai.com:443",
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
				func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
					_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
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
				// An empty cwd makes Claude provider inspection read the
				// REPOSITORY's own .claude settings — host state this test
				// does not author, and which an agent sandbox may deny
				// reading outright. A temp dir keeps the launch judged on
				// the authored profile alone.
				"cwd":                    t.TempDir(),
				"name":                   "worker",
				"harness":                tc.harnessName,
				"sandbox":                tc.harnessBuiltinMode,
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
	testharness.ClearModelTransportProxyEnv(t)
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
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
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
		// An empty cwd makes Claude provider inspection read the
		// REPOSITORY's own .claude settings — host state this test
		// does not author, and which an agent sandbox may deny
		// reading outright. A temp dir keeps the launch judged on
		// the authored profile alone.
		"cwd":                    t.TempDir(),
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
	testharness.ClearModelTransportProxyEnv(t)
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
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
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
		// An empty cwd makes Claude provider inspection read the
		// REPOSITORY's own .claude settings — host state this test
		// does not author, and which an agent sandbox may deny
		// reading outright. A temp dir keeps the launch judged on
		// the authored profile alone.
		"cwd":                    t.TempDir(),
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
			assert.Contains(t, failure.Error, "no explicit provider")
			assert.Contains(t, failure.Error, "network open")
			assert.Empty(t, resp.ConvID)
		})
	}
}

// TCL-895 at the surface an operator actually meets. The seam test above pins
// the PACKET gateway's refusal; this one pins what changed — a local preset
// that deploys an activated proxy engine is no longer refused for the packet
// gateway's reason, and is still refused for the engine-independent one.
//
// The two messages are the whole point: "these presets name no explicit
// provider" describes machinery a proxy launch never runs, while "requires an
// explicit provider/model launch model" is the contract that still binds it.
func TestLocalPresetsOpenCodeProxyEngineRefusesForTheProviderNotThePreset(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const profileName = "opencode-local-model-apis-proxy"
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: profileName,
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.anthropic.com", Ports: []int{443}},
				{Domain: "api.openai.com", Ports: []int{443}},
				{Loopback: true},
			},
			Engine: sandboxpolicy.NetworkEngineProxy,
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
	// The load-bearing half, and it holds on ANY host: whatever refuses this
	// launch, it is no longer the packet gateway's preset refusal. Revert the
	// gate and this is exactly the message that comes back.
	assert.NotContains(t, failure.Error, "local presets name no explicit provider",
		"the packet gateway's preset refusal must not be rendered for a proxy launch")
	assert.Empty(t, resp.ConvID)

	// The other half needs the launch to get PAST the floor, and the ordinary
	// test job has no bubblewrap. Asserting it unconditionally would make this
	// test depend on a host capability it is not about, so the floor's own
	// refusal is recognized and reported rather than silently accepted.
	if strings.Contains(failure.Error, "bwrap") ||
		strings.Contains(failure.Error, "bubblewrap") ||
		strings.Contains(failure.Error, "user namespaces") {
		t.Logf("floor unavailable on this host, so only the preset-refusal half is asserted: %s",
			failure.Error)
		return
	}
	assert.Equal(t, harness.SandboxCapabilityModelTransport, failure.Code)
	assert.Contains(t, failure.Error, "explicit provider/model launch model",
		"the engine-independent model-transport contract still binds this launch")
}

// TestProxyEngineSpawnOmitsThePacketPrerequisiteNotice is the daemon-spawn half
// of the engine-conditional prerequisite disclosure. The session boundary and
// this guard both append that notice, so a gate applied at only one of them
// would let a dashboard-spawned agent persist a launch-gate claim naming pasta
// and nft while the /enforcement preview for the same profile says the opposite.
func TestProxyEngineSpawnOmitsThePacketPrerequisiteNotice(t *testing.T) {
	testharness.ClearModelTransportProxyEnv(t)
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
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			return harness.LaunchOSSandbox{
				State:           "on",
				Source:          "test filtered gateway",
				FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
			}, nil
		},
	))
	// Codex rather than Claude Code, for the same reason the sibling test above
	// carries a CODEX_HOME: the control cases must reach the filtered launch
	// path, and Claude Code's provider inspection reads repository settings
	// that a developer machine may not expose. The guard under test is
	// harness-independent.
	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
		0o600))
	t.Setenv("CODEX_HOME", codexHome)
	for _, tc := range []struct {
		profile    string
		engine     sandboxpolicy.NetworkEngine
		wantNotice bool
	}{
		{"engine-unset-deny", sandboxpolicy.NetworkEngineUnset, true},
		{"engine-packet-deny", sandboxpolicy.NetworkEnginePacket, true},
		{"engine-proxy-deny", sandboxpolicy.NetworkEngineProxy, false},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: tc.profile,
				Network: &sandboxpolicy.NetworkRules{
					Baseline: sandboxpolicy.NetworkBaselineAllow,
					Deny: []sandboxpolicy.NetworkAllowEntry{{
						CIDR: "192.0.2.0/24", Ports: []int{443},
					}},
					Engine: tc.engine,
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   tc.profile + "-worker",
				"harness":                harness.CodexName,
				"sandbox":                harness.SandboxReadOnly,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        tc.profile,
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			snapshot, ok := f.World.SpawnSandboxPolicy(resp.ConvID)
			require.True(t, ok)
			require.NotNil(t, snapshot)
			found := false
			for _, notice := range snapshot.Effective.AccessNotices {
				if notice.Reason == sandboxpolicy.AccessNoticeReasonFilteredPrerequisite {
					found = true
					assert.Contains(t, notice.Detail, "nft",
						"the packet prerequisite notice names the packet gateway's checks")
				}
			}
			assert.Equal(t, tc.wantNotice, found,
				"the packet gateway's prerequisite disclosure must follow the engine")
		})
	}
}

// TestProxyEngineSpawnDoesNotGateOnThePacketPrerequisite is TCL-883's second
// deliverable at the daemon-spawn seam: the proxy floor gets its own
// spawn-time gate.
//
// The probe faked here is the packet gateway's — pasta, nft, and the namespace
// privileges they need — and it reports UNAVAILABLE, which is the ordinary
// state of the low-prerequisite hosts §2.5 says the proxy engine exists to
// serve. Before this change that answer widened every filtered profile to open
// regardless of engine, so a proxy-engine profile was refused enforcement for a
// prerequisite its launch never calls. The proxy floor must instead be gated by
// the posture-exact verdict, which is asserted here by recording the posture
// the guard actually asked the resolver to verify.
func TestProxyEngineSpawnDoesNotGateOnThePacketPrerequisite(t *testing.T) {
	testharness.ClearModelTransportProxyEnv(t)
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(
		func() session.FilteredNetworkPrerequisite {
			return session.FilteredNetworkPrerequisite{
				Detected: false,
				Detail:   "test host without pasta or nft",
			}
		},
	))
	var requested []sandboxpolicy.NetworkPosture
	var requestedEngines []sandboxpolicy.NetworkEngine
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			engine sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			requested = append(requested, posture)
			requestedEngines = append(requestedEngines, engine)
			return harness.LaunchOSSandbox{
				State:           "on",
				Source:          "test proxy floor",
				FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
			}, nil
		},
	))
	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
		0o600))
	t.Setenv("CODEX_HOME", codexHome)

	for _, tc := range []struct {
		profile     string
		engine      sandboxpolicy.NetworkEngine
		wantPosture sandboxpolicy.NetworkPosture
		// The engine the guard must verify the host for is the DEPLOYED one,
		// which for an unset selection on a discriminating policy is the packet
		// gateway — the pre-engine default, unchanged.
		wantEngine sandboxpolicy.NetworkEngine
	}{
		// The proxy floor is reached even though pasta and nft are absent.
		{
			"gate-engine-proxy",
			sandboxpolicy.NetworkEngineProxy,
			sandboxpolicy.NetworkFiltered,
			sandboxpolicy.NetworkEngineProxy,
		},
		// The packet gateway keeps its own gate exactly as before: an absent
		// prerequisite still widens it to host-open. This is the parity half —
		// TCL-883 gives the proxy floor a gate, it does not remove anyone's.
		{
			"gate-engine-packet",
			sandboxpolicy.NetworkEnginePacket,
			sandboxpolicy.NetworkHostOpen,
			sandboxpolicy.NetworkEnginePacket,
		},
		{
			"gate-engine-unset",
			sandboxpolicy.NetworkEngineUnset,
			sandboxpolicy.NetworkHostOpen,
			sandboxpolicy.NetworkEnginePacket,
		},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			requested = nil
			requestedEngines = nil
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: tc.profile,
				Network: &sandboxpolicy.NetworkRules{
					Baseline: sandboxpolicy.NetworkBaselineAllow,
					Deny: []sandboxpolicy.NetworkAllowEntry{{
						CIDR: "192.0.2.0/24", Ports: []int{443},
					}},
					Engine: tc.engine,
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   tc.profile + "-worker",
				"harness":                harness.CodexName,
				"sandbox":                harness.SandboxReadOnly,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        tc.profile,
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			require.NotEmpty(t, requested,
				"the guard must verify the host for the floor it intends to build")
			assert.Equal(t, tc.wantPosture, requested[0],
				"the posture the guard verifies must follow the deployed engine, not the packet probe")
			// The engine has to reach the resolver too: the proxy floor maps onto
			// the isolated posture's construction, and a resolve that did not
			// know the engine would probe the packet gateway's prerequisites for
			// a launch that never calls them.
			assert.Equal(t, tc.wantEngine, requestedEngines[0])
		})
	}
}

// TestProxyEngineSpawnRefusesAResolverReachingFilesystemGrant walks TCL-883's
// refusal end to end, from an authored profile through the daemon's own spawn
// path. The unit tests cover each half — that the derived axes carry the
// effective filesystem, and that the capability seam refuses on it — and this
// one proves the halves are actually joined on the path an operator uses.
//
// /run is deliberately the authored grant rather than a resolver socket path:
// it exists on every Linux host, so the profile layer accepts it, and it is an
// ancestor of the known resolver sockets. It is also the honest demonstration
// of the refusal's breadth — a broad grant refuses the proxy engine even from
// an operator who never meant to reach a resolver, and the remedy text says so.
func TestProxyEngineSpawnRefusesAResolverReachingFilesystemGrant(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			return harness.LaunchOSSandbox{
				State:           "on",
				Source:          "test proxy floor",
				FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
			}, nil
		},
	))
	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
		0o600))
	t.Setenv("CODEX_HOME", codexHome)

	for _, tc := range []struct {
		profile     string
		engine      sandboxpolicy.NetworkEngine
		wantRefusal bool
	}{
		{"fs-resolver-proxy", sandboxpolicy.NetworkEngineProxy, true},
		// Parity: the packet gateway's DNS broker holds name authority with a
		// resolver socket present, so the identical grant takes nothing away.
		{"fs-resolver-packet", sandboxpolicy.NetworkEnginePacket, false},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: tc.profile,
				Filesystem: []db.SandboxFilesystemGrant{
					{Path: "/run", Access: "read"},
				},
				Network: &sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeList,
					Allow: []sandboxpolicy.NetworkAllowEntry{
						{Domain: "example.com", Ports: []int{443}},
					},
					Engine: tc.engine,
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   tc.profile + "-worker",
				"harness":                harness.CodexName,
				"sandbox":                harness.SandboxReadOnly,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        tc.profile,
			})
			if !tc.wantRefusal {
				require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
				return
			}
			require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
				"spawn body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Contains(t, failure.Error, "proxy_engine_name_authority")
			assert.Contains(t, failure.Error, "/run")
			assert.Contains(t, failure.Error, "Packet filter engine",
				"the refusal must carry its remedy all the way to the operator")
			assert.Empty(t, resp.ConvID)
		})
	}
}

// TestProxyEngineSpawnRefusesAHostThatCannotBuildTheFloor is the other half of
// the gate. Its sibling above proves the guard stops asking the PACKET
// gateway's question under the proxy engine; this proves the question it asks
// instead is a real one.
//
// Removing a gate is only safe if something else refuses. Here the
// posture-exact verdict — which maps the proxy floor onto the isolated
// posture's construction and probes bubblewrap's namespaces plus pidfd —
// reports that this host cannot build it. The spawn must be refused, not
// silently started with the network rules unenforced.
func TestProxyEngineSpawnRefusesAHostThatCannotBuildTheFloor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(
		func() session.FilteredNetworkPrerequisite {
			// Detected, so a residual dependence on the packet probe could not
			// be what refuses this spawn.
			return session.FilteredNetworkPrerequisite{
				Detected: true,
				Detail:   "test namespace, pasta, and nft readiness",
			}
		},
	))
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			if posture == sandboxpolicy.NetworkFiltered {
				return harness.LaunchOSSandbox{}, errors.New(
					"tclaude-layer cannot create the bubblewrap mount, network, and PID namespaces")
			}
			return harness.LaunchOSSandbox{
				State: "on", Source: "test host-open boundary",
			}, nil
		},
	))
	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(codexHome, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`),
		0o600))
	t.Setenv("CODEX_HOME", codexHome)

	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "floor-unavailable-proxy",
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "example.com", Ports: []int{443}},
			},
			Engine: sandboxpolicy.NetworkEngineProxy,
		},
	})
	require.NoError(t, err)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "floor-unavailable-worker",
		"harness":                harness.CodexName,
		"sandbox":                harness.SandboxReadOnly,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		"sandbox_profile":        "floor-unavailable-proxy",
	})
	require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
		"a host that cannot build the proxy floor must refuse, not launch unfiltered; body=%s",
		resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Contains(t, failure.Error, "bubblewrap")
	assert.Empty(t, resp.ConvID)
}

// TestProxyEngineSpawnDisclosesTheNoProxyOverride is §7.4 at the surface an
// operator reads. The launcher's override of an inherited NO_PROXY is merged
// behavior; what this proves is that it reaches the persisted access notices,
// and only on the launches that actually perform it.
//
// The cases are chosen so a disclosure that fired on the wrong condition cannot
// pass: the packet engine performs no override and must stay silent with the
// same host value set, and the proxy engine must stay silent when the host
// carried no exemption to discard.
//
// Claude Code rather than Codex, unlike the sibling tests above: the proxy
// engine's capability cells are activated per harness, and a launch whose cells
// are unactivated widens to open and deploys no proxy — so there would be no
// override to disclose.
func TestProxyEngineSpawnDisclosesTheNoProxyOverride(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	// The ROUTING variables refuse a filtered launch outright (§7.3), so a
	// runner that exports them would fail this test for an unrelated reason.
	// Clearing them is what leaves NO_PROXY as the only proxy variable under
	// test; it is deliberately NOT cleared, because it is the input — and the
	// shared helper does not touch it.
	testharness.ClearModelTransportProxyEnv(t)
	t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(
		func() session.FilteredNetworkPrerequisite {
			return session.FilteredNetworkPrerequisite{
				Detected: true,
				Detail:   "test namespace, pasta, and nft readiness",
			}
		},
	))
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(
		func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
			_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
			return harness.LaunchOSSandbox{
				State:           "on",
				Source:          "test filtered gateway",
				FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
			}, nil
		},
	))

	for _, tc := range []struct {
		profile    string
		engine     sandboxpolicy.NetworkEngine
		noProxy    string
		wantNotice bool
	}{
		{"no-proxy-proxy-engine", sandboxpolicy.NetworkEngineProxy,
			"internal.example,10.0.0.0/8", true},
		{"no-proxy-packet-engine", sandboxpolicy.NetworkEnginePacket,
			"internal.example,10.0.0.0/8", false},
		{"no-proxy-absent", sandboxpolicy.NetworkEngineProxy, "", false},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			t.Setenv("NO_PROXY", tc.noProxy)
			t.Setenv("no_proxy", "")
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: tc.profile,
				Network: &sandboxpolicy.NetworkRules{
					Mode: sandboxpolicy.AccessModeList,
					Allow: []sandboxpolicy.NetworkAllowEntry{
						{Domain: "example.com", Ports: []int{443}},
						// The launch's own model destination: a filtered
						// profile that does not cover it is refused before any
						// notice is recorded.
						{Host: "api.anthropic.com", Ports: []int{443}},
					},
					Engine: tc.engine,
				},
			})
			require.NoError(t, err)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name": tc.profile + "-worker",
				// An empty cwd: Claude's provider inspection reads the
				// repository's own .claude settings, which is host state this
				// test does not author.
				"cwd":                    t.TempDir(),
				"harness":                harness.DefaultName,
				"sandbox":                harness.ClaudeSandboxOff,
				"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
				"sandbox_profile":        tc.profile,
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			snapshot, ok := f.World.SpawnSandboxPolicy(resp.ConvID)
			require.True(t, ok)
			require.NotNil(t, snapshot)
			found := false
			for _, notice := range snapshot.Effective.AccessNotices {
				if notice.Reason ==
					sandboxpolicy.AccessNoticeReasonProxyEngineNoProxyOverride {
					found = true
					assert.Equal(t,
						sandboxpolicy.AccessNoticeEffectEnvironmentOverridden,
						notice.Effect)
					assert.Contains(t, notice.Detail, "NO_PROXY")
					assert.Contains(t, notice.Detail, "fails closed")
				}
			}
			assert.Equal(t, tc.wantNotice, found,
				"the NO_PROXY override disclosure must follow the launches that perform it")
		})
	}
}
