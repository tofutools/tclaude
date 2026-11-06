package agentd_test

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// stage stages a download on this node as the peer and answers the staging
// job's own reply.
func (h *peerProcessHarness) stage(t *testing.T, path string) (int, string) {
	t.Helper()
	return h.awaitJob(t, "POST", "/api/peer-downloads", map[string]any{"path": path})
}

// finishUpload finishes a staged upload on path and answers the route's own
// reply.
func (h *peerProcessHarness) finishUpload(t *testing.T, id, path, contentType string) (int, string) {
	t.Helper()
	return h.awaitJob(t, "POST", "/api/peer-uploads/finish", map[string]any{"id": id, "path": path, "content_type": contentType})
}

// awaitJob sends a peer view job request and answers the job's own reply.
func (h *peerProcessHarness) awaitJob(t *testing.T, method, path string, payload any) (int, string) {
	t.Helper()
	code, body := h.call(method, path, payload)
	if code != 202 {
		return code, body
	}
	var started struct {
		Job string `json:"peer_job"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &started))
	var state struct {
		Status string `json:"status"`
		Result *struct {
			Status int    `json:"status"`
			Body   []byte `json:"body"`
		} `json:"result"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body = h.call("GET", "/api/peer-view-jobs/"+started.Job, nil)
		require.Equal(t, 200, code, body)
		require.NoError(t, json.Unmarshal([]byte(body), &state))
		if state.Status == "done" {
			return state.Result.Status, string(state.Result.Body)
		}
		require.True(t, time.Now().Before(deadline), "the peer view job never finished")
		time.Sleep(20 * time.Millisecond)
	}
}

func stagedID(t *testing.T, body string) string {
	t.Helper()
	var staged struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &staged))
	require.NotEmpty(t, staged.ID, body)
	require.Positive(t, staged.Size, body)
	return staged.ID
}

// A restricted peer downloads a group's archive under groups.export and
// sessions.files.read there; the grants are held again at every chunk read.
func TestPeerGroupExportStagesUnderItsGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	export := "/api/groups/crew/export"

	code, body := h.stage(t, export)
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsExport, "group=crew")
	code, body = h.stage(t, export)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermSessionsFilesRead)
	h.grant(t, agentd.PermSessionsFilesRead, "group=crew")
	code, body = h.stage(t, export+"?allow_flagged=true")
	require.Equal(t, 403, code, "a flagged export is the operator's: %s", body)
	require.Contains(t, body, "allow_flagged")
	code, body = h.stage(t, export+"?carry_workflow=x")
	require.Equal(t, 403, code, "carried workflows take unrestricted trust: %s", body)

	code, body = h.call("GET", export, nil)
	require.Equal(t, 403, code, "only the staged path serves a restricted peer: %s", body)
	require.Contains(t, body, "staged_only")
	code, body = h.stage(t, export)
	require.Equal(t, 200, code, body)
	id := stagedID(t, body)
	code, body = h.call("GET", "/api/peer-downloads/"+url.PathEscape(id)+"?offset=0&length=16", nil)
	require.Equal(t, 200, code, body)

	// Revoked after staging, the grant stops the download.
	h.revoke(t, agentd.PermSessionsFilesRead, "group=crew")
	code, body = h.call("GET", "/api/peer-downloads/"+url.PathEscape(id)+"?offset=16", nil)
	require.Equal(t, 403, code, body)
	code, body = h.call("GET", "/api/peer-downloads/"+url.PathEscape(id)+"?offset=16", nil)
	require.Equal(t, 404, code, "the staged file is gone: %s", body)
	h.grant(t, agentd.PermSessionsFilesRead, "group=crew")

	// An environment's values are withheld from the peer's view of the
	// group, so the archive that carries them takes their grant.
	_, err := db.SetAgentGroupEnvironment("crew", []sandboxpolicy.EnvironmentEntry{{Name: "TOKEN", Value: "secret"}})
	require.NoError(t, err)
	code, body = h.stage(t, export)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsSettingsEnvironment)
	h.grant(t, agentd.PermGroupsSettingsEnvironment, "group=crew")
	code, body = h.stage(t, export)
	require.Equal(t, 200, code, body)
}

// The other staged transfers stay unrestricted only, whatever the peer holds.
func TestPeerStagedTransfersOtherwiseStayUnrestricted(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.grant(t, agentd.PermGroupsExport, "")
	h.grant(t, agentd.PermSessionsFilesRead, "")
	h.grant(t, agentd.PermNodeSpawnRead, "")
	for _, path := range []string{"/api/snapshots/x/export", "/api/templates/x/export", "/api/logs/export"} {
		code, body := h.stage(t, path)
		require.Equal(t, 403, code, "%s: %s", path, body)
	}
	code, body := h.stage(t, "/api/groups")
	require.Equal(t, 403, code, "not a download: %s", body)
	// A restricted peer stages an upload, but spawn attachments refuse it.
	code, body = h.call("POST", "/api/peer-uploads", nil)
	require.Equal(t, 200, code, body)
	var opened struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &opened))
	code, body = h.finishUpload(t, opened.ID, "/api/spawn-attachments", "")
	require.Equal(t, 403, code, "spawn attachments: %s", body)
	code, body = h.call("GET", "/api/peer-downloads/pdl_000000000000000000000000?offset=0", nil)
	require.Equal(t, 404, code, body)
}

// A member agent's archive takes agent.bundle.export in every group it is
// in, and its transcript sessions.files.read too.
func TestPeerAgentBundleExportStagesUnderItsGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	export := "/api/agent-bundle/export?agent=" + url.QueryEscape(h.member)

	code, body := h.stage(t, export)
	require.Equal(t, 404, code, "the agent is not visible: %s", body)
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = h.stage(t, export)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermAgentBundleExport)
	h.grant(t, agentd.PermAgentBundleExport, "group=crew")
	code, body = h.stage(t, export+"&history=true")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermSessionsFilesRead)
	code, body = h.stage(t, export+"&summary=sum_1")
	require.Equal(t, 403, code, body)
	code, body = h.stage(t, export)
	require.NotEqual(t, 403, code, body)
	require.NotEqual(t, 404, code, body)

	// The archive carries the agent's launch environment, whose values the
	// peer view withholds without their grants.
	require.NoError(t, db.SetAgentInitialSpawnConfig(h.member, `{"environment":[{"name":"A","value":"b"}]}`))
	code, body = h.stage(t, export)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsSettingsEnvironment)
	h.grant(t, agentd.PermGroupsSettingsEnvironment, "group=crew")
	code, body = h.stage(t, export)
	require.NotEqual(t, 403, code, body)
	rec := fedHuman(t, h.f, "POST", "/v1/spawn-profiles", map[string]any{"name": "envd", "environment": []any{map[string]any{"name": "K", "value": "v"}}})
	require.Less(t, rec.Code, 300, rec.Body.String())
	require.NoError(t, db.SetAgentInitialSpawnConfig(h.member, `{"profile":"envd"}`))
	code, body = h.stage(t, export)
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermNodeSpawnRead)
}
