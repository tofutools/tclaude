package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wirePreLaunchProfile struct {
	Name      string `json:"name"`
	PreLaunch []struct {
		Name    string   `json:"name"`
		Script  string   `json:"script"`
		Exports []string `json:"exports"`
	} `json:"pre_launch"`
}

// A profile field that exists in the policy package but not in the wire type,
// the DB row, or any of the conversions between them is silently discarded on
// the way in and absent on the way out — the feature builds, its unit tests
// pass, and it does nothing in production. This walks the real API round trip
// so that gap cannot reopen: POST a profile with blocks, read it back, and
// confirm the stored row carries them.
func TestSandboxProfilePreLaunchBlocksSurviveTheAPIRoundTrip(t *testing.T) {
	f := newFlow(t)
	const script = "export PLAYWRIGHT_CLI_SESSION=\"render-$$\"\nmkdir -p /tmp/pw/{config,cache}\n"

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "with-blocks",
		"pre_launch": []map[string]any{
			{"name": "playwright", "script": script, "exports": []string{"PLAYWRIGHT_CLI_SESSION"}},
			{"name": "second", "script": "true\n"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/with-blocks", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got wirePreLaunchProfile
	testharness.DecodeJSON(t, rec, &got)

	require.Len(t, got.PreLaunch, 2, "the API must return the blocks it accepted")
	assert.Equal(t, "playwright", got.PreLaunch[0].Name)
	assert.Equal(t, script, got.PreLaunch[0].Script, "the operator's script must survive verbatim")
	assert.Equal(t, []string{"PLAYWRIGHT_CLI_SESSION"}, got.PreLaunch[0].Exports)
	assert.Equal(t, "second", got.PreLaunch[1].Name, "authored order is execution order")

	// …and the same through the storage layer, which is where the blocks have
	// to be for a launch to ever see them.
	stored, err := db.GetSandboxProfile("with-blocks")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.PreLaunch, 2)
	assert.Equal(t, script, stored.PreLaunch[0].Script)
}

// Validation must be enforced at the daemon boundary, not merely in the policy
// package: the daemon is the authority for profile documents.
func TestSandboxProfilePreLaunchValidationIsEnforcedAtTheAPI(t *testing.T) {
	f := newFlow(t)
	for _, tc := range []struct {
		name  string
		block map[string]any
	}{
		{"unnamed", map[string]any{"script": "true"}},
		{"empty script", map[string]any{"name": "b", "script": "  "}},
		{"invalid name", map[string]any{"name": "has space", "script": "true"}},
		{"invalid export", map[string]any{"name": "b", "script": "true", "exports": []string{"not-a-var"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
				"name":       "invalid-" + tc.name,
				"pre_launch": []map[string]any{tc.block},
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// Export/import is how a profile moves between machines, so blocks have to
// ride along or the receiving machine silently gets a profile that no longer
// prepares anything.
func TestSandboxProfilePreLaunchSurvivesExportImport(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "exported",
		"pre_launch": []map[string]any{{"name": "blk", "script": "export A=1\n", "exports": []string{"A"}}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var envelope struct {
		Format        string                 `json:"format"`
		FormatVersion int                    `json:"format_version"`
		Profiles      []wirePreLaunchProfile `json:"profiles"`
	}
	testharness.DecodeJSON(t, rec, &envelope)
	var exported *wirePreLaunchProfile
	for i := range envelope.Profiles {
		if envelope.Profiles[i].Name == "exported" {
			exported = &envelope.Profiles[i]
		}
	}
	require.NotNil(t, exported, "the exported bundle must contain the profile")
	require.Len(t, exported.PreLaunch, 1, "export must carry pre-launch blocks")
	assert.Equal(t, "export A=1\n", exported.PreLaunch[0].Script)

	// Re-import under a new name and confirm the blocks arrive intact.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format":         envelope.Format,
		"format_version": envelope.FormatVersion,
		"profiles": []map[string]any{{
			"name":       "imported",
			"pre_launch": []map[string]any{{"name": "blk", "script": "export A=1\n", "exports": []string{"A"}}},
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	stored, err := db.GetSandboxProfile("imported")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.PreLaunch, 1)
	assert.Equal(t, "export A=1\n", stored.PreLaunch[0].Script)
}

// The last link in the chain: a stored profile must resolve into the effective
// snapshot the launch path reads. Without this the blocks would sit in the
// registry and never reach a pane.
func TestStoredPreLaunchBlocksReachTheEffectiveProfile(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "resolvable",
		"pre_launch": []map[string]any{{"name": "blk", "script": "export A=1\n"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	stored, err := db.GetSandboxProfile("resolvable")
	require.NoError(t, err)
	require.NotNil(t, stored)

	profile := sandboxpolicy.Profile{Name: stored.Name, PreLaunch: stored.PreLaunch}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &profile})
	require.NoError(t, err)
	require.Len(t, effective.PreLaunch, 1,
		"a stored block must survive resolution into the launch snapshot")
	assert.Equal(t, "export A=1\n", effective.PreLaunch[0].Script)
}

// A client that edits a profile without resending pre_launch must not silently
// destroy the blocks. The dashboard builds its save body from an explicit field
// whitelist, so any field the editor does not know about is absent on the wire
// — and an absent pre_launch previously meant "clear it". Losing an operator's
// setup script because they renamed a profile is not an acceptable failure.
func TestSandboxProfileUpdateWithoutPreLaunchKeepsTheBlocks(t *testing.T) {
	f := newFlow(t)
	const script = "export PLAYWRIGHT_CLI_SESSION=keep-me\n"
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "keeps-blocks",
		"pre_launch": []map[string]any{{"name": "setup", "script": script}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// An update shaped like the dashboard's: every field it knows about, and no
	// pre_launch at all.
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/keeps-blocks", map[string]any{
		"name":              "keeps-blocks",
		"filesystem":        []map[string]any{},
		"environment":       []map[string]any{},
		"includes":          []string{},
		"agent_directories": []string{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	stored, err := db.GetSandboxProfile("keeps-blocks")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Len(t, stored.PreLaunch, 1,
		"an update that never mentioned pre_launch must not delete it")
	assert.Equal(t, script, stored.PreLaunch[0].Script)
}

// …but a client that means it must still be able to clear them, so absence and
// an explicit empty list cannot be the same thing.
func TestSandboxProfileUpdateWithEmptyPreLaunchClearsTheBlocks(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "clears-blocks",
		"pre_launch": []map[string]any{{"name": "setup", "script": "true\n"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/clears-blocks", map[string]any{
		"name":       "clears-blocks",
		"pre_launch": []map[string]any{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	stored, err := db.GetSandboxProfile("clears-blocks")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.PreLaunch, "an explicit empty list must clear the blocks")
}
