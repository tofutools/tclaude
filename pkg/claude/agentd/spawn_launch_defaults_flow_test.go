package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wireLaunchDefaults struct {
	Harness        string `json:"harness"`
	Sandbox        string `json:"sandbox"`
	Implementation string `json:"implementation"`
	ResolvedBy     string `json:"resolved_by"`
}

func getLaunchDefaults(t *testing.T, f *testharness.Flow, query string) wireLaunchDefaults {
	t.Helper()
	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-launch-defaults"+query, nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got wireLaunchDefaults
	testharness.DecodeJSON(t, rec, &got)
	return got
}

// TestSpawnLaunchDefaultsAnswerWhatABlankFieldResolvesTo covers the endpoint the
// spawn dialog's sandbox-implementation row asks before it can name its own
// default.
//
// The dialog cannot work this out locally, and the reason is the thing this test
// pins: clearing the dialog's profile row blanks the FORM, not the tiers. The
// daemon still fills a blank launch field from the group's default spawn profile
// and then the global one, so the honest answer only exists here.
func TestSpawnLaunchDefaultsAnswerWhatABlankFieldResolvesTo(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	// Nothing configured: the harness default answers, and says so.
	bare := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "claude", bare.Harness)
	assert.Equal(t, "harness-builtin", bare.Implementation,
		"a blank field with no profile tier resolves to the harness's own layer")
	assert.Equal(t, "harness default", bare.ResolvedBy)

	// A global default spawn profile that pins the tclaude layer.
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "house-launch", Harness: "claude", SandboxImplementation: "tclaude-layer",
	})
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPut, "/v1/spawn-profile-default",
		map[string]any{"name": "house-launch"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	global := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "tclaude-layer", global.Implementation)
	assert.Contains(t, global.ResolvedBy, `global default profile "house-launch"`)

	// A group default spawn profile outranks it. Both tiers are populated with
	// DIFFERENT implementations, so inverting the precedence changes the answer.
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "crew-launch", Harness: "claude", SandboxImplementation: "harness-builtin",
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "crew-launch")
	require.NoError(t, err)

	group := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "harness-builtin", group.Implementation,
		"the group tier outranks the global one, exactly as a real spawn resolves it")
	assert.Contains(t, group.ResolvedBy, `group default profile "crew-launch"`)

	// A group with no default of its own falls through to the global tier — the
	// same request, a different group, a different answer.
	_, err = db.CreateAgentGroup("solo", "")
	require.NoError(t, err)
	fallthrough_ := getLaunchDefaults(t, f, "?group=solo")
	assert.Equal(t, "tclaude-layer", fallthrough_.Implementation)
	assert.Contains(t, fallthrough_.ResolvedBy, `global default profile "house-launch"`)
}

// TestSpawnLaunchDefaultsHonorAnExplicitHarness pins the one input the dialog
// supplies. The harness select is an explicit choice that outranks every profile
// tier, and an implementation is only meaningful relative to it — so a profile
// pinning a layer for a FOREIGN harness must not be reported as this launch's
// answer.
func TestSpawnLaunchDefaultsHonorAnExplicitHarness(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "crew-launch", Harness: "claude", SandboxImplementation: "tclaude-layer",
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "crew-launch")
	require.NoError(t, err)

	claude := getLaunchDefaults(t, f, "?group=crew&harness=claude")
	assert.Equal(t, "claude", claude.Harness)
	assert.Equal(t, "tclaude-layer", claude.Implementation,
		"the group profile applies to the harness it was written for")

	codex := getLaunchDefaults(t, f, "?group=crew&harness=codex")
	assert.Equal(t, "codex", codex.Harness,
		"an explicit harness outranks the profile tier that would have chosen one")
	assert.Equal(t, "tclaude-layer", codex.Implementation,
		"tclaude-layer is valid for Codex too, so the ambient tier still applies")

	// An unknown harness is the caller's mistake, reported as such.
	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-launch-defaults?harness=nope", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// TestSpawnLaunchDefaultsRankTheNamedSpawnProfileFirst covers the tier that is
// easiest to forget and most misleading to omit.
//
// Picking a spawn profile pre-fills the dialog, so it is tempting to assume the
// named tier can never be the one answering a BLANK field. It can: the operator
// can set the implementation select back to blank (a harness switch clears it
// too) while the profile stays selected, and the spawn request still carries
// `profile`. handleGroupSpawn ranks that named profile above the group and
// global tiers, so a preview that walked only the ambient tiers would name an
// implementation the launch does not use — worse than naming nothing.
func TestSpawnLaunchDefaultsRankTheNamedSpawnProfileFirst(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "crew-launch", Harness: "claude", SandboxImplementation: "harness-builtin",
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "crew-launch")
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "layered", Aliases: []string{"layered-alias"},
		Harness: "claude", SandboxImplementation: "tclaude-layer",
	})
	require.NoError(t, err)

	// Without the named profile the group tier answers...
	ambient := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "harness-builtin", ambient.Implementation)

	// ...and with it selected, it outranks the group tier.
	named := getLaunchDefaults(t, f, "?group=crew&profile=layered")
	assert.Equal(t, "tclaude-layer", named.Implementation,
		"a selected spawn profile outranks the group default, exactly as a real spawn resolves it")
	assert.Contains(t, named.ResolvedBy, `profile "layered"`)

	// An alias resolves to the same profile and says which alias it came through,
	// matching the provenance a real spawn records.
	alias := getLaunchDefaults(t, f, "?group=crew&profile=layered-alias")
	assert.Equal(t, "tclaude-layer", alias.Implementation)
	assert.Contains(t, alias.ResolvedBy, `profile "layered" via alias "layered-alias"`)

	// A profile handle that resolves to nothing is the caller's mistake.
	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-launch-defaults?profile=ghost", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}
