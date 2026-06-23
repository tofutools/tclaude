package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// patchGroupDescr PATCHes /v1/groups/{name} with a descr field as the
// human — the exact request `tclaude agent groups set-descr` builds and
// the dashboard's 📝 click-to-edit chip sends. Returns the recorder so
// callers can assert the status / response body.
func patchGroupDescr(t *testing.T, f *testharness.Flow, group, descr string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/"+group,
		map[string]any{"descr": descr}))
	return testharness.Serve(f.Mux, r)
}

// dashGroupByName pulls one group out of the dashboard /api/snapshot
// response — the surface the dashboard's group header (and its 📝
// description chip) renders from.
func dashGroupByName(t *testing.T, name string) *dashGroup {
	t.Helper()
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	for i := range snap.Groups {
		if snap.Groups[i].Name == name {
			return &snap.Groups[i]
		}
	}
	t.Fatalf("group %q missing from /api/snapshot", name)
	return nil
}

// groupSummaryDescr lists groups via GET /v1/groups — the surface
// `tclaude agent groups ls` walks — and returns the named group's descr.
func groupSummaryDescr(t *testing.T, f *testharness.Flow, name string) string {
	t.Helper()
	rec := testharness.Serve(f.Mux, testharness.JSONRequest(t, http.MethodGet, "/v1/groups", nil))
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/groups body=%s", rec.Body.String())
	var groups []struct {
		Name  string `json:"name"`
		Descr string `json:"descr"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups), "decode /v1/groups")
	for _, g := range groups {
		if g.Name == name {
			return g.Descr
		}
	}
	t.Fatalf("group %q missing from GET /v1/groups", name)
	return ""
}

// Scenario: the human's exact problem — a group was created with a
// misspelled description and there was no way to fix it. set-descr
// (PATCH /v1/groups/{name} with {descr}) now rewrites it, and the
// corrected text surfaces everywhere a reader looks: the dashboard
// snapshot (the 📝 chip), GET /v1/groups (`groups ls`), and the group
// row itself.
func TestGroupDescr_EditFixesTypo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)

		// Create the group with a typo in its description — the
		// create-time path the dashboard's create modal / `groups create
		// --descr` ride.
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups",
			map[string]any{"name": "alpha", "descr": "Aplha squad — backedn work"}))
		require.Equal(t, http.StatusCreated, testharness.Serve(f.Mux, r).Code)

		// The typo is live on every read surface.
		assert.Equal(t, "Aplha squad — backedn work", groupSummaryDescr(t, f, "alpha"))
		assert.Equal(t, "Aplha squad — backedn work", dashGroupByName(t, "alpha").Descr)

		// Edit it — the fix the feature exists for.
		const fixed = "Alpha squad — backend work"
		rec := patchGroupDescr(t, f, "alpha", fixed)
		require.Equalf(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

		// The PATCH response echoes the stored description.
		var resp struct {
			Group string `json:"group"`
			Descr string `json:"descr"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "alpha", resp.Group)
		assert.Equal(t, fixed, resp.Descr)

		// Corrected text on the group row itself...
		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Equal(t, fixed, g.Descr, "group row descr")

		// ...on `groups ls`...
		assert.Equal(t, fixed, groupSummaryDescr(t, f, "alpha"), "GET /v1/groups descr")

		// ...and on the dashboard snapshot the 📝 chip renders from.
		assert.Equal(t, fixed, dashGroupByName(t, "alpha").Descr, "/api/snapshot descr")
	})
}

// Scenario: an empty descr clears the description. The CLI's omitted
// positional and the dashboard chip's empty input both send "" — a
// distinct, deliberate clear, not an omitted field.
func TestGroupDescr_EmptyClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		f := newFlow(t)
		f.HaveGroup("alpha")
		_, err := db.SetAgentGroupDescr("alpha", "some description")
		require.NoError(t, err)

		rec := patchGroupDescr(t, f, "alpha", "")
		require.Equalf(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Empty(t, g.Descr, "descr should be cleared")
		assert.Empty(t, dashGroupByName(t, "alpha").Descr, "cleared descr must surface as empty in /api/snapshot")
	})
}

// Scenario: a raw API caller sends a descr with embedded newlines —
// something neither the CLI positional nor the dashboard's
// <input type=text> can produce. The daemon folds them to spaces so a
// stray newline can never break the single-line dashboard header.
func TestGroupDescr_FoldsNewlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		rec := patchGroupDescr(t, f, "alpha", "  line one\r\nline two\nline three  ")
		require.Equalf(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Equal(t, "line one line two line three", g.Descr,
			"embedded newlines folded to spaces, surrounding whitespace trimmed")
	})
}

// Scenario: PATCH /v1/groups/{name} for a group that does not exist is
// a 404 — the dispatcher resolves {name} to a group row before the
// handler runs.
func TestGroupDescr_UnknownGroup404(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		rec := patchGroupDescr(t, f, "ghost", "anything")
		assert.Equalf(t, http.StatusNotFound, rec.Code,
			"PATCH on a missing group should 404; body=%s", rec.Body.String())
	})
}

// Scenario: a group created (POST /v1/groups) with an embedded newline
// in its description has that newline folded too — the one-line header
// invariant holds on the create path, not only on edit. Regression
// guard: the first cut folded newlines on update but passed the create
// descr through raw.
func TestGroupDescr_CreateFoldsNewlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups",
			map[string]any{"name": "alpha", "descr": "  line one\r\nline two\nline three  "}))
		require.Equalf(t, http.StatusCreated, testharness.Serve(f.Mux, r).Code,
			"create with a newline-laden descr should still succeed")

		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Equal(t, "line one line two line three", g.Descr,
			"create-path descr must be folded just like the update path")
	})
}

// Scenario: a non-owner agent cannot edit a group's description. The
// edit rides the existing groups.rename permission (default
// human-only); an agent peer that is neither the human nor a group
// owner is denied with 403, and the description is left untouched.
func TestGroupDescr_NonOwnerAgentDenied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")
		_, err := db.SetAgentGroupDescr("alpha", "original description")
		require.NoError(t, err)

		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/groups/alpha",
			map[string]any{"descr": "agent tried to change this"}),
			"rand-1111-2222-3333-444444444444")
		rec := testharness.Serve(f.Mux, r)
		assert.Equalf(t, http.StatusForbidden, rec.Code,
			"a non-owner agent must be denied; body=%s", rec.Body.String())

		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Equal(t, "original description", g.Descr,
			"a denied edit must not have touched the description")
	})
}
