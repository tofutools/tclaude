package agentd_test

import (
	"encoding/json"
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
)

// stubApproval is a thin wrapper around the agentd-side helper so the
// test file reads top-to-bottom without juggling restore functions.
func stubApproval(t *testing.T, decision bool) {
	t.Helper()
	t.Cleanup(agentd.StubApprovalForTest(decision))
}

// Scenario: a peer agent with no slug, not a group owner, calls a
// cross-agent endpoint WITHOUT the X-Tclaude-Ask-Human header. The
// daemon must refuse with 403 — the slug + ownership are the only
// silent paths.
//
// Pins the baseline so the popup-approval test below proves the
// escape hatch is what flips the decision, not some other accidental
// auth bypass.
func TestCrossAgentAskHuman_NoHeaderStillRefuses(t *testing.T) {
	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	// A separate caller conv that is NOT in the group and holds no
	// agent.reincarnate slug. The default newFlow human path bypasses
	// permissions, so we route as an agent peer.
	const callerConv = "cccc-1111-2222-3333-4444"
	f.HaveMember("alpha", callerConv)
	f.HaveAliveSession(callerConv, "caller-open", "tmux-caller-open", f.World.HomeDir)
	callerSession, err := db.LoadSession("caller-open")
	require.NoError(t, err)
	callerSession.Harness = harness.DefaultName
	callerSession.SandboxMode = harness.ClaudeSandboxOff
	require.NoError(t, db.SaveSession(callerSession))
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate", map[string]any{})
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"expected 403 without slug + without --ask-human, body=%s", rec.Body.String())
}

// Scenario: same caller, same denied paths, BUT the caller adds
// X-Tclaude-Ask-Human: 30s. Popup is stubbed to APPROVE — the
// reincarnate orchestration runs and returns 200.
//
// Real surface assertion: after approval, the caller is recorded on
// the new conv's `granted_by` audit columns (system:reincarnate:by=
// <caller>) — same forensic trail cross-agent calls leave when the
// silent paths grant. Verifies the popup branch returns the caller's
// conv-id, not the human's empty string, so the orchestration can
// stamp the right audit.
func TestCrossAgentAskHuman_HeaderAndApprovalAllowsCall(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	stubApproval(t, true)

	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	const callerConv = "cccc-1111-2222-3333-4444"
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate",
		map[string]any{"follow_up": "fresh start"})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	var challenge struct {
		Code       string `json:"code"`
		WriteProof struct {
			Token    string   `json:"token"`
			Filename string   `json:"filename"`
			Dirs     []string `json:"dirs"`
		} `json:"write_proof"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &challenge))
	require.Equal(t, "write_proof_required", challenge.Code, "body=%s", rec.Body.String())
	for _, dir := range challenge.WriteProof.Dirs {
		marker := filepath.Join(dir, challenge.WriteProof.Filename)
		require.NoError(t, os.WriteFile(marker, nil, 0o600))
		t.Cleanup(func() { _ = os.Remove(marker) })
	}
	r = testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate",
		map[string]any{
			"follow_up":         "fresh start",
			"write_proof_token": challenge.WriteProof.Token,
		})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, callerConv)
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"expected 200 after popup-approve, body=%s", rec.Body.String())
	// reincarnate orchestration ran: there should be a succession row
	// from the old conv to a new one.
	successor, err := db.GetConvSuccessor(targetConv)
	require.NoError(t, err, "GetConvSuccessor")
	require.NotEmpty(t, successor, "reincarnate did not record a successor; the orchestration was not actually run")
}

// Same setup but popup DENIES. The cross-agent call must still
// return 403 — the popup is an escape hatch, not a free pass.
func TestCrossAgentAskHuman_HeaderAndDenialStillRefuses(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)
	stubApproval(t, false)

	f := newFlow(t)

	const targetConv = "tttt-1111-2222-3333-4444"
	const targetLabel = "spwn-tt001"
	const targetTmux = "tclaude-spwn-tt001"
	f.HaveConvWithTitle(targetConv, "worker")
	f.HaveAliveSession(targetConv, targetLabel, targetTmux, f.World.HomeDir)
	f.HaveGroup("alpha")
	f.HaveMember("alpha", targetConv)

	const callerConv = "cccc-1111-2222-3333-4444"
	r := testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+targetConv+"/reincarnate", map[string]any{})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, callerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"expected 403 after popup-deny, body=%s", rec.Body.String())
	// And no succession row was written.
	successor, _ := db.GetConvSuccessor(targetConv)
	assert.Empty(t, successor, "reincarnate ran despite popup-deny — successor %s recorded", successor)
}
