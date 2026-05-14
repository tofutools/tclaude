package agentd_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a human runs
//
//   tclaude agent groups create reviewer-team \
//     --member alias=lead,role=tech-lead,descr=Owns the diff \
//     --member alias=tester,role=test-runner
//
// from a project tree. The CLI must:
//  1. create the group via POST /v1/groups,
//  2. spawn one fresh CC instance per --member via the existing
//     POST /v1/groups/{name}/spawn endpoint (same path agent spawn uses),
//  3. inherit the caller's cwd into each spawn body so members start
//     where the human is, not where the daemon was launched.
//
// Real surface assertion: agent_group_members has one row per member
// (with the right alias / role / descr) and each member has a live
// SessionRow whose Cwd matches the caller's cwd.
//
// Pins the bug class for a future refactor that loses caller-cwd
// propagation through the bootstrap path (the per-spawn cwd default
// is independent of agent.RunSpawn's; both must agree).
func TestGroupsCreateTeam_BootstrapsMembers(t *testing.T) {
	f := newFlow(t)
	bridgeAgentClientToMux(t, f.Mux)

	callerCwd := resolveSym(t, t.TempDir())
	chdirTo(t, callerCwd)

	stdout := new(bytes.Buffer)
	rc := agent.RunGroupsCreate(&agent.GroupsCreateParams{
		Name: "reviewer-team",
		Members: []string{
			"alias=lead,role=tech-lead,descr=Owns the diff",
			"alias=tester,role=test-runner",
		},
	}, stdout, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunGroupsCreate stdout=%s", stdout.String())

	g, err := db.GetAgentGroupByName("reviewer-team")
	require.NoError(t, err, "group lookup")
	require.NotNil(t, g, "group not found")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err, "ListAgentGroupMembers")
	require.Len(t, members, 2, "expected 2 members: %+v", members)
	byAlias := map[string]*db.AgentGroupMember{}
	for _, m := range members {
		byAlias[m.Alias] = m
	}
	lead := byAlias["lead"]
	require.NotNil(t, lead, "lead member missing")
	assert.Equal(t, "tech-lead", lead.Role, "lead.Role")
	assert.Equal(t, "Owns the diff", lead.Descr, "lead.Descr")

	tester := byAlias["tester"]
	require.NotNil(t, tester, "tester member missing")
	assert.Equal(t, "test-runner", tester.Role, "tester.Role")

	// Each member should have a live SessionRow whose cwd is the
	// caller's cwd (since neither --member spec pinned `cwd=`).
	for _, m := range members {
		rows, err := db.FindSessionsByConvID(m.ConvID)
		if !assert.NoError(t, err) || !assert.NotEmpty(t, rows,
			"member %q (conv %s) has no session row", m.Alias, m.ConvID) {
			continue
		}
		got := resolveSym(t, rows[0].Cwd)
		assert.Equal(t, callerCwd, got,
			"member %q SessionRow.Cwd (caller's cwd)", m.Alias)
	}
}

// Scenario: a member spec pins its own cwd. That cwd wins over the
// caller's cwd default.
func TestGroupsCreateTeam_PerMemberCwdOverride(t *testing.T) {
	f := newFlow(t)
	bridgeAgentClientToMux(t, f.Mux)

	callerCwd := resolveSym(t, t.TempDir())
	chdirTo(t, callerCwd)
	memberCwd := resolveSym(t, t.TempDir())

	stdout := new(bytes.Buffer)
	rc := agent.RunGroupsCreate(&agent.GroupsCreateParams{
		Name: "team",
		Members: []string{
			"alias=worker,cwd=" + memberCwd,
		},
	}, stdout, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunGroupsCreate stdout=%s", stdout.String())

	g, _ := db.GetAgentGroupByName("team")
	members, _ := db.ListAgentGroupMembers(g.ID)
	require.Len(t, members, 1, "want 1 member")
	rows, _ := db.FindSessionsByConvID(members[0].ConvID)
	require.NotEmpty(t, rows, "no session row")
	got := resolveSym(t, rows[0].Cwd)
	assert.Equal(t, memberCwd, got, "member SessionRow.Cwd (per-member override)")
}

// Scenario: the parser rejects a malformed spec before any DB work.
// A typo'd --member must NOT leave an empty group sitting around.
func TestGroupsCreateTeam_BadSpecAbortsBeforeCreate(t *testing.T) {
	f := newFlow(t)
	bridgeAgentClientToMux(t, f.Mux)

	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	rc := agent.RunGroupsCreate(&agent.GroupsCreateParams{
		Name: "doomed",
		Members: []string{
			"alias=ok",
			"role=tester", // missing alias — should fail
		},
	}, new(bytes.Buffer), stderr)
	require.NotEqual(t, 0, rc, "expected non-zero rc on bad spec; stderr=%s", stderr.String())

	g, err := db.GetAgentGroupByName("doomed")
	if err == nil {
		assert.Nil(t, g, "group should not have been created on bad spec")
	}
}

// Sanity: filepath.EvalSymlinks works on tempdir paths the same way it
// does in spawn_cli_flow_test.go's resolveSym. Just keeps the tests
// independent if that helper ever moves.
var _ = filepath.EvalSymlinks
