package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type tbImportResult struct {
	Input    string  `json:"input"`
	Task     *tbTask `json:"task"`
	Resolved bool    `json:"resolved"`
	Note     string  `json:"note"`
	Error    string  `json:"error"`
	Code     string  `json:"code"`
}

type tbImport struct {
	Group   string           `json:"group"`
	Created int              `json:"created"`
	Results []tbImportResult `json:"results"`
}

// newTaskboardImportFlow is a task board world with the awb and Linear proxies
// configured and their outbound transports stubbed: awb answers any tcl-
// issue, Linear any LIN- issue.
func newTaskboardImportFlow(t *testing.T, member string) *taskboardHarness {
	h := newTaskboardFlow(t)
	require.NoError(t, config.Save(&config.Config{
		Features: &config.FeaturesConfig{Taskboard: new(true)},
		Agent: &config.AgentConfig{
			AWBProxy: &config.AWBProxyConfig{URL: "https://awb.example", Username: "tclaude-bot",
				AllowedWorkspaces: []string{"tcl"}},
			LinearProxy: &config.LinearProxyConfig{AllowedTeams: []string{"LIN"}},
		},
	}))
	t.Setenv("AWB_PASSWORD", "hunter2")
	t.Setenv("LINEAR_API_KEY", "lin_api_testkey")
	awb := &awbRecorder{response: func(req agentd.AWBProxyRequest) (int, string) {
		id := req.URL[strings.LastIndex(req.URL, "/")+1:]
		if id == "tcl-gone00" {
			return http.StatusNotFound, `{"error":"not found"}`
		}
		return http.StatusOK, `{"id":"` + id + `","workspace":"tcl","title":"Title of ` + id + `","type":"task","status":"open"}`
	}}
	t.Cleanup(agentd.SetAWBTransportForTest(awb.do))
	lin := &linearRecorder{response: func(call linearCall) (int, string) {
		id, _ := call.Variables["id"].(string)
		return http.StatusOK, issueJSON(id, "LIN")
	}}
	t.Cleanup(agentd.SetLinearTransportForTestJSON(lin.do))

	h.agent(t, member)
	h.f.HaveEnrolledAgent(member)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", member)
	// An import creates unassigned tasks: it takes taskboard.write.
	h.groupWrites("crew")
	return h
}

func (h *taskboardHarness) importBatch(conv string, body map[string]any) tbImport {
	h.t.Helper()
	rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/tasks", body)
	require.Equal(h.t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out tbImport
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// A member with the proxy read rights imports awb and Linear refs: each
// becomes one idea titled from its tracker with its link filled in, a repeat
// in the batch and a ref already on the board are refused on their own, and
// a ref the tracker does not have still lands, titled by its id.
func TestTaskboardImportResolvesRefsThroughTheProxies(t *testing.T) {
	const member = "tb-importer-1111"
	h := newTaskboardImportFlow(t, member)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	require.NoError(t, db.GrantAgentPermission(member, agentd.PermAWBRead, "test"))
	require.NoError(t, db.GrantAgentPermission(member, agentd.PermLinearRead, "test"))

	out := h.importBatch(member, map[string]any{
		"refs": []string{"awb:tcl-abc123", "linear:LIN-7", "awb:tcl-abc123", "awb:tcl-gone00", "not a ref"},
	})
	require.Len(t, out.Results, 5)
	assert.Equal(t, 3, out.Created)

	awb := out.Results[0]
	require.NotNil(t, awb.Task, "%+v", awb)
	assert.True(t, awb.Resolved)
	assert.Equal(t, "Title of tcl-abc123", awb.Task.Title)
	assert.Equal(t, "idea", awb.Task.Stage, "an import lands in the ideas")
	require.NotNil(t, awb.Task.Ref)
	assert.Equal(t, "awb", awb.Task.Ref.Kind)
	assert.Equal(t, "https://awb.example/#/issues/tcl-abc123", awb.Task.Ref.URL)

	lin := out.Results[1]
	require.NotNil(t, lin.Task, "%+v", lin)
	assert.True(t, lin.Resolved)
	assert.Equal(t, "A thing", lin.Task.Title)
	assert.Equal(t, "https://linear.app/acme/issue/LIN-7", lin.Task.Ref.URL)

	assert.Equal(t, "duplicate", out.Results[2].Code, "a repeat in the batch is refused")
	assert.Nil(t, out.Results[2].Task)

	gone := out.Results[3]
	require.NotNil(t, gone.Task, "a failed lookup still adds the task")
	assert.False(t, gone.Resolved)
	assert.Equal(t, "tcl-gone00", gone.Task.Title)
	assert.NotEmpty(t, gone.Note)

	assert.Equal(t, "invalid_arg", out.Results[4].Code)

	// A second import: the ref already on the board is refused, the rest of
	// the batch lands, and --next puts it in the queue.
	out = h.importBatch(member, map[string]any{"refs": []string{"tcl-abc123", "LIN-8"}, "stage": "next"})
	assert.Equal(t, 1, out.Created)
	assert.Equal(t, "duplicate", out.Results[0].Code)
	assert.Contains(t, out.Results[0].Error, awb.Task.ID)
	require.NotNil(t, out.Results[1].Task)
	assert.Equal(t, "next", out.Results[1].Task.Stage)

	// Once the task is dropped, the ref may come back.
	h.task(h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+awb.Task.ID+"/status",
		map[string]any{"status": "dropped"}), http.StatusOK)
	out = h.importBatch(member, map[string]any{"refs": []string{"awb:tcl-abc123"}})
	assert.Equal(t, 1, out.Created)

	// History records the import.
	rec := h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+lin.Task.ID+"/events", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var hist struct {
		Events []struct{ Kind, Actor string } `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &hist))
	require.NotEmpty(t, hist.Events)
	assert.Equal(t, "imported", hist.Events[0].Kind)

	// Each lookup is audited as the member's own proxy read.
	var proxyReads int
	for _, e := range fetchAudit(t, agentd.BuildDashboardHandlerForTest(), "").Entries {
		if e.Verb == "awb.issue.show" || e.Verb == "linear.issue.view" {
			proxyReads++
			assert.Equal(t, "agent", e.ActorKind, "%+v", e)
		}
	}
	assert.Positive(t, proxyReads, "the lookups leave proxy audit rows")
}

// Without the proxy read rights, or with no proxy for the tracker, the ref
// still becomes a task, titled by its id, and the note says why.
func TestTaskboardImportFallsBackToTheID(t *testing.T) {
	const member = "tb-importer-2222"
	h := newTaskboardImportFlow(t, member)

	out := h.importBatch(member, map[string]any{
		"refs": []string{"awb:tcl-abc123", "acme/widgets#12", "https://github.com/acme/widgets/issues/12"},
	})
	require.Len(t, out.Results, 3)
	awb := out.Results[0]
	require.NotNil(t, awb.Task)
	assert.Equal(t, "tcl-abc123", awb.Task.Title)
	assert.Contains(t, awb.Note, "refused", "no proxy.awb.read: the lookup is refused")
	gh := out.Results[1]
	require.NotNil(t, gh.Task)
	assert.Equal(t, "acme/widgets#12", gh.Task.Title)
	assert.NotEmpty(t, gh.Note)
	assert.Equal(t, "duplicate", out.Results[2].Code, "the shorthand and the URL are the same issue")

	// Plain URLs on one host are different refs, keyed by the whole URL; the
	// title is the URL, not just its host. The same URL again is a duplicate.
	out = h.importBatch(member, map[string]any{"refs": []string{
		"https://jira.acme.com/browse/PROJ-1", "https://jira.acme.com/browse/PROJ-2", "https://jira.acme.com/browse/PROJ-1/",
	}})
	require.Len(t, out.Results, 3)
	assert.Equal(t, 2, out.Created, "%+v", out.Results)
	require.NotNil(t, out.Results[0].Task)
	assert.Equal(t, "https://jira.acme.com/browse/PROJ-1", out.Results[0].Task.Title)
	assert.Equal(t, "duplicate", out.Results[2].Code)

	// A task added by hand with a custom ref label still blocks its id.
	h.task(h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{
		"title": "by hand", "ref": "https://awb.example/#/issues/tcl-hand01", "ref_label": "Hand-made"}), http.StatusCreated)
	out = h.importBatch(member, map[string]any{"refs": []string{"awb:tcl-hand01"}})
	assert.Equal(t, "duplicate", out.Results[0].Code, "%+v", out.Results[0])

	// The import needs taskboard.write like any add.
	const outsider = "tb-outsider-3333"
	h.agent(t, outsider)
	rec := h.call(outsider, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"refs": []string{"LIN-1"}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// Lines: one task per non-empty line; a line that is only a ref becomes a
// task with that ref. A batch takes no single-task fields.
func TestTaskboardImportLines(t *testing.T) {
	const member = "tb-importer-4444"
	h := newTaskboardImportFlow(t, member)
	require.NoError(t, db.GrantAgentPermission(member, agentd.PermLinearRead, "test"))

	out := h.importBatch(member, map[string]any{
		"lines": []string{"try the fused card", "", "LIN-9", "evil\u202Egnp.exe"},
	})
	require.Len(t, out.Results, 3)
	assert.Equal(t, 2, out.Created)
	assert.Equal(t, "try the fused card", out.Results[0].Task.Title)
	assert.Nil(t, out.Results[0].Task.Ref)
	require.NotNil(t, out.Results[1].Task.Ref)
	assert.Equal(t, "linear", out.Results[1].Task.Ref.Kind)
	assert.Equal(t, "A thing", out.Results[1].Task.Title)
	assert.Equal(t, "invalid_arg", out.Results[2].Code)

	assert.Equal(t, []string{"try the fused card", "A thing"}, h.list(member, "/v1/groups/crew/taskboard"))

	rec := h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"lines": []string{"x"}, "title": "y"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type tbRefLookup struct {
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Label    string `json:"label"`
	Title    string `json:"title"`
	Resolved bool   `json:"resolved"`
	Note     string `json:"note"`
}

func (h *taskboardHarness) lookupRef(conv string, body map[string]any, want int) tbRefLookup {
	h.t.Helper()
	rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/ref-lookup", body)
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out tbRefLookup
	if want == http.StatusOK {
		require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return out
}

// The "from a tracker" form asks what a pasted ticket would be before it adds
// anything: the ref as `add --ref` stores it, and its title from the tracker
// as an import takes it. Nothing lands on the board; a ref the caller cannot
// look up still answers, with its id as the title and a note.
func TestTaskboardRefLookupTitlesATicketWithoutAddingIt(t *testing.T) {
	const member = "tb-lookup-4444"
	h := newTaskboardImportFlow(t, member)
	require.NoError(t, db.GrantAgentPermission(member, agentd.PermAWBRead, "test"))

	got := h.lookupRef(member, map[string]any{"ref": "https://awb.example/#/issues/tcl-abc123"}, http.StatusOK)
	assert.Equal(t, "awb", got.Kind)
	assert.Equal(t, "tcl-abc123", got.Label)
	assert.Equal(t, "https://awb.example/#/issues/tcl-abc123", got.URL)
	assert.Equal(t, "Title of tcl-abc123", got.Title)
	assert.True(t, got.Resolved)

	got = h.lookupRef(member, map[string]any{"ref": "tcl-abc123", "ref_label": "Checkout"}, http.StatusOK)
	assert.Equal(t, "Checkout", got.Label, "a given label is kept, as --ref-label does")
	assert.Equal(t, "https://awb.example/#/issues/tcl-abc123", got.URL, "a bare id gets its link")

	// No proxy.linear.read: the id is the title, and the note says why.
	got = h.lookupRef(member, map[string]any{"ref": "LIN-7"}, http.StatusOK)
	assert.Equal(t, "linear", got.Kind)
	assert.Equal(t, "LIN-7", got.Title)
	assert.False(t, got.Resolved)
	assert.NotEmpty(t, got.Note)

	// A plain URL is not looked up.
	got = h.lookupRef(member, map[string]any{"ref": "https://jira.acme.com/browse/PROJ-1"}, http.StatusOK)
	assert.Equal(t, "url", got.Kind)
	assert.False(t, got.Resolved)

	h.lookupRef(member, map[string]any{"ref": "not a ref"}, http.StatusBadRequest)
	h.lookupRef(member, map[string]any{"ref": " "}, http.StatusBadRequest)

	rec := h.call(member, http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "tcl-abc123", "a lookup adds nothing to the board")

	const outsider = "tb-outsider-5555"
	h.agent(t, outsider)
	rec = h.call(outsider, http.MethodPost, "/v1/groups/crew/taskboard/ref-lookup", map[string]any{"ref": "LIN-1"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "the lookup needs the board's read access")
}
