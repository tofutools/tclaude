package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// awbproxy_flow_test.go drives the AWB proxy through the real daemon mux with
// the outbound-HTTP boundary stubbed.
//
// The assertions concentrate on what would fail SILENTLY: that the project
// allow-list actually bounds what an agent can reach — in the request the
// daemon builds AND in the answer it will return — that reading does not confer
// writing, that the operator's own allow_write ceiling is independent of the
// permission slug, and that the hard delete cannot happen by accident.

const awbProxyTestConv = "conv-awb-proxy"

// awbRecorder captures every HTTP request the daemon builds and replays a
// scripted response. Capturing the fully-assembled URL is the point: it is what
// lets a test assert that a project filter really was written, and that a
// caller's string reached a query value rather than a path segment.
type awbRecorder struct {
	mu       sync.Mutex
	calls    []agentd.AWBProxyRequest
	response func(req agentd.AWBProxyRequest) (int, string)
	err      error
}

func (r *awbRecorder) do(
	_ context.Context, req agentd.AWBProxyRequest,
) (int, []byte, http.Header, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	respond := r.response
	stubErr := r.err
	r.mu.Unlock()

	if stubErr != nil {
		return 0, nil, nil, stubErr
	}
	if respond == nil {
		return http.StatusOK, []byte(`{}`), nil, nil
	}
	status, body := respond(req)
	return status, []byte(body), nil, nil
}

func (r *awbRecorder) snapshot() []agentd.AWBProxyRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentd.AWBProxyRequest(nil), r.calls...)
}

func (r *awbRecorder) sawAnyCall() bool { return len(r.snapshot()) > 0 }

// only returns the single call the recorder saw.
func (r *awbRecorder) only(t *testing.T) agentd.AWBProxyRequest {
	t.Helper()
	calls := r.snapshot()
	require.Len(t, calls, 1, "expected exactly one AWB call")
	return calls[0]
}

// last returns the final call, for the verbs that read before they write.
func (r *awbRecorder) last(t *testing.T) agentd.AWBProxyRequest {
	t.Helper()
	calls := r.snapshot()
	require.NotEmpty(t, calls, "expected at least one AWB call")
	return calls[len(calls)-1]
}

// awbWorld builds a daemon world with an enrolled agent and an operator policy.
// allowed == nil writes no awb_proxy block at all, which is the fail-closed
// "operator never opted in" case.
func awbWorld(
	t *testing.T, allowed []string, tweak ...func(*config.AWBProxyConfig),
) (*awbFlow, *awbRecorder) {
	t.Helper()
	f := newFlow(t)
	f.HaveConvWithTitle(awbProxyTestConv, "ticket-worker")
	f.HaveEnrolledAgent(awbProxyTestConv)

	if allowed == nil {
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{}}))
	} else {
		proxy := &config.AWBProxyConfig{
			URL:             "https://awb.example",
			Username:        "tclaude-bot",
			AllowedProjects: allowed,
		}
		for _, fn := range tweak {
			fn(proxy)
		}
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{AWBProxy: proxy}}))
	}

	rec := &awbRecorder{}
	t.Cleanup(agentd.SetAWBTransportForTest(rec.do))
	// A password in the environment, so the tests exercise the same resolution
	// path an operator without a password_file uses.
	t.Setenv("AWB_PASSWORD", "hunter2")
	return &awbFlow{t: t, flow: f}, rec
}

// awbFlow is the small slice of the harness these tests use, so that every
// scenario reads as "grant, post, assert" rather than as request plumbing.
type awbFlow struct {
	t    *testing.T
	flow *testharness.Flow
}

func (w *awbFlow) post(path string, body any) *httptest.ResponseRecorder {
	w.t.Helper()
	return testharness.Serve(w.flow.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(w.t, http.MethodPost, path, body), awbProxyTestConv))
}

func (w *awbFlow) grant(slug string) {
	w.t.Helper()
	require.NoError(w.t, db.GrantAgentPermission(awbProxyTestConv, slug, "test"))
}

// grantScoped is the per-agent half of the project gate.
func (w *awbFlow) grantScoped(slug, scopeJSON string) {
	w.t.Helper()
	require.NoError(w.t, db.GrantAgentPermissionWithScope(awbProxyTestConv, slug, scopeJSON, "test"))
}

// outcome decodes the daemon's success envelope.
func (w *awbFlow) outcome(res *httptest.ResponseRecorder) awbOutcomeView {
	w.t.Helper()
	require.Equal(w.t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var out awbOutcomeView
	require.NoError(w.t, json.Unmarshal(res.Body.Bytes(), &out))
	return out
}

type awbOutcomeView struct {
	Projects   []string        `json:"projects"`
	JSON       json.RawMessage `json:"json"`
	Text       string          `json:"text"`
	Content    []byte          `json:"content"`
	HasContent bool            `json:"has_content"`
}

// issueJSON builds a scripted issue response.
func awbIssueJSON(id, project string) string {
	return `{"id":"` + id + `","project":"` + project + `","title":"A thing","description":"",` +
		`"type":"task","status":"open","priority":2,"labels":[],"assignee":"","close_reason":"",` +
		`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
		`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[]}`
}

// projectsJSON is what GET /api/projects answers with, which every unfiltered
// listing resolves before it can build a filter.
func awbProjectsJSON(keys ...string) string {
	rows := make([]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, `{"key":"`+key+`","name":"`+key+`","description":"","active_issues":1,`+
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z"}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// query reads the query string off a recorded call.
func awbQuery(t *testing.T, req agentd.AWBProxyRequest) url.Values {
	t.Helper()
	u, err := url.Parse(req.URL)
	require.NoError(t, err)
	return u.Query()
}

// --- fail-closed configuration ---

// TestAWBProxy_DisabledUntilConfigured is the fail-closed contract: an operator
// who has not configured the proxy has not enabled it, and no grant can change
// that.
func TestAWBProxy_DisabledUntilConfigured(t *testing.T) {
	t.Run("no awb_proxy block at all", func(t *testing.T) {
		w, rec := awbWorld(t, nil)
		w.grant(agentd.PermAWBRead)

		res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f9c1"})
		assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "awb_not_configured")
		assert.False(t, rec.sawAnyCall(), "an unconfigured proxy must not reach any server")
	})

	t.Run("a server but no project policy is still disabled", func(t *testing.T) {
		w, rec := awbWorld(t, []string{})
		w.grant(agentd.PermAWBRead)

		res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f9c1"})
		assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "awb_proxy_disabled")
		assert.False(t, rec.sawAnyCall())
	})
}

// --- permission gating ---

// TestAWBProxy_WriteRequiresItsOwnSlug — reading an issue must not confer the
// ability to change the operator's tracker under their name.
func TestAWBProxy_WriteRequiresItsOwnSlug(t *testing.T) {
	t.Run("ungranted", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"})
		res := w.post("/v1/awb/issue/close", map[string]any{"id": "awb-a3f9c1"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall(), "a denied caller must not reach AWB — not even a probe")
	})

	t.Run("proxy.awb.read does not confer proxy.awb.write", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBRead)
		res := w.post("/v1/awb/issue/close", map[string]any{"id": "awb-a3f9c1"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})
}

// TestAWBProxy_WriteAlsoNeedsTheOperatorCeiling is the second half of the write
// gate, and the one that is easy to lose: the slug says THIS AGENT may write,
// allow_write says the OPERATOR wants any agent to be able to.
func TestAWBProxy_WriteAlsoNeedsTheOperatorCeiling(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}) // allow_write defaults to false
	w.grant(agentd.PermAWBWrite)

	res := w.post("/v1/awb/issue/close", map[string]any{"id": "awb-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "awb_write_disabled")
	assert.False(t, rec.sawAnyCall(),
		"the operator's ceiling must be checked before anything reaches AWB")
}

// --- the project gate ---

// TestAWBProxy_ProjectAllowListBoundsTheReference refuses a project the
// operator never allow-listed, before any credential is spent.
func TestAWBProxy_ProjectAllowListBoundsTheReference(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "secret-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "project_not_allowed")
	assert.False(t, rec.sawAnyCall(), "the cheap gate must run before the network, not after it")
}

// TestAWBProxy_BareHashIsRefusedBeforeTheNetwork covers the form awb accepts
// and this deliberately does not: a bare hash names no project, so it could
// only be judged after the issue had already been fetched.
func TestAWBProxy_BareHashIsRefusedBeforeTheNetwork(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "a3f9c1"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())
}

// TestAWBProxy_ProjectGateReChecksAWBsAnswer is the load-bearing half of the
// project gate.
//
// The reference check is a check on a string the CALLER supplied. AWB resolves
// an ID PREFIX, so a proxy that trusted the prefix alone would be trusting a
// label rather than the thing reached. The daemon must refuse on the project
// AWB REPORTED, and the issue's contents must not appear in the response.
func TestAWBProxy_ProjectGateReChecksAWBsAnswer(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		// The caller asked for awb-a3f; AWB answers with an issue elsewhere.
		return http.StatusOK, `{"id":"secret-9","project":"secret","title":"Acquisition plan",` +
			`"description":"confidential","type":"task","status":"open","priority":2,"labels":[],` +
			`"assignee":"","close_reason":"","created_at":"2026-08-26T09:12:03.412Z",` +
			`"updated_at":"2026-08-26T09:12:03.412Z","blocked":false,"blockers":[],` +
			`"relations":[],"links":[],"attachments":[]}`
	}

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.NotContains(t, res.Body.String(), "Acquisition plan",
		"an issue outside the allow-list must not leak through the refusal")
	assert.NotContains(t, res.Body.String(), "confidential")
}

// TestAWBProxy_ListIsBoundedByTheAllowList checks both halves of the listing
// gate: the filter the daemon SENDS, and the rows it will RETURN.
func TestAWBProxy_ListIsBoundedByTheAllowList(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "web"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/projects") {
			return http.StatusOK, awbProjectsJSON("awb", "web", "secret")
		}
		// AWB answers with a row from outside the filter. The daemon must drop
		// it: the filter is a request AWB honours, the gate is a check we make.
		return http.StatusOK, "[" + awbIssueJSON("awb-1", "awb") + "," +
			awbIssueJSON("secret-1", "secret") + "]"
	}

	res := w.post("/v1/awb/issue/list", map[string]any{})
	out := w.outcome(res)

	calls := rec.snapshot()
	require.Len(t, calls, 2, "the project list, then the listing itself")
	q := awbQuery(t, calls[1])
	assert.ElementsMatch(t, []string{"awb", "web"}, q["project"],
		"an unfiltered listing must still name every project it may see, and no more")
	assert.Equal(t, "50", q.Get("limit"), "the proxy bounds a listing awb would leave unbounded")

	assert.Equal(t, []string{"awb", "web"}, out.Projects)
	assert.Contains(t, string(out.JSON), "awb-1")
	assert.NotContains(t, string(out.JSON), "secret-1",
		"a row from outside the effective set must be dropped, not returned")
}

// TestAWBProxy_ListingSkipsAllowedProjectsTheServerDoesNotHave covers the
// misconfiguration that would otherwise break every unfiltered listing: AWB
// answers a `project` filter naming no project with a 404 rather than with an
// empty listing.
func TestAWBProxy_ListingSkipsAllowedProjectsTheServerDoesNotHave(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "gone"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/projects") {
			return http.StatusOK, awbProjectsJSON("awb")
		}
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/ready", map[string]any{})
	w.outcome(res)

	q := awbQuery(t, rec.last(t))
	assert.Equal(t, []string{"awb"}, q["project"],
		"a stale entry in the operator's allow-list must not 404 every listing")
}

// TestAWBProxy_ReadyRejectsTheFiltersItFixesForItself mirrors awb: `ready`
// answers one question, so a filter that would change the question is a usage
// error rather than a silently ignored field.
func TestAWBProxy_ReadyRejectsTheFiltersItFixesForItself(t *testing.T) {
	for _, body := range []map[string]any{
		{"mine": true},
		{"unassigned": true},
		{"assignees": []string{"claude-1"}},
		{"statuses": []string{"closed"}},
		{"include_closed": true},
	} {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		res := w.post("/v1/awb/issue/ready", body)
		assert.Equal(t, http.StatusBadRequest, res.Code, "%v: body=%s", body, res.Body.String())
		assert.False(t, rec.sawAnyCall(), "%v", body)
	}
}

// TestAWBProxy_SearchTermsTravelAsQueryValues is the injection-shaped
// assertion: a caller's text must reach a query VALUE and never a path.
func TestAWBProxy_SearchTermsTravelAsQueryValues(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/projects") {
			return http.StatusOK, awbProjectsJSON("awb")
		}
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/search", map[string]any{
		"terms": []string{"../../admin", "a&b=c"},
	})
	w.outcome(res)

	call := rec.last(t)
	u, err := url.Parse(call.URL)
	require.NoError(t, err)
	assert.Equal(t, "/api/search", u.Path, "no caller value may change the path")
	assert.Equal(t, []string{"../../admin", "a&b=c"}, u.Query()["q"])
}

// --- the write verbs ---

func TestAWBProxy_ClaimRecordsTheDaemonsAccount(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/issue/claim", map[string]any{"id": "awb-a3f9c1"})
	w.outcome(res)

	call := rec.only(t)
	assert.Equal(t, http.MethodPost, call.Method)
	assert.Equal(t, "https://awb.example/api/issues/awb-a3f9c1/claim", call.URL)
	assert.Equal(t, "tclaude-bot", call.Username)
	assert.Equal(t, "hunter2", call.Password)
	assert.JSONEq(t, `{"assignee":"tclaude-bot"}`, string(call.Body),
		"the assignee is stated explicitly, so a proxied claim records what `awb claim` would")
}

func TestAWBProxy_CreateGatesEveryRelationTarget(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)

	res := w.post("/v1/awb/issue/create", map[string]any{
		"project": "awb", "title": "New thing", "blocked_by": []string{"secret-000001"},
	})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall(),
		"a relation shows up at both ends, so the other end is inside the gate too")
}

func TestAWBProxy_CreateSendsAWBsOwnBody(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/issue/create", map[string]any{
		"project": "awb", "title": " Parser crashes ", "type": "bug", "priority": 1,
		"labels": []string{"parser"}, "blocked_by": []string{"awb-000001"},
		"compact": true,
	})
	out := w.outcome(res)
	assert.Equal(t, "awb-a3f9c1\n", out.Text,
		"create is awb's exception to 'a mutation prints nothing': the new id is the point")

	call := rec.only(t)
	assert.Equal(t, "https://awb.example/api/issues", call.URL)
	assert.JSONEq(t, `{"project":"awb","title":"Parser crashes","type":"bug","priority":1,`+
		`"labels":["parser"],"relations":[{"type":"blocked-by","other":"awb-000001"}]}`,
		string(call.Body))
}

// TestAWBProxy_DepGatesBothEnds — a relation is read from either end, so
// writing one into an unreachable project is a write outside the gate wearing
// the subject's clothes.
func TestAWBProxy_DepGatesBothEnds(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)

	res := w.post("/v1/awb/dep/add", map[string]any{
		"id": "awb-a3f9c1", "type": "blocked-by", "other": "secret-000001",
	})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())
}

// TestAWBProxy_DeleteNeedsForce keeps the hard delete from happening by
// accident: it is not recoverable, and it orphans children and drops relations.
func TestAWBProxy_DeleteNeedsForce(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)

	res := w.post("/v1/awb/issue/delete", map[string]any{"id": "awb-a3f9c1"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())

	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, `{"id":"awb-a3f9c1","project":"awb","title":"A thing",` +
			`"description":"","type":"task","status":"open","priority":2,"labels":[],"assignee":"",` +
			`"close_reason":"","created_at":"2026-08-26T09:12:03.412Z",` +
			`"updated_at":"2026-08-26T09:12:03.412Z","blocked":false,"blockers":[],` +
			`"relations":[{"type":"blocked-by","other":"awb-000001","direction":"out"}],` +
			`"links":[],"attachments":[]}`
	}
	res = w.post("/v1/awb/issue/delete",
		map[string]any{"id": "awb-a3f9c1", "force": true, "compact": true})
	out := w.outcome(res)
	assert.Equal(t, http.MethodDelete, rec.only(t).Method)
	assert.Equal(t, "Deleted awb-a3f9c1 and 1 relation(s).\n", out.Text,
		"the count is derived from the pre-deletion issue AWB answers with")
}

// TestAWBProxy_LabelRemovalTravelsAsAQueryValue: a label may contain a slash,
// and a slash in a path segment is a different path.
func TestAWBProxy_LabelRemovalTravelsAsAQueryValue(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/label/rm", map[string]any{"id": "awb-a3f9c1", "label": "team/backend"})
	w.outcome(res)

	call := rec.only(t)
	assert.Equal(t, http.MethodDelete, call.Method)
	u, err := url.Parse(call.URL)
	require.NoError(t, err)
	assert.Equal(t, "/api/issues/awb-a3f9c1/labels", u.Path)
	assert.Equal(t, "team/backend", u.Query().Get("label"))
}

// --- attachments ---

// TestAWBProxy_AttachGetReturnsBytes covers the one verb with no output mode,
// and the size bound that makes carrying content through a JSON body safe.
func TestAWBProxy_AttachGetReturnsBytes(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.HasSuffix(req.URL, "/content") {
			return http.StatusOK, "hello\x00bytes"
		}
		return http.StatusOK, `{"issue":"awb-a3f9c1","name":"notes.md","content_type":"text/markdown",` +
			`"size":11,"sha256":"9f86d0","created_at":"2026-08-26T09:12:03.412Z"}`
	}

	res := w.post("/v1/awb/attach/get", map[string]any{"id": "awb-a3f9c1", "name": "notes.md"})
	out := w.outcome(res)
	assert.True(t, out.HasContent)
	assert.Equal(t, "hello\x00bytes", string(out.Content))
	assert.Empty(t, out.JSON, "--json and --compact do not apply to content")
}

func TestAWBProxy_AttachGetRefusesAnOversizedAttachment(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, `{"issue":"awb-a3f9c1","name":"big.bin","content_type":"application/octet-stream",` +
			`"size":999999999,"sha256":"9f86d0","created_at":"2026-08-26T09:12:03.412Z"}`
	}

	res := w.post("/v1/awb/attach/get", map[string]any{"id": "awb-a3f9c1", "name": "big.bin"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Len(t, rec.snapshot(), 1,
		"the metadata read is what bounds the download; the content must not be fetched at all")
}

func TestAWBProxy_AttachAddSendsOctetStream(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, `{"issue":"awb-a3f9c1","name":"notes.md","content_type":"text/markdown",` +
			`"size":5,"sha256":"9f86d0","created_at":"2026-08-26T09:12:03.412Z"}`
	}

	res := w.post("/v1/awb/attach/add", map[string]any{
		"id": "awb-a3f9c1", "name": "notes.md", "content": []byte("hello"),
		"content_type": "text/markdown",
	})
	w.outcome(res)

	call := rec.only(t)
	assert.Equal(t, "application/octet-stream", call.ContentType,
		"the body is the file's bytes and nothing else")
	assert.Equal(t, []byte("hello"), call.Body)
	q := awbQuery(t, call)
	assert.Equal(t, "notes.md", q.Get("name"))
	assert.Equal(t, "text/markdown", q.Get("content-type"))
}

// TestAWBProxy_AttachNameCannotEscapeItsSegment: a name is never somewhere to
// write, and it must not become a path either.
func TestAWBProxy_AttachNameCannotEscapeItsSegment(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	for _, name := range []string{"../../../etc/passwd", "a/b", "..", "."} {
		res := w.post("/v1/awb/attach/show", map[string]any{"id": "awb-a3f9c1", "name": name})
		assert.Equal(t, http.StatusBadRequest, res.Code, "%q: body=%s", name, res.Body.String())
	}
	assert.False(t, rec.sawAnyCall())
}

// --- dep tree ---

// TestAWBProxy_DepTreePrunesOutOfScopeChildren covers the one read whose answer
// can reach outside the gate on its own: AWB follows children across project
// boundaries by design.
func TestAWBProxy_DepTreePrunesOutOfScopeChildren(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		root := `{"id":"awb-a00001","project":"awb","title":"Root","description":"","type":"epic",` +
			`"status":"open","priority":0,"labels":[],"assignee":"","close_reason":"",` +
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
			`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[],"children":[` +
			`{"id":"secret-b00002","project":"secret","title":"Merger terms","description":"confidential",` +
			`"type":"task","status":"open","priority":2,"labels":[],"assignee":"","close_reason":"",` +
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
			`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[],"children":[]}]}`
		return http.StatusOK, root
	}

	res := w.post("/v1/awb/dep/tree", map[string]any{"id": "awb-a00001"})
	out := w.outcome(res)
	assert.Contains(t, string(out.JSON), "awb-a00001")
	assert.NotContains(t, string(out.JSON), "Merger terms",
		"a child in an unreachable project arrives as a complete issue unless it is pruned")
	assert.NotContains(t, string(out.JSON), "confidential")
}

// --- the scoped grant ---

// TestAWBProxy_GrantScopeNarrowsTheOperatorList is the per-agent half of the
// gate: an operator can narrow ONE agent without touching the global list, and
// a grant can never widen past it.
func TestAWBProxy_GrantScopeNarrowsTheOperatorList(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "web"})
	w.grantScoped(agentd.PermAWBRead, `{"awb_project":["awb"]}`)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/projects") {
			return http.StatusOK, awbProjectsJSON("awb", "web")
		}
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/list", map[string]any{})
	out := w.outcome(res)
	assert.Equal(t, []string{"awb"}, out.Projects,
		"the echoed set is what THIS caller may reach, not what the operator allows in general")
	assert.Equal(t, []string{"awb"}, awbQuery(t, rec.last(t))["project"])

	t.Run("and the project the operator allows but the grant does not is refused", func(t *testing.T) {
		res := w.post("/v1/awb/issue/show", map[string]any{"id": "web-a3f9c1"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "project_out_of_scope",
			"the refusal must name the list that actually excluded it")
	})
}

// TestAWBProxy_ScopedGrantIsTheWholePolicyWithNoOperatorList mirrors the Linear
// proxy: a scope-only posture is supported, and it is the only way an unscoped
// grant is refused while a scoped one works.
func TestAWBProxy_ScopedGrantIsTheWholePolicyWithNoOperatorList(t *testing.T) {
	w, rec := awbWorld(t, []string{})
	w.grantScoped(agentd.PermAWBRead, `{"awb_project":["awb"]}`)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f9c1"})
	out := w.outcome(res)
	assert.Equal(t, []string{"awb"}, out.Projects)
}
