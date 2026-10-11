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
// group A and drops the clone into group B. The JS issues ONE call:
//
//	POST /api/agents/{conv}/clone {"group":"B"}
//
// so the clone and its drop-target membership land together. This flow
// test pins the daemon-side guarantee: after the call, the new conv-id is
// a member of B (and of A, inherited) and the original is untouched in A.
func TestDashboardDnDClone_JoinsTargetGroupInOneRequest(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const oldConv = "clon-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-clon", "tmux-clon", f.TestCwd("clon"))
	srcGroup := f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", oldConv)

	mux := agentd.BuildDashboardHandlerForTest()

	// no_copy_conv: true skips the history copy, matching CloneFresh in the
	// existing v1 tests.
	cloneBody := strings.NewReader(`{"no_copy_conv":true,"group":"beta"}`)
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

	// Daemon-side state: original untouched in alpha; clone in BOTH
	// groups (alpha because it inherited the source's memberships,
	// beta because the request named it). Verify against the production
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
// The JS sends the drop group here too; naming a group the source is
// already in must not duplicate the membership. Pins the
// "clone-onto-source-is-not-a-no-op" branch the JS depends on.
func TestDashboardDnDClone_OntoSourceGroupYieldsSibling(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const oldConv = "sib-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-sib", "tmux-sib", f.TestCwd("sib"))
	srcGroup := f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	mux := agentd.BuildDashboardHandlerForTest()

	cloneBody := strings.NewReader(`{"no_copy_conv":true,"group":"alpha"}`)
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

	// Group alpha now has original + clone, once each.
	members, _ := db.ListAgentGroupMembers(srcGroup.ID)
	require.Len(t, members, 2, "alpha members; want 2 (original + sibling clone); got %+v", members)
}
