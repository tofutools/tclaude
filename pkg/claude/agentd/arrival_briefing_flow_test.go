package agentd_test

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func arrivalInbox(t *testing.T, conv string) string {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	var result []string
	for _, m := range msgs {
		result = append(result, m.Body)
	}
	return strings.Join(result, "\n")
}
func TestAgentBundleArrivalBriefing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "arrival-branch")
	git("config", "user.name", "fixture")
	git("config", "user.email", "fixture@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "tracked"), []byte("before"), 0600))
	git("add", "tracked")
	git("commit", "-m", "fixture")
	commit := git("rev-parse", "HEAD")
	git("remote", "add", "origin", "https://example.invalid/repo.git")
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "tracked"), []byte("changed"), 0600))
	dirty := true
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{
		Name: "traveller", Harness: "claude", Profile: json.RawMessage(`{"sandbox_implementation":"off"}`),
		InitialMessage: "Keep doing the task.", Paths: agentbundle.Paths{Cwd: cwd, Worktree: "/origin/worktree", Branch: "arrival-branch"},
		Origin: &agentbundle.Origin{Instance: "source node", Trigger: "operator\nFAKE-INSTRUCTION", Commit: commit, Dirty: &dirty},
	}}}
	rec := postAgentBundle(t, f, b, "apply=true&group=receiver&cwd="+url.QueryEscape(cwd), "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Spawn agent.SpawnResponse `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotEmpty(t, out.Spawn.ConvID)
	brief := arrivalInbox(t, out.Spawn.ConvID)
	for _, fact := range []string{"Arrival briefing — bundle import", out.Spawn.AgentID, cwd, "receiver", "explicit import path", "exists here: yes", "arrival-branch", "dirty", "present: yes", commit, "uncommitted changes reported", "source permissions not copied", "credentials, sidecars or mail", "Keep doing the task.", `operator\nFAKE-INSTRUCTION`} {
		assert.Contains(t, brief, fact)
	}
	assert.NotContains(t, brief, "operator\nFAKE-INSTRUCTION")
	snap, err := db.GetAgentStartupSnapshot(out.Spawn.AgentID)
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Greater(t, snap.BriefMessageID, int64(0))
}
func TestCloneArrivalBriefingWithoutFollowup(t *testing.T) {
	f := newFlow(t)
	const source = "c1f1aaaa-bbbb-cccc-dddd-eeeeffff0001"
	f.HaveConvWithTitle(source, "traveller")
	f.HaveEnrolledAgent(source)
	f.HaveAliveSession(source, "arrival-source", "arrival-pane", f.TestCwd("project"))
	f.HaveGroup("project")
	f.HaveMember("project", source)
	c := f.AsHuman().CloneWith(source, map[string]any{"no_copy_conv": true})
	require.Equal(t, 200, c.Code, c.Raw)
	brief := arrivalInbox(t, c.NewConv)
	for _, fact := range []string{"Arrival briefing — local clone", "local permissions inherited", "independent inbox", "project", "inherited source cwd", "operator"} {
		assert.Contains(t, brief, fact)
	}
	id, err := db.AgentIDForConv(c.NewConv)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	assert.Contains(t, brief, id)
	assertNoSendKeysTo(t, f, c.TmuxTarget())
}
