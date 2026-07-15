package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func setGlobalLaunchProfile(t *testing.T, prof db.SpawnProfile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	id, err := db.CreateSpawnProfile(&prof)
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardProfileRef(
		db.DashboardDefaultSpawnProfileNamePrefKey,
		db.DashboardDefaultSpawnProfileIDPrefKey,
		prof.Name, id))
}

func setupEmptyLaunchProfileDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
}

func TestGlobalDefaultLaunchProfile_InheritsHarnessModelAndEffort(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	params := &NewParams{}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{}))
	assert.Equal(t, "codex", params.Harness)
	assert.Equal(t, "gpt-5.6-sol", params.Model)
	assert.Equal(t, "high", params.Effort)
}

func TestGlobalDefaultLaunchProfile_NoProfileUsesInstalledHarness(t *testing.T) {
	setupEmptyLaunchProfileDB(t)
	params := &NewParams{}
	lookPath := func(binary string) (string, error) {
		if binary == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	require.NoError(t, applyGlobalDefaultLaunchProfileWithLookPath(
		params, explicitLaunchFields{}, lookPath))
	assert.Equal(t, "codex", params.Harness)
	assert.Empty(t, params.Model)
	assert.Empty(t, params.Effort)
}

func TestGlobalDefaultLaunchProfile_GlobalFlagRemainsAHumanLaunch(t *testing.T) {
	setupEmptyLaunchProfileDB(t)
	params := &NewParams{Global: true}
	lookPath := func(binary string) (string, error) {
		if binary == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	require.NoError(t, applyGlobalDefaultLaunchProfileWithLookPath(
		params, explicitLaunchFields{}, lookPath))
	assert.Equal(t, "codex", params.Harness)
}

func TestFirstInstalledHarness_PrefersClaude(t *testing.T) {
	assert.Equal(t, "claude", firstInstalledHarness(func(binary string) (string, error) {
		if binary == "claude" || binary == "codex" {
			return "/usr/bin/" + binary, nil
		}
		return "", errors.New("not found")
	}))
	assert.Empty(t, firstInstalledHarness(func(string) (string, error) {
		return "", errors.New("not found")
	}))
}

func TestFirstInstalledHarness_ExcludesShellSentinel(t *testing.T) {
	assert.Empty(t, firstInstalledHarness(func(binary string) (string, error) {
		if binary == ShellHarnessName {
			return "/bin/sh", nil
		}
		return "", errors.New("not found")
	}))
}

func TestGlobalDefaultLaunchProfile_ExplicitFieldsWinIndependently(t *testing.T) {
	tests := []struct {
		name     string
		params   NewParams
		explicit explicitLaunchFields
		want     NewParams
	}{
		{
			name:     "harness",
			params:   NewParams{Harness: "claude"},
			explicit: explicitLaunchFields{harness: true},
			want:     NewParams{Harness: "claude", Effort: "high"},
		},
		{
			name:     "model",
			params:   NewParams{Model: "gpt-5.4"},
			explicit: explicitLaunchFields{model: true},
			want:     NewParams{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
		},
		{
			name:     "effort",
			params:   NewParams{Effort: "low"},
			explicit: explicitLaunchFields{effort: true},
			want:     NewParams{Harness: "codex", Model: "gpt-5.6-sol", Effort: "low"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setGlobalLaunchProfile(t, db.SpawnProfile{
				Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
			})
			params := tt.params
			require.NoError(t, applyGlobalDefaultLaunchProfile(&params, tt.explicit))
			assert.Equal(t, tt.want.Harness, params.Harness)
			assert.Equal(t, tt.want.Model, params.Model)
			assert.Equal(t, tt.want.Effort, params.Effort)
		})
	}
}

func TestGlobalDefaultLaunchProfile_ExplicitEmptyFieldUsesHarnessDefault(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	params := &NewParams{}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{model: true}))
	assert.Equal(t, "codex", params.Harness)
	assert.Empty(t, params.Model, "an explicit --model= must not be filled from the profile")
	assert.Equal(t, "high", params.Effort)
}

func TestGlobalDefaultLaunchProfile_DoesNotReResolveDaemonOrResumeLaunches(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	for _, params := range []*NewParams{
		{ManagedLaunch: true},
		{Resume: "conversation-id"},
		{JoinGroup: "workers"},
	} {
		require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{}))
		assert.Empty(t, params.Harness)
		assert.Empty(t, params.Model)
		assert.Empty(t, params.Effort)
	}
}

func TestGlobalDefaultLaunchProfile_DisabledProfileBlocksFreshTerminalLaunch(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "paused", Disabled: true, DisabledReason: "maintenance",
	})
	err := applyGlobalDefaultLaunchProfile(&NewParams{}, explicitLaunchFields{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `global default spawn profile "paused" is disabled: maintenance`)
}
