package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// slugView mirrors the PermSlug JSON the dashboard + CLI consume, including
// the owner_implied flag this feature adds.
type slugView struct {
	Slug         string `json:"slug"`
	OwnerImplied bool   `json:"owner_implied"`
}

func ownerImpliedMap(slugs []slugView) map[string]bool {
	m := map[string]bool{}
	for _, s := range slugs {
		m[s.Slug] = s.OwnerImplied
	}
	return m
}

// Scenario: the owner-bypass slug set must be discoverable by BOTH dashboard
// surfaces that drive the permission editor — the /v1/permissions/slugs
// registry endpoint (the CLI's `permissions slugs` source) and the
// /api/snapshot Slugs array (what the editor modal reads to mark rows). A
// regression here is exactly the reported bug: an owner's extra permissions
// are invisible because nothing tells the UI which slugs ownership confers.
func TestPermOwnerVisible_SlugsExposeOwnerImplied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The dashboard test handler only injects the Origin header (needed by
		// checkDashboardAuth) when popupBaseURL is set — mirror the other
		// dashboard flow tests.
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

		f := newFlow(t)

		// 1) The registry endpoint (CLI `permissions slugs`).
		rec := testharness.Serve(f.Mux,
			testharness.JSONRequest(t, http.MethodGet, "/v1/permissions/slugs", nil))
		require.Equal(t, http.StatusOK, rec.Code, "/v1/permissions/slugs body=%s", rec.Body.String())
		var regSlugs []slugView
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &regSlugs), "decode slugs")
		assertOwnerImpliedShape(t, ownerImpliedMap(regSlugs))

		// 2) The dashboard snapshot (the editor modal's source).
		mux := agentd.BuildDashboardHandlerForTest()
		srec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, srec.Code, "/api/snapshot body=%s", srec.Body.String())
		var snap struct {
			Slugs []slugView `json:"slugs"`
		}
		require.NoError(t, json.Unmarshal(srec.Body.Bytes(), &snap), "decode snapshot")
		assertOwnerImpliedShape(t, ownerImpliedMap(snap.Slugs))
	})
}

// assertOwnerImpliedShape spot-checks representative slugs on both sides of
// the owner-bypass line. Not the full set (that's pinned white-box by
// TestPermissionRegistry_OwnerImpliedSet) — here we only prove the flag
// survives JSON serialization on each surface.
func assertOwnerImpliedShape(t *testing.T, m map[string]bool) {
	t.Helper()
	for _, owner := range []string{
		agentd.PermGroupsSpawn, agentd.PermGroupsRetire, agentd.PermAgentReincarnate,
		agentd.PermHumanNotify, agentd.PermGroupsLinkAdd,
	} {
		assert.Truef(t, m[owner], "slug %q must be marked owner_implied", owner)
	}
	for _, plain := range []string{
		agentd.PermGroupsCreate, agentd.PermPermissionsGrant, agentd.PermMemberAdd,
		agentd.PermSelfRename,
	} {
		assert.Falsef(t, m[plain], "slug %q must NOT be marked owner_implied", plain)
	}
}
