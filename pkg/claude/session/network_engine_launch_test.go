package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func engineLaunchRules(engine sandboxpolicy.NetworkEngine) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		},
		Engine: engine,
	}
}

func engineLaunchSnapshot(
	engine sandboxpolicy.NetworkEngine,
) *sandboxpolicy.Snapshot {
	rules := engineLaunchRules(engine)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &rules
	return &snapshot
}

// TestLaunchSpecDerivesTheEngineFromTheProfile proves the launch contract reads
// the engine from composed policy rather than from a separate launch input. A
// call site that forgot to copy the field would otherwise run pre-engine
// behavior for a profile that authored an engine, with nothing in the preview
// to say so.
func TestLaunchSpecDerivesTheEngineFromTheProfile(t *testing.T) {
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude",
		Cwd:         t.TempDir(),
		StateRoot:   t.TempDir(),
		Snapshot:    engineLaunchSnapshot(sandboxpolicy.NetworkEngineProxy),
	})
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine)

	unset, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName: "claude",
		Cwd:         t.TempDir(),
		StateRoot:   t.TempDir(),
		Snapshot:    engineLaunchSnapshot(sandboxpolicy.NetworkEngineUnset),
	})
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineUnset, unset.Contract.NetworkEngine)
}

// TestLaunchSpecRefusesAnEngineTheProfileDoesNotAuthor keeps the derivation
// from being quietly overridable. A caller that names a different engine is
// about to launch something the operator did not author, so it fails loudly
// rather than being merged with.
func TestLaunchSpecRefusesAnEngineTheProfileDoesNotAuthor(t *testing.T) {
	_, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   "claude",
		Cwd:           t.TempDir(),
		StateRoot:     t.TempDir(),
		Snapshot:      engineLaunchSnapshot(sandboxpolicy.NetworkEnginePacket),
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the effective sandbox profile authors")

	// Agreement is fine: a caller repeating what the profile already says is
	// not a conflict.
	_, err = BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:   "claude",
		Cwd:           t.TempDir(),
		StateRoot:     t.TempDir(),
		Snapshot:      engineLaunchSnapshot(sandboxpolicy.NetworkEngineProxy),
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	})
	require.NoError(t, err)
}

// TestFilteredPrerequisiteNoticeFollowsTheEngine is the first carried item. The
// probe this notice reports is the packet gateway's pasta/nft check; the proxy
// engine reaches its floor through a plain unshare and has none of those
// prerequisites, so disclosing them would name a launch gate that does not gate
// this launch.
func TestFilteredPrerequisiteNoticeFollowsTheEngine(t *testing.T) {
	probe := func() FilteredNetworkPrerequisite {
		return FilteredNetworkPrerequisite{
			Detected: true,
			Detail:   "namespace execution passed; executables found",
		}
	}
	packet := appendFilteredNetworkPrerequisiteNotice(
		nil, true, engineLaunchRules(sandboxpolicy.NetworkEnginePacket), true, probe)
	require.Len(t, packet, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredPrerequisite,
		packet[0].Reason)

	unset := appendFilteredNetworkPrerequisiteNotice(
		nil, true, engineLaunchRules(sandboxpolicy.NetworkEngineUnset), true, probe)
	require.Len(t, unset, 1,
		"an unset engine keeps the packet gateway and its prerequisites")

	proxy := appendFilteredNetworkPrerequisiteNotice(
		nil, true, engineLaunchRules(sandboxpolicy.NetworkEngineProxy), true,
		func() FilteredNetworkPrerequisite {
			t.Fatal("the proxy engine must not run the packet gateway's prerequisite probe")
			return FilteredNetworkPrerequisite{}
		})
	assert.Empty(t, proxy)
}

// TestLoopbackProviderPreflightFollowsTheEngine is the third carried item. The
// synthetic host-loopback name is a packet-engine mapping; under the proxy
// engine the loopback selector means localhost through the proxy on both
// platforms, so the two spellings swap.
func TestLoopbackProviderPreflightFollowsTheEngine(t *testing.T) {
	localhost := harness.ResolvedModelTransport{
		Model:            "local/llama",
		Provider:         "ollama",
		BaseURL:          "http://localhost:11434/v1",
		ProviderResolved: true,
	}
	synthetic := harness.ResolvedModelTransport{
		Model:            "local/llama",
		Provider:         "ollama",
		BaseURL:          "http://host.tclaude.internal:11434/v1",
		ProviderResolved: true,
	}

	// Packet engine, unchanged: localhost is sandbox-private on Linux and the
	// remedy is the synthetic name.
	packetErr := validateModelTransportLoopbackForPlatform(
		harness.Default(), localhost, "linux", sandboxpolicy.NetworkEnginePacket)
	require.Error(t, packetErr)
	assert.Contains(t, packetErr.Error(),
		sandboxpolicy.FilteredNetworkHostLoopbackName)

	// Proxy engine: localhost is correct on both platforms and needs no remedy.
	for _, goos := range []string{"linux", "darwin"} {
		require.NoError(t, validateModelTransportLoopbackForPlatform(
			harness.Default(), localhost, goos, sandboxpolicy.NetworkEngineProxy),
			"localhost is the authored spelling under the proxy engine on %s", goos)
	}

	// And the synthetic name, which only the packet engine installs, is now
	// wrong on Linux too rather than only on Darwin.
	for _, goos := range []string{"linux", "darwin"} {
		syntheticErr := validateModelTransportLoopbackForPlatform(
			harness.Default(), synthetic, goos, sandboxpolicy.NetworkEngineProxy)
		require.Errorf(t, syntheticErr,
			"the synthetic name has no mapping under the proxy engine on %s", goos)
		assert.Contains(t, syntheticErr.Error(), "only the packet filtering engine installs")
		assert.Contains(t, syntheticErr.Error(), "localhost")
		assert.Contains(t, syntheticErr.Error(), "loopback rule")
	}
}

// TestModelTransportRequirementDescribesTheEngineSpelling keeps the requirement
// text an operator is asked to satisfy in the same spelling the preflight above
// will accept.
func TestModelTransportRequirementDescribesTheEngineSpelling(t *testing.T) {
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
	requirement := harness.ModelTransportRequirement{
		Destinations: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{11434}},
		},
	}
	detail := describeModelTransportRequirementForPlatform(
		rules, requirement, "linux")
	assert.Contains(t, detail, "localhost:11434")
	assert.NotContains(t, detail, sandboxpolicy.FilteredNetworkHostLoopbackName)

	rules.Engine = sandboxpolicy.NetworkEnginePacket
	packetDetail := describeModelTransportRequirementForPlatform(
		rules, requirement, "linux")
	assert.Contains(t, packetDetail, sandboxpolicy.FilteredNetworkHostLoopbackName)
}
