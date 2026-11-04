package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tcl-kdpz4l: a birth grant records the spawn profile (and role) it came from,
// so a re-apply of that profile can change exactly those rows. Grants the
// request set carry none, and any later write of a row clears it.

func birthSources(t *testing.T, conv string) map[string]*db.PermissionBirthSource {
	t.Helper()
	rows, err := db.ListAgentPermissionOverrideRowsForConv(conv)
	require.NoError(t, err)
	out := map[string]*db.PermissionBirthSource{}
	for _, row := range rows {
		out[row.Slug] = row.BirthSource
	}
	return out
}

func TestPermissionBirthSource_RecordsTheProfileThatWon(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.CreateRole(&db.Role{Name: "provenance-role", Brief: "b",
		Permissions: []db.PermissionGrant{{Slug: "human.clipboard"}}})
	require.NoError(t, err)
	created := createProfile(t, f, map[string]any{
		"name": "worker", "harness": "claude", "role_refs": []string{"provenance-role"},
		"permission_overrides": map[string]any{"human.notify": map[string]any{"effect": "grant"}},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	worker := profileID(t, "worker")

	fromProfile := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "a", "profile": "worker"})
	require.Equalf(t, http.StatusOK, fromProfile.Code, "spawn body=%s", fromProfile.Raw)
	assert.Equal(t, map[string]*db.PermissionBirthSource{
		"human.notify":    {ProfileID: worker},
		"human.clipboard": {ProfileID: worker, Role: "provenance-role"},
	}, birthSources(t, fromProfile.ConvID))

	// The request's own overrides replace the profile's and record no source;
	// the role grant still came from the profile.
	explicit := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "b", "profile": "worker",
		"permission_overrides": map[string]any{"human.notify": map[string]any{"effect": "deny"}}})
	require.Equalf(t, http.StatusOK, explicit.Code, "spawn body=%s", explicit.Raw)
	assert.Equal(t, map[string]*db.PermissionBirthSource{
		"human.notify":    nil,
		"human.clipboard": {ProfileID: worker, Role: "provenance-role"},
	}, birthSources(t, explicit.ConvID))

	// Roles the request named are the request's, not the profile's.
	named := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "c", "profile": "worker",
		"role_refs": []string{"provenance-role"}})
	require.Equalf(t, http.StatusOK, named.Code, "spawn body=%s", named.Raw)
	assert.Nil(t, birthSources(t, named.ConvID)["human.clipboard"])

	// An operator's later write makes the row theirs.
	require.NoError(t, db.SetAgentPermissionOverride(fromProfile.ConvID, "human.notify", db.PermEffectGrant, "operator"))
	assert.Nil(t, birthSources(t, fromProfile.ConvID)["human.notify"])
	assert.NotNil(t, birthSources(t, fromProfile.ConvID)["human.clipboard"], "other rows keep theirs")
}
