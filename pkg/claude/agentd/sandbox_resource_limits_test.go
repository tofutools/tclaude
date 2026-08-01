package agentd

import (
	"runtime"
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
	require.Nil(t, failure)
	if runtime.GOOS == "linux" {
		// The live controller probe belongs to session new.
		assert.Empty(t, notices)
	} else {
		require.Len(t, notices, 1)
		assert.Equal(t, "resource_limits", notices[0].Axis)
		assert.Contains(t, notices[0].Detail, "not enforced")
	}
}

func TestResourceLimitsRequireExportVersion11(t *testing.T) {
	envelope := sandboxProfileExportEnvelope{
		Format: sandboxProfileExportFormat, FormatVersion: 10,
		Profiles: []sandboxProfileJSON{{
			Name: "limited", ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "1GiB"},
		}},
	}
	failure := validateSandboxProfileExportVersionContent(envelope)
	require.NotNil(t, failure)
	assert.Equal(t, "invalid_format", failure.Kind)
	assert.Contains(t, failure.Msg, "version 11")

	envelope.FormatVersion = 11
	assert.Nil(t, validateSandboxProfileExportVersionContent(envelope))
}
