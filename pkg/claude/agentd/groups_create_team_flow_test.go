package agentd_test

import (
	"bytes"
	"path/filepath"
	"testing"

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
	if rc != 0 {
		t.Fatalf("RunGroupsCreate rc=%d stdout=%s", rc, stdout.String())
	}

	g, err := db.GetAgentGroupByName("reviewer-team")
	if err != nil || g == nil {
		t.Fatalf("group not found: err=%v g=%v", err, g)
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		t.Fatalf("ListAgentGroupMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d: %+v", len(members), members)
	}
	byAlias := map[string]*db.AgentGroupMember{}
	for _, m := range members {
		byAlias[m.Alias] = m
	}
	lead := byAlias["lead"]
	if lead == nil {
		t.Fatal("lead member missing")
	}
	if lead.Role != "tech-lead" {
		t.Errorf("lead.Role = %q, want %q", lead.Role, "tech-lead")
	}
	if lead.Descr != "Owns the diff" {
		t.Errorf("lead.Descr = %q, want %q", lead.Descr, "Owns the diff")
	}

	tester := byAlias["tester"]
	if tester == nil {
		t.Fatal("tester member missing")
	}
	if tester.Role != "test-runner" {
		t.Errorf("tester.Role = %q, want %q", tester.Role, "test-runner")
	}

	// Each member should have a live SessionRow whose cwd is the
	// caller's cwd (since neither --member spec pinned `cwd=`).
	for _, m := range members {
		rows, err := db.FindSessionsByConvID(m.ConvID)
		if err != nil || len(rows) == 0 {
			t.Errorf("member %q (conv %s) has no session row: %v", m.Alias, m.ConvID, err)
			continue
		}
		if got := resolveSym(t, rows[0].Cwd); got != callerCwd {
			t.Errorf("member %q SessionRow.Cwd = %q, want %q (caller's cwd)",
				m.Alias, got, callerCwd)
		}
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
	if rc != 0 {
		t.Fatalf("RunGroupsCreate rc=%d stdout=%s", rc, stdout.String())
	}

	g, _ := db.GetAgentGroupByName("team")
	members, _ := db.ListAgentGroupMembers(g.ID)
	if len(members) != 1 {
		t.Fatalf("want 1 member, got %d", len(members))
	}
	rows, _ := db.FindSessionsByConvID(members[0].ConvID)
	if len(rows) == 0 {
		t.Fatal("no session row")
	}
	if got := resolveSym(t, rows[0].Cwd); got != memberCwd {
		t.Errorf("member SessionRow.Cwd = %q, want %q (per-member override)",
			got, memberCwd)
	}
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
	if rc == 0 {
		t.Fatalf("expected non-zero rc on bad spec; stderr=%s", stderr.String())
	}

	if g, err := db.GetAgentGroupByName("doomed"); err == nil && g != nil {
		t.Errorf("group %q should not have been created on bad spec", g.Name)
	}
}

// Sanity: filepath.EvalSymlinks works on tempdir paths the same way it
// does in spawn_cli_flow_test.go's resolveSym. Just keeps the tests
// independent if that helper ever moves.
var _ = filepath.EvalSymlinks
