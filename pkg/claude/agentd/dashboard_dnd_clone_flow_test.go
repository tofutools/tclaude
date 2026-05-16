package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the dashboard's Ctrl-drag handler clones a member of
// group A and drops the clone into group B. The JS issues:
//
//	POST /api/agents/{conv}/clone           # daemon-side fork
//	POST /api/groups/{B}/members            # add clone to drop target
//
// This flow test pins the daemon-side guarantee: after both calls,
// the new conv-id is a member of B and the original is untouched in
// A. Mirrors the shape of the existing v1 clone tests but routes
// through the dashboard cookie endpoint twin.
func TestDashboardDnDClone_PostsCloneThenAddsToTargetGroup(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const oldConv = "clon-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-clon", "tmux-clon", "/tmp/clon")
	srcGroup := f.HaveGroup("alpha")
	tgtGroup := f.HaveGroup("beta")
	_ = tgtGroup
	f.HaveMember("alpha", oldConv)

	mux := agentd.BuildDashboardHandlerForTest()

	// Step 1: clone via the dashboard endpoint (the Ctrl-drag
	// handler's first call). no_copy_conv: true skips the .jsonl
	// copy path, matching CloneFresh in the existing v1 tests.
	cloneBody := strings.NewReader(`{"no_copy_conv":true}`)
	cloneReq, _ := http.NewRequest(http.MethodPost,
		"/api/agents/"+oldConv+"/clone", cloneBody)
	cloneReq.Header.Set("Content-Type", "application/json")
	cloneRec := testharness.Serve(mux, cloneReq)
	require.Equal(t, http.StatusOK, cloneRec.Code,
		"POST /api/agents/{conv}/clone body=%s", cloneRec.Body.String())
	var cloneResp struct {
		OldConv string `json:"old_conv"`
		NewConv string `json:"new_conv"`
	}
	require.NoError(t, json.Unmarshal(cloneRec.Body.Bytes(), &cloneResp), "decode clone resp")
	require.NotEmpty(t, cloneResp.NewConv, "clone resp missing new_conv: body=%s", cloneRec.Body.String())
	assert.Equal(t, oldConv, cloneResp.OldConv, "clone resp old_conv")

	// Step 2: add the new conv to the drop target group (the second
	// call the Ctrl-drag handler issues).
	addBody := strings.NewReader(`{"conv":"` + cloneResp.NewConv + `"}`)
	addReq, _ := http.NewRequest(http.MethodPost,
		"/api/groups/beta/members", addBody)
	addReq.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, addReq)
	require.Equal(t, http.StatusOK, rec.Code,
		"POST /api/groups/beta/members body=%s", rec.Body.String())

	// Daemon-side state: original untouched in alpha; clone in BOTH
	// groups (alpha because it inherited the source's memberships,
	// beta because we just added it). Verify against the production
	// read path.
	srcMembers, _ := db.ListAgentGroupMembers(srcGroup.ID)
	{
		hasOld, hasNew := false, false
		for _, m := range srcMembers {
			if m.ConvID == oldConv {
				hasOld = true
			}
			if m.ConvID == cloneResp.NewConv {
				hasNew = true
			}
		}
		assert.True(t, hasOld, "alpha lost its original: %s missing from members", oldConv)
		assert.True(t, hasNew, "alpha lost the clone: %s missing from members", cloneResp.NewConv)
	}

	// Snapshot also surfaces both groups containing the clone.
	post := fetchDashSnapshot(t, mux)
	cloneAgent := findAgent(post.Agents, cloneResp.NewConv)
	require.NotNil(t, cloneAgent, "post-clone snapshot: clone %s missing from agents[]", cloneResp.NewConv)
	assert.True(t, containsString(cloneAgent.Groups, "alpha") && containsString(cloneAgent.Groups, "beta"),
		"clone groups = %v, want includes both alpha and beta", cloneAgent.Groups)
	// Original is untouched in alpha and not in beta.
	origAgent := findAgent(post.Agents, oldConv)
	require.NotNil(t, origAgent, "post-clone snapshot: original %s missing from agents[]", oldConv)
	assert.True(t, containsString(origAgent.Groups, "alpha") && !containsString(origAgent.Groups, "beta"),
		"original groups = %v, want [alpha] only (clone is ADD-only)", origAgent.Groups)
}

// Scenario: drop-on-source clone (Ctrl+drag onto the same group the
// row came from). The JS allows this — it's meaningful: forks a
// sibling that joins the same group alongside the original. This
// test pins the daemon-side state: a single clone POST, no add-step
// follow-up needed because the clone already inherited the source
// group; the alpha group ends up with original + clone.
//
// The dashboard endpoint is identical to the previous test's first
// call; we just don't fire the second add. Pins the
// "clone-onto-source-is-not-a-no-op" branch the JS depends on.
func TestDashboardDnDClone_OntoSourceGroupYieldsSibling(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const oldConv = "sib-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-sib", "tmux-sib", "/tmp/sib")
	srcGroup := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	mux := agentd.BuildDashboardHandlerForTest()

	cloneBody := strings.NewReader(`{"no_copy_conv":true}`)
	cloneReq, _ := http.NewRequest(http.MethodPost,
		"/api/agents/"+oldConv+"/clone", cloneBody)
	cloneReq.Header.Set("Content-Type", "application/json")
	cloneRec := testharness.Serve(mux, cloneReq)
	require.Equal(t, http.StatusOK, cloneRec.Code, "clone body=%s", cloneRec.Body.String())
	var resp struct {
		NewConv string `json:"new_conv"`
	}
	_ = json.Unmarshal(cloneRec.Body.Bytes(), &resp)
	require.NotEmpty(t, resp.NewConv, "missing new_conv: %s", cloneRec.Body.String())

	// Group alpha now has original + clone, no second add-step needed.
	members, _ := db.ListAgentGroupMembers(srcGroup.ID)
	require.Len(t, members, 2, "alpha members; want 2 (original + sibling clone); got %+v", members)
}
