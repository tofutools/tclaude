package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// minimalImportPlan wraps a Group in the smallest valid GroupImportPlan: a
// memberless group import. Every conv-scoped table is empty, so the only row
// written is agent_groups (plus, for a legacy archive, the synthesized spawn
// profile under test).
func minimalImportPlan(target string, g groupexport.Group) GroupImportPlan {
	return GroupImportPlan{
		Export: &groupexport.Export{
			FormatVersion: groupexport.FormatVersion,
			SourceGroup:   target,
			Group:         g,
		},
		TargetName: target,
		TargetCwd:  "/tmp/import-target",
		ConvRemap:  map[string]string{},
	}
}

// TestImportGroup_LegacyDefaultModelSynthesizesProfile covers the JOH-220
// import back-compat path: a pre-v2 archive that still carries a per-group
// default_model must not silently regress its spawn default. The importer
// synthesizes a claude spawn profile from the legacy model and points the
// imported group's default_profile at it.
func TestImportGroup_LegacyDefaultModelSynthesizesProfile(t *testing.T) {
	setupTestDB(t)

	_, err := ImportGroup(minimalImportPlan("legacy-team", groupexport.Group{
		Descr:        "imported from a v1 archive",
		DefaultModel: "sonnet", // the retired column, carried by an old export
		MaxMembers:   0,
	}))
	require.NoError(t, err, "ImportGroup")

	g, err := GetAgentGroupByName("legacy-team")
	require.NoError(t, err, "GetAgentGroupByName")
	require.NotNil(t, g, "imported group exists")
	assert.Equal(t, "group-default-legacy-team", g.DefaultProfile,
		"imported group points at the synthesized profile")

	prof, err := GetSpawnProfile("group-default-legacy-team")
	require.NoError(t, err, "GetSpawnProfile")
	require.NotNil(t, prof, "synthesized profile exists")
	assert.Equal(t, "claude", prof.Harness, "synthesized profile is a claude profile")
	assert.Equal(t, "sonnet", prof.Model, "synthesized profile carries the legacy model")
}

// TestImportGroup_LegacyDefaultModelDedupesName guards the UNIQUE(name)
// collision: when "group-default-<group>" is already taken (e.g. by a real
// profile or a prior import), the synthesized profile takes a numeric suffix
// instead of failing the import.
func TestImportGroup_LegacyDefaultModelDedupesName(t *testing.T) {
	setupTestDB(t)

	// A human-made profile already holds the base name.
	_, err := CreateSpawnProfile(&SpawnProfile{Name: "group-default-team", Model: "opus"})
	require.NoError(t, err, "seed colliding profile")

	_, err = ImportGroup(minimalImportPlan("team", groupexport.Group{DefaultModel: "haiku"}))
	require.NoError(t, err, "ImportGroup dedupes the synthesized name")

	g, err := GetAgentGroupByName("team")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "group-default-team-2", g.DefaultProfile,
		"synthesized profile takes a -2 suffix")

	prof, err := GetSpawnProfile("group-default-team-2")
	require.NoError(t, err)
	require.NotNil(t, prof)
	assert.Equal(t, "haiku", prof.Model, "the suffixed profile carries the legacy model")
}

// TestImportGroup_V2ArchiveNoSynthesis confirms a current (v2) archive — which
// carries no default_model — imports with no synthesized profile and an unset
// default_profile, so the back-compat path is a strict no-op there.
func TestImportGroup_V2ArchiveNoSynthesis(t *testing.T) {
	setupTestDB(t)

	_, err := ImportGroup(minimalImportPlan("modern-team", groupexport.Group{
		Descr: "imported from a v2 archive",
		// DefaultModel deliberately empty — a v2 export never carries it.
	}))
	require.NoError(t, err, "ImportGroup")

	g, err := GetAgentGroupByName("modern-team")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "", g.DefaultProfile, "no default profile synthesized for a v2 archive")

	profs, err := ListSpawnProfiles()
	require.NoError(t, err)
	assert.Empty(t, profs, "no spawn profile synthesized for a v2 archive")
}
