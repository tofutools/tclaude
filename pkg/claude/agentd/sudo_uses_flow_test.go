package agentd_test

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func haveLimitedSpawnSudo(t *testing.T, conv string, uses int64) int64 {
	t.Helper()
	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: conv, Slug: agentd.PermGroupsMembersSpawn,
		GrantedAt: now, ExpiresAt: now.Add(time.Hour), GrantedBy: "test", UsesLimit: uses, UsesLeft: uses})
	require.NoError(t, err)
	return id
}

func sudoUsesLeft(t *testing.T, id int64) int64 {
	t.Helper()
	grant, err := db.GetSudoGrant(id)
	require.NoError(t, err)
	return grant.UsesLeft
}

func spawnCapableLead(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	const lead = "sudouses-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("alpha")
	haveSpawnCapableMember(t, f, "alpha", lead)
	return lead
}

// Scenario: "any group for an hour, at most 2 agents". The third spawn is
// refused with time left, and the last use works like any other.
func TestSudoUses_LimitEndsTheGrantBeforeItsTime(t *testing.T) {
	f := newFlow(t)
	lead := spawnCapableLead(t, f)
	id := haveLimitedSpawnSudo(t, lead, 2)

	first := spawnAttempt(t, f, lead, "alpha", "one")
	require.Equalf(t, http.StatusOK, first.Code, "body=%s", first.Body)
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))

	last := spawnAttempt(t, f, lead, "alpha", "two")
	require.Equalf(t, http.StatusOK, last.Code, "the last use is a use; body=%s", last.Body)
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))

	over := spawnAttempt(t, f, lead, "alpha", "three")
	assert.Equalf(t, http.StatusForbidden, over.Code, "body=%s", over.Body)
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
}

// A spawn refused after the gate, and one that fails to start, spawned nothing.
func TestSudoUses_FailedSpawnGivesItsUseBack(t *testing.T) {
	f := newFlow(t)
	lead := spawnCapableLead(t, f)
	id := haveLimitedSpawnSudo(t, lead, 1)

	refused := agentReq(t, f, lead, http.MethodPost, "/v1/groups/alpha/spawn",
		map[string]any{"name": "bad", "harness": "no-such-harness"})
	require.Equalf(t, http.StatusBadRequest, refused.Code, "body=%s", refused.Body.String())
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))

	ok := spawnAttempt(t, f, lead, "alpha", "good")
	require.Equalf(t, http.StatusOK, ok.Code, "the use came back; body=%s", ok.Body)
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
}

// Standing authority pays for the spawn, so the grant is not what allowed it.
func TestSudoUses_StandingAccessSpendsNothing(t *testing.T) {
	f := newFlow(t)
	lead := spawnCapableLead(t, f)
	require.NoError(t, db.SetAgentPermissionOverride(lead, agentd.PermGroupsMembersSpawn, db.PermEffectGrant, "test"))
	id := haveLimitedSpawnSudo(t, lead, 1)

	ok := spawnAttempt(t, f, lead, "alpha", "one")
	require.Equalf(t, http.StatusOK, ok.Code, "body=%s", ok.Body)
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))
}

func TestSudoUses_DashboardGrantValidatesTheLimit(t *testing.T) {
	c := newStewardCarryFlow(t)
	steward := c.summon()
	grant := func(slug string, uses int) int {
		return c.post("/api/sudo", map[string]any{"agent_id": c.agentID(steward), "slugs": []string{slug},
			"duration": "5m", "uses": map[string]int{slug: uses}}).Code
	}
	assert.Equal(t, http.StatusBadRequest, grant(agentd.PermSelfSchedule, 3), "nothing to count")
	assert.Equal(t, http.StatusBadRequest, grant(agentd.PermGroupsMembersSpawn, 0))
	assert.Equal(t, http.StatusBadRequest, grant(agentd.PermGroupsMembersSpawn, 1001))
	require.Equal(t, http.StatusOK, grant(agentd.PermGroupsMembersSpawn, 4))

	held := c.sudoSlugs(steward)[agentd.PermGroupsMembersSpawn]
	assert.Equal(t, int64(4), held.UsesLimit)
	assert.Equal(t, int64(4), held.UsesLeft)
}

// A new Steward inherits what is left, never a fresh count.
func TestSudoUses_NewStewardNeverRefillsAStoredGrant(t *testing.T) {
	c := newStewardCarryFlow(t)
	first := c.summon()
	rec := c.post("/api/sudo", map[string]any{"agent_id": c.agentID(first), "slugs": []string{agentd.PermGroupsMembersSpawn},
		"duration": "45s", "carry": []string{agentd.PermGroupsMembersSpawn},
		"uses": map[string]int{agentd.PermGroupsMembersSpawn: 2}})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	_, taken, err := db.TakeSudoUse(c.sudoSlugs(first)[agentd.PermGroupsMembersSpawn].ID)
	require.NoError(t, err)
	require.True(t, taken)
	c.shutDown(first)

	second := c.summon()
	held := c.sudoSlugs(second)[agentd.PermGroupsMembersSpawn]
	require.NotNil(t, held)
	assert.Equal(t, int64(2), held.UsesLimit)
	assert.Equal(t, int64(1), held.UsesLeft)

	_, taken, err = db.TakeSudoUse(held.ID)
	require.NoError(t, err)
	require.True(t, taken)
	c.shutDown(second)
	assert.Empty(t, c.sudoSlugs(c.summon()), "a used-up grant is not handed on")
}

// A launch that never happened spawned nothing.
func TestSudoUses_LaunchFailureGivesItsUseBack(t *testing.T) {
	f := newFlow(t)
	lead := spawnCapableLead(t, f)
	id := haveLimitedSpawnSudo(t, lead, 1)
	previous := agentd.Spawn
	agentd.Spawn = &failingBundleSpawner{}
	t.Cleanup(func() { agentd.Spawn = previous })

	failed := spawnAttempt(t, f, lead, "alpha", "one")
	require.NotEqualf(t, http.StatusOK, failed.Code, "body=%s", failed.Body)
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))
}

// A scribe summon is a spawn, and a limit reaches it. A forged decision header
// must not spend someone else's grant.
func TestSudoUses_ScribeSummonCounts(t *testing.T) {
	f := newFlow(t)
	lead := spawnCapableLead(t, f)
	id := haveLimitedSpawnSudo(t, lead, 1)
	stubScribeTerminal(t)
	require.NoError(t, db.SetAgentPermissionOverride(lead, agentd.PermPermissionsGrant, db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(lead, agentd.PermAgentStop, db.PermEffectGrant, "test"))

	const victim = "sudouses-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(victim, "victim")
	other := haveLimitedSpawnSudo(t, victim, 1)

	summon := func(name string) *http.Request {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{"name": name, "brief": "Help.", "slugs": []string{agentd.PermAgentStop}}), lead)
		r.Header.Set("X-Tclaude-Internal-Authorized-Sudo-Grant", strconv.FormatInt(other, 10))
		return r
	}
	first := testharness.Serve(f.Mux, summon("helper-one"))
	require.Equalf(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
	assert.Equal(t, int64(1), sudoUsesLeft(t, other), "only the caller's own grant is spent")

	over := testharness.Serve(f.Mux, summon("helper-two"))
	assert.Equalf(t, http.StatusForbidden, over.Code, "body=%s", over.Body.String())
}

// Remote spawns do not pass the reservation, so the pair cannot be granted.
func TestSudoUses_PeerScopeTakesNoLimit(t *testing.T) {
	fh := newFedHarness(t)
	const conv = "sudouses-aaaa-bbbb-cccc-000000000003"
	fh.f.HaveConvWithTitle(conv, "worker")
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)

	rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodPost, "/api/sudo", map[string]any{
		"agent_id": agentID, "slugs": []string{agentd.PermGroupsMembersSpawn}, "duration": "5m",
		"uses":   map[string]int{agentd.PermGroupsMembersSpawn: 3},
		"scopes": map[string]any{agentd.PermGroupsMembersSpawn: map[string]any{"peer": []string{fh.peer.id.ID() + "/builders"}}},
	}))
	assert.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "local actions only")
}

func haveLimitedSudo(t *testing.T, conv, slug string, uses int64) int64 {
	t.Helper()
	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: conv, Slug: slug,
		GrantedAt: now, ExpiresAt: now.Add(time.Hour), GrantedBy: "test", UsesLimit: uses, UsesLeft: uses})
	require.NoError(t, err)
	return id
}

func sudoUsesWorker(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	const worker = "sudouses-aaaa-bbbb-cccc-000000000002"
	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-uses", "tmux-uses", f.TestCwd("uses"))
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfTags, db.PermEffectDeny, "test"))
	return worker
}

func setOwnTags(t *testing.T, f *testharness.Flow, conv, method string, tags ...string) int {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/whoami/tags",
		map[string]any{"tags": tags}), conv)).Code
}

// One allowed action is one use, for a slug no handler knows is counted: a read
// is free, a failed attempt gives its use back, and the last use still works.
func TestSudoUses_GateCountsAnyActionSlug(t *testing.T) {
	f := newFlow(t)
	worker := sudoUsesWorker(t, f)
	id := haveLimitedSudo(t, worker, agentd.PermSelfTags, 2)

	require.Equal(t, http.StatusOK, setOwnTags(t, f, worker, http.MethodPut, "one"))
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/whoami/tags", nil), worker))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int64(1), sudoUsesLeft(t, id), "reading is not an action")

	assert.Equal(t, http.StatusBadRequest, setOwnTags(t, f, worker, http.MethodPut, strings.Repeat("x", 200)))
	assert.Equal(t, int64(1), sudoUsesLeft(t, id), "a failed attempt gives its use back")

	require.Equal(t, http.StatusOK, setOwnTags(t, f, worker, http.MethodPut, "two"))
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
	assert.Equal(t, http.StatusForbidden, setOwnTags(t, f, worker, http.MethodPut, "three"))
}

func TestSudoUses_ParallelActionsStopAtTheLimit(t *testing.T) {
	f := newFlow(t)
	worker := sudoUsesWorker(t, f)
	id := haveLimitedSudo(t, worker, agentd.PermSelfTags, 3)
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if setOwnTags(t, f, worker, http.MethodPut, "t"+strconv.Itoa(i)) == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(3), ok.Load())
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
}

func TestSudoUses_StandingAccessLeavesAnActionGrantUnspent(t *testing.T) {
	f := newFlow(t)
	worker := sudoUsesWorker(t, f)
	id := haveLimitedSudo(t, worker, agentd.PermSelfTags, 1)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfTags, db.PermEffectGrant, "test"))
	require.Equal(t, http.StatusOK, setOwnTags(t, f, worker, http.MethodPut, "one"))
	assert.Equal(t, int64(1), sudoUsesLeft(t, id))
}

// A direct message is checked outside the gates, and still costs one use however
// many times one request checks it.
func TestSudoUses_DirectMessageCounts(t *testing.T) {
	f := newFlow(t)
	worker := sudoUsesWorker(t, f)
	const stranger = "sudouses-aaaa-bbbb-cccc-000000000003"
	f.HaveConvWithTitle(stranger, "stranger")
	f.HaveAliveSession(stranger, "lbl-str", "tmux-str", f.TestCwd("str"))
	f.HaveGroup("beta")
	f.HaveMember("beta", stranger)
	id := haveLimitedSudo(t, worker, agentd.PermMessageDirect, 1)
	send := func() int {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages",
			map[string]any{"to": stranger, "body": "hello"}), worker)).Code
	}
	require.Equal(t, http.StatusOK, send())
	assert.Equal(t, int64(0), sudoUsesLeft(t, id))
	assert.Equal(t, http.StatusForbidden, send())
}

// With nothing to count against, a limited grant does not speak at all.
func TestSudoUses_ACheckWithoutARequestGetsNoAuthority(t *testing.T) {
	f := newFlow(t)
	worker := sudoUsesWorker(t, f)
	haveLimitedSudo(t, worker, agentd.PermMessageDirect, 2)
	assert.False(t, agentd.HoldsPermissionForTest(worker, agentd.PermMessageDirect))
}

func TestSudoUses_EverySlugCountsOrSaysWhyNot(t *testing.T) {
	slugs := agentd.SudoCountabilityForTest()
	assert.Greater(t, len(slugs), 100)
	for slug, why := range slugs {
		assert.Equal(t, agentd.IsCountableForTest(slug), why == "", slug)
	}
	assert.Equal(t, "It only reads, so there is no action to count.", slugs[agentd.PermUsageRead])
	assert.Contains(t, slugs[agentd.PermGroupsMembersSpawnCwd], "groups.members.spawn")
}
