package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Native history is budgeted by effective context window, not model name.
func TestLandsOnSmallerWindow(t *testing.T) {
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	var std string
	for _, m := range claude.Models.Models() {
		if m != "" {
			std = m
			break
		}
	}
	require.NotEmpty(t, std)
	origin := func(m string) *agentbundle.Origin { return &agentbundle.Origin{Model: m} }
	assert.True(t, landsOnSmallerWindow(claude, origin(std+"[1m]"), std), "1M onto 200k is the same model with a smaller window")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std+"[1m]"), "a larger window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std), "an equal window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), "claude-unlisted"), "the same model with an unknown window keeps native fidelity")
	assert.True(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), std), "a different model from an unknown window is budgeted")
}

// A relaunch profile records a blank implementation as harness-builtin, which
// OpenCode refuses. That must not cost the agent its stricter recorded mode.
func TestReceiverSandboxProfile_OpenCodeKeepsATighterModeFromARelaunchProfile(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: harness.OpenCodeName, Sandbox: harness.OpenCodeSandboxOff})
	require.NoError(t, err)
	opencode, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)

	got := receiverSandboxProfile(spawnProfileJSON{
		Sandbox: harness.OpenCodeSandboxAccessControl, SandboxImplementation: "harness-builtin",
	}, opencode, "", "landing")

	assert.Equal(t, harness.OpenCodeSandboxAccessControl, got.Sandbox)
	assert.Empty(t, got.SandboxImplementation)
}

// A file may tighten approval, tools and auto-review, never widen them, against
// what a spawn into the landing would resolve. A wider value is reported.
func TestReceiverApprovalProfile(t *testing.T) {
	yes, no := true, false
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	opencode, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		h            *harness.Harness
		landing      db.SpawnProfile
		recorded     spawnProfileJSON
		wantApproval string
		wantTools    string
		wantReview   *bool
		wantDropped  []string
	}{
		{
			name: "codex on-request over untrusted is wider", h: codex,
			landing:     db.SpawnProfile{Approval: "untrusted"},
			recorded:    spawnProfileJSON{Approval: "on-request"},
			wantDropped: []string{"approval on-request"},
		},
		{
			name: "codex untrusted under never stays", h: codex,
			landing:      db.SpawnProfile{Approval: "never"},
			recorded:     spawnProfileJSON{Approval: "untrusted"},
			wantApproval: "untrusted",
		},
		{
			name: "auto-review on is wider than a human reviewing", h: codex,
			landing:     db.SpawnProfile{Approval: "on-request"},
			recorded:    spawnProfileJSON{AutoReview: &yes},
			wantDropped: []string{"auto-review on"},
		},
		{
			name: "a recorded false matching the landing says nothing", h: codex,
			landing:  db.SpawnProfile{Approval: "on-request"},
			recorded: spawnProfileJSON{AutoReview: &no},
		},
		{
			name: "opencode tools allow over ask is wider", h: opencode,
			landing:     db.SpawnProfile{ToolGovernance: harness.OpenCodeToolsAsk},
			recorded:    spawnProfileJSON{ToolGovernance: harness.OpenCodeToolsAllow},
			wantDropped: []string{"tools allow"},
		},
		{
			name: "opencode tools deny under allow stays", h: opencode,
			recorded:  spawnProfileJSON{ToolGovernance: harness.OpenCodeToolsDeny},
			wantTools: harness.OpenCodeToolsDeny,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			tc.landing.Name, tc.landing.Harness = "landing", tc.h.Name
			_, err := db.CreateSpawnProfile(&tc.landing)
			require.NoError(t, err)

			got, dropped := receiverApprovalProfile(tc.recorded, tc.h, "", "landing")

			assert.Equal(t, tc.wantApproval, got.Approval)
			assert.Equal(t, tc.wantTools, got.ToolGovernance)
			assert.Equal(t, tc.wantReview, got.AutoReview)
			require.Len(t, dropped, len(tc.wantDropped))
			for i, want := range tc.wantDropped {
				assert.Contains(t, dropped[i], want)
			}
		})
	}
}

func TestReceiverLaunchFieldsProfile(t *testing.T) {
	yes, no := true, false
	secret := sandboxpolicy.EnvironmentEntry{Name: "OPENCODE_CONFIG_CONTENT", Value: "hunter2"}
	shared := sandboxpolicy.EnvironmentEntry{Name: "SHARED", Value: "same"}
	for _, tc := range []struct {
		name        string
		landing     db.SpawnProfile
		recorded    spawnProfileJSON
		want        spawnProfileJSON
		wantDropped []string
	}{
		{
			name:        "trust and remote control are dropped",
			recorded:    spawnProfileJSON{TrustDir: &yes, RemoteControl: &yes},
			wantDropped: []string{"trusted working directory", "remote control"},
		},
		{
			name:     "what a spawn here gives needs no warning",
			landing:  db.SpawnProfile{TrustDir: &yes, RemoteControl: &yes},
			recorded: spawnProfileJSON{TrustDir: &yes, RemoteControl: &yes},
		},
		{
			name:     "a false asks for less and stays",
			landing:  db.SpawnProfile{TrustDir: &yes, RemoteControl: &yes},
			recorded: spawnProfileJSON{TrustDir: &no, RemoteControl: &no},
			want:     spawnProfileJSON{TrustDir: &no, RemoteControl: &no},
		},
		{
			name:        "environment is dropped, named and not quoted",
			landing:     db.SpawnProfile{Environment: []sandboxpolicy.EnvironmentEntry{shared}},
			recorded:    spawnProfileJSON{Environment: []sandboxpolicy.EnvironmentEntry{secret, shared}},
			wantDropped: []string{"OPENCODE_CONFIG_CONTENT"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			h, err := harness.Resolve(harness.DefaultName)
			require.NoError(t, err)
			tc.landing.Name, tc.landing.Harness = "landing", h.Name
			_, err = db.CreateSpawnProfile(&tc.landing)
			require.NoError(t, err)

			got, dropped := receiverLaunchFieldsProfile(tc.recorded, h, "", "landing")

			assert.Equal(t, tc.want.TrustDir, got.TrustDir)
			assert.Equal(t, tc.want.RemoteControl, got.RemoteControl)
			assert.Empty(t, got.Environment)
			require.Len(t, dropped, len(tc.wantDropped))
			for i, want := range tc.wantDropped {
				assert.Contains(t, dropped[i], want)
				assert.NotContains(t, dropped[i], secret.Value)
				assert.NotContains(t, dropped[i], shared.Name)
			}
		})
	}
}
