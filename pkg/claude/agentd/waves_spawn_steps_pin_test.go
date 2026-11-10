package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin spawnWaveAgents' per-member exits that the deploy flow tests do
// not reach: the restart-reuse path, and the refusals that happen before
// executeSpawn (per-agent worktree, role, saved prompts, cwd write-proof).
// Each refusal must be counted as Failed, keep its typed kind and message, and
// carry no worktree fields; the roster's results stay in template order.

// noSpawnExpected fails the test if a refusal pin reaches executeSpawn: every
// member in these tests must be decided before a session is started.
type noSpawnExpected struct{ t *testing.T }

func (s noSpawnExpected) SpawnNew(args clcommon.SpawnArgs) error {
	s.t.Errorf("unexpected spawn of %q", args.Label)
	return assert.AnError
}

func (s noSpawnExpected) SpawnResume(args clcommon.SpawnArgs) error {
	s.t.Errorf("unexpected resume of %q", args.ConvID)
	return assert.AnError
}

func waveStepsPinGroup(t *testing.T) *db.AgentGroup {
	t.Helper()
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	previous := Spawn
	Spawn = noSpawnExpected{t}
	t.Cleanup(func() { Spawn = previous })
	groupID, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	g, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	return g
}

// waveStepsPinRepo makes a one-commit git repo one level under a temp dir, so
// the sibling worktrees `git worktree add` cuts stay inside it.
func waveStepsPinRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(testutil.CanonicalTempDir(t), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "tclaude tests"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
		{"branch", "-M", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return repo
}

func TestSpawnWaveAgents_ReusesExistingConvAndRestampsTag(t *testing.T) {
	g := waveStepsPinGroup(t)
	agentID, _, err := db.EnsureAgentForConv("conv-prior", "test")
	require.NoError(t, err)

	wr := spawnWaveAgents(g, []db.GroupTemplateAgent{{Name: "worker"}},
		map[string]string{"crew-worker": "conv-prior"},
		waveDeploy{cwd: t.TempDir(), templateName: "squad"})

	assert.Equal(t, 1, wr.Spawned)
	assert.Zero(t, wr.Failed)
	assert.Equal(t, []instantiateAgentResult{{Name: "worker", FinalName: "crew-worker", ConvID: "conv-prior"}}, wr.Results)
	assert.Equal(t, map[string]string{"worker": "conv-prior"}, wr.SpawnedConvs)
	assert.Equal(t, []string{"conv-prior"}, wr.SpawnedOrder)
	tags, err := db.ListAgentTags(agentID)
	require.NoError(t, err)
	assert.Contains(t, tags, "tf:squad", "the reuse path re-stamps the task-force tag")
}

func TestSpawnWaveAgents_WorktreeFailureIsCountedWithoutPaths(t *testing.T) {
	g := waveStepsPinGroup(t)
	notARepo := t.TempDir()

	wr := spawnWaveAgents(g, []db.GroupTemplateAgent{{Name: "worker"}}, nil, waveDeploy{
		cwd:               notARepo,
		perAgentWorktrees: &db.WavePerAgentWorktrees{Repo: notARepo, WorktreeAsCwd: true},
	})

	require.Len(t, wr.Results, 1)
	res := wr.Results[0]
	assert.Contains(t, res.Error, "create worktree: ")
	assert.Empty(t, res.ErrorKind)
	assert.Empty(t, res.WorktreePath)
	assert.Empty(t, res.WorktreeBranch)
	assert.Equal(t, 1, wr.Failed)
	assert.Zero(t, wr.Spawned)
	assert.Empty(t, wr.SpawnedOrder)
}

func TestSpawnWaveAgents_RoleAndPromptRefusalsKeepTheirKind(t *testing.T) {
	g := waveStepsPinGroup(t)
	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prompted", PromptRefs: []string{"no-such-prompt"},
	})
	require.NoError(t, err)

	wr := spawnWaveAgents(g, []db.GroupTemplateAgent{
		{Name: "reused"},
		{Name: "unroled", SpawnProfile: "vanished"},
		{Name: "prompted", SpawnProfile: "prompted", SpawnProfileID: profileID},
	}, map[string]string{"crew-reused": "conv-reused"}, waveDeploy{
		cwd:                t.TempDir(),
		sharedWorktreePath: "/shared/wt",
	})

	require.Len(t, wr.Results, 3)
	names := []string{wr.Results[0].Name, wr.Results[1].Name, wr.Results[2].Name}
	assert.Equal(t, []string{"reused", "unroled", "prompted"}, names, "results keep template order")
	assert.Equal(t, 1, wr.Spawned)
	assert.Equal(t, 2, wr.Failed)
	assert.Equal(t, []string{"conv-reused"}, wr.SpawnedOrder)

	unroled := wr.Results[1]
	assert.Equal(t, "invalid_profile", unroled.ErrorKind, unroled.Error)
	assert.Contains(t, unroled.Error, `"vanished"`)
	assert.Empty(t, unroled.WorktreePath, "a refused member names no worktree")
	assert.Nil(t, unroled.Resolved, "a roles refusal carries no launch echo")

	prompted := wr.Results[2]
	assert.Equal(t, "invalid_prompt", prompted.ErrorKind, prompted.Error)
	assert.Contains(t, prompted.Error, `"no-such-prompt"`)
	assert.Empty(t, prompted.WorktreePath)
}

func TestSpawnWaveAgents_CwdWriteProofFailureIsCountedWithoutPaths(t *testing.T) {
	g := waveStepsPinGroup(t)
	repo := waveStepsPinRepo(t)

	wr := spawnWaveAgents(g, []db.GroupTemplateAgent{{Name: "worker"}}, nil, waveDeploy{
		cwd:               repo,
		perAgentWorktrees: &db.WavePerAgentWorktrees{Repo: repo, BranchPrefix: "pin/", WorktreeAsCwd: true},
		proofToken:        "tok",
		proofDirs:         []string{repo}, // the worktree's parent is not write-proofed
	})

	require.Len(t, wr.Results, 1)
	res := wr.Results[0]
	assert.Contains(t, res.Error, "prepare cwd write-proof: worktree parent ")
	assert.Empty(t, res.ErrorKind)
	assert.Empty(t, res.WorktreePath)
	assert.Empty(t, res.WorktreeBranch)
	assert.Equal(t, 1, wr.Failed)
	assert.Zero(t, wr.Spawned)

}

// A member whose own worktree is its cwd gets a write-proof marker planted
// there before its saved prompts are composed. When composing them fails, the
// member is refused and the marker must not be left behind.
func TestSpawnWaveAgents_PromptRefusalRemovesThePlantedCwdMarker(t *testing.T) {
	g := waveStepsPinGroup(t)
	repo := waveStepsPinRepo(t)
	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prompted", PromptRefs: []string{"no-such-prompt"},
	})
	require.NoError(t, err)

	wr := spawnWaveAgents(g, []db.GroupTemplateAgent{
		{Name: "worker", SpawnProfile: "prompted", SpawnProfileID: profileID},
	}, nil, waveDeploy{
		cwd:               repo,
		perAgentWorktrees: &db.WavePerAgentWorktrees{Repo: repo, BranchPrefix: "pin/", WorktreeAsCwd: true},
		proofToken:        "tok",
		proofDirs:         []string{filepath.Dir(repo)}, // the worktrees' parent is proofed
	})

	require.Len(t, wr.Results, 1)
	res := wr.Results[0]
	require.Equal(t, "invalid_prompt", res.ErrorKind, res.Error)
	assert.Equal(t, 1, wr.Failed)
	worktrees, err := filepath.Glob(filepath.Join(filepath.Dir(repo), "*"))
	require.NoError(t, err)
	require.Greater(t, len(worktrees), 1, "the member's worktree was cut beside the repo: %v", worktrees)
	markers, err := filepath.Glob(filepath.Join(filepath.Dir(repo), "*", dirWriteProofFilePrefix+"tok"))
	require.NoError(t, err)
	assert.Empty(t, markers, "the refused member's cwd marker is removed")
}
