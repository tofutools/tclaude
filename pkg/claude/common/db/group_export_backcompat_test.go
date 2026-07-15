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

// TestImportGroup_V2ArchiveNoSynthesis confirms the older v2 shape — which
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

func TestGroupExport_GroupPermissionsRoundTrip(t *testing.T) {
	setupTestDB(t)

	srcID, err := CreateAgentGroup("src", "")
	require.NoError(t, err)
	require.NoError(t, ReplaceAgentGroupPermissions(srcID, []string{"human.notify", "groups.spawn"}, "test"))
	exp, err := CollectGroupExport("src")
	require.NoError(t, err)
	require.Len(t, exp.Group.Permissions, 2)
	assert.Equal(t, "groups.spawn", exp.Group.Permissions[0].Slug)
	assert.Equal(t, "human.notify", exp.Group.Permissions[1].Slug)
	assert.Equal(t, "test", exp.Group.Permissions[0].GrantedBy)
	assert.NotEmpty(t, exp.Group.Permissions[0].GrantedAt)

	_, err = ImportGroup(GroupImportPlan{
		Export: exp, TargetName: "dst", TargetCwd: "/tmp/import-target", ConvRemap: map[string]string{},
	})
	require.NoError(t, err)
	dst, err := GetAgentGroupByName("dst")
	require.NoError(t, err)
	got, err := ListAgentGroupPermissions(dst.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"groups.spawn", "human.notify"}, got)
	d, err := Open()
	require.NoError(t, err)
	var grantedAt, grantedBy string
	require.NoError(t, d.QueryRow(`SELECT granted_at, granted_by FROM agent_group_permissions WHERE group_id = ? AND slug = ?`,
		dst.ID, "groups.spawn").Scan(&grantedAt, &grantedBy))
	assert.Equal(t, exp.Group.Permissions[0].GrantedAt, grantedAt, "timestamp preserved")
	assert.Equal(t, exp.Group.Permissions[0].GrantedBy, grantedBy, "attribution preserved")
}

// TestGroupExport_GroupTargetCronJob_RoundTrips covers the JOH-26 PR3a fix to
// the cron export/import path: a group fan-out job (target_kind='group') must
// round-trip as one. Before the fix the export dropped target_kind, so the
// importer defaulted it to 'conv' and — now that a group job's target_agent is
// empty — the job came back as a broken conv job addressed to the empty conv.
func TestGroupExport_GroupTargetCronJob_RoundTrips(t *testing.T) {
	setupTestDB(t)

	srcID, err := CreateAgentGroup("src", "")
	require.NoError(t, err, "CreateAgentGroup")

	// A group fan-out cron job: the discriminator is target_kind='group', and
	// group_id IS the target group (no per-conv target).
	_, err = InsertAgentCronJob(&AgentCronJob{
		Name: "team-ping", TargetKind: CronTargetGroup, GroupID: srcID,
		IntervalSeconds: 600, Body: "standup", Enabled: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")

	exp, err := CollectGroupExport("src")
	require.NoError(t, err, "CollectGroupExport")
	require.Len(t, exp.CronJobs, 1, "the group job is exported")
	assert.Equal(t, CronTargetGroup, exp.CronJobs[0].TargetKind,
		"export carries the conv/group discriminator")

	_, err = ImportGroup(GroupImportPlan{
		Export: exp, TargetName: "dst", TargetCwd: "/tmp/import-target",
		ConvRemap: map[string]string{},
	})
	require.NoError(t, err, "ImportGroup")

	dst, err := GetAgentGroupByName("dst")
	require.NoError(t, err, "GetAgentGroupByName")
	require.NotNil(t, dst, "imported group exists")

	jobs, err := ListAgentCronJobs()
	require.NoError(t, err, "ListAgentCronJobs")
	var imported *AgentCronJob
	for _, j := range jobs {
		if j.GroupID == dst.ID {
			imported = j
		}
	}
	require.NotNil(t, imported, "the cron job landed in the imported group")
	assert.True(t, imported.IsGroupTarget(),
		"a group fan-out job round-trips as group-target, not a broken conv job")
	assert.Equal(t, CronTargetGroup, imported.TargetKind, "target_kind preserved")
}

// actorMeta is the raw agents-table metadata the import's enrollment replay
// touches, read as stored strings (no time-parse coupling).
type actorMeta struct {
	createdAt, createdVia, retiredAt, retiredBy, retireReason, pendingName string
}

func readActorMeta(t *testing.T, agentID string) actorMeta {
	t.Helper()
	d, err := Open()
	require.NoError(t, err, "open db")
	var m actorMeta
	require.NoError(t, d.QueryRow(`SELECT created_at, created_via, retired_at,
		retired_by, retire_reason, pending_name FROM agents WHERE agent_id = ?`, agentID).Scan(
		&m.createdAt, &m.createdVia, &m.retiredAt, &m.retiredBy, &m.retireReason, &m.pendingName),
		"read actor metadata")
	return m
}

// TestImportGroup_PreexistingRetiredActorCollisionRollsBack pins the import
// preflight/transaction TOCTOU boundary. Production preflight remaps every
// locally-present conv, so finding one inside the IMMEDIATE import transaction
// means the actor appeared after inspection. The import must abort instead of
// attaching archive authority that could revive on reinstatement.
func TestImportGroup_PreexistingRetiredActorCollisionRollsBack(t *testing.T) {
	setupTestDB(t)

	// A pre-existing local actor with its own metadata.
	localAgent, _, err := EnsureAgentForConv("conv-local", "spawn")
	require.NoError(t, err, "seed local actor")
	d, err := Open()
	require.NoError(t, err, "open db")
	_, err = d.Exec(`UPDATE agents SET created_at = '2020-01-01T00:00:00Z',
		retired_at = '2021-01-01T00:00:00Z', retired_by = 'local-human',
		retire_reason = 'local-reason', pending_name = 'local-pending'
		WHERE agent_id = ?`, localAgent)
	require.NoError(t, err, "stamp local metadata")

	// Import a stale plan whose member conv remaps ONTO conv-local.
	_, err = ImportGroup(GroupImportPlan{
		Export: &groupexport.Export{
			FormatVersion: groupexport.FormatVersion,
			SourceGroup:   "src",
			Group:         groupexport.Group{Descr: "imported"},
			Members:       []groupexport.Member{{ConvID: "conv-src", Role: "member", JoinedAt: "2026-01-01T00:00:00Z"}},
			Enrollments: []groupexport.Enrollment{{
				ConvID: "conv-src", EnrolledAt: "2099-01-01T00:00:00Z", EnrolledVia: "import",
				RetiredAt: "2099-02-02T00:00:00Z", RetiredBy: "import-human",
				RetireReason: "import-reason", PendingName: "import-pending",
			}},
		},
		TargetName: "imported-team",
		TargetCwd:  "/tmp/imported",
		ConvRemap:  map[string]string{"conv-src": "conv-local"},
	})
	require.ErrorContains(t, err, "actor collision for conv conv-local")
	imported, getErr := GetAgentGroupByName("imported-team")
	require.NoError(t, getErr)
	assert.Nil(t, imported, "collision rolls back the entire imported group")

	got := readActorMeta(t, localAgent)
	assert.Equal(t, "2020-01-01T00:00:00Z", got.createdAt, "created_at not clobbered")
	assert.Equal(t, "spawn", got.createdVia, "created_via not clobbered")
	assert.Equal(t, "2021-01-01T00:00:00Z", got.retiredAt, "retired_at not clobbered")
	assert.Equal(t, "local-human", got.retiredBy, "retired_by not clobbered")
	assert.Equal(t, "local-reason", got.retireReason, "retire_reason not clobbered")
	assert.Equal(t, "local-pending", got.pendingName, "pending_name not clobbered")
}

func TestImportGroup_AllowsExactOEXCLClaimedIndexPath(t *testing.T) {
	setupTestDB(t)
	const conv = "44444444-dead-beef-cafe-000000000000"
	const claimedPath = "/tmp/import-target/44444444-dead-beef-cafe-000000000000.jsonl"
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID: conv, ProjectDir: "/tmp/import-target", FullPath: claimedPath,
	}))

	_, err := ImportGroup(GroupImportPlan{
		Export: &groupexport.Export{
			FormatVersion: groupexport.FormatVersion,
			SourceGroup:   "claimed-index",
			Members:       []groupexport.Member{{ConvID: conv, Role: "lead"}},
		},
		TargetName:               "claimed-index",
		TargetCwd:                "/tmp/import-target",
		ConvRemap:                map[string]string{conv: conv},
		ClaimedConversationPaths: map[string]string{conv: claimedPath},
	})
	require.NoError(t, err, "an exact daemon-owned O_EXCL path is the import's own monitor row")
	groups, err := ListGroupsForConv(conv)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "claimed-index", groups[0].Name)
}

// TestImportGroup_FreshActorMetadataRestored is the other half of the import
// collision guard: an actor THIS import freshly creates still gets archived
// facts restored, but a retired archive cannot retain executable authority.
func TestImportGroup_FreshActorMetadataRestored(t *testing.T) {
	setupTestDB(t)

	_, err := ImportGroup(GroupImportPlan{
		Export: &groupexport.Export{
			FormatVersion: groupexport.FormatVersion,
			SourceGroup:   "src2",
			Group:         groupexport.Group{Descr: "imported"},
			Members:       []groupexport.Member{{ConvID: "conv-fresh", Role: "member", JoinedAt: "2026-01-01T00:00:00Z"}},
			Owners:        []groupexport.Owner{{ConvID: "conv-fresh", GrantedAt: "2026-01-01T00:00:00Z", GrantedBy: "source"}},
			Permissions: []groupexport.Permission{{
				ConvID: "conv-fresh", Slug: "groups.spawn", Effect: PermEffectGrant,
				GrantedAt: "2026-01-01T00:00:00Z", GrantedBy: "source",
			}},
			Enrollments: []groupexport.Enrollment{{
				ConvID: "conv-fresh", EnrolledAt: "2022-03-03T00:00:00Z", EnrolledVia: "spawn",
				RetiredAt: "2022-04-04T00:00:00Z", RetiredBy: "src-human",
				RetireReason: "src-reason", PendingName: "src-pending",
			}},
			SudoGrants: []groupexport.SudoGrant{{
				ConvID: "conv-fresh", Slug: "groups.spawn", GrantedAt: "2026-01-01T00:00:00Z",
				ExpiresAt: "2099-01-01T00:00:00Z", GrantedBy: "source", Reason: "source grant",
			}},
			CronJobs: []groupexport.CronJob{{
				ID: 1, Name: "source-cron", OwnerConv: "conv-fresh", TargetKind: CronTargetGroup,
				IntervalSeconds: 60, Body: "must be disabled", Enabled: 1, CreatedAt: "2026-01-01T00:00:00Z",
			}},
		},
		TargetName: "fresh-team",
		TargetCwd:  "/tmp/fresh",
		ConvRemap:  map[string]string{}, // identity: conv-fresh does not exist locally
	})
	require.NoError(t, err, "ImportGroup")

	actor, err := GetAgentByConv("conv-fresh")
	require.NoError(t, err, "GetAgentByConv")
	require.NotNil(t, actor, "actor created by import")

	got := readActorMeta(t, actor.AgentID)
	assert.Equal(t, "2022-03-03T00:00:00Z", got.createdAt, "archived created_at restored")
	assert.Equal(t, "2022-04-04T00:00:00Z", got.retiredAt, "archived retired_at restored")
	assert.Equal(t, "src-human", got.retiredBy, "archived retired_by restored")
	assert.Equal(t, "src-reason", got.retireReason, "archived retire_reason restored")
	assert.Equal(t, "src-pending", got.pendingName, "archived pending_name restored")
	groups, err := ListGroupsForConv("conv-fresh")
	require.NoError(t, err)
	assert.Empty(t, groups, "retired imported actor keeps no membership authority")
	group, err := GetAgentGroupByName("fresh-team")
	require.NoError(t, err)
	require.NotNil(t, group)
	owners, err := ListAgentGroupOwners(group.ID)
	require.NoError(t, err)
	assert.Empty(t, owners, "retired imported actor keeps no ownership authority")
	overrides, err := ListAgentPermissionOverridesForConv("conv-fresh")
	require.NoError(t, err)
	assert.Empty(t, overrides, "retired imported actor keeps no permanent authority")
	activeSudo, err := ListActiveSudoGrants("conv-fresh")
	require.NoError(t, err)
	assert.Empty(t, activeSudo, "retired imported actor keeps no sudo authority")
	jobs, err := ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.False(t, jobs[0].Enabled, "retired imported actor keeps no scheduled authority")
	assert.Equal(t, CronDisabledReasonAgentRetired, jobs[0].DisabledReason)
}
