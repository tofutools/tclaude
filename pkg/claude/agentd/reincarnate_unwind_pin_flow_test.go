package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These pin what reincarnation leaves behind, at each point it can fail and
// when it succeeds, as read from the database, the tmux sim and the response.
// A failure before the identity rotation leaves no actor or handoff message for
// the successor, and no launch claim. (The failing launches here never create
// a session row, so the unchanged session count only pins that the unwind adds
// none of its own.) A refused rotation leaves the successor running as an
// orphan, by design. Either way the source stays the agent's live, unarchived
// head: its pane is alive and was neither archive-renamed nor told to exit, and
// its sandbox snapshot is restored.

// reincarnateSpawner records each fresh launch and lets the scenario decide
// what it does.
type reincarnateSpawner struct {
	inner agentd.Spawner
	newFn func(clcommon.SpawnArgs) error
	news  []clcommon.SpawnArgs
}

func (s *reincarnateSpawner) SpawnNew(a clcommon.SpawnArgs) error {
	s.news = append(s.news, a)
	return s.newFn(a)
}

func (s *reincarnateSpawner) SpawnResume(a clcommon.SpawnArgs) error { return s.inner.SpawnResume(a) }

// spawnSourceThenEditItsSandbox spawns the agent to reincarnate under a
// sandbox profile, then edits that profile. The reincarnation's refreshed
// sandbox snapshot then differs from the one stored at the source's launch,
// so a failure that does not restore the stored snapshot shows.
func spawnSourceThenEditItsSandbox(t *testing.T, f *testharness.Flow) testharness.SpawnResp {
	t.Helper()
	f.HaveGroup("crew")
	id, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "worker-policy",
		Environment: []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"}}})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "worker-policy"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	profile, err := db.GetSandboxProfileByID(id)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"}, {Name: "ADDED", Value: "x"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	require.NotEmpty(t, spawn.TmuxSession)
	return spawn
}

// reincarnateBefore is what the failure pins compare against.
type reincarnateBefore struct {
	agents, sessions int
	sandbox          string
}

func snapshotBeforeReincarnate(t *testing.T, src string) reincarnateBefore {
	t.Helper()
	agents, err := db.ListActiveAgents()
	require.NoError(t, err)
	sessions, err := db.ListSessions()
	require.NoError(t, err)
	return reincarnateBefore{agents: len(agents), sessions: len(sessions), sandbox: sandboxSnapshotOf(t, src)}
}

func sandboxSnapshotOf(t *testing.T, conv string) string {
	t.Helper()
	sb, err := db.AgentEffectiveSandboxConfigForConv(conv)
	require.NoError(t, err)
	raw, err := json.Marshal(sb)
	require.NoError(t, err)
	return string(raw)
}

// assertSourceStillTheHead checks the source after a failed reincarnation: the
// agent's live conversation is still the source, which is not archived, and
// its sandbox snapshot is what it was, and its pane is alive and was sent no
// archive rename or soft exit.
func assertSourceStillTheHead(t *testing.T, f *testharness.Flow, source testharness.SpawnResp, before reincarnateBefore) {
	t.Helper()
	src := source.ConvID
	assert.True(t, f.World.Tmux.IsAlive(source.TmuxSession), "the source's pane is still alive")
	for _, k := range f.World.Tmux.Sent() {
		if strings.HasPrefix(k.Target, source.TmuxSession) {
			assert.NotContains(t, k.Text, "/exit", "the source is not soft-stopped")
			assert.NotContains(t, k.Text, "/rename", "the source is not archive-renamed")
		}
	}
	actor, err := db.GetAgentByConv(src)
	require.NoError(t, err)
	require.NotNil(t, actor, "the source keeps its actor")
	assert.Equal(t, src, actor.CurrentConvID, "the source is still the agent's live conversation")
	idx, err := db.GetConvIndex(src)
	require.NoError(t, err)
	assert.False(t, idx != nil && idx.IsArchived(), "the source is not archived")
	assert.Equal(t, before.sandbox, sandboxSnapshotOf(t, src), "the source's sandbox snapshot is restored")
}

func errorBody(t *testing.T, raw []byte) (code, msg string) {
	t.Helper()
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &body), "body=%s", raw)
	return body.Code, body.Error
}

// Before the rotation commits, a failed reincarnation leaves no trace of the
// successor and the source untouched.
func TestReincarnateUnwind_AFailedLaunchLeavesNoSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newFn    func(clcommon.SpawnArgs) error
		status   int
		code     string
		errorHas string
	}{
		{"launch refused",
			func(clcommon.SpawnArgs) error { return errors.New("pinned launch refusal") },
			http.StatusInternalServerError, "spawn", "pinned launch refusal"},
		{"wrapper dies after the launch",
			func(a clcommon.SpawnArgs) error {
				agentd.SignalSpawnWrapperFailureForTest(a.Label, errors.New("pinned wrapper death"))
				return nil
			},
			http.StatusInternalServerError, "spawn", "pinned wrapper death"},
		{"successor never comes online",
			func(clcommon.SpawnArgs) error { return nil },
			http.StatusGatewayTimeout, "timeout", "the timed-out successor was stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The successor's online wait is a deadline poll; keep it short.
			t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(500 * time.Millisecond))
			f := newFlow(t)
			source := spawnSourceThenEditItsSandbox(t, f)
			src := source.ConvID
			before := snapshotBeforeReincarnate(t, src)
			sp := &reincarnateSpawner{inner: agentd.Spawn, newFn: tc.newFn}
			prev := agentd.Spawn
			agentd.Spawn = sp
			t.Cleanup(func() { agentd.Spawn = prev })

			resp := f.AsHuman().ReincarnateWith(src, map[string]any{"follow_up": "carry on"})

			assert.Equal(t, tc.status, resp.Code, "body=%s", resp.Raw)
			code, msg := errorBody(t, resp.Raw)
			assert.Equal(t, tc.code, code)
			assert.Contains(t, msg, tc.errorHas)
			require.Len(t, sp.news, 1, "exactly one successor launch was attempted")
			succ := sp.news[0].SessionID
			require.NotEmpty(t, succ, "the successor launch preset its conv")
			assert.Contains(t, sp.news[0].InitialPrompt, "message #",
				"the handoff was written before the launch, so its rollback is exercised")

			agentID, err := db.AgentIDForConv(succ)
			require.NoError(t, err)
			assert.Empty(t, agentID, "no actor is left for the successor")
			msgs, err := db.ListAgentMessagesForConv(succ, 50)
			require.NoError(t, err)
			assert.Empty(t, msgs, "the pre-launch handoff message is rolled back")
			agents, err := db.ListActiveAgents()
			require.NoError(t, err)
			assert.Len(t, agents, before.agents, "no active actor is added")
			sessions, err := db.ListSessions()
			require.NoError(t, err)
			assert.Len(t, sessions, before.sessions, "the unwind adds no session row")
			assert.Zero(t, agentd.AgentLaunchClaimsForTest(), "the launch claim is released")
			assertSourceStillTheHead(t, f, source, before)
		})
	}
}

// A refused identity rotation aborts after the successor came up. The
// successor is left running as an orphan (reachable by attach_cmd, by
// design), and the source stays the agent's head, alive and unarchived.
// The refusal is real: the successor's conv is already owned by another
// actor that holds a grant, so the rotation cannot absorb it.
func TestReincarnateUnwind_ARefusedRotationKeepsTheSourceAsHead(t *testing.T) {
	t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(500 * time.Millisecond))
	f := newFlow(t)
	source := spawnSourceThenEditItsSandbox(t, f)
	src := source.ConvID
	before := snapshotBeforeReincarnate(t, src)
	var rival string
	sp := &reincarnateSpawner{inner: agentd.Spawn}
	sp.newFn = func(a clcommon.SpawnArgs) error {
		id, _, err := db.EnsureAgentForConv(a.SessionID, "test")
		if err != nil {
			return err
		}
		rival = id
		if err := db.GrantAgentPermission(a.SessionID, agentd.PermHumanClipboard, "test"); err != nil {
			return err
		}
		return sp.inner.SpawnNew(a)
	}
	prev := agentd.Spawn
	agentd.Spawn = sp
	t.Cleanup(func() { agentd.Spawn = prev })

	resp := f.AsHuman().ReincarnateWith(src, map[string]any{"follow_up": "carry on"})

	assert.Equal(t, http.StatusInternalServerError, resp.Code, "body=%s", resp.Raw)
	code, msg := errorBody(t, resp.Raw)
	assert.Equal(t, "identity_migration", code)
	assert.Contains(t, msg, "successor owned elsewhere")
	require.Len(t, sp.news, 1)
	succ := sp.news[0].SessionID
	owner, err := db.AgentIDForConv(succ)
	require.NoError(t, err)
	assert.Equal(t, rival, owner, "the successor's conv keeps the owner that refused the rotation")
	sessions, err := db.ListSessions()
	require.NoError(t, err)
	assert.Len(t, sessions, before.sessions+1, "the orphan successor's session is left for manual cleanup")
	assert.True(t, f.World.Tmux.IsAlive(sp.news[0].Label), "the orphan successor's pane is left running")
	assert.Zero(t, agentd.AgentLaunchClaimsForTest(), "the launch claim is released")
	assertSourceStillTheHead(t, f, source, before)
}

// A successful reincarnation rotates the agent onto the successor, archives
// the source, keeps the base name on the successor, and reports it all.
func TestReincarnateUnwind_ASuccessRotatesAndArchives(t *testing.T) {
	f := newFlow(t)
	source := spawnSourceThenEditItsSandbox(t, f)
	src := source.ConvID
	sandboxBefore := sandboxSnapshotOf(t, src)
	agentBefore, err := db.AgentIDForConv(src)
	require.NoError(t, err)

	resp := f.AsHuman().ReincarnateWith(src, map[string]any{"follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", resp.Raw)

	var body map[string]any
	require.NoError(t, json.Unmarshal(resp.Raw, &body))
	for _, key := range []string{"attach_cmd", "caller_conv", "follow_up", "label", "message_id", "migrated",
		"new_conv", "new_title", "note", "old_conv", "retired_title", "switched_clients", "tmux_session"} {
		assert.Contains(t, body, key, "the response reports %s", key)
	}
	assert.Equal(t, src, body["old_conv"])
	assert.Equal(t, "carry on", body["follow_up"])
	assert.Equal(t, "worker", body["new_title"], "the successor keeps the base name")
	assert.Equal(t, "worker-x", body["retired_title"], "the source is kept under a retired name")
	succ, _ := body["new_conv"].(string)
	require.NotEmpty(t, succ)

	actor, err := db.GetAgentByConv(succ)
	require.NoError(t, err)
	require.NotNil(t, actor)
	assert.Equal(t, agentBefore, actor.AgentID, "the agent keeps its identity across the rotation")
	assert.Equal(t, succ, actor.CurrentConvID, "the successor is the agent's live conversation")
	idx, err := db.GetConvIndex(src)
	require.NoError(t, err)
	require.NotNil(t, idx)
	assert.True(t, idx.IsArchived(), "the source is archived")
	// The failure pins' "no /exit, no /rename" checks match on this target.
	assert.True(t, f.World.Tmux.WaitForSendKeys(source.TmuxSession+":0.0", "/rename worker-x", 10*time.Second),
		"the source is archive-renamed; sent=%+v", f.World.Tmux.Sent())
	assert.True(t, f.World.Tmux.WaitForSendKeys(source.TmuxSession+":0.0", "/exit", 10*time.Second),
		"the source is soft-stopped; sent=%+v", f.World.Tmux.Sent())
	msgs, err := db.ListAgentMessagesForConv(succ, 50)
	require.NoError(t, err)
	assert.NotEmpty(t, msgs, "the follow-up is delivered to the successor")
	assert.Zero(t, agentd.AgentLaunchClaimsForTest(), "the launch claim is released")
	assert.NotEqual(t, sandboxBefore, sandboxSnapshotOf(t, succ),
		"the agent's sandbox snapshot is the refreshed one, so the failure pins' restore check is not vacuous")
}
