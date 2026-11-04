package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A spawn copy re-applied to a stopped agent is what it relaunches with
// (tcl-a2e791 slice 6e): applySpawnCopy resolves the copy through the
// first-spawn resolver and writes the fields it owns into the agent's
// relaunch profile, which every relaunch path reads.

func relaunchProfileJSON(t *testing.T, convID string) string {
	t.Helper()
	p, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, p)
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return string(raw)
}

func storedRelaunch(t *testing.T, convID string) *db.AgentRelaunchProfile {
	t.Helper()
	p, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, p)
	return p
}

func agentIDFor(t *testing.T, convID string) string {
	t.Helper()
	id, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	return id
}

func editedCopy(t *testing.T, name string, edit func(*db.SpawnProfile)) db.AgentSpawnCopy {
	t.Helper()
	prof, err := db.GetSpawnProfile(name)
	require.NoError(t, err)
	edit(prof)
	return db.AgentSpawnCopy{Tiers: []db.SpawnCopyTier{agentd.SpawnCopyTierForTest(db.SpawnCopyTierNamed, prof)}}
}

// spawnStopped spawns an agent and stops it, as applySpawnCopy requires.
func spawnStopped(t *testing.T, f *testharness.Flow, group string, body map[string]any) testharness.SpawnResp {
	t.Helper()
	spawn := f.AsHuman().SpawnWith(group, body)
	require.Equalf(t, http.StatusOK, spawn.Code, "%v: spawn body=%s", body, spawn.Raw)
	f.MarkOffline(spawn.TmuxSession)
	return spawn
}

// resumeAgain resumes an agent that may have been resumed before, stopping
// its current session first.
func resumeAgain(t *testing.T, f *testharness.Flow, convID string) {
	t.Helper()
	if sess, err := db.FindSessionByConvID(convID); err == nil && sess != nil {
		f.MarkOffline(sess.TmuxSession)
	}
	f.AssertResumeSpawned(f.AsHuman().Resume(convID))
}

// Re-applying the very copy an agent was spawned with changes nothing: the
// relaunch resolver gives the values the first spawn gave, for a named
// profile, a group default profile, both, a Codex profile, no profile and an
// explicit value over a profile.
func TestSpawnCopyRelaunch_ReapplyingTheSameCopyChangesNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	for _, body := range []map[string]any{
		{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto", "sandbox": "on"},
		{"name": "grouped", "ask_user_question_timeout": "10m", "remote_control": true},
		{"name": "codexy", "harness": "codex", "model": "gpt-5.5", "sandbox": "tclaude-agent"},
	} {
		rec := createProfile(t, f, body)
		require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	}
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "beta", "grouped").Code)

	for _, tc := range []struct {
		group string
		body  map[string]any
		// attributed is the one known difference: a direct Codex spawn does
		// not record which profile chose its sandbox mode, and the resolver
		// names it.
		attributed string
	}{
		{"alpha", map[string]any{"name": "plain"}, ""},
		{"alpha", map[string]any{"name": "named", "profile": "agentic"}, ""},
		{"beta", map[string]any{"name": "defaulted"}, ""},
		{"beta", map[string]any{"name": "both", "profile": "agentic"}, ""},
		{"alpha", map[string]any{"name": "codex", "profile": "codexy"}, `profile "codexy"`},
		{"alpha", map[string]any{"name": "explicit", "profile": "agentic", "ask_user_question_timeout": "60s"}, ""},
	} {
		spawn := spawnStopped(t, f, tc.group, tc.body)
		before := relaunchProfileJSON(t, spawn.ConvID)
		if tc.attributed != "" {
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(before), &fields))
			require.Equal(t, "", fields["sandbox_mode_source"], "the direct spawn's attribution changed; drop this case's exception")
			fields["sandbox_mode_source"] = tc.attributed
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			before = string(raw)
		}
		layers, err := db.AgentLaunchLayersForConv(spawn.ConvID)
		require.NoError(t, err)
		require.NotNil(t, layers)
		require.NotNil(t, layers.SpawnCopy)

		kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID), *layers.SpawnCopy)
		require.Emptyf(t, kind, "%v: %s", tc.body, msg)
		assert.JSONEqf(t, before, relaunchProfileJSON(t, spawn.ConvID), "%v", tc.body)
	}
}

// An edited profile re-applied to a stopped agent is what its next resume
// and reincarnation launch with; a value the spawn gave explicitly stays.
func TestSpawnCopyRelaunch_RelaunchUsesTheReappliedCopy(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	copied := spawnStopped(t, f, "alpha", map[string]any{"name": "copied", "profile": "agentic"})
	explicit := spawnStopped(t, f, "alpha", map[string]any{"name": "explicit", "profile": "agentic",
		"ask_user_question_timeout": "60s"})
	newCopy := editedCopy(t, "agentic", func(p *db.SpawnProfile) { p.AskUserQuestionTimeout, p.Approval = "10m", "plan" })

	for _, tc := range []struct {
		spawn                testharness.SpawnResp
		askTimeout, approval string
	}{
		{copied, "10m", "plan"},
		{explicit, "60s", "plan"},
	} {
		kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, tc.spawn.ConvID), newCopy)
		require.Empty(t, kind, msg)
		f.AssertResumeSpawned(f.AsHuman().Resume(tc.spawn.ConvID))
		got, ok := f.World.SpawnAskTimeout(tc.spawn.ConvID)
		require.True(t, ok)
		assert.Equal(t, tc.askTimeout, got, "ask timeout after the re-applied copy")
		approval, ok := f.World.SpawnApproval(tc.spawn.ConvID)
		require.True(t, ok)
		assert.Equal(t, tc.approval, approval, "approval after the re-applied copy")
	}

	// The running session's own saves keep what it launched with, and a
	// reincarnation launches the successor with it too.
	rec = agentReq(t, f, copied.ConvID, http.MethodPost, "/v1/whoami/reincarnate", map[string]any{"follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, rec.Code, "reincarnate body=%s", rec.Body.String())
	var response testharness.ReincarnateResp
	testharness.DecodeJSON(t, rec, &response)
	got, ok := f.World.SpawnAskTimeout(response.NewConv)
	require.True(t, ok)
	assert.Equal(t, "10m", got)
}

// A running agent is refused with a code a caller can sequence on (stop,
// apply, resume); its launch settings stay as they were.
func TestSpawnCopyRelaunch_RefusesARunningAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "profile": "agentic"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	before := relaunchProfileJSON(t, spawn.ConvID)

	kind, _ := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID),
		editedCopy(t, "agentic", func(p *db.SpawnProfile) { p.AskUserQuestionTimeout = "10m" }))
	assert.Equal(t, "agent_live", kind)
	assert.JSONEq(t, before, relaunchProfileJSON(t, spawn.ConvID))
}

// A morph after a re-apply wins on the next launch: the copy was written
// once, not laid over later writes.
func TestSpawnCopyRelaunch_ALaterMorphWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m", "approval": "auto"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "worker", "profile": "agentic"})

	kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID), editedCopy(t, "agentic", func(p *db.SpawnProfile) {
		p.AskUserQuestionTimeout, p.Approval = "10m", "plan"
	}))
	require.Empty(t, kind, msg)
	morphed := "60s"
	previous, err := db.ApplyAgentMorphForConv(spawn.ConvID, db.AgentMorphForm{AskTimeout: &morphed})
	require.NoError(t, err)
	require.NotNil(t, previous.AskTimeout)
	assert.Equal(t, "10m", *previous.AskTimeout, "morph-back records what the copy gave the agent")

	f.AssertResumeSpawned(f.AsHuman().Resume(spawn.ConvID))
	got, _ := f.World.SpawnAskTimeout(spawn.ConvID)
	assert.Equal(t, "60s", got, "the morph wins over the re-applied copy")
	approval, _ := f.World.SpawnApproval(spawn.ConvID)
	assert.Equal(t, "plan", approval, "the copy's other field still applies")

	later := "never"
	_, err = db.ApplyAgentMorphForConv(spawn.ConvID, db.AgentMorphForm{AskTimeout: &later})
	require.NoError(t, err)
	resumeAgain(t, f, spawn.ConvID)
	got, _ = f.World.SpawnAskTimeout(spawn.ConvID)
	assert.Equal(t, "never", got)
}

// A copy may not give an agent more than its spawner could grant a child,
// and an apply whose spawner or group can no longer be read is refused.
func TestSpawnCopyRelaunch_ApplyIsBoundedByTheSpawner(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-copy-aaaa-bbbb-cccc-111111111111"
	haveLineageParent(t, f, "alpha", parent, "claude", "on", "", "default")
	rec := createProfile(t, f, map[string]any{"name": "bounded", "ask_user_question_timeout": "5m", "sandbox": "on"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	child := f.AsAgent(parent).SpawnWith("alpha", map[string]any{"name": "child", "profile": "bounded"})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	f.MarkOffline(child.TmuxSession)
	agentID := agentIDFor(t, child.ConvID)

	kind, _ := agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "bounded", func(p *db.SpawnProfile) {
		p.Approval = "bypassPermissions"
	}))
	assert.Equal(t, "approval_restricted", kind)
	kind, _ = agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "bounded", func(p *db.SpawnProfile) {
		p.Sandbox = "off"
	}))
	assert.Equal(t, "sandbox_restricted", kind)
	before := relaunchProfileJSON(t, child.ConvID)
	kind, msg := agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "bounded", func(p *db.SpawnProfile) {
		p.AskUserQuestionTimeout = "10m"
	}))
	require.Empty(t, kind, "a copy within the spawner's grant applies: %s", msg)
	assert.NotEqual(t, before, relaunchProfileJSON(t, child.ConvID))

	snapshot, err := db.GetAgentStartupSnapshot(agentID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	gone := *snapshot
	gone.SpawnedByAgent = "agt_gone"
	require.NoError(t, db.UpsertAgentStartupSnapshot(gone))
	kind, msg = agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "bounded", func(*db.SpawnProfile) {}))
	assert.Equal(t, "spawner_gone", kind)
	assert.Contains(t, msg, "agt_gone")

	gone = *snapshot
	gone.SpawnGroupID = 999999
	require.NoError(t, db.UpsertAgentStartupSnapshot(gone))
	kind, _ = agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "bounded", func(*db.SpawnProfile) {}))
	assert.Equal(t, "spawn_group_gone", kind)
}

// Only fields the copy owns are written: a field the copy leaves to the
// harness or install default keeps what the agent launched with.
func TestSpawnCopyRelaunch_FieldsTheCopyDoesNotOwnKeepTheirStoredValue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := createProfile(t, f, map[string]any{"name": "agentic", "ask_user_question_timeout": "5m"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "worker", "profile": "agentic"})
	agentID := agentIDFor(t, spawn.ConvID)

	// Something other than the copy has since moved fields the copy never set.
	plan := "plan"
	_, err := db.ApplyAgentMorphForConv(spawn.ConvID, db.AgentMorphForm{Approval: &plan})
	require.NoError(t, err)
	before := storedRelaunch(t, spawn.ConvID)

	kind, msg := agentd.ApplySpawnCopyForTest(agentID, editedCopy(t, "agentic", func(p *db.SpawnProfile) {
		p.AskUserQuestionTimeout = "10m"
	}))
	require.Empty(t, kind, msg)
	after := storedRelaunch(t, spawn.ConvID)
	assert.Equal(t, "10m", *after.AskUserQuestionTimeout)
	assert.Equal(t, "plan", *after.ApprovalPolicy, "approval was not the copy's to set")
	assert.Equal(t, *before.SandboxImplementation, *after.SandboxImplementation, "nor the install-default implementation")
	assert.Equal(t, *before.HarnessBuiltinMode, *after.HarnessBuiltinMode)
}

// An agent without launch layers is refused: which of its values were
// explicit is not known.
func TestSpawnCopyRelaunch_RefusesAnAgentWithoutLayers(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "old"})
	agentID := agentIDFor(t, spawn.ConvID)
	require.NoError(t, db.SetAgentLaunchLayers(agentID, nil))

	kind, _ := agentd.ApplySpawnCopyForTest(agentID, db.AgentSpawnCopy{})
	assert.Equal(t, "no_launch_layers", kind)
}

// A copy that would give an agent an approval only a confining sandbox may
// run is refused, as a fresh launch is: a pi agent on its built-in sandbox
// cannot be re-applied onto approval auto.
func TestSpawnCopyRelaunch_RefusesAnUnconfinedApproval(t *testing.T) {
	t.Setenv(session.ResourceDelegationDirEnv, "")
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := createProfile(t, f, map[string]any{"name": "pikit", "harness": "pi", "approval": "read-only"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	spawn, _ := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker", "profile": "pikit"})
	f.MarkOffline(spawn.TmuxSession)
	before := relaunchProfileJSON(t, spawn.ConvID)

	kind, msg := agentd.ApplySpawnCopyForTest(agentIDFor(t, spawn.ConvID),
		editedCopy(t, "pikit", func(p *db.SpawnProfile) { p.Approval = "auto" }))
	assert.Equal(t, "unconfined_approval", kind, msg)
	assert.JSONEq(t, before, relaunchProfileJSON(t, spawn.ConvID))
}

// A retired agent is refused: it will not launch again.
func TestSpawnCopyRelaunch_RefusesARetiredAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "gone"})
	agentID := agentIDFor(t, spawn.ConvID)
	_, err := db.RetireAgentByID(agentID, "test", "retired")
	require.NoError(t, err)
	before := relaunchProfileJSON(t, spawn.ConvID)

	kind, _ := agentd.ApplySpawnCopyForTest(agentID, db.AgentSpawnCopy{})
	assert.Equal(t, "agent_retired", kind)
	assert.Equal(t, before, relaunchProfileJSON(t, spawn.ConvID))
}
