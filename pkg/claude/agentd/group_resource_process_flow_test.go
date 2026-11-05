package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A group's members read its runs and the templates pinned to it, and save
// those templates, with no process.* slug. Run mutations stay on
// process.runs.manage, and template metadata on process.templates.manage.
func TestGroupProcessesAreMembersToReadAndEdit(t *testing.T) {
	f := scopedRunFlow(t)
	team := f.HaveGroup("team")
	f.HaveGroup("other")
	const (
		member   = "grp-proc-member-1111"
		outsider = "grp-proc-outsider-2222"
	)
	f.HaveConvWithTitle(member, "member")
	f.HaveConvWithTitle(outsider, "outsider")
	f.HaveMember("team", member)
	for _, conv := range []string{member, outsider} {
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
	}
	human := func(method, path string, body any) *httptest.ResponseRecorder {
		return processRuntimeRequest(t, f, method, path, body)
	}
	agent := func(conv, method, path string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), conv))
	}
	require.Equal(t, http.StatusOK, human(http.MethodPost, "/v1/process/groups/team/processes", map[string]any{"templateId": "gate"}).Code)
	rec := human(http.MethodPost, "/v1/process/runs", map[string]any{"id": "run_team", "templateId": "gate", "group": "team", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = human(http.MethodPost, "/v1/process/runs", map[string]any{"id": "run_other", "templateId": "gate", "group": "other", "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Runs: the member lists and reads its group's, not another group's.
	rec = agent(member, http.MethodGet, "/v1/process/runs?group=team", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "run_team")
	assert.NotContains(t, rec.Body.String(), "run_other")
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/process/runs/run_team", nil).Code)
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/process/runs/run_team/events", nil).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/runs/run_other", nil).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/runs?group=other", nil).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/runs", nil).Code, "the unfiltered list stays process.runs.read's")
	assert.Equal(t, http.StatusForbidden, agent(outsider, http.MethodGet, "/v1/process/runs/run_team", nil).Code)
	// Steering a run is not a member's by default.
	rec = agent(member, http.MethodPost, "/v1/process/runs/run_team/pause", map[string]any{})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), agentd.PermProcessRunsManage)

	// Templates: the member lists and reads the one pinned to its group.
	rec = agent(member, http.MethodGet, "/v1/process/templates?all=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct{ Templates []struct{ ID string } }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Templates, 1, "only the pinned template: %s", rec.Body.String())
	assert.Equal(t, "gate", list.Templates[0].ID)
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/process/templates/gate", nil).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/templates/prog", nil).Code, "not pinned to its group")
	assert.Equal(t, http.StatusForbidden, agent(outsider, http.MethodGet, "/v1/process/templates/gate", nil).Code)

	// It saves a new version of it, but not its listing or pins.
	save := func(conv string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		var head struct {
			SourceHash string `json:"sourceHash"`
		}
		got := human(http.MethodGet, "/v1/process/templates/gate", nil)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		require.NoError(t, json.Unmarshal(got.Body.Bytes(), &head))
		body["template"] = processHumanGateTemplate("gate")
		body["sourceHash"] = head.SourceHash
		return agent(conv, http.MethodPost, "/v1/process/templates/gate", body)
	}
	rec = save(member, map[string]any{})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusForbidden, save(member, map[string]any{"listed": true}).Code)
	assert.Equal(t, http.StatusForbidden, save(outsider, map[string]any{}).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodDelete, "/v1/process/templates/gate", nil).Code)

	// The lock refuses the member's save and keeps its read.
	rec = testharness.Serve(agentd.BuildDashboardHandlerForTest(), boardDashReq(t, http.MethodPost, "/api/resource-locks",
		map[string]any{"kind": "process", "id": "gate", "locked": true}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = save(member, map[string]any{})
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "resource_locked")
	assert.Equal(t, http.StatusOK, agent(member, http.MethodGet, "/v1/process/templates/gate", nil).Code)

	// Leaving the group ends it.
	require.NoError(t, db.RemoveAgentGroupMember(team.ID, member))
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/runs/run_team", nil).Code)
	assert.Equal(t, http.StatusForbidden, agent(member, http.MethodGet, "/v1/process/templates/gate", nil).Code)
}
