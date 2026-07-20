package agentd_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (JOH-205 inc2): a dashboard Codex spawn whose conv-id does not
// materialise within the inline grace must NOT hang or orphan. Instead the
// spawn returns a PENDING agent (200, empty conv_id) and records its full
// enrollment intent in pending_spawns; once the operator clears the startup
// gate and Codex takes its first turn — landing the conv-id on the session
// row — the sweeper back-fills the enrollment and drops the pending row.
//
// This pins the inc2 contract end to end:
//
//   - The spawn endpoint returns PENDING rather than blocking to a timeout
//     (the JOH-205 spawn-freeze): 200 with an empty conv_id but a real
//     label + tmux session.
//   - pending_spawns durably captures the intent (group, name) keyed by
//     label, so the back-fill survives a restart.
//   - SAFETY: nothing is injected into the Codex pane before its conv-id
//     exists — the no-send-keys-before-connection property JOH-205 requires.
//   - Once the conv-id materialises, one sweep enrolls the agent into the
//     group, clears the pending row, and the post-init welcome finally lands
//     on the (now un-gated) pane.
//
// The "gated Codex" is modelled by a spawner that builds the CodexSim but
// does NOT Start it: no rollout (so the spawn poll's conv-store discovery
// finds nothing) and no first turn (so no hook writes the conv-id) — exactly
// the unattended-pane-behind-a-modal condition. firstTurn() then Starts it
// and writes the conv-id, the way clearing the gate would.
//
// The briefing here is deliberately OVER the inline cap (set tiny), so the
// launch seed is only a stand-by welcome and the real inbox-pointer welcome is
// the post-connect injection the back-fill delivers — the send-keys surface
// this test asserts. (A short briefing rides inline in the seed instead, so it
// has no post-connect send-keys to observe; that path is covered by the
// non-gated Codex inline flow tests + the buildSpawnSeedPrompt unit table.)
func TestCodexAgent_PendingSpawnBackfillEnrollment(t *testing.T) {
	f := newFlow(t)

	// Force the over-cap (stand-by seed + post-connect pointer) path with a tiny
	// inline cap, so the back-fill still injects a welcome over tmux.
	tiny := 10
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnInlineMaxChars: &tiny},
	}))

	// Drive the pending path without a real multi-second wait: a genuinely
	// gated Codex blows the production grace; the test shrinks it.
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(50 * time.Millisecond))

	// Swap in the gated spawner. Non-codex spawns + all resumes delegate to
	// the default simulator-backed spawner; only the codex SpawnNew is gated.
	gated := &gatedCodexSpawner{
		t:     t,
		w:     f.World,
		inner: f.World.DefaultMocks(t).Spawner,
		sims:  map[string]*testharness.CodexSim{},
	}
	prevSpawn := agentd.Spawn
	agentd.Spawn = gated
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	g := f.HaveGroup("codex-crew")

	// Spawn a Codex agent. Its conv-id never materialises within the grace,
	// so the endpoint returns PENDING — 200 with an EMPTY conv_id — instead
	// of hanging until a timeout (the freeze) or erroring.
	resp, stdout := runSpawnCLI(t, f, &agent.SpawnParams{
		Group:          "codex-crew",
		Name:           "codex-worker",
		Harness:        "codex",
		InitialMessage: "Audit the auth module for timing-safe comparison bugs",
	})
	require.Empty(t, resp.ConvID, "pending spawn returns an empty conv_id")
	require.True(t, strings.HasPrefix(resp.AgentID, db.AgentIDPrefix), "pending spawn returns a stable agent_id")
	assert.Contains(t, stdout, "Spawned "+agent.ShortAgentID(resp.AgentID, "")+" in group \"codex-crew\"")
	assert.NotContains(t, stdout, "Spawned  in group", "operator output must never have a blank identity")
	require.NotEmpty(t, resp.Label, "pending spawn returns its label")
	require.NotEmpty(t, resp.TmuxSession, "pending spawn returns its tmux session")

	// The enrollment intent was persisted, keyed by label — restart-safe and
	// carrying what the sweeper needs to finish later.
	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err, "GetPendingSpawn")
	require.NotNil(t, ps, "spawn recorded a pending_spawns row")
	assert.Equal(t, resp.AgentID, ps.AgentID, "pending row persists the returned stable identity")
	assert.Equal(t, g.ID, ps.GroupID, "pending row carries the target group")
	assert.Equal(t, "codex-worker", ps.Name, "pending row carries the requested name")

	// Not enrolled yet — there is no conv-id to enroll.
	assert.Empty(t, f.ListGroupMembers("codex-crew"), "no member before the conv-id materialises")

	// SAFETY: nothing whatsoever has been injected into the pane — a Codex
	// behind a startup gate must receive NO send-keys until it is past
	// connection. Assert on the raw send-keys log (not just "contains the
	// name") so the property holds even if the welcome text changes.
	target := resp.TmuxSession + ":0.0"
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target {
			t.Fatalf("no send-keys must reach the pane before the conv-id materialises; got %q", sk.Text)
		}
	}

	// The operator clears the gate; Codex takes its first turn and its
	// conv-id finally lands on the session row (the first-turn hook's write).
	convID := gated.firstTurn(t, resp.Label)

	// One sweep back-fills the enrollment and clears the pending row.
	agentd.RunPendingSpawnSweepForTest()

	gone, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	assert.Nil(t, gone, "sweeper deletes the pending row after enrolling")

	m, err := db.FindMemberInGroup(g.ID, convID)
	require.NoError(t, err, "FindMemberInGroup")
	require.NotNil(t, m, "sweeper enrolled the conv into the group")
	boundAgentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, resp.AgentID, boundAgentID, "eventual enrollment binds the reserved stable identity")

	// The over-cap briefing landed in the inbox during back-fill.
	msg := soleInboxMessage(t, convID)

	// End-to-end: the post-init inbox-pointer welcome now lands on the pane —
	// only after the conv-id materialised, completing the back-fill. It points
	// the (now un-gated) agent at the briefing it can finally read.
	f.AssertSentContains(target, fmt.Sprintf("inbox read %d", msg.ID), 10*time.Second)
}

func TestCodexAgent_PendingResponseThenInlineBackgroundEnrollment(t *testing.T) {
	f := newFlow(t)

	t.Cleanup(agentd.SetCodexAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(700 * time.Millisecond))

	delayed := &delayedCodexSpawner{
		t:       t,
		w:       f.World,
		inner:   f.World.DefaultMocks(t).Spawner,
		release: make(chan struct{}),
		conv:    map[string]string{},
	}
	prevSpawn := agentd.Spawn
	agentd.Spawn = delayed
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	g := f.HaveGroup("codex-crew")
	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":            "codex-worker",
		"harness":         "codex",
		"initial_message": "Audit the auth module for timing-safe comparison bugs",
	})
	require.Equal(t, http.StatusOK, resp.Code, "pending spawn still returns 200 (raw=%s)", resp.Raw)
	require.Empty(t, resp.ConvID, "response should not wait for the delayed Codex conv-id")
	require.True(t, strings.HasPrefix(resp.AgentID, db.AgentIDPrefix), "fast response carries reserved stable identity")
	require.NotEmpty(t, resp.Label, "pending spawn returns its label")

	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, ps, "fast response records the pending row immediately")
	assert.Equal(t, resp.AgentID, ps.AgentID)
	close(delayed.release)

	var convID string
	require.Eventually(t, func() bool {
		convID = delayed.convID(resp.Label)
		if convID == "" {
			return false
		}
		m, err := db.FindMemberInGroup(g.ID, convID)
		return err == nil && m != nil
	}, 10*time.Second, 20*time.Millisecond, "background inline back-fill should enroll without a sweeper tick")

	require.Eventually(t, func() bool {
		gone, err := db.GetPendingSpawn(resp.Label)
		return err == nil && gone == nil
	}, 10*time.Second, 20*time.Millisecond, "background enrollment clears the pending row")
	f.AssertGroupMember("codex-crew", convID, "codex-worker", 10*time.Second)
	boundAgentID, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	assert.Equal(t, resp.AgentID, boundAgentID, "background enrollment keeps the response identity")
}

func TestCodexAgent_LaunchingReservationRejectsPendingDelete(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetCodexAsyncSpawnResponseGraceForTest(20 * time.Millisecond))

	started := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	blocked := &blockedLaunchCodexSpawner{
		inner:   f.World.DefaultMocks(t).Spawner,
		started: started,
		release: release,
	}
	prevSpawn := agentd.Spawn
	agentd.Spawn = blocked
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	f.HaveGroup("codex-crew")
	spawned := make(chan testharness.SpawnResp, 1)
	go func() {
		spawned <- f.AsHuman().SpawnWith("codex-crew", map[string]any{
			"name":    "codex-worker",
			"harness": "codex",
		})
	}()

	var label string
	select {
	case label = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not reach the blocked launch boundary")
	}
	ps, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	require.NotNil(t, ps, "stable identity reservation is visible before launch")
	require.True(t, ps.Launching)
	require.NotEmpty(t, ps.AgentID)
	_, err = db.LoadSession(label)
	require.ErrorIs(t, err, sql.ErrNoRows, "blocked launch has not created a session yet")

	mux := agentd.BuildDashboardHandlerForTest()
	deleted := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/pending/delete/"+label, nil))
	require.Equal(t, http.StatusConflict, deleted.Code, "launching delete body=%s", deleted.Body.String())

	stillReserved, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	require.NotNil(t, stillReserved, "rejected cancellation keeps the launch reservation")
	assert.Equal(t, ps.AgentID, stillReserved.AgentID)

	releaseOnce.Do(func() { close(release) })
	var resp testharness.SpawnResp
	select {
	case resp = <-spawned:
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not finish after the launch boundary was released")
	}
	require.Equal(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	assert.Equal(t, ps.AgentID, resp.AgentID, "rejected cancellation preserves the returned identity")
}

func TestCodexAgent_LateSessionClearsLaunchingReservation(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetCodexAsyncSpawnResponseGraceForTest(20 * time.Millisecond))
	t.Cleanup(agentd.SetAsyncSpawnInlineGraceForTest(700 * time.Millisecond))

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	created := make(chan error, 1)
	late := &lateSessionCodexSpawner{
		t:       t,
		w:       f.World,
		inner:   f.World.DefaultMocks(t).Spawner,
		release: release,
		created: created,
	}
	prevSpawn := agentd.Spawn
	agentd.Spawn = late
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	f.HaveGroup("codex-crew")
	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":    "codex-worker",
		"harness": "codex",
	})
	require.Equal(t, http.StatusOK, resp.Code, "pending spawn body=%s", resp.Raw)
	require.Empty(t, resp.ConvID)
	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err)
	require.NotNil(t, ps)
	require.True(t, ps.Launching, "the original response poll ended before any session existed")

	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-created)
	require.Eventually(t, func() bool {
		updated, err := db.GetPendingSpawn(resp.Label)
		return err == nil && updated != nil && !updated.Launching
	}, 10*time.Second, 20*time.Millisecond, "post-response session discovery clears the launch marker")

	mux := agentd.BuildDashboardHandlerForTest()
	deleted := testharness.Serve(mux,
		testharness.JSONRequest(t, http.MethodPost, "/api/pending/delete/"+resp.Label, nil))
	require.Equal(t, http.StatusOK, deleted.Code, "late-session pending spawn is cancellable; body=%s", deleted.Body.String())
}

func TestPendingSpawn_SweeperClearsLaunchingAfterSessionAppears(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("codex-crew")
	const label = "spwn-late-session"
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{
		Label:     label,
		AgentID:   db.NewAgentID(),
		Launching: true,
		GroupID:   g.ID,
		Name:      "codex-worker",
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		Status:      "running",
		Harness:     "codex",
	}))

	agentd.RunPendingSpawnSweepForTest()
	ps, err := db.GetPendingSpawn(label)
	require.NoError(t, err)
	require.NotNil(t, ps, "gated spawn remains pending without a conv-id")
	assert.False(t, ps.Launching, "restart-safe sweeper clears the marker once the session exists")
}

// gatedCodexSpawner is a SpawnerLike whose codex SpawnNew models a Codex
// blocked behind a startup gate: it builds the CodexSim but leaves it
// unstarted (no rollout, not alive) and writes a SessionRow with NO conv-id,
// so executeSpawn's conv-id poll exhausts and the spawn goes pending. Every
// other path delegates to a default simulator-backed spawner.
type gatedCodexSpawner struct {
	t     *testing.T
	w     *testharness.World
	inner testharness.SpawnerLike

	mu   sync.Mutex
	sims map[string]*testharness.CodexSim
}

func (s *gatedCodexSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if args.Harness != "codex" {
		return s.inner.SpawnNew(args)
	}
	label := args.Label
	// The stable actor reservation must be durable before the harness process
	// can emit a hook or be seen by the reaper. Force a sweep in this exact
	// pre-session window: the Launching marker must protect the reservation.
	agentd.RunPendingSpawnSweepForTest()
	ps, err := db.GetPendingSpawn(label)
	if err != nil {
		return fmt.Errorf("lookup pre-launch pending reservation: %w", err)
	}
	if ps == nil || ps.AgentID == "" || !ps.Launching {
		return fmt.Errorf("pending agent identity was not reserved before launch")
	}
	// Build the sim but do NOT Start it: no rollout (the spawn poll's
	// conv-store discovery finds nothing) and not alive yet.
	cx := testharness.NewCodexSim(s.t, s.w.HomeDir, args.Cwd)
	// The pane exists, but the SessionRow carries NO conv-id — the first-turn
	// hook has not fired, the JOH-205 freeze condition.
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		Cwd:         cx.Cwd,
		Status:      "running",
		Harness:     "codex",
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cx.Cwd, cx)
	s.mu.Lock()
	s.sims[label] = cx
	s.mu.Unlock()
	return nil
}

func (s *gatedCodexSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

type delayedCodexSpawner struct {
	t       *testing.T
	w       *testharness.World
	inner   testharness.SpawnerLike
	release chan struct{}

	mu   sync.Mutex
	conv map[string]string
}

type blockedLaunchCodexSpawner struct {
	inner   testharness.SpawnerLike
	started chan<- string
	release <-chan struct{}
}

type lateSessionCodexSpawner struct {
	t       *testing.T
	w       *testharness.World
	inner   testharness.SpawnerLike
	release <-chan struct{}
	created chan<- error
}

func (s *lateSessionCodexSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if args.Harness != "codex" {
		return s.inner.SpawnNew(args)
	}
	cx := testharness.NewCodexSim(s.t, s.w.HomeDir, args.Cwd)
	go func() {
		<-s.release
		if err := db.SaveSession(&db.SessionRow{
			ID:          args.Label,
			TmuxSession: args.Label,
			Cwd:         cx.Cwd,
			Status:      "running",
			Harness:     "codex",
		}); err != nil {
			s.created <- err
			return
		}
		s.w.Tmux.Register(args.Label, cx.Cwd, cx)
		s.created <- nil
	}()
	return nil
}

func (s *lateSessionCodexSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

func (s *blockedLaunchCodexSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if args.Harness != "codex" {
		return s.inner.SpawnNew(args)
	}
	s.started <- args.Label
	<-s.release
	return s.inner.SpawnNew(args)
}

func (s *blockedLaunchCodexSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

func (s *delayedCodexSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	if args.Harness != "codex" {
		return s.inner.SpawnNew(args)
	}
	label := args.Label
	cx := testharness.NewCodexSim(s.t, s.w.HomeDir, args.Cwd)
	if err := db.SaveSession(&db.SessionRow{
		ID:          label,
		TmuxSession: label,
		Cwd:         cx.Cwd,
		Status:      "running",
		Harness:     "codex",
	}); err != nil {
		return err
	}
	s.w.Tmux.Register(label, cx.Cwd, cx)
	go func() {
		<-s.release
		if err := cx.Start(); err != nil {
			s.t.Logf("delayed codex start failed: %v", err)
			return
		}
		if err := cx.WriteThreadRow(testharness.CodexThreadSeed{
			Cwd:       cx.Cwd,
			Model:     args.Model,
			CreatedAt: cx.CreatedUnix(),
			UpdatedAt: cx.CreatedUnix(),
		}); err != nil {
			s.t.Logf("delayed codex thread seed failed: %v", err)
			return
		}
		s.w.RecordSpawnInitialPrompt(cx.ConvID, args.InitialPrompt)
		if err := db.SaveSession(&db.SessionRow{
			ID:          label,
			TmuxSession: label,
			ConvID:      cx.ConvID,
			Cwd:         cx.Cwd,
			Status:      "running",
			Harness:     "codex",
		}); err != nil {
			s.t.Logf("delayed codex session update failed: %v", err)
			return
		}
		s.w.Codexes.Set(label, cx)
		s.mu.Lock()
		s.conv[label] = cx.ConvID
		s.mu.Unlock()
	}()
	return nil
}

func (s *delayedCodexSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

func (s *delayedCodexSpawner) convID(label string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conv[label]
}

// firstTurn models the spawned Codex finally taking its first turn once the
// operator clears the startup gate: the rollout materialises and the pane
// goes live (Start), Codex creates its threads row (so the post-init rename
// has a row to land on), and the first-turn hook writes the conv-id onto the
// session row keyed by label — the signal the sweeper waits for. Returns the
// now-known conv-id.
func (s *gatedCodexSpawner) firstTurn(t *testing.T, label string) string {
	t.Helper()
	s.mu.Lock()
	cx := s.sims[label]
	s.mu.Unlock()
	require.NotNil(t, cx, "no gated codex sim for label %q", label)

	require.NoError(t, cx.Start(), "codex first turn: Start (materialise rollout + go alive)")
	require.NoError(t, cx.WriteThreadRow(testharness.CodexThreadSeed{
		Cwd:       cx.Cwd,
		Model:     cx.Model,
		CreatedAt: cx.CreatedUnix(),
		UpdatedAt: cx.CreatedUnix(),
	}), "codex first turn: seed threads row")
	require.NoError(t, db.SetSessionConvID(label, cx.ConvID), "codex first turn: hook writes conv-id onto the session row")
	return cx.ConvID
}

// Scenario: a pending_spawns row whose session row NEVER existed — the forked
// `tclaude session new` wrapper died before SaveSessionState (observed live
// when a daemon with a deleted cwd launched panes that crashed at startup) —
// is a terminal orphan, not a transient error. LoadSession surfaces the
// missing row as sql.ErrNoRows; the sweeper must treat that like "session row
// gone" and drop the pending row, not warn-loop on it every tick forever.
func TestPendingSpawn_OrphanWithoutSessionRowIsDropped(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("orphan-crew")

	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{
		Label:   "spwn-orphan-test",
		GroupID: g.ID,
		Name:    "never-born",
	}), "seed an orphaned pending row (no matching session row)")

	agentd.RunPendingSpawnSweepForTest()

	gone, err := db.GetPendingSpawn("spwn-orphan-test")
	require.NoError(t, err, "GetPendingSpawn after sweep")
	assert.Nil(t, gone, "sweeper drops a pending row whose session row never existed")
}
