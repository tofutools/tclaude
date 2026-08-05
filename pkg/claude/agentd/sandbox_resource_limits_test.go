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

// The limits axis is the whole point of resource-only, and it travels inside
// the resolved sandbox snapshot. Omitting the profile tiers replaces that
// snapshot with an empty one, so anything that omits them for this
// implementation silently discards the limits and leaves it enforcing nothing.
//
// Codex is the case that makes this sharp rather than theoretical:
// resource-only resolves its no-confinement mode, danger-full-access, which is
// exactly the mode the Codex rule treats as a profile opt-out. The
// implementation therefore has to be answered before the mode is consulted.
func TestResourceOnlyNeverOmitsTheProfileTiersThatCarryItsLimits(t *testing.T) {
	for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
		t.Run(harnessName, func(t *testing.T) {
			h := harness.MustGet(harnessName)

			mode, failure := resolveSandboxImplementationMode(
				h, "", string(sandboxpolicy.ImplementationResourceOnly))
			require.Nil(t, failure)
			assert.False(t,
				sandboxProfilesDisabled(harnessName, mode,
					string(sandboxpolicy.ImplementationResourceOnly)),
				"resource-only must keep resolving profiles; omitting them would drop "+
					"resource_limits and make the implementation a no-op")

			offMode, failure := resolveSandboxImplementationMode(
				h, "", string(sandboxpolicy.ImplementationOff))
			require.Nil(t, failure)
			assert.True(t,
				sandboxProfilesDisabled(harnessName, offMode,
					string(sandboxpolicy.ImplementationOff)),
				"off must keep omitting them: it carries no limits to preserve")
		})
	}

	// Codex's mode-keyed opt-out must still hold on its own terms, so the
	// short-circuit above is not mistaken for having removed it.
	assert.True(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxDangerFull,
		string(sandboxpolicy.ImplementationHarnessBuiltin)))
}

// The enforcement preview must admit a resource-only target, or the dashboard
// would show the one implementation built for limits refusing to carry them.
func TestResourceOnlyPassesTheResourceLimitCompatibilityGate(t *testing.T) {
	profile := sandboxpolicy.Profile{
		Name: "limited", ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
	}
	target := parsedSandboxProfileEnforcementTarget{
		implementation: sandboxpolicy.ImplementationResourceOnly,
		harness:        harness.MustGet(harness.DefaultName), platform: "linux",
	}
	assert.Nil(t, sandboxResourceLimitRefusal(profile, target))

	darwin := target
	darwin.platform = "darwin"
	refusal := sandboxResourceLimitRefusal(profile, darwin)
	require.NotNil(t, refusal)
	assert.Contains(t, refusal.Message, "Linux only")
}
