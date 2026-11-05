package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A spawn that fails after its pre-fork enrollment but before the fork leaves
// nothing behind. The proved launch directory is swapped for a symlink between
// the proof check and executeSpawn, so the late re-assertion refuses it after
// the new member, its actor and its briefing were already written. The
// refusal must unwind every one of them; the pane never started, so any
// leftover would be a ghost the operator clears by hand.
func TestSpawn_PreForkFailureAfterEnrollmentLeavesNoGhost(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-pfk1-aaaa-bbbb-cccc-111111111111"
	haveSpawnCapableSandboxParent(t, f, "alpha", parent, harness.DefaultName, harness.ClaudeSandboxInherit)

	dir := filepath.Join(testutil.CanonicalTempDir(t), "launch")
	require.NoError(t, os.Mkdir(dir, 0o700))
	body := map[string]any{"name": "worker", "cwd": dir, "initial_message": "do the thing"}
	ch := decodeWriteProofChallenge(t, agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body))
	answerChallenge(t, ch)
	body["write_proof_token"] = ch.WriteProof.Token

	before := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	messagesBefore, err := db.CountAgentMessages()
	require.NoError(t, err)
	sessionsBefore, err := db.ListSessions()
	require.NoError(t, err)

	t.Cleanup(agentd.SetBeforeExecuteSpawnForTest(func() {
		moved := dir + ".real"
		require.NoError(t, os.Rename(dir, moved))
		require.NoError(t, os.Symlink(moved, dir))
	}))
	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	require.Equalf(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "write_proof_failed", got.Code)
	assert.Equal(t, "launch directory "+dir+" was replaced by a symlink to "+dir+".real after its write-proof "+
		"was verified; refusing to launch there", got.Error)

	after := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	require.Len(t, after.Groups, len(before.Groups))
	for i := range before.Groups {
		assert.Lenf(t, after.Groups[i].Members, len(before.Groups[i].Members),
			"group %q gains no member from the refused spawn", before.Groups[i].Name)
	}
	assert.Len(t, after.Agents, len(before.Agents), "the refused spawn strands no actor")
	assert.Len(t, after.Ungrouped, len(before.Ungrouped), "the refused spawn strands no ungrouped ghost")
	messagesAfter, err := db.CountAgentMessages()
	require.NoError(t, err)
	assert.Equal(t, messagesBefore, messagesAfter, "the refused spawn leaves no orphan briefing")
	sessionsAfter, err := db.ListSessions()
	require.NoError(t, err)
	assert.Len(t, sessionsAfter, len(sessionsBefore), "the refused spawn starts no session")
}
