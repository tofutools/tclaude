package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only a birth write records a source; every other write of the row clears it,
// so a re-apply can never mistake a changed row for the profile's.
func TestPermissionBirthSourceIsClearedByEveryOtherWrite(t *testing.T) {
	setupTestDB(t)
	src := &PermissionBirthSource{ProfileID: 7, Role: "r"}
	birth := func(conv, slug string) {
		require.NoError(t, SetAgentBirthPermissionOverride(conv, slug, PermEffectGrant, "", "spawner", src))
	}
	source := func(conv, slug string) *PermissionBirthSource {
		rows, err := ListAgentPermissionOverrideRowsForConv(conv)
		require.NoError(t, err)
		for _, row := range rows {
			if row.Slug == slug {
				return row.BirthSource
			}
		}
		t.Fatalf("no row %s", slug)
		return nil
	}
	const conv = "birth-source-conv"
	birth(conv, "a")
	assert.Equal(t, src, source(conv, "a"))

	writes := map[string]func(slug string){
		"set": func(slug string) { require.NoError(t, SetAgentPermissionOverride(conv, slug, PermEffectGrant, "op")) },
		"apply": func(slug string) {
			require.NoError(t, ApplyAgentPermissionOverrides(conv, map[string]PermissionOverride{slug: {Effect: PermEffectGrant}}, "gen", false, false))
		},
		"apply, preserved": func(slug string) {
			require.NoError(t, ApplyAgentPermissionOverrides(conv, map[string]PermissionOverride{slug: {Effect: PermEffectGrant}}, "gen", false, true))
		},
		"birth, no source": func(slug string) {
			require.NoError(t, SetAgentBirthPermissionOverride(conv, slug, PermEffectGrant, "", "spawner", nil))
		},
	}
	for name, write := range writes {
		slug := "slug-" + strings.ReplaceAll(name, " ", "-")
		birth(conv, slug)
		require.NotNil(t, source(conv, slug), name)
		write(slug)
		assert.Nil(t, source(conv, slug), name)
	}

	agentID, err := AgentIDForConv(conv)
	require.NoError(t, err)
	birth(conv, "scoped")
	require.NoError(t, UpdateAgentPermissionScopes(agentID, []string{"scoped"}, "op",
		func(slug string, cur PermissionRow) (string, bool, bool, error) { return "", false, false, nil }))
	assert.Nil(t, source(conv, "scoped"), "a scope rewrite")
}

func TestMigrateV307IsIdempotent(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, migrateV306toV307(d), "a re-run is a clean no-op")
	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agent_permissions') WHERE name = 'birth_source'`).Scan(&n))
	assert.Equal(t, 1, n)
}
