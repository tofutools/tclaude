package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// grantScoped issues a scoped per-agent grant through the production
// permissions endpoint, as the human operator would.
func grantScoped(t *testing.T, f *testharness.Flow, target, slug string, scope map[string]any) {
	t.Helper()
	body := map[string]any{"target": target, "slug": slug}
	if scope != nil {
		body["scope"] = scope
	}
	res := postPermissionScope(t, f, "grant", body)
	require.Equal(t, http.StatusOK, res.Code, res.Body)
}

// haveSpawnCapableMember enrolls convID in group as a plain (non-owner)
// member with the maximally-authorized launch posture the spawn path
// requires, and NO permission grant — the grant is what each scenario varies.
func haveSpawnCapableMember(t *testing.T, f *testharness.Flow, group, convID string) {
	t.Helper()
	f.HaveMember(group, convID)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + convID, TmuxSession: "tmux-" + convID, ConvID: convID,
		Cwd: f.World.HomeDir, Status: "running", Harness: harness.DefaultName,
		HarnessBuiltinMode: harness.ClaudeSandboxOff, ApprovalPolicy: "bypassPermissions",
	}))
}

func spawnAttempt(t *testing.T, f *testharness.Flow, caller, group, name string) *httpResult {
	t.Helper()
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/groups/"+group+"/spawn",
		map[string]any{"name": name})
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

// Scenario: an operator narrows a lead's spawn capability to one of the two
// groups it belongs to. The lead spawns into the granted group and is refused
// in the other — the whole point of scoped grants, at the wire surface.
//
// The same scenario pins the display-can't-drift invariant: `permissions ls`
// renders the scope the gate enforced, so a reader is never told the lead
// holds groups.members.spawn without being told where.
func TestPermissionScope_GroupScopedGrantGatesSpawnPerGroup(t *testing.T) {
	f := newFlow(t)
	// The Audit tab's handler requires a dashboard origin; assert on the
	// audit row through the same endpoint the tab reads.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")

	// A plain member of both groups: no ownership, so nothing but the grant
	// can admit it (the owner bypass would mask what we are testing).
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	f.HaveMember("beta", lead)
	grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn, map[string]any{"group": []string{"alpha"}})

	allowed := spawnAttempt(t, f, lead, "alpha", "in-scope-worker")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	refused := spawnAttempt(t, f, lead, "beta", "out-of-scope-worker")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body)
	assert.Contains(t, refused.Body, agentd.PermGroupsMembersSpawn,
		"refusal must be the ordinary not-granted path, naming the slug")

	// Nothing was spawned into beta.
	for _, m := range f.ListGroupMembers("beta") {
		assert.Equal(t, lead, m.ConvID, "a refused spawn must not enroll anyone")
	}

	// Listing == gate: the same resolver feeds both, so the effective view
	// carries the winning tier's scope.
	view := getPermissionsTarget(t, f, lead, lead)
	require.Equal(t, http.StatusOK, view.Code, view.Body)
	var effective struct {
		Provenance map[string]string `json:"provenance"`
	}
	require.NoError(t, json.Unmarshal([]byte(view.Body), &effective))
	assert.Equal(t, "override [group=alpha]", effective.Provenance[agentd.PermGroupsMembersSpawn])

	// The audit row for the allowed spawn names the scope that admitted it.
	entries := fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "").Entries
	var spawnDetail string
	for _, e := range entries {
		if e.Verb == "spawn" && e.Status == http.StatusOK {
			spawnDetail = e.Detail
		}
	}
	assert.Contains(t, spawnDetail, "scope: "+agentd.PermGroupsMembersSpawn+" [group=alpha]",
		"a scoped authorization must be recorded on the audit row")
}

// A dimension the call site does not describe fails CLOSED. handleGroupSpawn
// describes spawn_profile only when a NAMED profile resolves, so a spawn with
// no profile (and no group/global default to fall back to) leaves it
// undescribed — and a profile-scoped grant authorizes nothing rather than
// everything. See TestAttenuation_SpawnProfileScopedGrantGatesPerProfile for
// the satisfied half.
func TestPermissionScope_UndescribedDimensionFailsClosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000002"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn, map[string]any{"spawn_profile": []string{"locked"}})

	refused := spawnAttempt(t, f, lead, "alpha", "profile-scoped-worker")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body)
}

func TestPermissionScope_SandboxProfileScopedGrantGatesAgentSelection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000005"
	haveSpawnCapableMember(t, f, "alpha", lead)
	for _, name := range []string{"locked", "open"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name})
		require.NoError(t, err)
	}
	grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn,
		map[string]any{"sandbox_profile": []string{"locked"}})

	allowed := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
		"name": "sandbox-scoped-worker", "sandbox_profile": "locked",
	})
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Raw)

	refused := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
		"name": "wrong-sandbox-worker", "sandbox_profile": "open",
	})
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Raw)

	missing := spawnAttempt(t, f, lead, "alpha", "missing-sandbox-worker")
	assert.Equal(t, http.StatusForbidden, missing.Code, missing.Body,
		"with no group or global assignment to inherit, the dimension stays undescribed")
}

// The inherited half of the same gate: a caller that selects nothing is judged
// on the profile it will actually run under, so the operator's own default
// satisfies a grant that names it. Scoping a lead to the group default is the
// natural way to say "spawn workers exactly as configured, nothing wider", and
// before this the lead had to restate the default by name to spawn at all.
func TestPermissionScope_SandboxProfileScopeAcceptsInheritedDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		assign  func(t *testing.T)
		granted string
		want    int
	}{
		{
			name: "group-default",
			assign: func(t *testing.T) {
				_, err := db.SetAgentGroupSandboxProfile("alpha", "locked")
				require.NoError(t, err)
			},
			granted: "locked",
			want:    http.StatusOK,
		},
		{
			name:    "global-default",
			assign:  func(t *testing.T) { require.NoError(t, db.SetGlobalSandboxProfile("locked")) },
			granted: "locked",
			want:    http.StatusOK,
		},
		{
			// The group assignment is the tier the launch resolves to, so a
			// grant that only admits the global one does not reach it.
			name: "group-default-shadows-allowed-global",
			assign: func(t *testing.T) {
				require.NoError(t, db.SetGlobalSandboxProfile("locked"))
				_, err := db.SetAgentGroupSandboxProfile("alpha", "open")
				require.NoError(t, err)
			},
			granted: "locked",
			want:    http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000008"
			haveSpawnCapableMember(t, f, "alpha", lead)
			for _, name := range []string{"locked", "open"} {
				_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: name})
				require.NoError(t, err)
			}
			tc.assign(t)
			grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn,
				map[string]any{"sandbox_profile": []string{tc.granted}})

			inherited := spawnAttempt(t, f, lead, "alpha", "inherited-sandbox-worker")
			assert.Equal(t, tc.want, inherited.Code, inherited.Body)
		})
	}
}

// An inherited tier authorizes the spawn only as long as it BINDS the child, so
// a caller may not spend a sandbox_profile-scoped grant on a launch that would
// not enforce the profile. Two distinct routes drop it: a mode that omits the
// profile tiers outright, and an implementation that composes the whole chain
// and records it while standing the OS boundary down. Both must refuse, or the
// scope is satisfied on paper and confines nothing.
func TestPermissionScope_ScopedSandboxProfileMustBindTheChild(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      map[string]any
		linuxOnly bool
	}{
		{
			name: "mode-omits-the-tiers",
			body: map[string]any{"harness": harness.CodexName, "sandbox": harness.SandboxDangerFull},
		},
		{
			// resource-only composes and records `locked` in full, so the
			// composition predicate says nothing was dropped — but it enforces
			// none of the access rules.
			//
			// Linux-only: elsewhere the implementation is refused 422 as
			// unavailable before the request ever reaches this guard, so there
			// would be nothing here to assert on.
			name:      "implementation-omits-os-confinement",
			body:      map[string]any{"sandbox_implementation": string(sandboxpolicy.ImplementationResourceOnly)},
			linuxOnly: true,
		},
		{
			name: "implementation-off",
			body: map[string]any{"sandbox_implementation": string(sandboxpolicy.ImplementationOff)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.linuxOnly && runtime.GOOS != "linux" {
				t.Skip("sandbox implementation resource-only is Linux only; " +
					"the availability check refuses it before the scope-binding guard runs")
			}
			f := newFlow(t)
			f.HaveGroup("alpha")
			const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000009"
			f.HaveMember("alpha", lead)
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "sess-" + lead, TmuxSession: "tmux-" + lead, ConvID: lead,
				Cwd: f.World.HomeDir, Status: "running", Harness: harness.CodexName,
				HarnessBuiltinMode: harness.SandboxDangerFull, ApprovalPolicy: "never",
			}))
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
			require.NoError(t, err)
			_, err = db.SetAgentGroupSandboxProfile("alpha", "locked")
			require.NoError(t, err)
			grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn,
				map[string]any{"sandbox_profile": []string{"locked"}})

			body := map[string]any{"name": "unbound-sandbox-worker"}
			for k, v := range tc.body {
				body[k] = v
			}
			spawn := f.AsAgent(lead).SpawnWith("alpha", body)
			assert.Equal(t, http.StatusForbidden, spawn.Code, spawn.Raw)
			assert.Contains(t, string(spawn.Raw), "sandbox_profile_restricted")
		})
	}
}

// The counterpart the guard must NOT catch. Binding the launch to the profile
// is owed only by the caller whose authority was conditioned on it; an agent
// holding an ordinary unscoped grant never traded on the profile, so the mere
// existence of an ambient assignment somewhere in the deployment is not a
// reason to refuse it. The global tier makes this sharp: it resolves for EVERY
// group, so a guard keyed on "a profile resolved" rather than "the grant
// pinned one" would bind every agent caller in the whole install.
func TestPermissionScope_UnscopedSpawnGrantIsNotBoundByAmbientSandboxProfile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		assign func(t *testing.T)
	}{
		{
			name:   "global-assignment-only",
			assign: func(t *testing.T) { require.NoError(t, db.SetGlobalSandboxProfile("locked")) },
		},
		{
			name: "group-assignment",
			assign: func(t *testing.T) {
				_, err := db.SetAgentGroupSandboxProfile("alpha", "locked")
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000010"
			f.HaveMember("alpha", lead)
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "sess-" + lead, TmuxSession: "tmux-" + lead, ConvID: lead,
				Cwd: f.World.HomeDir, Status: "running", Harness: harness.CodexName,
				HarnessBuiltinMode: harness.SandboxDangerFull, ApprovalPolicy: "never",
			}))
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
			require.NoError(t, err)
			tc.assign(t)
			// Unscoped: the grant says nothing about sandbox profiles.
			require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsMembersSpawn, "test"))

			spawn := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
				"name": "unscoped-omitting-worker", "harness": harness.CodexName,
				"sandbox": harness.SandboxDangerFull,
			})
			assert.Equal(t, http.StatusOK, spawn.Code, spawn.Raw)
		})
	}
}

func TestPermissionScope_GlobalAgentSpawnUsesProfileScopes(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000006"
	f.HaveConvWithTitle(lead, "global-spawner")
	f.HaveEnrolledAgent(lead)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + lead, TmuxSession: "tmux-" + lead, ConvID: lead,
		Cwd: f.World.HomeDir, Status: "running", Harness: harness.DefaultName,
		HarnessBuiltinMode: harness.ClaudeSandboxOff, ApprovalPolicy: "bypassPermissions",
	}))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "global-allowed"})
	require.NoError(t, err)
	grantScoped(t, f, lead, agentd.PermAgentSpawn, map[string]any{
		"group": []string{"alpha"}, "sandbox_profile": []string{"global-allowed"},
	})

	spawn := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
		"name": "global-spawn-worker", "sandbox_profile": "global-allowed",
	})
	require.Equal(t, http.StatusOK, spawn.Code, spawn.Raw)
}

func TestPermissionScope_SandboxProfileCannotNameAnOmittedProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000007"
	f.HaveMember("alpha", lead)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + lead, TmuxSession: "tmux-" + lead, ConvID: lead,
		Cwd: f.World.HomeDir, Status: "running", Harness: harness.CodexName,
		HarnessBuiltinMode: harness.SandboxDangerFull, ApprovalPolicy: "never",
	}))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
	require.NoError(t, err)
	grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn,
		map[string]any{"sandbox_profile": []string{"locked"}})

	spawn := f.AsAgent(lead).SpawnWith("alpha", map[string]any{
		"name": "omitted-sandbox-worker", "harness": harness.CodexName,
		"sandbox": harness.SandboxDangerFull, "sandbox_profile": "locked",
	})
	assert.Equal(t, http.StatusForbidden, spawn.Code, spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "resolved launch mode omits sandbox profiles")
}

// Regression guard for the ~129 gate sites that pass no ActionContext: an
// UNSCOPED grant must still decide exactly as it did before scopes existed.
func TestPermissionScope_UnscopedGrantIsUnaffected(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000003"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsMembersSpawn, nil)

	allowed := spawnAttempt(t, f, lead, "alpha", "unscoped-worker")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	// And no scope noise on the audit row for an unscoped decision.
	for _, e := range fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "").Entries {
		if e.Verb == "spawn" {
			assert.NotContains(t, e.Detail, "scope: ")
		}
	}
}

// A target that has no recorded lineage remains outside @descendants. This is
// especially important for pre-migration agents, whose SpawnRequest snapshot
// does not identify their spawner and therefore cannot be safely backfilled.
func TestPermissionScope_SelectorRefusesUnrelatedTarget(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const caller = "scopegate-caller-aaaa-bbbb-cccc-00000000004"
	const target = "scopegate-target-aaaa-bbbb-cccc-00000000004"
	f.HaveMember("alpha", caller)
	f.HaveMember("alpha", target)

	grantScoped(t, f, caller, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})
	rec := agentReq(t, f, caller, http.MethodPost,
		"/v1/agent/"+target+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	state, err := db.AgentState(target)
	require.NoError(t, err)
	assert.NotEqual(t, db.AgentStateRetired, state, "a refused retire must not demote the target")

	// Control: the same grant without a scope retires the peer, proving the
	// refusal above came from the selector and not from the setup.
	grantScoped(t, f, caller, agentd.PermAgentRetire, nil)
	rec = agentReq(t, f, caller, http.MethodPost,
		"/v1/agent/"+target+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestPermissionScope_LineageSelectorsGateRetire(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	const parent = "scope-lineage-parent-aaaa-bbbb-cccc-00000001"
	haveSpawnCapableMember(t, f, "alpha", parent)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermGroupsMembersSpawn, "test"))

	child := f.AsAgent(parent).SpawnWith("alpha", map[string]any{"name": "lineage-child"})
	require.Equalf(t, http.StatusOK, child.Code, "child spawn: %s", child.Raw)
	require.NotEmpty(t, child.ConvID)
	require.NoError(t, db.GrantAgentPermission(child.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	grandchild := f.AsAgent(child.ConvID).SpawnWith("alpha", map[string]any{"name": "lineage-grandchild"})
	require.Equalf(t, http.StatusOK, grandchild.Code, "grandchild spawn: %s", grandchild.Raw)
	require.NotEmpty(t, grandchild.ConvID)

	const unrelated = "scope-lineage-unrelated-aaaa-bbbb-cccc-00001"
	f.HaveMember("alpha", unrelated)

	grantScoped(t, f, parent, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@self-spawned"}})
	refusedGrandchild := agentReq(t, f, parent, http.MethodPost,
		"/v1/agent/"+grandchild.ConvID+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusForbidden, refusedGrandchild.Code, refusedGrandchild.Body.String())
	allowedChild := agentReq(t, f, parent, http.MethodPost,
		"/v1/agent/"+child.ConvID+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusOK, allowedChild.Code, allowedChild.Body.String())

	grantScoped(t, f, parent, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})
	refusedUnrelated := agentReq(t, f, parent, http.MethodPost,
		"/v1/agent/"+unrelated+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusForbidden, refusedUnrelated.Code, refusedUnrelated.Body.String())
	allowedGrandchild := agentReq(t, f, parent, http.MethodPost,
		"/v1/agent/"+grandchild.ConvID+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusOK, allowedGrandchild.Code, allowedGrandchild.Body.String())

	parentID, err := db.AgentIDForConv(parent)
	require.NoError(t, err)
	childID, err := db.AgentIDForConv(child.ConvID)
	require.NoError(t, err)
	grandchildID, err := db.AgentIDForConv(grandchild.ConvID)
	require.NoError(t, err)
	direct, err := db.IsDirectAgentChild(parentID, childID)
	require.NoError(t, err)
	assert.True(t, direct, "retiring a direct child preserves its lineage row")
	descendant, err := db.IsAgentDescendant(parentID, grandchildID)
	require.NoError(t, err)
	assert.True(t, descendant, "retiring both descendants preserves the transitive facts")
}

func TestPermissionScope_CloneDoesNotBecomeDescendant(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const source = "scope-clone-source-aaaa-bbbb-cccc-000000001"
	f.HaveConvWithTitle(source, "clone-source")
	f.HaveAliveSession(source, "spwn-clone-source", "tmux-clone-source", f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", source)
	require.NoError(t, db.GrantAgentPermission(source, agentd.PermAgentClone, "test"))

	clone := f.AsAgent(source).CloneFresh(source)
	require.Equalf(t, http.StatusOK, clone.Code, "clone: %s", clone.Raw)
	require.NotEmpty(t, clone.NewConv)
	grantScoped(t, f, source, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})
	rec := agentReq(t, f, source, http.MethodPost,
		"/v1/agent/"+clone.NewConv+"/retire?shutdown=0&delete_worktree=0", nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	sourceID, err := db.AgentIDForConv(source)
	require.NoError(t, err)
	cloneID, err := db.AgentIDForConv(clone.NewConv)
	require.NoError(t, err)
	matched, err := db.IsAgentDescendant(sourceID, cloneID)
	require.NoError(t, err)
	assert.False(t, matched, "clone is a fork, not a spawned child")
}

// setGroupGrantScope attaches a scope to an existing group grant. Phase 1
// made group-grant scopes storable and importable but gave the permissions
// endpoint no group target, so a test writes the column directly.
func setGroupGrantScope(t *testing.T, groupID int64, slug, scopeJSON string) {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	res, err := d.Exec(`UPDATE agent_group_permissions SET scope_json = ? WHERE group_id = ? AND slug = ?`,
		scopeJSON, groupID, slug)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "expected exactly one group grant row to scope")
}

// The route surface retains membership plus exact-target-group provenance,
// then uses the central resolver and evaluator. It must still honour a grant's
// scope — otherwise a grant that reads as narrow in `permissions ls` is a
// wildcard here.
//
// Both tiers a scope can ride on are covered: the per-agent override and the
// target group's own grant.
func TestPermissionScope_RouteGateHonoursGroupScope(t *testing.T) {
	skipDarwinRouteAuthorityFlow(t)
	f := newFlow(t)
	const publisher = "scopegate-publisher-0005"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", publisher)
	f.HaveMember("beta", publisher)

	// Per-agent tier: scoped to alpha, so beta must refuse.
	grantScoped(t, f, publisher, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})
	rec, _ := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "alpha", "name": "in-scope", "target": "tcp://127.0.0.1:43227",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "beta", "name": "out-of-scope", "target": "tcp://127.0.0.1:43228",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "route_permission", body["code"])

	// Group tier: clear the per-agent override and let beta grant the slug
	// itself, scoped to a group it is not. The grant belongs to beta but
	// speaks only for alpha, so it must not authorize a beta publish.
	_, err := db.RevokeAgentPermission(publisher, agentd.PermRoutesPublish)
	require.NoError(t, err)
	beta, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(beta.ID, []string{agentd.PermRoutesPublish}, "test"))
	setGroupGrantScope(t, beta.ID, agentd.PermRoutesPublish, `{"group":["alpha"]}`)
	rec, body = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "beta", "name": "group-scoped-elsewhere", "target": "tcp://127.0.0.1:43229",
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "route_permission", body["code"])

	// Re-scope beta's own grant to beta and the same publish succeeds.
	setGroupGrantScope(t, beta.ID, agentd.PermRoutesPublish, `{"group":["beta"]}`)
	rec, _ = serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "beta", "name": "group-scoped-here", "target": "tcp://127.0.0.1:43230",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// The central source loader is normally best-effort, but routes historically
// failed closed when a higher-precedence tier could not be read. Preserve that
// contract across the fold: an unreadable override tier must not disappear and
// expose the target group's lower-precedence allow.
func TestPermissionScope_RouteGateFailsClosedOnPermissionTierReadError(t *testing.T) {
	skipDarwinRouteAuthorityFlow(t)
	f := newFlow(t)
	const publisher = "scopegate-read-error-publisher-0008"
	f.HaveConvWithTitle(publisher, "publisher")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", publisher)
	alpha, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(alpha.ID, []string{agentd.PermRoutesPublish}, "test"))

	database, err := db.Open()
	require.NoError(t, err)
	_, err = database.Exec(`DROP TABLE agent_permissions`)
	require.NoError(t, err)

	rec, body := serveRouteAgent(t, f, http.MethodPost, "/v1/routes/publish", publisher, map[string]any{
		"group": "alpha", "name": "must-not-open", "target": "tcp://127.0.0.1:43231",
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Equal(t, "route_authority", body["code"])
}

// A process_template scope speaks in the same stable template ids accepted by
// POST /v1/process/runs. One grant may name several ids; a different template
// falls through to the ordinary popup-or-403 permission path before any run is
// created. Program authorizations remain an independent, in-run boundary.
func TestPermissionScope_ProcessRunCreateIsBoundedByTemplateID(t *testing.T) {
	f, root := processRuntimeFlow(t)
	templateRefs := map[string]string{}
	for _, id := range []string{"a", "b", "c"} {
		templateRefs[id] = putProcessRuntimeTemplate(t, root, processRuntimeTemplate(id, 1)).Ref
	}

	const worker = "scopegate-process-worker-0007"
	f.HaveConvWithTitle(worker, "process worker")
	f.HaveEnrolledAgent(worker)
	grantScoped(t, f, worker, agentd.PermProcessRunsManage,
		map[string]any{"process_template": []string{"a", "b"}})

	create := func(templateID string) *httptest.ResponseRecorder {
		return agentReq(t, f, worker, http.MethodPost, "/v1/process/runs", map[string]any{
			"templateId": templateID, "authorizeProgramProfiles": []string{"safe"},
		})
	}
	for _, templateID := range []string{"a", "b"} {
		created := create(templateID)
		require.Equalf(t, http.StatusCreated, created.Code, "template %s: %s", templateID, created.Body.String())
	}

	refused := create("c")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), agentd.PermProcessRunsManage)
	assert.NotContains(t, refused.Body.String(), "process_program_unauthorized",
		"program authorizations are downstream of the template-scoped verb gate")

	listed := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	var page struct {
		Runs []struct {
			TemplateRef string `json:"templateRef"`
		} `json:"runs"`
	}
	testharness.DecodeJSON(t, listed, &page)
	require.Len(t, page.Runs, 2, "the refused template must not create a run")
	for _, run := range page.Runs {
		assert.NotEqual(t, templateRefs["c"], run.TemplateRef)
	}
}

// Deny stays unscoped and terminal: it beats a scoped allow underneath it,
// and it is not weakened by the action falling inside that allow's scope.
func TestPermissionScope_DenyBeatsScopedAllowAtTheGate(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "scopegate-lead-aaaa-bbbb-cccc-000000000006"
	haveSpawnCapableMember(t, f, "alpha", lead)

	alpha, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(alpha.ID, []string{agentd.PermGroupsMembersSpawn}, "test"))
	setGroupGrantScope(t, alpha.ID, agentd.PermGroupsMembersSpawn, `{"group":["alpha"]}`)

	// The group grant alone admits the spawn — the scope matches the target.
	allowed := spawnAttempt(t, f, lead, "alpha", "group-scoped-worker")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	// A per-agent deny above it is authoritative all the same.
	require.NoError(t, db.SetAgentPermissionOverride(lead, agentd.PermGroupsMembersSpawn, db.PermEffectDeny, "test"))
	refused := spawnAttempt(t, f, lead, "alpha", "denied-worker")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body)
}
