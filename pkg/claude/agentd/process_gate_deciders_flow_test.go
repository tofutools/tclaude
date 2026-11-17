package agentd_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// tk_44d02f32: the process editor sets each decider of a gate on its own row:
// its own ask, "If nobody" (spawn one with a profile) and "Not <step>"
// (differentFrom). The run does what those rows say.
//
// Scenario: one developer and one reviewer. Decider 1 is anyone in the group
// but not who built it, asked for a code review; decider 2 is a reviewer,
// asked for a security review, spawning one with sonnet-dev if nobody can.
// The developer, idle, is not given vote 1; the reviewer is. Vote 2 cannot go
// to the reviewer again (no agent votes twice at one gate), so it spawns,
// with its own ask. Both approve and the run completes.
// promptRecordingSpawner records each fresh spawn's first prompt.
type promptRecordingSpawner struct {
	inner   agentd.Spawner
	mu      sync.Mutex
	prompts []string
}

func (s *promptRecordingSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	s.mu.Lock()
	s.prompts = append(s.prompts, args.InitialPrompt)
	s.mu.Unlock()
	return s.inner.SpawnNew(args)
}

func (s *promptRecordingSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	return s.inner.SpawnResume(args)
}

func TestProcessGateDeciders_OwnAskIfNobodyAndNotTheImplementer(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	spawner := &promptRecordingSpawner{inner: agentd.Spawn}
	agentd.Spawn = spawner
	t.Cleanup(func() { agentd.Spawn = spawner.inner })
	const devConv, revConv = "gddv-1111-2222-3333-4444", "gdrv-1111-2222-3333-4444"
	dev := havePerformerAgent(t, f, "crew", devConv, "dana", "dev", "idle")
	rev := havePerformerAgent(t, f, "crew", revConv, "rita", "reviewer", "working")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "sonnet-dev", Harness: "claude"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("crew", testutil.CanonicalTempDir(t))
	require.NoError(t, err)

	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "gate-deciders", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "implement"}},
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"next": "review"},
				Performer: agentStep("Build it.", func(p *model.Performer) { p.Role = "dev" })},
			"review": {Type: model.NodeTypeDecision, Agree: "approved",
				Deciders: []model.Performer{
					{Kind: model.PerformerAgent, Anyone: true, Prompt: "Code review: approve or send back.",
						DifferentFrom: []string{"implement"}},
					{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Security review: secrets, auth.",
						Fallback: &model.PerformerFallback{Action: model.FallbackSpawn, Profile: "sonnet-dev"}},
				},
				Next:  model.Next{"approved": "done", "changes": "implement", "stop": "stopped"},
				Loops: map[string]model.LoopBound{"changes": {MaxIterations: 2, OnExhausted: "stop"}}},
			"done":    {Type: model.NodeTypeEnd, Result: "success"},
			"stopped": {Type: model.NodeTypeEnd, Result: "canceled"},
		}}

	const runID = "run_gate_deciders"
	startLoopRun(t, f, root, runID, tmpl)
	view := showPerformerRun(t, f, runID)
	agentID, attempt, _ := commandFor(t, view, "implement")
	require.Equal(t, dev, agentID)
	reportStep(t, f, devConv, runID, "implement", attempt, nil)

	view = showPerformerRun(t, f, runID)
	voter1, attempt1, _ := commandFor(t, view, "review.vote.1")
	assert.Equal(t, rev, voter1, "not the agent who did implement, though it is idle and the reviewer is busy")
	revInbox := processInbox(t, revConv)
	require.Len(t, revInbox, 1)
	assert.Contains(t, revInbox[0].Body, "Code review: approve or send back.", "vote 1 is asked its own ask")
	assert.NotContains(t, revInbox[0].Body, "Security review")
	assert.Len(t, processInbox(t, devConv), 1, "the developer is not asked to vote")

	voter2, attempt2, _ := commandFor(t, view, "review.vote.2")
	require.NotEmpty(t, voter2)
	assert.NotContains(t, []string{dev, rev}, voter2, "vote 2 is a spawned reviewer")
	assert.Equal(t, "spawned", resolutionOf(t, view, "review.vote.2"))
	spawnedConv, err := db.CurrentConvForAgent(voter2)
	require.NoError(t, err)
	member, err := db.FindMemberInGroup(g.ID, spawnedConv)
	require.NoError(t, err)
	require.NotNil(t, member, "the spawned decider joins the run's group")
	spawner.mu.Lock()
	prompts := spawner.prompts
	spawner.mu.Unlock()
	require.Len(t, prompts, 1, "one decider spawned")
	assert.Contains(t, prompts[0], "Security review: secrets, auth.", "vote 2 is briefed with its own ask")
	assert.NotContains(t, prompts[0], "Code review")

	voteAs(t, f, revConv, runID, "review.vote.1", attempt1, "approved", "looks fine")
	voteAs(t, f, spawnedConv, runID, "review.vote.2", attempt2, "approved", "no secrets")
	assert.Equal(t, engine.RunCompleted, showGateRun(t, f, runID).Status)
}
