package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-1107: remote_control, auto_memory, peer_messaging and
// include_group_context resolve the same way on every spawn path — direct
// spawn, template deploy, scribe, trigger worker and process worker:
//
//	explicit > group policy > named profile > group default > global default > off
//
// (include_group_context falls back to the path's own default instead of off).
// Each path gets two pins: with nothing configured the launch is what it always
// was, and a profile (or the group's remote-control policy) that turns the
// fields on is now honoured. All assertions are at the Spawn seam (what the
// launch received) and the new agent's inbox (what it was told).

const uniformGroupGuidance = "UNIFORM GROUP GUIDANCE"

// uniformToggles is what one launch received for the three harness toggles.
type uniformToggles struct{ RemoteControl, AutoMemory, PeerMessaging bool }

func spawnedToggles(t *testing.T, f *testharness.Flow, conv string) uniformToggles {
	t.Helper()
	rc, ok := f.World.SpawnRemoteControl(conv)
	require.Truef(t, ok, "no spawn recorded for conv %s", conv)
	am, ok := f.World.SpawnAutoMemory(conv)
	require.True(t, ok)
	pm, ok := f.World.SpawnPeerMessaging(conv)
	require.True(t, ok)
	return uniformToggles{RemoteControl: rc, AutoMemory: am, PeerMessaging: pm}
}

// createAllOnProfile stores a Claude profile that turns all three toggles on
// and the group context off — the opposite of every path's default.
func createAllOnProfile(t *testing.T, f *testharness.Flow, name string) {
	t.Helper()
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": name, "harness": "claude",
		"remote_control": true, "auto_memory": true, "peer_messaging": true,
		"include_group_default_context": false,
	}).Code, "create profile %s", name)
}

func inboxBodies(t *testing.T, conv string) string {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	var b strings.Builder
	for _, m := range rows {
		b.WriteString(m.Body)
		b.WriteString("\n")
	}
	return b.String()
}

var (
	allOff = uniformToggles{}
	allOn  = uniformToggles{RemoteControl: true, AutoMemory: true, PeerMessaging: true}
)

// --- direct spawn ---

func TestLaunchUniform_Direct_NothingConfigured(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", uniformGroupGuidance)
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "initial_message": "Do it."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Equal(t, allOff, spawnedToggles(t, f, spawn.ConvID))
	assert.Contains(t, inboxBodies(t, spawn.ConvID), uniformGroupGuidance)
}

func TestLaunchUniform_Direct_GlobalDefaultProfileHonoured(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	setGroupContext(t, f, "alpha", uniformGroupGuidance)
	createAllOnProfile(t, f, "all-on")
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "all-on").Code)
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "initial_message": "Do it."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Equal(t, allOn, spawnedToggles(t, f, spawn.ConvID))
	body := inboxBodies(t, spawn.ConvID)
	assert.Contains(t, body, "Do it.")
	assert.NotContains(t, body, uniformGroupGuidance)
}

// --- template deploy ---

func deployUniformTemplate(t *testing.T, f *testharness.Flow, agentSpec map[string]any) string {
	t.Helper()
	agentSpec["name"], agentSpec["role"], agentSpec["initial_message"] = "member", "dev", "Do it."
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "uniform-team", "default_context": uniformGroupGuidance,
		"agents": []map[string]any{agentSpec},
	}).Code, "create template")
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/uniform-team/deploy", map[string]any{
		"group_name": "uniform-force",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equalf(t, 1, res.Spawned, "member spawned: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()
	return res.Agents[0].ConvID
}

func TestLaunchUniform_TemplateDeploy_NothingConfigured(t *testing.T) {
	f := newFlow(t)
	conv := deployUniformTemplate(t, f, map[string]any{})
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
	assert.Contains(t, inboxBodies(t, conv), uniformGroupGuidance)
}

// Before TCL-1107 a deploy consulted only the template's own profile tiers for
// these fields, so the workspace default below was silently dropped.
func TestLaunchUniform_TemplateDeploy_GlobalDefaultProfileHonoured(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "all-on")
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "all-on").Code)
	conv := deployUniformTemplate(t, f, map[string]any{})
	assert.Equal(t, allOn, spawnedToggles(t, f, conv))
	body := inboxBodies(t, conv)
	assert.Contains(t, body, "Do it.")
	assert.NotContains(t, body, uniformGroupGuidance)
}

// A referenced profile already spoke for the toggles; it now speaks for the
// group context too.
func TestLaunchUniform_TemplateDeploy_ReferencedProfileHonoured(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "all-on")
	conv := deployUniformTemplate(t, f, map[string]any{"spawn_profile": "all-on"})
	assert.Equal(t, allOn, spawnedToggles(t, f, conv))
	assert.NotContains(t, inboxBodies(t, conv), uniformGroupGuidance)
}

// --- scribe ---

func summonUniformScribe(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	stubScribeTerminal(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{"name": "uniform-scribe", "slugs": []string{agentd.PermTemplatesManage}, "brief": "Edit things."})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	return decodeScribeResp(t, rec).ConvID
}

func TestLaunchUniform_Scribe_NothingConfigured(t *testing.T) {
	f := newFlow(t)
	conv := summonUniformScribe(t, f)
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
}

func TestLaunchUniform_Scribe_GlobalDefaultProfileHonoured(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "all-on")
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "all-on").Code)
	conv := summonUniformScribe(t, f)
	assert.Equal(t, allOn, spawnedToggles(t, f, conv))
}

// --- trigger worker ---

// fireUniformTrigger runs one trigger-spawned worker with the named profile
// into group alpha and returns its conv.
func fireUniformTrigger(t *testing.T, f *testharness.Flow, profile string) string {
	t.Helper()
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	const author = "uniform-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "uniform", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftExclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
			Spawn: &db.TriggerSpawnAction{Profile: profile, NameTemplate: "review-{{pr.number}}",
				InstructionTemplate: "Review {{pr.url}}.", MaxLiveWorkers: 1}}}})
	require.NoError(t, err)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPRDetails(authorAgent, "https://github.com/o/r/pull/9", "ready", "open", "topic", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Actions, 1)
	require.Equalf(t, "spawned", rows[0].Actions[0].Outcome, "%+v", rows[0].Actions[0])
	conv, err := db.CurrentConvForAgent(rows[0].Actions[0].SpawnedAgent)
	require.NoError(t, err)
	agentd.WaitForBackgroundForTest()
	return conv
}

func uniformTriggerGroup(t *testing.T) *testharness.Flow {
	t.Helper()
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	setGroupContext(t, f, "alpha", uniformGroupGuidance)
	return f
}

func TestLaunchUniform_Trigger_NothingConfigured(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
	assert.Contains(t, inboxBodies(t, conv), uniformGroupGuidance)
}

// Before TCL-1107 an unattended worker always launched with all three off,
// whatever its profile said (operator sub-choice (a): remote_control too).
func TestLaunchUniform_Trigger_NamedProfileHonoured(t *testing.T) {
	f := uniformTriggerGroup(t)
	createAllOnProfile(t, f, "reviewer")
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, allOn, spawnedToggles(t, f, conv))
	body := inboxBodies(t, conv)
	assert.Contains(t, body, "pull/9")
	assert.NotContains(t, body, uniformGroupGuidance)
}

func TestLaunchUniform_Trigger_GroupRemoteControlPolicyHonoured(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "optin").Code)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, uniformToggles{RemoteControl: true}, spawnedToggles(t, f, conv))
}

// The group policy sits above the named profile here too.
func TestLaunchUniform_Trigger_GroupDenyBeatsNamedProfile(t *testing.T) {
	f := uniformTriggerGroup(t)
	createAllOnProfile(t, f, "reviewer")
	require.Equal(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "deny").Code)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, uniformToggles{AutoMemory: true, PeerMessaging: true}, spawnedToggles(t, f, conv))
}

// --- process worker ---

func spawnUniformPerformer(t *testing.T, profile *db.SpawnProfile, setup ...func()) (*testharness.Flow, string) {
	t.Helper()
	f, root := processRuntimeFlow(t)
	for _, fn := range setup {
		fn()
	}
	g := f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
	require.NoError(t, err)
	setGroupContext(t, f, "crew", uniformGroupGuidance)
	_, err = db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	performer := devPerformer()
	performer.Spawn = model.SpawnAllowed
	view := startPerformerRun(t, f, root, "run_uniform", performerTemplate("uniform", performer, nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	require.NotNil(t, view.Commands[0].Performer, "%+v", view)
	require.Equal(t, "spawned", view.Commands[0].Performer.Resolution)
	conv, err := db.CurrentConvForAgent(view.Commands[0].Performer.AgentID)
	require.NoError(t, err)
	agentd.WaitForBackgroundForTest()
	return f, conv
}

func TestLaunchUniform_Process_NothingConfigured(t *testing.T) {
	f, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude"})
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
	assert.Contains(t, inboxBodies(t, conv), uniformGroupGuidance)
}

func TestLaunchUniform_Process_NamedProfileHonoured(t *testing.T) {
	on, off := true, false
	f, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude",
		RemoteControl: &on, AutoMemory: &on, PeerMessaging: &on, IncludeGroupDefaultContext: &off})
	assert.Equal(t, allOn, spawnedToggles(t, f, conv))
	assert.NotContains(t, inboxBodies(t, conv), uniformGroupGuidance)
}

// A group policy that opts in must not fail a worker whose harness has no
// Remote Access: the policy-derived value is clamped off and the spawn goes on.
func TestLaunchUniform_Trigger_OptInPolicyClampedForCodex(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "codex"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGroupRemoteControlPolicy(t, f, "alpha", "optin").Code)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
}

// A scribe is briefed by its summon, so its group's context stays out unless a
// profile tier asks for it.
func TestLaunchUniform_Scribe_GroupContextFollowsProfile(t *testing.T) {
	f := newFlow(t)
	first := summonUniformScribe(t, f)
	setGroupContext(t, f, "uniform-scribe", uniformGroupGuidance)
	assert.NotContains(t, inboxBodies(t, first), uniformGroupGuidance)
	plain := summonUniformScribe(t, f)
	assert.NotContains(t, inboxBodies(t, plain), uniformGroupGuidance, "nothing configured: left out")

	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "with-context", "harness": "claude", "include_group_default_context": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "with-context").Code)
	briefed := summonUniformScribe(t, f)
	assert.Contains(t, inboxBodies(t, briefed), uniformGroupGuidance, "a profile tier turns it on")
}
