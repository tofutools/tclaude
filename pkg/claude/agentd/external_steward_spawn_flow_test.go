package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/stewardtools"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// spawnSetup is an operator's working pin: group crew, spawn profile worker at the
// baseline, sandbox profile locked.
type spawnSetup struct {
	*externalStewardFlow
	dir    string
	issued externalStewardIssued
}

func newSpawnSetup(t *testing.T) *spawnSetup {
	t.Helper()
	s := newExternalStewardFlow(t, true)
	s.f.HaveGroup("crew")
	dir := testutil.CanonicalTempDir(t)
	_, err := db.SetAgentGroupDefaultCwd("crew", dir)
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "worker", Harness: harness.DefaultName, Approval: "default"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
	require.NoError(t, err)
	return &spawnSetup{externalStewardFlow: s, dir: dir, issued: s.create("assistant")}
}

func (s *spawnSetup) pin(body map[string]any) (int, string) {
	s.t.Helper()
	return s.humanPost("/v1/external-stewards/assistant/spawn", body)
}

func (s *spawnSetup) pinned() {
	s.t.Helper()
	code, body := s.pin(map[string]any{"groups": []string{"crew"}, "spawn_profile": "worker", "sandbox_profile": "locked"})
	require.Equal(s.t, http.StatusOK, code, body)
}

func (s *spawnSetup) spawn(group string, body map[string]any) (int, string) {
	s.t.Helper()
	return s.call(s.issued.Credential, http.MethodPost, "/v1/groups/"+group+"/spawn", body)
}

// worker starts one and returns its conv.
func (s *spawnSetup) worker(name string) string {
	s.t.Helper()
	code, body := s.spawn("crew", map[string]any{"name": name, "initial_message": "start"})
	require.Equal(s.t, http.StatusOK, code, body)
	var out struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(s.t, json.Unmarshal([]byte(body), &out))
	require.NotEmpty(s.t, out.ConvID)
	return out.ConvID
}

func TestExternalStewardSpawn_OffUntilTheOperatorPins(t *testing.T) {
	s := newSpawnSetup(t)
	code, body := s.spawn("crew", map[string]any{"name": "w"})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, "start workers")
}

func TestExternalStewardSpawn_ThroughTheToolAtTheBaselineUnderThePin(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	session := s.mcpSession(s.issued.Credential)
	var out struct {
		ConvID string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.SpawnAgent, map[string]any{
		"group": "crew", "name": "builder", "initial_message": "start", "task_url": "https://example.com/1",
	})), &out))

	row, err := db.FindSessionByConvID(out.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "default", row.ApprovalPolicy, "the baseline, from the pinned spawn profile")
	assert.Equal(t, s.dir, row.Cwd, "the group's own directory")

	stewards, err := db.ListExternalStewards()
	require.NoError(t, err)
	children, err := db.ListAgentChildren(stewards[0].AgentID)
	require.NoError(t, err)
	assert.Contains(t, children, mustAgentID(t, out.ConvID), "the worker's lineage names the principal")

	snapshot := s.dashboard(http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, snapshot.Code, snapshot.Body.String())
	assert.Contains(t, snapshot.Body.String(), `"spawned_by_external_steward":"assistant"`)
}

func mustAgentID(t *testing.T, conv string) string {
	t.Helper()
	id, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	return id
}

// A grant has to name everything, or it isn't made.
func TestExternalStewardSpawn_PinIsCheckedWhenGranted(t *testing.T) {
	s := newSpawnSetup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "auto", Harness: harness.DefaultName, Approval: "auto"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "blank", Harness: harness.DefaultName})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "copilot", Harness: harness.CopilotName, Approval: harness.CopilotApprovalInherit})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "shell", Harness: harness.ShellName})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "theirs", Harness: harness.DefaultName, Approval: "default", OperatorOnly: true})
	require.NoError(t, err)
	s.f.HaveGroup("nodir")

	pin := func(groups []string, profile, sandbox string) map[string]any {
		return map[string]any{"groups": groups, "spawn_profile": profile, "sandbox_profile": sandbox}
	}
	for name, body := range map[string]map[string]any{
		"no groups":     pin(nil, "worker", "locked"),
		"no profile":    pin([]string{"crew"}, "", "locked"),
		"no sandbox":    pin([]string{"crew"}, "worker", ""),
		"wildcard":      pin([]string{"*"}, "worker", "locked"),
		"unknown group": pin([]string{"ghost"}, "worker", "locked"),
		"no directory":  pin([]string{"nodir"}, "worker", "locked"),
		"unknown spawn": pin([]string{"crew"}, "ghost", "locked"),
		"auto":          pin([]string{"crew"}, "auto", "locked"),
		"no approval":   pin([]string{"crew"}, "blank", "locked"),
		"copilot":       pin([]string{"crew"}, "copilot", "locked"),
		"shell":         pin([]string{"crew"}, "shell", "locked"),
		"operator-only": pin([]string{"crew"}, "theirs", "locked"),
		"unknown box":   pin([]string{"crew"}, "worker", "ghost"),
	} {
		code, out := s.pin(body)
		assert.Equalf(t, http.StatusBadRequest, code, "%s: %s", name, out)
	}

	grant := func(scope map[string]any) int {
		rec := postPermissionScope(t, s.f, "grant", map[string]any{"target": s.issued.AgentID, "slug": agentd.PermAgentSpawn, "scope": scope})
		return rec.Code
	}
	assert.Equal(t, http.StatusConflict, grant(nil), "the general route refuses an open grant")
	assert.Equal(t, http.StatusConflict, grant(map[string]any{"group": []string{"crew"}}))
	assert.Equal(t, http.StatusConflict, grant(map[string]any{
		"group": []string{"crew"}, "spawn_profile": []string{"worker", "auto"}, "sandbox_profile": []string{"locked"},
	}), "one spawn profile")
	assert.Equal(t, http.StatusOK, grant(map[string]any{
		"group": []string{"crew"}, "spawn_profile": []string{"worker"}, "sandbox_profile": []string{"locked"},
	}), "a whole pin is a pin, by whichever route")
}

// What was fine when granted can stop being fine, so it is all checked again.
func TestExternalStewardSpawn_PinIsCheckedAtSpawn(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	s.f.HaveGroup("other")
	_, err := db.SetAgentGroupDefaultCwd("other", s.dir)
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "second", Harness: harness.DefaultName, Approval: "default"})
	require.NoError(t, err)

	code, body := s.spawn("other", map[string]any{"name": "w"})
	assert.Equal(t, http.StatusForbidden, code, "a group it wasn't given: %s", body)
	code, body = s.spawn("crew", map[string]any{"name": "w", "profile": "second"})
	assert.Equal(t, http.StatusForbidden, code, "a profile it wasn't given: %s", body)

	profile, err := db.GetSpawnProfile("worker")
	require.NoError(t, err)
	profile.Approval = "auto"
	require.NoError(t, db.UpdateSpawnProfile(profile))
	code, body = s.spawn("crew", map[string]any{"name": "w"})
	assert.Equal(t, http.StatusForbidden, code, "the profile moved off the baseline: %s", body)
	profile.Approval = "default"
	require.NoError(t, db.UpdateSpawnProfile(profile))

	_, err = db.SetAgentGroupDefaultCwd("crew", "")
	require.NoError(t, err)
	code, body = s.spawn("crew", map[string]any{"name": "w"})
	assert.Equal(t, http.StatusConflict, code, "the group lost its directory: %s", body)
}

// Owning a group gives groups.members.spawn, which names no sandbox.
func TestExternalStewardSpawn_OwningAGroupIsNoWayIn(t *testing.T) {
	s := newSpawnSetup(t)
	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, s.conv(), "human"))
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermAgentSpawn}, "human"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g.ID, ConvID: s.conv()}))
	code, body := s.spawn("crew", map[string]any{"name": "w"})
	assert.Equal(t, http.StatusForbidden, code, body)
}

func TestExternalStewardSpawn_OtherDoorsAreShut(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	conv := s.worker("w1")
	for _, slug := range []string{agentd.PermAgentClone, agentd.PermAgentMorph, agentd.PermGroupsMembersClone, agentd.PermGroupsMembersMorph} {
		assert.Truef(t, agentd.ExternalStewardRefusesForTest(slug), "%s", slug)
	}
	code, body := s.call(s.issued.Credential, http.MethodPost, "/v1/agent/"+conv+"/clone", map[string]any{})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = s.call(s.issued.Credential, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "s", "scope": map[string]any{"kind": "steward"}, "brief": "x",
	})
	assert.Equal(t, http.StatusForbidden, code, body)
}

// B's condition. The child stands where it would otherwise get these slugs from
// everywhere at once, and the control proves that setup does give them.
func TestExternalStewardSpawn_ChildDeniesBeatOwnershipAndGroupsAndDefaults(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	child := s.worker("w1")
	const control = "ctrl-aaaa-bbbb-cccc-000000000001"
	s.f.HaveConvWithTitle(control, "control")
	s.f.HaveAliveSession(control, "ctrl", "tclaude-ctrl", s.f.TestCwd("ctrl"))
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	cfg.Agent.DefaultPermissions = []string{agentd.PermSelfSchedule, agentd.PermGroupsAdmin}
	require.NoError(t, config.Save(cfg))

	g := s.f.HaveGroup("theirs")
	require.NoError(t, db.ReplaceAgentGroupPermissions(g.ID, []string{agentd.PermSelfSchedule, agentd.PermAgentSpawn}, "human"))
	for _, conv := range []string{child, control} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g.ID, ConvID: conv}))
		require.NoError(t, db.AddAgentGroupOwner(g.ID, conv, "human"))
	}
	schedule := func(conv string) *httptest.ResponseRecorder {
		return testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
			"target": conv, "interval": "10m", "subject": "later", "body": "later",
		}), conv))
	}
	spawn := func(conv string) *httptest.ResponseRecorder {
		return testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/groups/theirs/spawn", map[string]any{
			"name": "grandchild",
		}), conv))
	}
	refusedBySlug := func(rec *httptest.ResponseRecorder, slug string) bool {
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), `"code":"permission"`) &&
			strings.Contains(rec.Body.String(), slug)
	}
	// The control gets past each gate: scheduling works, and its spawn is stopped
	// only later, for the write proof a permission can't stand in for.
	require.Equal(t, http.StatusOK, schedule(control).Code)
	assert.Contains(t, spawn(control).Body.String(), "write_proof_required")

	scheduled := schedule(child)
	assert.Truef(t, refusedBySlug(scheduled, agentd.PermSelfSchedule), "a deny beats a group grant, ownership and the default: %s", scheduled.Body.String())
	spawned := spawn(child)
	assert.Truef(t, refusedBySlug(spawned, agentd.PermAgentSpawn), "and the tree stops here: %s", spawned.Body.String())

	for _, slug := range agentd.ExternalStewardChildDeniesForTest() {
		effect, ok, err := db.AgentPermissionOverride(child, slug)
		require.NoError(t, err)
		assert.Truef(t, ok && effect == db.PermEffectDeny, "%s", slug)
	}
}

func TestExternalStewardSpawn_CapCountsRunningWorkers(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	first := s.worker("w1")
	s.worker("w2")
	s.worker("w3")
	code, body := s.spawn("crew", map[string]any{"name": "w4"})
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "external_steward_child_limit")

	s.f.World.Tmux.KillBySignalForTest(mustSession(t, first))
	s.worker("w4")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward.ExternalMaxChildren = 4
	require.NoError(t, config.Save(cfg))
	s.worker("w5")
}

func mustSession(t *testing.T, conv string) string {
	t.Helper()
	row, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row.TmuxSession
}

func TestExternalStewardSpawn_RevokeListsWorkersAndLeavesThemRunning(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	conv := s.worker("w1")

	code, body := s.humanPost("/v1/external-stewards/assistant/revoke", map[string]any{})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, mustAgentID(t, conv))
	assert.True(t, s.f.World.Tmux.IsAlive(mustSession(t, conv)), "workers keep running")

	code, body = s.humanPost("/v1/external-stewards/assistant/revoke", map[string]any{"stop_children": true})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"stopped":[{`)
}

func TestExternalStewardSpawn_ExpiryTellsTheOperatorOnce(t *testing.T) {
	s := newSpawnSetup(t)
	s.pinned()
	s.worker("w1")
	notices := func() int {
		count, err := db.CountHumanMessages(db.HumanMessageFilter{})
		require.NoError(t, err)
		return count
	}
	before := notices()
	agentd.NotifyExternalStewardExpiriesForTest(time.Now())
	assert.Equal(t, before, notices(), "not while it can still sign in")
	later := time.Now().Add(365 * 24 * time.Hour)
	agentd.NotifyExternalStewardExpiriesForTest(later)
	assert.Equal(t, before+1, notices())
	agentd.NotifyExternalStewardExpiriesForTest(later)
	assert.Equal(t, before+1, notices(), "once")
}
