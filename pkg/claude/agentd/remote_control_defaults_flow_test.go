package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (JOH-262): an operator arms Claude Code's built-in Remote Access at
// spawn by DEFAULT, configured at two levels — a spawn profile's remote-control
// default and a group's remote-control policy, with the group policy OVERRIDING
// the profile. The effective intent is resolved at spawn and threaded into
// SpawnSpec.RemoteControl (the JOH-258 primitive), so a profile/group default
// reaches every spawn path without per-agent toggling.
//
// Precedence (highest first):
//
//	group policy (force on/off)  >  explicit per-spawn opt-in  >  profile default  >  off
//
// These pin the matrix at the Spawner boundary (World.SpawnRemoteControl — the
// same surface the JOH-258/261 remote-control tests assert), the production seam
// where the --remote-control flag is later built.

// setGroupRemoteControlPolicy PATCHes a group's remote-control policy
// ("inherit" | "optin" | "deny"). Returns the recorder for status/body asserts.
func setGroupRemoteControlPolicy(t *testing.T, f *testharness.Flow, group, policy string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/"+group,
		map[string]any{"remote_control_policy": policy}))
	return testharness.Serve(f.Mux, r)
}

// makeProfileWithRC creates a spawn profile carrying a remote-control default and
// sets it as the group's default profile.
func makeProfileWithRC(t *testing.T, f *testharness.Flow, group, profile string, rc bool) {
	t.Helper()
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": profile, "remote_control": rc}).Code,
		"create profile %q (remote_control=%v)", profile, rc)
	require.Equalf(t, http.StatusOK,
		setGroupProfile(t, f, group, profile).Code, "set group %q default_profile=%q", group, profile)
}

// assertSpawnArmed spawns a plain CC worker into the group and asserts the
// Spawner received the expected remote-control intent.
func assertSpawnArmed(t *testing.T, f *testharness.Flow, group string, want bool, msg string) {
	t.Helper()
	spawn := f.AsHuman().Spawn(group, "worker")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.Truef(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equalf(t, want, got, msg)
}

// TestRemoteControlDefaults_GroupOptInOverridesProfileOff: group "optin" forces
// Remote Access on even though the profile default is off — the group is the
// override level.
func TestRemoteControlDefaults_GroupOptInOverridesProfileOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	makeProfileWithRC(t, f, "alpha", "p-off", false)
	require.Equalf(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "optin").Code, "set group policy")

	assertSpawnArmed(t, f, "alpha", true, "group optin must force remote-control on over a profile default of off")
}

// TestRemoteControlDefaults_GroupDenyOverridesProfileOn: group "deny" forces
// Remote Access OFF even though the profile default is on — the "actively deny"
// case the override level exists for.
func TestRemoteControlDefaults_GroupDenyOverridesProfileOn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	makeProfileWithRC(t, f, "alpha", "p-on", true)
	require.Equalf(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "deny").Code, "set group policy")

	assertSpawnArmed(t, f, "alpha", false, "group deny must force remote-control off over a profile default of on")
}

// TestRemoteControlDefaults_ProfileOnInherited: group policy unset (inherit) —
// the profile default of on applies.
func TestRemoteControlDefaults_ProfileOnInherited(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	makeProfileWithRC(t, f, "alpha", "p-on", true)
	// No group policy set → inherit (the column default).

	assertSpawnArmed(t, f, "alpha", true, "an unset group policy must defer to the profile's on default")
}

// TestRemoteControlDefaults_AllUnsetIsOff: no profile default, no group policy —
// Remote Access stays off.
func TestRemoteControlDefaults_AllUnsetIsOff(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	assertSpawnArmed(t, f, "alpha", false, "with nothing set, remote-control must default off")
}

// TestRemoteControlDefaults_GroupOptInNoProfile: group "optin" with no default
// profile at all — the group policy alone arms it.
func TestRemoteControlDefaults_GroupOptInNoProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equalf(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "optin").Code, "set group policy")

	assertSpawnArmed(t, f, "alpha", true, "group optin must arm remote-control even with no default profile")
}

// TestRemoteControlDefaults_ExplicitOptInOverProfile: with the group policy
// unset, an explicit per-spawn remote_control:true arms even when the profile
// default is off — the explicit opt-in outranks the profile.
func TestRemoteControlDefaults_ExplicitOptInOverProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	makeProfileWithRC(t, f, "alpha", "p-off", false)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "remote_control": true})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.True(t, got, "an explicit per-spawn opt-in must arm over a profile default of off")
}

// TestRemoteControlDefaults_GroupDenyBeatsExplicitOptIn: a group "deny" is
// authoritative — it force-disables even an explicit per-spawn opt-in, so a
// sensitive group stays unreachable regardless of a per-spawn tick.
func TestRemoteControlDefaults_GroupDenyBeatsExplicitOptIn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equalf(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "deny").Code, "set group policy")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "remote_control": true})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.False(t, got, "group deny must override an explicit per-spawn opt-in")
}

// TestRemoteControlDefaults_CodexClampsPolicyForceOn: a group/profile force-on
// must NOT fail a Codex spawn (which has no Remote Access) — it is silently
// clamped to off, not a 400. (An EXPLICIT per-spawn opt-in on Codex still 400s;
// that is TestCodexSpawn_RejectsRemoteControl.)
func TestRemoteControlDefaults_CodexClampsPolicyForceOn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")
	require.Equalf(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "codex-crew", "optin").Code, "set group policy")

	spawn := f.AsHuman().SpawnWith("codex-crew", map[string]any{"name": "worker", "harness": "codex"})
	require.Equalf(t, http.StatusOK, spawn.Code,
		"a policy-derived force-on must not fail a Codex spawn; body=%s", spawn.Raw)
	got, ok := f.World.SpawnRemoteControl(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.False(t, got, "a Codex spawn must be clamped to no remote-control even under a group optin")
}

// TestRemoteControlDefaults_InvalidGroupPolicyRejected: a bad policy token is a
// 400, not a silent "inherit".
func TestRemoteControlDefaults_InvalidGroupPolicyRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := setGroupRemoteControlPolicy(t, f, "alpha", "maybe")
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"an invalid remote_control_policy must be a 400; body=%s", rec.Body.String())
}

// TestRemoteControlDefaults_ProfileRejectsRemoteControlOnCodex: a profile may
// not default Remote Access on for a Codex harness — that is a 400 at save, the
// same gate the spawn path applies.
func TestRemoteControlDefaults_ProfileRejectsRemoteControlOnCodex(t *testing.T) {
	f := newFlow(t)
	rec := createProfile(t, f, map[string]any{"name": "cdx", "harness": "codex", "remote_control": true})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"a Codex profile with remote_control=true must be refused; body=%s", rec.Body.String())
}

// TestRemoteControlDefaults_GroupPolicyRoundTrips: the PATCHed policy lands on
// the group row and reads back through the canonical wire token.
func TestRemoteControlDefaults_GroupPolicyRoundTrips(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	for _, tc := range []struct{ set, want string }{
		{"optin", "optin"},
		{"deny", "deny"},
		{"inherit", "inherit"},
	} {
		rec := setGroupRemoteControlPolicy(t, f, "alpha", tc.set)
		require.Equalf(t, http.StatusOK, rec.Code, "set policy %q body=%s", tc.set, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), `"remote_control_policy":"`+tc.want+`"`,
			"policy %q must echo back as %q", tc.set, tc.want)
	}
}
