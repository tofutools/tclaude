package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario: the `tclaude session new` self-guard is armed — a
// `claude`/`node` ancestor is "present" — and an agent that is itself
// running under Claude Code asks the daemon to spawn a teammate.
//
// The spawn must still succeed. The nested-spawn guard only blocks a CC
// instance running `tclaude session new` *directly*; a daemon-mediated
// spawn forks a fresh `tclaude` whose ancestry is agentd (the human's
// daemon), not claude, so GuardAgainstNestedSpawn never trips for it.
//
// This is the regression guard for the task's CRITICAL note: do NOT let
// the nested-spawn guard leak into the daemon's spawn path. With
// session.ClaudeAncestorCheck forced true for the whole test, the guard
// is provably armed (asserted directly below). If a future change
// routed handleGroupSpawn / SpawnDetachedTclaudeNew through
// GuardAgainstNestedSpawn in-process, f.Spawn would fail here.
func TestDaemonSpawn_NotBlockedByNestedClaudeGuard(t *testing.T) {
	// Arm the guard: pretend this process sits under Claude Code.
	prev := session.ClaudeAncestorCheck
	session.ClaudeAncestorCheck = func() bool { return true }
	t.Cleanup(func() { session.ClaudeAncestorCheck = prev })

	// Sanity: with the guard armed a *direct* `tclaude session new`
	// would be refused — otherwise this scenario proves nothing.
	require.Error(t, session.GuardAgainstNestedSpawn(),
		"guard must be armed for this regression test to be meaningful")

	f := newFlow(t)
	f.HaveGroup("alpha")

	// The requester is an agent (AsAgentPeer => HasClaudeAncestor=true)
	// with `groups.spawn` granted — the realistic "a lead agent spawns
	// a worker" path, and the one most at risk of being broken.
	const lead = "lead-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"),
		"grant groups.spawn to the requesting agent")

	// f.Spawn fatals on any non-200, so a guard that wrongly blocked
	// the daemon path would fail the test right here.
	resp := f.AsAgent(lead).Spawn("alpha", "worker")
	require.NotEmpty(t, resp.ConvID, "daemon spawn must succeed with the guard armed")

	// Real surface: the new teammate shows up in
	// `tclaude agent groups members alpha`. The /rename that sets its
	// "worker" title is async; here we only pin that the membership
	// row landed — spawn_flow_test.go covers the title surfacing.
	var found bool
	for _, m := range f.ListGroupMembers("alpha") {
		if m.ConvID == resp.ConvID {
			found = true
		}
	}
	assert.True(t, found, "spawned worker must appear in group alpha")
}
