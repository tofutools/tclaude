package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
// the new conv-id is a member of B (with the clone-alias suffix
// scheme intact) and the original is untouched in A. Mirrors the
// shape of the existing v1 clone tests but routes through the
// dashboard cookie endpoint twin.
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
	f.HaveMember("alpha", oldConv, "worker")

	mux := agentd.BuildDashboardHandlerForTest()

	// Step 1: clone via the dashboard endpoint (the Ctrl-drag
	// handler's first call). no_copy_conv: true skips the .jsonl
	// copy path, matching CloneFresh in the existing v1 tests.
	cloneBody := strings.NewReader(`{"no_copy_conv":true}`)
	cloneReq, _ := http.NewRequest(http.MethodPost,
		"/api/agents/"+oldConv+"/clone", cloneBody)
	cloneReq.Header.Set("Content-Type", "application/json")
	cloneRec := testharness.Serve(mux, cloneReq)
	if cloneRec.Code != http.StatusOK {
		t.Fatalf("POST /api/agents/{conv}/clone status=%d body=%s",
			cloneRec.Code, cloneRec.Body.String())
	}
	var cloneResp struct {
		OldConv string `json:"old_conv"`
		NewConv string `json:"new_conv"`
	}
	if err := json.Unmarshal(cloneRec.Body.Bytes(), &cloneResp); err != nil {
		t.Fatalf("decode clone resp: %v", err)
	}
	if cloneResp.NewConv == "" {
		t.Fatalf("clone resp missing new_conv: body=%s", cloneRec.Body.String())
	}
	if cloneResp.OldConv != oldConv {
		t.Errorf("clone resp old_conv = %q, want %q", cloneResp.OldConv, oldConv)
	}

	// Step 2: add the new conv to the drop target group (the second
	// call the Ctrl-drag handler issues).
	addBody := strings.NewReader(`{"conv":"` + cloneResp.NewConv + `"}`)
	addReq, _ := http.NewRequest(http.MethodPost,
		"/api/groups/beta/members", addBody)
	addReq.Header.Set("Content-Type", "application/json")
	if rec := testharness.Serve(mux, addReq); rec.Code != http.StatusOK {
		t.Fatalf("POST /api/groups/beta/members status=%d body=%s",
			rec.Code, rec.Body.String())
	}

	// Daemon-side state: original untouched in alpha; clone in BOTH
	// groups (alpha because it inherited the source's memberships,
	// beta because we just added it). Verify against the production
	// read path.
	srcMembers, _ := db.ListAgentGroupMembers(srcGroup.ID)
	{
		hasOld, hasNew := false, false
		var newAlias string
		for _, m := range srcMembers {
			if m.ConvID == oldConv {
				hasOld = true
			}
			if m.ConvID == cloneResp.NewConv {
				hasNew = true
				newAlias = m.Alias
			}
		}
		if !hasOld {
			t.Errorf("alpha lost its original: %s missing from members", oldConv)
		}
		if !hasNew {
			t.Errorf("alpha lost the clone: %s missing from members", cloneResp.NewConv)
		}
		if newAlias != "worker-c-1" {
			t.Errorf("clone alias in alpha = %q, want %q (the inherited per-group alias from runCloneOrchestration)",
				newAlias, "worker-c-1")
		}
	}

	// Snapshot also surfaces both groups containing the clone.
	post := fetchDashSnapshot(t, mux)
	cloneAgent := findAgent(post.Agents, cloneResp.NewConv)
	if cloneAgent == nil {
		t.Fatalf("post-clone snapshot: clone %s missing from agents[]", cloneResp.NewConv)
	}
	if !containsString(cloneAgent.Groups, "alpha") || !containsString(cloneAgent.Groups, "beta") {
		t.Errorf("clone groups = %v, want includes both alpha and beta", cloneAgent.Groups)
	}
	// Original is untouched in alpha and not in beta.
	origAgent := findAgent(post.Agents, oldConv)
	if origAgent == nil {
		t.Fatalf("post-clone snapshot: original %s missing from agents[]", oldConv)
	}
	if !containsString(origAgent.Groups, "alpha") || containsString(origAgent.Groups, "beta") {
		t.Errorf("original groups = %v, want [alpha] only (clone is ADD-only)", origAgent.Groups)
	}
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
	f.HaveMember("alpha", oldConv, "worker")

	mux := agentd.BuildDashboardHandlerForTest()

	cloneBody := strings.NewReader(`{"no_copy_conv":true}`)
	cloneReq, _ := http.NewRequest(http.MethodPost,
		"/api/agents/"+oldConv+"/clone", cloneBody)
	cloneReq.Header.Set("Content-Type", "application/json")
	cloneRec := testharness.Serve(mux, cloneReq)
	if cloneRec.Code != http.StatusOK {
		t.Fatalf("clone status=%d body=%s", cloneRec.Code, cloneRec.Body.String())
	}
	var resp struct{ NewConv string `json:"new_conv"` }
	_ = json.Unmarshal(cloneRec.Body.Bytes(), &resp)
	if resp.NewConv == "" {
		t.Fatalf("missing new_conv: %s", cloneRec.Body.String())
	}

	// Group alpha now has original + clone, no second add-step needed.
	members, _ := db.ListAgentGroupMembers(srcGroup.ID)
	if len(members) != 2 {
		t.Fatalf("alpha members = %d, want 2 (original + sibling clone); got %+v",
			len(members), members)
	}
}
