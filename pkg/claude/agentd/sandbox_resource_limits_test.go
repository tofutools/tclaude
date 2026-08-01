package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestSandboxResourceLimitPredictionCompatibility(t *testing.T) {
	h := harness.MustGet(harness.DefaultName)
	profile := sandboxpolicy.Profile{
		Name: "limited", ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "4GiB"},
	}
	linux := parsedSandboxProfileEnforcementTarget{
		implementation: sandboxpolicy.ImplementationHarnessBuiltin,
		harness:        h, platform: "linux",
	}
	assert.Nil(t, sandboxResourceLimitRefusal(profile, linux))

	darwin := linux
	darwin.platform = "darwin"
	refusal := sandboxResourceLimitRefusal(profile, darwin)
	require.NotNil(t, refusal)
	assert.Equal(t, "unsupported_resource_limits", refusal.Kind)
	assert.Contains(t, refusal.Message, "Linux only")

	off := linux
	off.implementation = sandboxpolicy.ImplementationOff
	refusal = sandboxResourceLimitRefusal(profile, off)
	require.NotNil(t, refusal)
	assert.Contains(t, refusal.Message, "implementation off")

	assert.Nil(t, sandboxResourceLimitRefusal(sandboxpolicy.Profile{Name: "blank"}, darwin),
		"blank profiles do not acquire a platform gate")
}

func TestResourceLimitOperatorOverrideNoticeIsRecorded(t *testing.T) {
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "1GiB", MemoryBytes: 1 << 30},
	}, nil)
	notices, failure := planSandboxProfileAccessForLaunch(
		harness.DefaultName, harness.ClaudeSandboxOn, &snapshot,
		string(sandboxpolicy.ImplementationHarnessBuiltin),
		session.ModelTransportLaunchContext{}, true,
	)
	// This host is Linux, so there is nothing to override at the product
	// compatibility layer; the live controller probe belongs to session new.
	require.Nil(t, failure)
	assert.Empty(t, notices)
}
