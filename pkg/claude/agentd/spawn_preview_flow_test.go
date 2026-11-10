package agentd_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// tcl-ezvrww: the spawn preview resolves a spawn request through the spawn
// handler itself and answers with each field's value and source, without
// spawning anything.

func spawnPreview(t *testing.T, h http.Handler, group string, body map[string]any) (int, agent.ResolvedLaunch, string) {
	t.Helper()
	req := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/spawn-preview", body)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	rec := testharness.Serve(h, agentd.AsHumanPeer(req))
	var out struct {
		Resolved agent.ResolvedLaunch `json:"resolved"`
	}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out.Resolved, rec.Body.String()
}

func TestSpawnPreview_ResolvesWithoutSpawning(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	on := true
	_, err := db.SetAgentGroupRemoteControl("crew", &on)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"auto_memory": true, "model": "opus",
	})).Code)
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "worker"})
	require.Equal(t, http.StatusOK, code, raw)
	require.NotNil(t, got.RemoteControl)
	assert.Equal(t, agent.ResolvedField{Value: "true", Source: "group remote-control policy"}, *got.RemoteControl)
	require.NotNil(t, got.AutoMemory)
	assert.Equal(t, "true", got.AutoMemory.Value)
	assert.Contains(t, got.AutoMemory.Source, "worker")
	require.NotNil(t, got.PeerMessaging)
	assert.Equal(t, agent.ResolvedField{Value: "false", Source: agent.ProvHarnessDefault}, *got.PeerMessaging)
	assert.Equal(t, "opus", got.Model.Value)
	assert.Equal(t, "claude", got.Harness.Value)

	code, got, raw = spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "worker",
		"remote_control": false, "peer_messaging": true})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, agent.ResolvedField{Value: "false", Source: agent.ProvExplicit}, *got.RemoteControl)
	assert.Equal(t, agent.ResolvedField{Value: "true", Source: agent.ProvExplicit}, *got.PeerMessaging)

	code, _, raw = spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "nope"})
	assert.Equal(t, http.StatusBadRequest, code, "the preview refuses what the spawn would: %s", raw)

	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "a preview spawns nothing")
}

// dbFingerprint is every table's full contents, hashed per table, so any
// insert, update or delete shows as a changed table.
func dbFingerprint(t *testing.T) map[string]string {
	t.Helper()
	d, err := db.Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Close())
	out := map[string]string{}
	for _, table := range tables {
		r, err := d.Query(fmt.Sprintf(`SELECT * FROM "%s"`, table))
		require.NoError(t, err)
		cols, err := r.Columns()
		require.NoError(t, err)
		h := sha256.New()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, r.Scan(ptrs...))
			_, _ = fmt.Fprintf(h, "%v\n", vals)
		}
		require.NoError(t, r.Close())
		out[table] = fmt.Sprintf("%x", h.Sum(nil))
	}
	return out
}

// Guard for the preview's cut-off: a spawn preview writes nothing. Every
// table (agents, sessions, audit log, spawn queue, rate limits, ...) is the
// same afterwards, no pane was started and the launch directory is
// untouched. A side effect moved above the cut-off in handleGroupSpawn
// fails this test.
func TestSpawnPreview_HasNoSideEffects(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	on := true
	_, err := db.SetAgentGroupRemoteControl("crew", &on)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"auto_memory": true, "model": "opus", "startup_context": "Be brief.",
	})).Code)
	cwd := testutil.CanonicalTempDir(t)
	h := agentd.BuildDashboardHandlerForTest()
	sessionsBefore, err := db.ListSessions()
	require.NoError(t, err)
	before := dbFingerprint(t)

	for _, body := range []map[string]any{
		{"name": "w", "profile": "worker", "cwd": cwd, "initial_message": "hi", "task_url": "https://example.com/t/1"},
		{"name": "w", "profile": "worker", "cwd": cwd, "harness": "codex", "remote_control": false},
		{"name": "w", "profile": "nope", "cwd": cwd},
	} {
		code, _, raw := spawnPreview(t, h, "crew", body)
		require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, code, raw)
	}

	assert.Equal(t, before, dbFingerprint(t), "a preview changes no table")
	sessionsAfter, err := db.ListSessions()
	require.NoError(t, err)
	assert.Equal(t, len(sessionsBefore), len(sessionsAfter), "a preview starts no session")
	entries, err := os.ReadDir(cwd)
	require.NoError(t, err)
	assert.Empty(t, entries, "a preview writes nothing into the launch directory")
}

// The preview reveals what a spawn would resolve (profiles, policy), so only
// a caller who could spawn here reaches it: a restricted peer, even one
// granted to request spawns, is refused (it is not served as local), and the
// spawn handler's own permission gate runs for everyone else.
func TestSpawnPreview_RestrictedPeerIsRefused(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("crew")
	setFedTrustLevel(t, fh, db.FederationTrustRestricted)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost, "/api/groups/crew/spawn-preview?as=local", map[string]any{"name": "w"}))
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"resolved"`)
}

// The spawn preview's relaunch rows are the dashboard's live relaunch read for
// the same inputs: one walk (resolveSpawnRelaunchRows) over the spawn's own
// tiers serves both, with a stable kind beside the readable source.
func TestSpawnPreview_RelaunchRowsMatchTheLiveRead(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "pinned-env", "relaunch_policy": map[string]string{"group_env": "reuse"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "crew-default", "relaunch_policy": map[string]string{"harness_config": "chain-wins"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_, err := db.SetAgentGroupDefaultProfile("crew", "crew-default")
	require.NoError(t, err)
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "pinned-env",
		"relaunch_policy": map[string]string{"sandbox_profile_contents": "reuse"}})
	require.Equal(t, http.StatusOK, code, raw)
	live := previewRelaunch(t, f, "group=crew&profile=pinned-env&explicit=sandbox_profile_contents=reuse")
	require.NotEmpty(t, got.RelaunchPolicy)
	require.Len(t, got.RelaunchPolicy, len(live))
	for key, row := range got.RelaunchPolicy {
		assert.Equal(t, map[string]string{"value": row.Value, "source": row.Source, "kind": row.Kind}, live[key], key)
	}
	assert.Equal(t, agent.ResolvedField{Value: "reuse", Source: "this spawn", Kind: "explicit"}, got.RelaunchPolicy["sandbox_profile_contents"])
	assert.Equal(t, "profile", got.RelaunchPolicy["group_env"].Kind)
	assert.Equal(t, "profile crew-default", got.RelaunchPolicy["harness_config"].Source)
}
