package agentd_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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
func TestCodexAgent_PendingSpawnBackfillEnrollment(t *testing.T) {
	f := newFlow(t)

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
	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":    "codex-worker",
		"harness": "codex",
	})
	require.Equal(t, http.StatusOK, resp.Code, "pending spawn still returns 200 (raw=%s)", resp.Raw)
	require.Empty(t, resp.ConvID, "pending spawn returns an empty conv_id")
	require.NotEmpty(t, resp.Label, "pending spawn returns its label")
	require.NotEmpty(t, resp.TmuxSession, "pending spawn returns its tmux session")

	// The enrollment intent was persisted, keyed by label — restart-safe and
	// carrying what the sweeper needs to finish later.
	ps, err := db.GetPendingSpawn(resp.Label)
	require.NoError(t, err, "GetPendingSpawn")
	require.NotNil(t, ps, "spawn recorded a pending_spawns row")
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

	// End-to-end: the post-init welcome injection now lands on the pane —
	// only after the conv-id materialised, completing the back-fill.
	f.AssertSentContains(target, "codex-worker", 2*time.Second)
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

func (s *gatedCodexSpawner) SpawnNew(label, cwd, effort, model, harnessName, sandbox, approval string, autoReview, trustDir bool) error {
	if harnessName != "codex" {
		return s.inner.SpawnNew(label, cwd, effort, model, harnessName, sandbox, approval, autoReview, trustDir)
	}
	// Build the sim but do NOT Start it: no rollout (the spawn poll's
	// conv-store discovery finds nothing) and not alive yet.
	cx := testharness.NewCodexSim(s.t, s.w.HomeDir, cwd)
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

func (s *gatedCodexSpawner) SpawnResume(convID, cwd, effort, model, harnessName, sandbox, approval string, autoReview bool) error {
	return s.inner.SpawnResume(convID, cwd, effort, model, harnessName, sandbox, approval, autoReview)
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
