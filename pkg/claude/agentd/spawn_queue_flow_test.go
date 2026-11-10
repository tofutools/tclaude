package agentd_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func setCapacity(t *testing.T, k config.CapacityConfig) {
	t.Helper()
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Capacity = &k
		return nil
	})
	require.NoError(t, err)
}

func spawnQueueList(t *testing.T, f *testharness.Flow, asAgent string) (agent.SpawnQueueList, string) {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-queue", nil)
	if asAgent == "" {
		r = agentd.AsHumanPeer(r)
	} else {
		r = agentd.AsAgentPeer(r, asAgent)
	}
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list agent.SpawnQueueList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	return list, rec.Body.String()
}

func cancelQueued(t *testing.T, f *testharness.Flow, id int64, asAgent string) int {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodDelete, "/v1/spawn-queue/"+strconv.FormatInt(id, 10), nil)
	if asAgent == "" {
		r = agentd.AsHumanPeer(r)
	} else {
		r = agentd.AsAgentPeer(r, asAgent)
	}
	return testharness.Serve(f.Mux, r).Code
}

func queuedResp(t *testing.T, resp testharness.SpawnResp) agent.SpawnResponse {
	t.Helper()
	require.Equal(t, http.StatusAccepted, resp.Code, string(resp.Raw))
	var out agent.SpawnResponse
	require.NoError(t, json.Unmarshal(resp.Raw, &out))
	require.True(t, out.Queued, string(resp.Raw))
	return out
}

// Scenario: max 1 agent, queue mode. Two more spawns wait in order with
// positions and no body echo; when the first agent stops, the queue launches
// the head as the human who asked, and the second moves up.
func TestSpawnQueue_QueuesInOrderAndLaunchesWhenCapacityFrees(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})

	first := f.AsHuman().Spawn("team", "first")

	second := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{
		"name": "second", "environment": []map[string]string{{"name": "QUEUE_SECRET", "value": "hunter2"}},
	}))
	assert.Equal(t, 1, second.Position)
	assert.Contains(t, second.QueueReason, "1 of 1")
	third := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "third"}))
	assert.Equal(t, 2, third.Position)

	list, raw := spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 2)
	assert.Equal(t, "second", list.Entries[0].Name)
	assert.Equal(t, 1, list.Entries[0].Position)
	assert.Equal(t, "third", list.Entries[1].Name)
	assert.Equal(t, "queue", list.Capacity.WhenFull)
	assert.Equal(t, 2, list.Capacity.Queued)
	assert.NotContains(t, raw, "hunter2", "a listing never carries the queued request")
	assert.NotContains(t, raw, "QUEUE_SECRET")

	// Still full: nothing launches, nothing jumps the line.
	agentd.RunSpawnQueueForTest()
	list, _ = spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 2)
	jumper := f.AsHuman().SpawnWith("team", map[string]any{"name": "jumper", "no_queue": true})
	require.Equal(t, http.StatusConflict, jumper.Code, string(jumper.Raw))
	assert.Contains(t, string(jumper.Raw), "node_busy")

	f.MarkOffline(first.TmuxSession)
	agentd.RunSpawnQueueForTest()

	list, _ = spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 1, "the head launched; capacity holds one, so the next still waits")
	assert.Equal(t, "third", list.Entries[0].Name)
	assert.Equal(t, 1, list.Entries[0].Position)

	ended, err := db.GetSpawnQueueEntry(second.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, ended.State, ended.Reason)
	body, err := db.SpawnQueueBody(second.QueueID)
	require.NoError(t, err)
	assert.Empty(t, body, "the stored request is dropped when the entry ends")
	launched, err := db.GetAgent(ended.ReservedAgent)
	require.NoError(t, err)
	require.NotNil(t, launched, "the launch used the reserved agent id")
	f.AssertGroupMember("team", launched.CurrentConvID, "second", 5*time.Second)
}

// Scenario: the free-RAM threshold refuses by default, with the reason, and
// a probe that cannot read RAM fails closed.
func TestSpawnQueue_RAMThresholdRefusesByDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	setCapacity(t, config.CapacityConfig{MinFreeRAMMB: 1024})
	t.Cleanup(agentd.SetCapacityResourcesForTest(512, 1<<20))

	resp := f.AsHuman().SpawnWith("team", map[string]any{"name": "w"})
	require.Equal(t, http.StatusConflict, resp.Code, string(resp.Raw))
	assert.Contains(t, string(resp.Raw), "free RAM 512 MB is below capacity.min_free_ram_mb 1024")

	t.Cleanup(agentd.SetCapacityResourcesForTest(-1, 1<<20))
	resp = f.AsHuman().SpawnWith("team", map[string]any{"name": "w"})
	require.Equal(t, http.StatusConflict, resp.Code, string(resp.Raw))
	assert.Contains(t, string(resp.Raw), "free RAM unknown")

	t.Cleanup(agentd.SetCapacityResourcesForTest(4096, 1<<20))
	f.AsHuman().Spawn("team", "fits")
}

// Scenario: the disk threshold queues in queue mode and launches once space
// is back.
func TestSpawnQueue_DiskThresholdQueues(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	setCapacity(t, config.CapacityConfig{MinFreeDiskMB: 100, WhenFull: config.CapacityWhenFullQueue})
	t.Cleanup(agentd.SetCapacityResourcesForTest(1<<20, 10))

	q := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "w"}))
	assert.Contains(t, q.QueueReason, "below capacity.min_free_disk_mb 100")
	agentd.RunSpawnQueueForTest()
	e, err := db.GetSpawnQueueEntry(q.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueQueued, e.State)

	t.Cleanup(agentd.SetCapacityResourcesForTest(1<<20, 1<<20))
	agentd.RunSpawnQueueForTest()
	e, err = db.GetSpawnQueueEntry(q.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, e.State, e.Reason)
}

// Scenario: an agent queues a spawn. Other agents see it only when they share
// the group; only the requester, a group owner or the human may cancel.
// Retiring the requester fails its other entry rather than launching it as
// anyone else.
func TestSpawnQueue_AgentVisibilityCancelAndRetiredRequester(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	f.HaveGroup("elsewhere")
	const requester = "req00000-aaaa-bbbb-cccc-111111111111"
	const teammate = "mate0000-aaaa-bbbb-cccc-111111111111"
	const outsider = "outs0000-aaaa-bbbb-cccc-111111111111"
	haveLineageParent(t, f, "team", requester, "claude", "off", "", "bypassPermissions")
	f.HaveMember("team", teammate)
	f.HaveMember("elsewhere", outsider)
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	occupant := f.AsHuman().Spawn("team", "occupant")

	a := queuedResp(t, f.AsAgent(requester).SpawnWith("team", map[string]any{"name": "a"}))
	b := queuedResp(t, f.AsAgent(requester).SpawnWith("team", map[string]any{"name": "b"}))

	list, _ := spawnQueueList(t, f, teammate)
	require.Len(t, list.Entries, 2, "a group mate sees entries targeting the shared group")
	list, _ = spawnQueueList(t, f, outsider)
	assert.Empty(t, list.Entries, "an agent outside the group sees nothing")
	assert.Equal(t, 2, list.Capacity.Queued, "the count is not an entry listing")

	assert.Equal(t, http.StatusNotFound, cancelQueued(t, f, a.QueueID, outsider))
	assert.Equal(t, http.StatusForbidden, cancelQueued(t, f, a.QueueID, teammate))
	assert.Equal(t, http.StatusOK, cancelQueued(t, f, a.QueueID, requester))
	assert.Equal(t, http.StatusConflict, cancelQueued(t, f, a.QueueID, ""), "already cancelled")

	_, err := db.RetireAgent(requester, "test", "retired while its spawn waited")
	require.NoError(t, err)
	f.MarkOffline(occupant.TmuxSession)
	agentd.RunSpawnQueueForTest()
	e, err := db.GetSpawnQueueEntry(b.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueFailed, e.State)
	assert.Contains(t, e.Reason, "retired")
}

// Scenario: a daemon crash leaves the head launching. Restart puts it back in
// its place when no launch record exists, and settles it as launched when one does.
func TestSpawnQueue_RestartReconcilesLaunchingEntries(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	id, _, err := db.InsertSpawnQueueEntry(&db.SpawnQueueEntry{GroupID: g.ID, Name: "lost", RequesterKind: db.SpawnRequesterHuman}, `{"name":"lost"}`, 0)
	require.NoError(t, err)
	ok, err := db.ClaimSpawnQueueEntry(id, db.NewAgentID())
	require.NoError(t, err)
	require.True(t, ok)
	id2, _, err := db.InsertSpawnQueueEntry(&db.SpawnQueueEntry{GroupID: g.ID, Name: "landed", RequesterKind: db.SpawnRequesterHuman}, `{"name":"landed"}`, 0)
	require.NoError(t, err)
	landed := db.NewAgentID()
	ok, err = db.ClaimSpawnQueueEntry(id2, landed)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{Label: "spwn-landed", AgentID: landed, GroupID: g.ID, Launching: true, CapacityReserved: true}))

	agentd.ReconcileSpawnQueueForTest()
	e, err := db.GetSpawnQueueEntry(id)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueQueued, e.State)
	assert.Equal(t, 1, e.Position)
	e2, err := db.GetSpawnQueueEntry(id2)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, e2.State)
}

// Scenario: a sandboxed agent proves its directory and asks for a trusted
// child there while the node is full. The queued launch keeps that proof: the
// child launches with --trust-dir instead of failing as an unproved request.
func TestSpawnQueue_QueuedAgentSpawnKeepsDirProof(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("squad")
	const parent = "parent-queue-proof-aaaa-1111111111"
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	haveAgentCallerInDir(t, f, "squad", parent, workspace)
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	occupant := f.AsHuman().Spawn("squad", "occupant")

	rec := agentReqProof(t, f, parent, http.MethodPost, "/v1/groups/squad/spawn", map[string]any{
		"name": "trusted", "cwd": workspace, "trust_dir": true,
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var q agent.SpawnResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &q))
	markers, _ := filepath.Glob(filepath.Join(workspace, ".tclaude-write-proof-*"))
	assert.Empty(t, markers, "the requester's proof markers go when the spawn queues")

	f.MarkOffline(occupant.TmuxSession)
	agentd.RunSpawnQueueForTest()
	e, err := db.GetSpawnQueueEntry(q.QueueID)
	require.NoError(t, err)
	require.Equal(t, db.SpawnQueueLaunched, e.State, e.Reason)
	a, err := db.GetAgent(e.ReservedAgent)
	require.NoError(t, err)
	require.NotNil(t, a)
	got, ok := f.World.SpawnTrustDir(a.CurrentConvID)
	require.True(t, ok)
	assert.True(t, got, "the queued launch still threads --trust-dir")
}

// Scenario: the queue's head check sees room, but capacity is gone by the
// time the spawner's own gate runs (node_busy). The entry keeps its place in
// the queue instead of failing, and launches once room is back.
func TestSpawnQueue_CapacityRaceAtTheGateRequeues(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	f.AsHuman().Spawn("team", "first")
	queued := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "second"}))

	// Room appears for the head check, and is taken back just before the gate.
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 2, WhenFull: config.CapacityWhenFullQueue})
	restore := agentd.SetBeforeExecuteSpawnForTest(func() {
		setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	})
	t.Cleanup(restore)
	agentd.RunSpawnQueueForTest()
	restore()

	entry, err := db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueQueued, entry.State, "a capacity race keeps the entry queued; reason=%q", entry.Reason)
	list, _ := spawnQueueList(t, f, "")
	require.Len(t, list.Entries, 1)
	assert.Equal(t, "second", list.Entries[0].Name)

	// Room is back for good: the requeued entry launches.
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 2, WhenFull: config.CapacityWhenFullQueue})
	agentd.RunSpawnQueueForTest()
	entry, err = db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, entry.State, "reason=%q", entry.Reason)
	launched, err := db.GetAgent(entry.ReservedAgent)
	require.NoError(t, err)
	require.NotNil(t, launched, "the second attempt used its own reserved agent id")
	f.AssertGroupMember("team", launched.CurrentConvID, "second", 5*time.Second)
}

// Scenario: the queued launch starts but the child is not seen to come up
// (spawn_unconfirmed). It may still be running, so the entry is recorded as
// launched and tracked, not failed as refused.
func TestSpawnQueue_UnconfirmedLaunchIsNotRecordedAsRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, WhenFull: config.CapacityWhenFullQueue})
	first := f.AsHuman().Spawn("team", "first")
	queued := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "second"}))

	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(50 * time.Millisecond))
	f.World.SkipSpawnRow = true
	f.MarkOffline(first.TmuxSession)
	agentd.RunSpawnQueueForTest()

	entry, err := db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, entry.State, "reason=%q", entry.Reason)
	assert.Contains(t, entry.Reason, "launch unconfirmed")
}

// Scenario: the request and the group leave cwd blank, so the spawner falls
// back to where the CLI ran (caller_cwd), and that disk is the full one. The
// queue's head check probes that same directory, so the entry waits in the
// queue without ever reaching the spawner's gate, and launches there once the
// disk frees up.
func TestSpawnQueue_HeadCheckProbesTheCallerCwdTheGateWill(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	full := testutil.CanonicalTempDir(t)
	freeMB := atomic.Int64{}
	freeMB.Store(10)
	t.Cleanup(agentd.SetCapacityDiskForTest(1<<20, func(dir string) int64 {
		if dir == full {
			return freeMB.Load()
		}
		return 1 << 20
	}))
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, MinFreeDiskMB: 100, WhenFull: config.CapacityWhenFullQueue})
	first := f.AsHuman().Spawn("team", "first")
	queued := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "second", "caller_cwd": full}))

	var gateRuns atomic.Int32
	t.Cleanup(agentd.SetBeforeExecuteSpawnForTest(func() { gateRuns.Add(1) }))
	f.MarkOffline(first.TmuxSession)
	agentd.RunSpawnQueueForTest()

	entry, err := db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueQueued, entry.State, "reason=%q", entry.Reason)
	assert.Zero(t, gateRuns.Load(), "the head check saw the full caller_cwd disk; the launch was never attempted")

	freeMB.Store(1 << 20)
	agentd.RunSpawnQueueForTest()
	entry, err = db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueLaunched, entry.State, "reason=%q", entry.Reason)
}

// Scenario: the spawner's gate refuses for capacity that the queue's own
// head check does not see. Requeueing would hold the head of the queue, and
// every entry behind it, without a word; the entry fails visibly with the
// gate's reason instead.
func TestSpawnQueue_GateRefusalTheHeadCheckCannotSeeFailsVisibly(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	var armed atomic.Bool
	t.Cleanup(agentd.SetCapacityDiskForTest(1<<20, func(string) int64 {
		if armed.CompareAndSwap(true, false) {
			return 10 // the gate's probe only
		}
		return 1 << 20
	}))
	setCapacity(t, config.CapacityConfig{MaxLiveAgents: 1, MinFreeDiskMB: 100, WhenFull: config.CapacityWhenFullQueue})
	first := f.AsHuman().Spawn("team", "first")
	queued := queuedResp(t, f.AsHuman().SpawnWith("team", map[string]any{"name": "second"}))

	t.Cleanup(agentd.SetBeforeExecuteSpawnForTest(func() { armed.Store(true) }))
	f.MarkOffline(first.TmuxSession)
	agentd.RunSpawnQueueForTest()

	entry, err := db.GetSpawnQueueEntry(queued.QueueID)
	require.NoError(t, err)
	assert.Equal(t, db.SpawnQueueFailed, entry.State, "reason=%q", entry.Reason)
	assert.Contains(t, entry.Reason, "capacity.min_free_disk_mb")
	list, _ := spawnQueueList(t, f, "")
	assert.Empty(t, list.Entries, "nothing is left holding the head of the queue")
}
