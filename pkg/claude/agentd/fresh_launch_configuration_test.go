package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestFreshLaunchConfigurationResolvesAliasAndCapturedPrecedence(t *testing.T) {
	updated := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	input := freshLaunchConfigurationInput{
		NamedProfileHandle: "layered-alias",
		Profiles: []capturedLaunchProfile{
			{
				Profile: db.SpawnProfile{
					ID: 41, Name: "layered", Aliases: []string{"layered-alias"},
					Harness: "claude", Sandbox: "on", SandboxImplementation: "harness-builtin",
					UpdatedAt: updated,
				},
				Source: `profile "layered" via alias "layered-alias"`, Kind: capturedNamedProfile,
				ID: 41, UpdatedAt: updated,
			},
			{
				Profile: db.SpawnProfile{Name: "house", Harness: "codex", SandboxImplementation: "tclaude-layer"},
				Source:  agent.ProvGlobalProfileSource("house"), DefaultTier: true, Kind: capturedGlobalProfile,
			},
		},
	}

	got, refusal := resolveFreshLaunchConfiguration(input)
	require.Nil(t, refusal)
	require.NotNil(t, got.Harness)
	assert.Equal(t, "claude", got.Harness.Name)
	assert.Equal(t, input.Profiles[0].Source, got.HarnessSelection.Source)
	assert.Equal(t, "on", got.HarnessBuiltinMode.Selected)
	assert.Equal(t, "on", got.HarnessBuiltinMode.Effective)
	assert.Equal(t, "harness-builtin", got.SandboxImplementation.Selected)
	assert.Equal(t, freshLaunchSelected, got.SandboxImplementation.State)
}

func TestFreshLaunchConfigurationAuthorityFootprintUsesCapturedCanonicalProfile(t *testing.T) {
	groupDefault := freshLaunchConfigurationCapture{Input: freshLaunchConfigurationInput{
		Profiles: []capturedLaunchProfile{{
			Profile: db.SpawnProfile{Name: "crew-default"},
			Kind:    capturedGroupProfile, DefaultTier: true,
		}},
	}}
	assert.Equal(t, "crew-default", groupDefault.canonicalSpawnProfile())

	alias := groupDefault
	alias.Input.NamedProfileHandle = "worker-alias"
	alias.Input.Profiles = append([]capturedLaunchProfile{{
		Profile: db.SpawnProfile{Name: "worker"}, Kind: capturedNamedProfile,
	}}, alias.Input.Profiles...)
	assert.Equal(t, "worker", alias.canonicalSpawnProfile())

	missing := groupDefault
	missing.Input.NamedProfileHandle = "ghost"
	missing.Issue = &freshLaunchCaptureIssue{Kind: freshLaunchProfileMissing, Handle: "ghost"}
	assert.Empty(t, missing.canonicalSpawnProfile(),
		"an explicitly missing handle stays undescribed rather than falling through for authority")
}

func TestFreshLaunchConfigurationPreservesInheritedAndExplicitStates(t *testing.T) {
	inherited, refusal := resolveFreshLaunchConfiguration(freshLaunchConfigurationInput{})
	require.Nil(t, refusal)
	assert.Equal(t, freshLaunchInherited, inherited.HarnessBuiltinMode.State)
	assert.Equal(t, freshLaunchInherited, inherited.SandboxImplementation.State)
	assert.Empty(t, inherited.HarnessBuiltinMode.Selected)
	assert.Equal(t, "harness-builtin", inherited.SandboxImplementation.Effective)
	assert.Equal(t, agent.ProvHarnessDefault, inherited.SandboxImplementation.Source)

	explicit, refusal := resolveFreshLaunchConfiguration(freshLaunchConfigurationInput{
		Request: freshLaunchConfigurationRequest{
			Harness: "claude", HarnessBuiltinMode: "inherit",
			SandboxImplementation: "harness-builtin",
		},
	})
	require.Nil(t, refusal)
	assert.Equal(t, freshLaunchSelected, explicit.HarnessBuiltinMode.State,
		"an explicit inherit is known intent even when its effective mode is empty")
	assert.Equal(t, "inherit", explicit.HarnessBuiltinMode.Selected)
	assert.Equal(t, "inherit", explicit.HarnessBuiltinMode.Effective)
	assert.Equal(t, agent.ProvExplicit, explicit.HarnessBuiltinMode.Source)
	assert.Equal(t, freshLaunchSelected, explicit.SandboxImplementation.State)
	assert.Equal(t, agent.ProvExplicit, explicit.SandboxImplementation.Source)
}

func TestFreshLaunchConfigurationSkipsForeignAmbientAndRefusesExplicitInvalid(t *testing.T) {
	input := freshLaunchConfigurationInput{
		Request: freshLaunchConfigurationRequest{Harness: "opencode"},
		Profiles: []capturedLaunchProfile{{
			Profile: db.SpawnProfile{
				Name: "claude-house", Harness: "claude", SandboxImplementation: "harness-builtin",
			},
			Source:      agent.ProvGroupProfileSource("claude-house"),
			DefaultTier: true, Kind: capturedGroupProfile,
		}},
	}
	got, refusal := resolveFreshLaunchConfiguration(input)
	require.Nil(t, refusal)
	require.Len(t, got.SandboxImplementation.Skipped, 1)
	assert.Equal(t, input.Profiles[0].Source, got.SandboxImplementation.Skipped[0].Source)
	assert.Contains(t, freshLaunchSelectionNote(got.SandboxImplementation), "not valid for opencode")

	input.Request.SandboxImplementation = "harness-builtin"
	_, refusal = resolveFreshLaunchConfiguration(input)
	require.NotNil(t, refusal)
	assert.Equal(t, freshLaunchInvalidImplementation, refusal.Kind)
	assert.Empty(t, refusal.Profile, "the refusal is direct caller intent, not blamed on a profile")
}

func TestFreshLaunchConfigurationCaptureIsMutationIsolated(t *testing.T) {
	on := true
	original := &db.SpawnProfile{
		ID: 7, Name: "safe", Harness: "claude", Sandbox: "on",
		Aliases: []string{"safe-alias"}, AutoReview: &on,
		ContextFeatures: map[string]string{"tools": "off"},
		PermissionOverrides: map[string]db.PermissionOverride{
			"agent.spawn": db.ScopedOverride("grant", `{"group":["crew"]}`),
		},
	}
	captured := capturedProfile(original, `profile "safe"`, false, capturedNamedProfile)
	original.Aliases[0] = "changed"
	*original.AutoReview = false
	original.ContextFeatures["tools"] = "on"
	original.PermissionOverrides["agent.spawn"] = db.Deny()

	assert.Equal(t, []string{"safe-alias"}, captured.Profile.Aliases)
	require.NotNil(t, captured.Profile.AutoReview)
	assert.True(t, *captured.Profile.AutoReview)
	assert.Equal(t, "off", captured.Profile.ContextFeatures["tools"])
	assert.Equal(t, "grant", captured.Profile.PermissionOverrides["agent.spawn"].Effect)

	capture := freshLaunchConfigurationCapture{Input: freshLaunchConfigurationInput{
		NamedProfileHandle: "safe", Profiles: []capturedLaunchProfile{captured},
	}}
	first := capture.legacyProfileTiers()
	first[0].profile.ContextFeatures["tools"] = "on"
	second := capture.legacyProfileTiers()
	assert.Equal(t, "off", second[0].profile.ContextFeatures["tools"],
		"legacy consumers receive their own clone of the captured source")
}
