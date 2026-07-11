package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the scribe-summon endpoint behind the dashboard's "Edit
// with agent" buttons (JOH-361): a human summon spawns a pre-briefed,
// pre-granted scribe; a repeat click reuses the live one rather than
// double-spawning; and an agent caller is gated exactly like the spawn path
// (groups.spawn + — because a summon carries birth-time grants —
// permissions.grant). Asserted at the real surfaces the dashboard reads:
// db.ListAgentGroupMembers (the agent listing) and
// db.ListAgentPermissionOverridesForConv (the granted slugs).

// scribeSummonResp is the decoded /v1/scribe response.
type scribeSummonResp struct {
	Name      string `json:"name"`
	ConvID    string `json:"conv_id"`
	Reused    bool   `json:"reused"`
	FocusMode string `json:"focus_mode"`
}

// stubScribeTerminal records how many times a scribe window was opened and
// returns success (native), so summonScribe's auto-focus never touches a real
// terminal.
func stubScribeTerminal(t *testing.T) *int {
	t.Helper()
	var opens int
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opens++
		return nil
	}))
	return &opens
}

func decodeScribeResp(t *testing.T, rec *httptest.ResponseRecorder) scribeSummonResp {
	t.Helper()
	var resp scribeSummonResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode /v1/scribe body=%s", rec.Body.String())
	return resp
}

// Scenario: a human summons a scribe. It comes up in its own eponymous
// one-member group, holding exactly the requested slug, and its window is
// opened.
func TestScribeSummon_HumanCreatesGrantedScribe(t *testing.T) {
	// The scribe=true wire assertion below fetches /api/snapshot, whose
	// auth pins the Origin to popupBaseURL — set it so the test handler
	// injects a matching Origin (same shim the group-descr snapshot tests use).
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "You edit summoning circles on this daemon. Discover them with `tclaude agent templates ls`.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())

	resp := decodeScribeResp(t, rec)
	require.NotEmpty(t, resp.ConvID, "summon returned a conv-id")
	assert.False(t, resp.Reused, "a first summon is a fresh spawn, not a reuse")

	// The scribe's eponymous group exists and holds exactly one member — the
	// agent-listing surface the dashboard renders.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g, "scribe group was created")
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "exactly one scribe in the group")
	assert.Equal(t, resp.ConvID, members[0].ConvID, "the member is the summoned scribe")

	// The requested slug is a real persisted grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(resp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "templates.manage granted at birth")

	// The snapshot flags the scribe's eponymous group as `scribe` — the wire
	// bit the Groups tab keys off to hide it by default (only shown when the
	// human ticks "show circle-scribe" in the view popover).
	assert.True(t, dashGroupByName(t, "circle-scribe").Scribe, "snapshot marks the scribe group scribe=true")

	// The scribe's window was opened.
	assert.Equal(t, 1, *opens, "summon opened the scribe's terminal window")
}

// Scenario: a repeat click reuses the live scribe rather than spawning a
// second — the group still holds exactly one member — and re-opens its window.
func TestScribeSummon_ReuseIfAliveNoDoubleSpawn(t *testing.T) {
	f := newFlow(t)
	opens := stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit the circle named feature-team.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	assert.False(t, first.Reused, "first summon spawns")

	second := summon()
	assert.True(t, second.Reused, "second summon reuses the live scribe")
	assert.Equal(t, first.ConvID, second.ConvID, "reuse returns the same scribe conv")

	// Still exactly one scribe — reuse did not litter a second.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "reuse-if-alive spawned no second scribe")

	// The grant is (still) present after reuse's idempotent re-grant.
	overrides, err := db.ListAgentPermissionOverridesForConv(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermTemplatesManage], "grant intact after reuse")

	// Both summons opened a window (fresh spawn's auto-focus + reuse's re-focus).
	assert.Equal(t, 2, *opens, "each summon opened the scribe's window")
}

// A capability-reducing scribe can pin explicit denies as well as its narrow
// grant. Reuse must reapply those denies so stale/manual/default grants cannot
// silently widen the live scribe past the safety boundary promised by its UI.
func TestScribeSummon_ReuseReappliesExplicitDenies(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":      "sandbox-scribe",
				"slugs":     []string{agentd.PermSandboxProfilesDraft},
				"exclusive": true,
				"brief":     "Prepare a draft only; never save or launch anything.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.NoError(t, db.GrantAgentPermission(first.ConvID, agentd.PermSandboxProfilesManage, "simulated-stale-grant"))
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID: first.ConvID, Slug: agentd.PermSandboxProfilesManage,
		ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "test", Reason: "simulated stale elevation",
	})
	require.NoError(t, err)
	second := summon()
	require.True(t, second.Reused)
	require.Equal(t, first.ConvID, second.ConvID)

	overrides, err := db.ListAgentPermissionOverridesForConv(first.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, overrides[agentd.PermSandboxProfilesDraft])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSandboxProfilesManage])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermGroupsSpawn])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermTemplatesUse])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSelfClone])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermSelfReincarnate])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermPermissionsGrant])
	assert.Equal(t, db.PermEffectDeny, overrides[agentd.PermPermissionsRevoke])
	activeSudo, err := db.ListActiveSudoGrants(first.ConvID)
	require.NoError(t, err)
	assert.Empty(t, activeSudo, "exclusive summon revokes elevations that override permanent denies")
}

// Scenario: after the scribe's session dies (e.g. a daemon restart leaves the
// membership row but kills the tmux session), a re-summon spawns a FRESH scribe
// and prunes the dead one — the group does not accumulate stale members.
func TestScribeSummon_DeadScribePrunedOnResummon(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit summoning circles.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.False(t, first.Reused, "first summon spawns")

	// Kill the scribe's tmux session but leave its membership row (what a daemon
	// restart does).
	sessions, err := db.FindSessionsByConvID(first.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the fresh scribe has a session row")
	f.MarkOffline(sessions[0].TmuxSession)

	second := summon()
	assert.False(t, second.Reused, "a dead scribe is not reused — a fresh one is spawned")
	assert.NotEqual(t, first.ConvID, second.ConvID, "the fresh scribe is a new conv")

	// The dead scribe was pruned: exactly one (live) member remains.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1, "the dead scribe was pruned, not accumulated")
	assert.Equal(t, second.ConvID, members[0].ConvID, "the sole member is the fresh scribe")
}

// Scenario: gating parity with the spawn path. An agent caller is refused
// unless it holds groups.spawn AND — because a summon applies birth-time grants
// — permissions.grant; the human always passes.
func TestScribeSummon_AgentGatedLikeSpawn(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)
	f.HaveGroup("callers")

	post := func(conv string) *httptest.ResponseRecorder {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	}

	// (a) An agent with NEITHER slug is refused outright.
	const bare = "bare-1111-2222-3333-4444"
	f.HaveMember("callers", bare)
	assert.Equalf(t, http.StatusForbidden, post(bare).Code,
		"an agent without groups.spawn is refused")

	// (b) An agent with groups.spawn but NOT permissions.grant is still refused —
	// a summon carries birth-time grants, so it needs the grant slug too.
	const spawnOnly = "spwn-1111-2222-3333-4444"
	f.HaveMember("callers", spawnOnly)
	require.NoError(t, db.GrantAgentPermission(spawnOnly, agentd.PermGroupsSpawn, "test"))
	assert.Equalf(t, http.StatusForbidden, post(spawnOnly).Code,
		"granting a scribe slugs needs permissions.grant")

	// (c) An agent holding both slugs is allowed — same bar the spawn path sets.
	const granter = "good-1111-2222-3333-4444"
	f.HaveMember("callers", granter)
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.GrantAgentPermission(granter, agentd.PermPermissionsGrant, "test"))
	rec := post(granter)
	require.Equalf(t, http.StatusOK, rec.Code, "authorised agent summon body=%s", rec.Body.String())
	assert.Equal(t, "grant",
		mustOverrides(t, decodeScribeResp(t, rec).ConvID)[agentd.PermTemplatesManage],
		"authorised agent's scribe carries the grant")
}

// Scenario: the JOH-369 fix. A summon spawns the scribe into the stable,
// shared, pre-trusted workdir (~/.tclaude/scribe) — NOT $HOME — so its
// detached pane can start unprompted; the CC folder-trust store is pre-seeded
// for that dir; and a reuse-if-alive click keeps the SAME cwd (the dir must
// never move under a cwd-bound CC conversation).
func TestScribeSummon_SpawnsInSharedTrustedWorkdir(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// Paths derived from the flow's temp HOME (World.New t.Setenv's HOME to it,
	// so summonScribe's os.UserHomeDir() resolves here — no real ~ is touched).
	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	summon := func() scribeSummonResp {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
			map[string]any{
				"name":  "circle-scribe",
				"slugs": []string{agentd.PermTemplatesManage},
				"brief": "Edit summoning circles.",
			})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec)
	}

	first := summon()
	require.False(t, first.Reused, "first summon spawns")

	// (1) The scribe spawned with cwd = the shared workdir — observed at the
	// SessionRow surface (what conv/session lookups walk), exactly how other
	// spawn flow tests observe the launch cwd.
	sessions, err := db.FindSessionsByConvID(first.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, wantCwd, sessions[0].Cwd, "scribe spawns in the shared ~/.tclaude/scribe workdir, not $HOME")

	// (2) The workdir was actually created on disk.
	fi, err := os.Stat(wantCwd)
	require.NoError(t, err, "the scribe workdir exists")
	assert.True(t, fi.IsDir(), "the scribe workdir is a directory")

	// (3) The CC folder-trust store was pre-seeded for that dir, so an
	// interactive CC start there won't raise the trust dialog.
	assert.True(t, claudeDirTrusted(t, claudeJSON, wantCwd),
		"~/.claude.json marks the scribe workdir hasTrustDialogAccepted=true")

	// (4) Reuse-if-alive keeps the SAME cwd — the dir is stable under the
	// cwd-bound CC conversation.
	second := summon()
	require.True(t, second.Reused, "second summon reuses the live scribe")
	require.Equal(t, first.ConvID, second.ConvID)
	sessions2, err := db.FindSessionsByConvID(second.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions2)
	assert.Equal(t, wantCwd, sessions2[0].Cwd, "reuse keeps the same stable workdir")
}

// Scenario: trust-seeding is best-effort. A malformed ~/.claude.json (which
// makes the CC trust editor refuse) must NOT fail the summon — the scribe
// still spawns (worst case it sees a one-time trust dialog), and the daemon
// leaves the broken config untouched rather than corrupting it further.
func TestScribeSummon_ProceedsWhenClaudeConfigMalformed(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")
	const garbage = "{ this is not valid json ][" // parse fails → editor refuses
	require.NoError(t, os.WriteFile(claudeJSON, []byte(garbage), 0o600))

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon must succeed despite a malformed CC config; body=%s", rec.Body.String())
	resp := decodeScribeResp(t, rec)
	require.NotEmpty(t, resp.ConvID, "the scribe still spawned")

	// The daemon did not touch the broken file (best-effort skip, not a rewrite).
	after, err := os.ReadFile(claudeJSON)
	require.NoError(t, err)
	assert.Equal(t, garbage, string(after), "a malformed CC config is left untouched, not corrupted further")
}

// claudeDirTrusted reports whether ~/.claude.json marks dir as
// hasTrustDialogAccepted=true — the surface Claude Code reads at startup.
func claudeDirTrusted(t *testing.T, claudeJSONPath, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(claudeJSONPath)
	require.NoErrorf(t, err, "read %s", claudeJSONPath)
	var root struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(data, &root))
	return root.Projects[dir].HasTrustDialogAccepted
}

// mustOverrides is a tiny read helper for the per-conv override map.
func mustOverrides(t *testing.T, conv string) map[string]string {
	t.Helper()
	m, err := db.ListAgentPermissionOverridesForConv(conv)
	require.NoError(t, err)
	return m
}

// Scenario: confused-deputy guard. A summon whose name collides with a real,
// non-scribe group must NOT resolve that group — it fails closed rather than
// re-granting/re-briefing a foreign agent or spawning a stray scribe into it.
func TestScribeSummon_RefusesForeignGroupCollision(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// A real working group + a live member that happens to share the scribe name.
	f.HaveGroup("circle-scribe")
	const foreigner = "frgn-1111-2222-3333-4444"
	f.HaveMember("circle-scribe", foreigner)

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	assert.Equalf(t, http.StatusConflict, rec.Code, "summon into a non-scribe group must 409; body=%s", rec.Body.String())

	// The foreign member was neither granted the scribe slug nor pulled in.
	overrides, err := db.ListAgentPermissionOverridesForConv(foreigner)
	require.NoError(t, err)
	assert.Empty(t, overrides[agentd.PermTemplatesManage], "the foreign agent was not granted templates.manage")
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "no stray scribe was spawned into the foreign group")
}

// Scenario: input validation at the boundary — an unknown slug, a missing
// slug set, a missing brief and a missing name each 400 without spawning.
func TestScribeSummon_ValidationRejections(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"unknown slug", map[string]any{"name": "circle-scribe", "slugs": []string{"not.a.real.slug"}, "brief": "x"}},
		{"no slugs", map[string]any{"name": "circle-scribe", "slugs": []string{}, "brief": "x"}},
		{"no brief", map[string]any{"name": "circle-scribe", "slugs": []string{agentd.PermTemplatesManage}}},
		{"no name", map[string]any{"slugs": []string{agentd.PermTemplatesManage}, "brief": "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", tc.body)))
			assert.Equalf(t, http.StatusBadRequest, rec.Code, "expected 400; body=%s", rec.Body.String())
		})
	}
}

// summonCircleScribe posts a standard human summon for the "circle-scribe"
// name and returns the decoded 200 response — the shared spawn used by the
// JOH-371 profile scenarios below.
func summonCircleScribe(t *testing.T, f *testharness.Flow) scribeSummonResp {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe",
		map[string]any{
			"name":  "circle-scribe",
			"slugs": []string{agentd.PermTemplatesManage},
			"brief": "Edit summoning circles.",
		})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	return decodeScribeResp(t, rec)
}

// Scenario (JOH-371): the operator pins config.scribe.profile to a saved spawn
// profile. A fresh summon adopts that profile's harness / model / effort — the
// sim spawner records them — and the profile is stamped as the scribe group's
// default (mechanism (a): the existing group-default-profile resolution then
// carries the launch shape with no new spawn logic).
func TestScribeSummon_AppliesConfiguredProfile(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// A saved spawn profile pinning a specific Claude model + effort.
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name:    "editor-prof",
		Harness: harness.DefaultName,
		Model:   "sonnet",
		Effort:  "high",
	})
	require.NoError(t, err)

	// The operator's Config-tab choice, on disk in the same config.json the CLI
	// reads (config.Save writes under the flow's temp HOME).
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "editor-prof"},
	}))

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID)
	require.False(t, resp.Reused, "first summon spawns fresh")

	// The sim spawner observed the profile's harness / model / effort.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, harness.DefaultName, sessions[0].Harness, "spawn adopted the profile's harness")

	model, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok, "the spawn was observed by the sim spawner")
	assert.Equal(t, "sonnet", model, "spawn adopted the profile's model")
	effort, ok := f.World.SpawnEffort(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "spawn adopted the profile's effort")

	// Mechanism (a): the profile was stamped as the scribe group's default, so
	// scribeSpawnHarness + applyDefaultProfile resolve from one source.
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "editor-prof", g.DefaultProfile, "config.scribe.profile stamped as the group default")
}

// Scenario (JOH-371 coupling to JOH-369): a Codex scribe profile makes the
// summon launch on Codex AND pre-seed the CODEX dir-trust store (not CC's) for
// the scribe workdir — the trust seed follows the harness the spawn actually
// resolves to, because both read the same stamped group default profile.
func TestScribeSummon_CodexProfileSeedsCodexTrust(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "codex-prof", Harness: harness.CodexName})
	require.NoError(t, err)
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "codex-prof"},
	}))

	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	codexTOML := filepath.Join(f.World.HomeDir, ".codex", "config.toml")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID)
	require.False(t, resp.Reused, "first summon spawns fresh")

	// The scribe launched on Codex — observed at the SessionRow harness column.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "the scribe has a session row")
	assert.Equal(t, harness.CodexName, sessions[0].Harness, "codex-profile scribe launched on codex")

	// The CODEX trust store was seeded for the scribe workdir — the harness the
	// spawn resolved to (the JOH-369 coupling this ticket must preserve).
	codexData, err := os.ReadFile(codexTOML)
	require.NoError(t, err, "the codex trust store was written")
	assert.Contains(t, string(codexData), `[projects."`+wantCwd+`"]`)
	assert.Contains(t, string(codexData), `trust_level = "trusted"`)

	// CC's trust store was NOT seeded — a codex scribe must never touch
	// ~/.claude.json (that would be the wrong-harness trust seed the ticket
	// explicitly warns against).
	_, err = os.Stat(claudeJSON)
	assert.Truef(t, os.IsNotExist(err), "codex-profile scribe must not seed the CC trust store (~/.claude.json), got err=%v", err)
}

// Scenario (JOH-371): a config.scribe.profile that names a since-deleted /
// renamed profile self-heals to the no-profile default — the scribe still
// summons, on the harness default (Claude), with CC dir-trust seeding — rather
// than wedging the summon. The dangling name is stamped as-is; resolution
// (groupDefaultProfile → nil) is what self-heals.
func TestScribeSummon_DeletedProfileSelfHeals(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	// Point at a profile that was never created (or was deleted after config
	// was written) — no db.CreateSpawnProfile for "gone-prof".
	require.NoError(t, config.Save(&config.Config{
		Scribe: &config.ScribeConfig{Profile: "gone-prof"},
	}))

	wantCwd := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	claudeJSON := filepath.Join(f.World.HomeDir, ".claude.json")

	resp := summonCircleScribe(t, f)
	require.NotEmpty(t, resp.ConvID, "the scribe still summoned despite the dangling profile")
	require.False(t, resp.Reused)

	// Self-healed to the harness default: Claude, with no pinned model/effort.
	sessions, err := db.FindSessionsByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	assert.Equal(t, harness.DefaultName, sessions[0].Harness, "dangling profile self-heals to the default harness")
	if model, ok := f.World.SpawnModel(resp.ConvID); ok {
		assert.Empty(t, model, "no model is pinned when the profile self-heals")
	}
	if effort, ok := f.World.SpawnEffort(resp.ConvID); ok {
		assert.Empty(t, effort, "no effort is pinned when the profile self-heals")
	}

	// CC dir-trust was seeded (the default-harness path), not skipped.
	assert.True(t, claudeDirTrusted(t, claudeJSON, wantCwd),
		"the default-harness path still pre-seeds the CC trust store")
}

// Scenario (JOH-371): the config change applies to the NEXT fresh summon only.
// A live scribe that gets reused keeps the launch shape it was born with —
// changing config.scribe.profile between summons does not re-stamp or relaunch
// the reused scribe.
func TestScribeSummon_ReuseIgnoresConfigChange(t *testing.T) {
	f := newFlow(t)
	stubScribeTerminal(t)

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prof-a", Harness: harness.DefaultName, Model: "sonnet", Effort: "high",
	})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "prof-b", Harness: harness.DefaultName, Model: "opus", Effort: "low",
	})
	require.NoError(t, err)

	// First summon under profile A → fresh spawn, born with A's shape.
	require.NoError(t, config.Save(&config.Config{Scribe: &config.ScribeConfig{Profile: "prof-a"}}))
	first := summonCircleScribe(t, f)
	require.False(t, first.Reused, "first summon spawns fresh")
	if model, ok := f.World.SpawnModel(first.ConvID); ok {
		assert.Equal(t, "sonnet", model, "the fresh scribe was born with profile A's model")
	}

	// Operator changes the config to profile B between summons.
	require.NoError(t, config.Save(&config.Config{Scribe: &config.ScribeConfig{Profile: "prof-b"}}))

	// Second summon reuses the live scribe — no relaunch.
	second := summonCircleScribe(t, f)
	require.True(t, second.Reused, "second summon reuses the live scribe")
	require.Equal(t, first.ConvID, second.ConvID, "reuse returns the same scribe conv")

	// The reused scribe kept its born launch shape (the sim never re-recorded a
	// new model — reuse does not respawn).
	if model, ok := f.World.SpawnModel(second.ConvID); ok {
		assert.Equal(t, "sonnet", model, "reuse keeps the born launch shape, not the changed config's profile B")
	}
	// And the group default was NOT re-stamped to B — the reuse path never
	// touches the profile stamp (config applies to the next FRESH summon).
	g, err := db.GetAgentGroupByName("circle-scribe")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "prof-a", g.DefaultProfile, "reuse did not re-stamp the group default to the changed config's profile")
}
