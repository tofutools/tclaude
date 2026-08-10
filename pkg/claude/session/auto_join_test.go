package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestResolveAutomaticGroupConfigPrecedence(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name     string
		params   NewParams
		explicit explicitLaunchFields
		cfg      *config.Config
		wantJoin bool
		wantMake bool
	}{
		{name: "built-in defaults", cfg: &config.Config{}, wantJoin: true},
		{name: "configured inverse", cfg: &config.Config{Session: &config.SessionConfig{AutoJoinGroup: &off, AutoJoinOrCreateGroup: &on}}, wantMake: true},
		{name: "explicit solo opt out suppresses configured create", params: NewParams{AutoJoinGroup: false}, explicit: explicitLaunchFields{"auto-join-group": true}, cfg: &config.Config{Session: &config.SessionConfig{AutoJoinOrCreateGroup: &on}}},
		{name: "explicit flags win", params: NewParams{AutoJoinGroup: false, AutoJoinOrCreateGroup: true}, explicit: explicitLaunchFields{"auto-join-group": true, "auto-join-or-create-group": true}, cfg: &config.Config{Session: &config.SessionConfig{AutoJoinGroup: &on, AutoJoinOrCreateGroup: &off}}, wantMake: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := tc.params
			resolveAutomaticGroupConfig(&params, tc.explicit, tc.cfg)
			assert.Equal(t, tc.wantJoin, params.AutoJoinGroup)
			assert.Equal(t, tc.wantMake, params.AutoJoinOrCreateGroup)
		})
	}
}

func TestValidateUnmatchedGroupSpawnFlags(t *testing.T) {
	require.NoError(t, validateUnmatchedGroupSpawnFlags(&NewParams{Model: "opus"}), "direct-session flags remain valid")
	err := validateUnmatchedGroupSpawnFlags(&NewParams{Profile: "worker"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--profile")
	assert.Contains(t, err.Error(), "--auto-join-or-create-group")
}

func TestAutomaticGroupEligibilityPreservesDirectModes(t *testing.T) {
	assert.False(t, automaticGroupEligible(&NewParams{ManagedLaunch: true}, nil))
	assert.False(t, automaticGroupEligible(&NewParams{NoDaemon: true}, nil))
	assert.False(t, automaticGroupEligible(&NewParams{Resume: "abc"}, nil))
	assert.False(t, automaticGroupEligible(&NewParams{Shell: true}, nil))
	assert.False(t, automaticGroupEligible(&NewParams{}, explicitLaunchFields{"label": true}))
	assert.True(t, automaticGroupEligible(&NewParams{}, nil))
}

func TestNoDaemonIsNotAGroupLaunchMode(t *testing.T) {
	params := &NewParams{NoDaemon: true, AutoJoinGroup: true, AutoJoinOrCreateGroup: true}
	assert.False(t, automaticGroupEligible(params, explicitLaunchFields{"no-daemon": true}))

	params.AutoJoinGroup = false
	params.AutoJoinOrCreateGroup = false
	require.NoError(t, validateUnmatchedGroupSpawnFlags(params))
}

func TestRecordLaunchFlagPresencePreservesExplicitCodexAppServerFalse(t *testing.T) {
	params := &NewParams{CodexAppServer: false}
	recordLaunchFlagPresence(params, explicitLaunchFields{"codex-app-server": true})
	assert.True(t, params.CodexAppServerSpecified)
}
