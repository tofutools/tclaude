package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestManagedRulesLaunchAndLibraryAuthority(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	put := func(group, name, text string) int {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, "POST", "/v1/instructions/rule", map[string]any{"group": group, "name": name, "content": []byte(text)})))
		return rec.Code
	}
	require.Equal(t, 200, put("alpha", "safety", "MANAGED RULE FIXTURE"))
	spawn := f.AsHuman().Spawn("alpha", "worker")
	snapshot, err := db.GetManagedInstructionLaunch(spawn.AgentID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	t.Cleanup(func() { _ = os.RemoveAll(snapshot.Root) })
	require.Contains(t, soleInboxMessage(t, spawn.ConvID).Body, "MANAGED RULE FIXTURE")
	require.NoDirExists(t, filepath.Join(os.Getenv("HOME"), ".claude", "skills"))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/instructions/rule", map[string]any{"group": "alpha", "name": "bad", "content": []byte("replace")}), spawn.ConvID))
	require.Equal(t, 403, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/instructions/rule?group=alpha", nil), spawn.ConvID))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	f.HaveGroup("private")
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/instructions/rule?group=private", nil), spawn.ConvID))
	require.Equal(t, 403, rec.Code)
}

func TestManagedRulesStayFrozenAcrossCompaction(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	group, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	_, err = db.PutManagedInstruction(group.ID, "rule", "safety", []byte("ORIGINAL MANAGED RULE"), false)
	require.NoError(t, err)
	spawn := f.AsHuman().Spawn("alpha", "worker")
	snapshot, err := db.GetManagedInstructionLaunch(spawn.AgentID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	t.Cleanup(func() { _ = os.RemoveAll(snapshot.Root) })
	_, err = db.PutManagedInstruction(group.ID, "rule", "safety", []byte("NEXT LAUNCH RULE"), true)
	require.NoError(t, err)
	require.Equal(t, 200, f.Compact(spawn.ConvID).Code)
	require.Eventually(t, func() bool { return len(reinjectedInbox(t, spawn.ConvID, spawn.AgentID)) == 1 }, 10*time.Second, 20*time.Millisecond)
	after := reinjectedInbox(t, spawn.ConvID, spawn.AgentID)
	require.Contains(t, after[0].Body, "ORIGINAL MANAGED RULE")
	require.NotContains(t, after[0].Body, "NEXT LAUNCH RULE")
}

func TestManagedRulesBriefingAllHarnesses(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode", "copilot", "gemini"} {
		t.Run(harness, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			_, err := db.PutManagedInstruction(0, "rule", "operator", []byte("CROSS HARNESS RULE"), false)
			require.NoError(t, err)
			spawned := f.AsHuman().SpawnHarness("alpha", "worker", harness)
			snapshot, err := db.GetManagedInstructionLaunch(spawned.AgentID)
			require.NoError(t, err)
			require.NotNil(t, snapshot)
			t.Cleanup(func() { _ = os.RemoveAll(snapshot.Root) })
			require.Contains(t, soleInboxMessage(t, spawned.ConvID).Body, "CROSS HARNESS RULE")
			rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+spawned.ConvID+"&format=paf", nil)
			require.NotEqual(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "managed instruction transfer support")
			require.DirExists(t, snapshot.Root)
		})
	}
}

func TestManagedRulesResumeRefreshAndRetireCleanup(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.PutManagedInstruction(0, "rule", "operator", []byte("ORIGINAL RULE"), false)
	require.NoError(t, err)
	spawned := f.AsHuman().Spawn("alpha", "worker")
	before, err := db.GetManagedInstructionLaunch(spawned.AgentID)
	require.NoError(t, err)
	require.NotNil(t, before)
	t.Cleanup(func() { _ = os.RemoveAll(before.Root) })
	_, err = db.PutManagedInstruction(0, "rule", "operator", []byte("UPDATED RULE"), true)
	require.NoError(t, err)
	f.MarkOffline(spawned.TmuxSession)
	resumed := f.AsHuman().Resume(spawned.ConvID)
	require.Equal(t, "resumed", resumed.Action, string(resumed.Raw))
	after, err := db.GetManagedInstructionLaunch(spawned.AgentID)
	require.NoError(t, err)
	require.NotNil(t, after)
	t.Cleanup(func() { _ = os.RemoveAll(after.Root) })
	require.Contains(t, after.Body, "UPDATED RULE")
	require.NotContains(t, after.Body, "ORIGINAL RULE")
	require.NoDirExists(t, before.Root)
	f.MarkOffline(spawned.TmuxSession)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/agent/"+spawned.ConvID+"/retire?shutdown=0&delete_worktree=0", nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoDirExists(t, after.Root)
	retired, err := db.GetManagedInstructionLaunch(spawned.AgentID)
	require.NoError(t, err)
	require.Nil(t, retired)
}
