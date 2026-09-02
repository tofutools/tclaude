package agentd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
// The assertions concentrate on what would fail SILENTLY: that the workspace
// allow-list actually bounds what an agent can reach — in the request the
// daemon builds AND in the answer it will return — that reading does not confer
// writing, that the operator's own allow_write ceiling is independent of the
// permission slug, and that the hard delete cannot happen by accident.

const awbProxyTestConv = "conv-awb-proxy"

// awbRecorder captures every HTTP request the daemon builds and replays a
// scripted response. Capturing the fully-assembled URL is the point: it is what
// lets a test assert that a workspace filter really was written, and that a
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
			URL:               "https://awb.example",
			Username:          "tclaude-bot",
			AllowedWorkspaces: allowed,
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

// grantScoped is the per-agent half of the workspace gate.
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
	Workspaces     []string        `json:"workspaces"`
	LegacyProjects []string        `json:"projects"`
	JSON           json.RawMessage `json:"json"`
	Text           string          `json:"text"`
	Content        []byte          `json:"content"`
	HasContent     bool            `json:"has_content"`
}

// issueJSON builds a scripted issue response.
func awbIssueJSON(id, workspace string) string {
	return `{"id":"` + id + `","workspace":"` + workspace + `","title":"A thing","description":"",` +
		`"type":"task","status":"open","priority":2,"labels":[],"assignees":[],` +
		`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
		`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[]}`
}

// workspacesJSON is what GET /api/workspaces answers with, which every unfiltered
// listing resolves before it can build a filter.
func awbWorkspacesJSON(keys ...string) string {
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

	t.Run("a server but no workspace policy is still disabled", func(t *testing.T) {
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

// --- the workspace gate ---

// TestAWBProxy_WorkspaceAllowListBoundsTheReference refuses a workspace the
// operator never allow-listed, before any credential is spent.
func TestAWBProxy_WorkspaceAllowListBoundsTheReference(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "secret-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "workspace_not_allowed")
	assert.False(t, rec.sawAnyCall(), "the cheap gate must run before the network, not after it")
}

// TestAWBProxy_BareHashIsRefusedBeforeTheNetwork covers the form awb accepts
// and this deliberately does not: a bare hash names no workspace, so it could
// only be judged after the issue had already been fetched.
func TestAWBProxy_BareHashIsRefusedBeforeTheNetwork(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "a3f9c1"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())
}

// TestAWBProxy_WorkspaceGateReChecksAWBsAnswer is the load-bearing half of the
// workspace gate.
//
// The reference check is a check on a string the CALLER supplied. AWB resolves
// an ID PREFIX, so a proxy that trusted the prefix alone would be trusting a
// label rather than the thing reached. The daemon must refuse on the workspace
// AWB REPORTED, and the issue's contents must not appear in the response.
func TestAWBProxy_WorkspaceGateReChecksAWBsAnswer(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		// The caller asked for awb-a3f; AWB answers with an issue elsewhere.
		return http.StatusOK, `{"id":"secret-9","workspace":"secret","title":"Acquisition plan",` +
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
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb", "web", "secret")
		}
		// AWB answers with a row from outside the filter. The daemon must drop
		// it: the filter is a request AWB honours, the gate is a check we make.
		return http.StatusOK, "[" + awbIssueJSON("awb-1", "awb") + "," +
			awbIssueJSON("secret-1", "secret") + "]"
	}

	res := w.post("/v1/awb/issue/list", map[string]any{})
	out := w.outcome(res)

	calls := rec.snapshot()
	require.Len(t, calls, 2, "the workspace list, then the listing itself")
	q := awbQuery(t, calls[1])
	assert.ElementsMatch(t, []string{"awb", "web"}, q["workspace"],
		"an unfiltered listing must still name every workspace it may see, and no more")
	assert.Equal(t, "50", q.Get("limit"), "the proxy bounds a listing awb would leave unbounded")

	assert.Equal(t, []string{"awb", "web"}, out.Workspaces)
	assert.Equal(t, out.Workspaces, out.LegacyProjects,
		"the compatibility alias must carry the same effective workspace set")
	assert.Contains(t, string(out.JSON), "awb-1")
	assert.NotContains(t, string(out.JSON), "secret-1",
		"a row from outside the effective set must be dropped, not returned")
}

func TestAWBProxy_ListAcceptsLegacyProjectsFilter(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/list", map[string]any{"projects": []string{"awb"}})
	w.outcome(res)

	q := awbQuery(t, rec.last(t))
	assert.Equal(t, []string{"awb"}, q["workspace"],
		"a pre-rename client must still constrain a new daemon by workspace")
}

// TestAWBProxy_ListingSkipsAllowedWorkspacesTheServerDoesNotHave covers the
// misconfiguration that would otherwise break every unfiltered listing: AWB
// answers a `workspace` filter naming no workspace with a 404 rather than with an
// empty listing.
func TestAWBProxy_ListingSkipsAllowedWorkspacesTheServerDoesNotHave(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "gone"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb")
		}
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/ready", map[string]any{})
	w.outcome(res)

	q := awbQuery(t, rec.last(t))
	assert.Equal(t, []string{"awb"}, q["workspace"],
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
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb")
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
		"workspace": "awb", "title": "New thing", "blocked_by": []string{"secret-000001"},
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
		"workspace": "awb", "title": " Parser crashes ", "type": "bug", "priority": 1,
		"labels": []string{"parser"}, "assignees": []string{"claude-1", "claude-2"},
		"blocked_by": []string{"awb-000001"},
		"compact":    true,
	})
	out := w.outcome(res)
	assert.Equal(t, "awb-a3f9c1\n", out.Text,
		"create is awb's exception to 'a mutation prints nothing': the new id is the point")

	call := rec.only(t)
	assert.Equal(t, "https://awb.example/api/issues", call.URL)
	assert.JSONEq(t, `{"workspace":"awb","title":"Parser crashes","type":"bug","priority":1,`+
		`"assignees":["claude-1","claude-2"],"labels":["parser"],`+
		`"relations":[{"type":"blocked-by","other":"awb-000001"}]}`,
		string(call.Body))
}

func TestAWBProxy_ShowRendersEveryAssignee(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, strings.Replace(awbIssueJSON("awb-a3f9c1", "awb"),
			`"assignees":[]`, `"assignees":["claude-1","claude-2"]`, 1)
	}

	out := w.outcome(w.post("/v1/awb/issue/show", map[string]any{
		"id": "awb-a3f9c1", "compact": true,
	}))
	assert.Contains(t, out.Text, "@claude-1 @claude-2",
		"the flow response must preserve every assignee returned by AWB")
}

func TestAWBProxy_CreateInfersTheOnlyVisibleWorkspace(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "gone"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb")
		}
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/issue/create", map[string]any{"title": "Parser crashes"})
	w.outcome(res)
	calls := rec.snapshot()
	require.Len(t, calls, 2, "workspace discovery must precede the mutation")
	assert.JSONEq(t, `{"workspace":"awb","title":"Parser crashes"}`, string(calls[1].Body))
}

func TestAWBProxy_CreateValidatesRelationsBeforeWorkspaceInference(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)

	res := w.post("/v1/awb/issue/create", map[string]any{
		"title": "New thing", "blocked_by": []string{"secret-000001"},
	})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall(), "relation and field validation must stay ahead of workspace discovery")
}

func TestAWBProxy_CreateRequiresWorkspaceWhenSeveralAreVisible(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "web"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb", "web")
		}
		require.Fail(t, "create mutation must not be attempted while workspace is ambiguous")
		return http.StatusInternalServerError, `{}`
	}

	res := w.post("/v1/awb/issue/create", map[string]any{"title": "Parser crashes"})
	assert.Equal(t, http.StatusBadRequest, res.Code)
	assert.Contains(t, res.Body.String(), "--workspace is required")
	require.Len(t, rec.snapshot(), 1)
}

// TestAWBProxy_DepGatesBothEnds — a relation is read from either end, so
// writing one into an unreachable workspace is a write outside the gate wearing
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
		return http.StatusOK, `{"id":"awb-a3f9c1","workspace":"awb","title":"A thing",` +
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
// can reach outside the gate on its own: AWB follows children across workspace
// boundaries by design.
func TestAWBProxy_DepTreePrunesOutOfScopeChildren(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		root := `{"id":"awb-a00001","workspace":"awb","title":"Root","description":"","type":"epic",` +
			`"status":"open","priority":0,"labels":[],"assignee":"","close_reason":"",` +
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
			`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[],"children":[` +
			`{"id":"secret-b00002","workspace":"secret","title":"Merger terms","description":"confidential",` +
			`"type":"task","status":"open","priority":2,"labels":[],"assignee":"","close_reason":"",` +
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
			`"blocked":false,"blockers":[],"relations":[],"links":[],"attachments":[],"children":[]}]}`
		return http.StatusOK, root
	}

	res := w.post("/v1/awb/dep/tree", map[string]any{"id": "awb-a00001"})
	out := w.outcome(res)
	assert.Contains(t, string(out.JSON), "awb-a00001")
	assert.NotContains(t, string(out.JSON), "Merger terms",
		"a child in an unreachable workspace arrives as a complete issue unless it is pruned")
	assert.NotContains(t, string(out.JSON), "confidential")
}

// --- the scoped grant ---

// TestAWBProxy_GrantScopeNarrowsTheOperatorList is the per-agent half of the
// gate: an operator can narrow ONE agent without touching the global list, and
// a grant can never widen past it.
func TestAWBProxy_GrantScopeNarrowsTheOperatorList(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "web"})
	w.grantScoped(agentd.PermAWBRead, `{"awb_workspace":["awb"]}`)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.Contains(req.URL, "/api/workspaces") {
			return http.StatusOK, awbWorkspacesJSON("awb", "web")
		}
		return http.StatusOK, "[]"
	}

	res := w.post("/v1/awb/issue/list", map[string]any{})
	out := w.outcome(res)
	assert.Equal(t, []string{"awb"}, out.Workspaces,
		"the echoed set is what THIS caller may reach, not what the operator allows in general")
	assert.Equal(t, []string{"awb"}, awbQuery(t, rec.last(t))["workspace"])

	t.Run("and the workspace the operator allows but the grant does not is refused", func(t *testing.T) {
		res := w.post("/v1/awb/issue/show", map[string]any{"id": "web-a3f9c1"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "workspace_out_of_scope",
			"the refusal must name the list that actually excluded it")
	})
}

// TestAWBProxy_ScopedGrantIsTheWholePolicyWithNoOperatorList mirrors the Linear
// proxy: a scope-only posture is supported, and it is the only way an unscoped
// grant is refused while a scoped one works.
func TestAWBProxy_ScopedGrantIsTheWholePolicyWithNoOperatorList(t *testing.T) {
	w, rec := awbWorld(t, []string{})
	w.grantScoped(agentd.PermAWBRead, `{"awb_workspace":["awb"]}`)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, awbIssueJSON("awb-a3f9c1", "awb")
	}

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f9c1"})
	out := w.outcome(res)
	assert.Equal(t, []string{"awb"}, out.Workspaces)
}

// --- the review's three findings ---

// TestAWBProxy_WhoamiKeepsOutOfScopeWorkspacesToTheirKey is the discovery verb's
// own share of the workspace gate.
//
// `whoami` exists so a refused agent can tell its human which key to add, so an
// unreachable workspace's KEY has to be in the answer — every refusal already
// names the operator's list anyway. What must NOT be there is what the workspace
// contains: its name, its issue count, and the account's access in it are
// facts about a workspace this caller may not read.
func TestAWBProxy_WhoamiKeepsOutOfScopeWorkspacesToTheirKey(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb", "acquisition"})
	w.grantScoped(agentd.PermAWBRead, `{"awb_workspace":["awb"]}`)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		switch {
		case strings.Contains(req.URL, "/api/identity"):
			return http.StatusOK, `{"identity":"tclaude-bot"}`
		case strings.Contains(req.URL, "/api/users/"):
			return http.StatusOK, `{"name":"tclaude-bot","workspace_admin":true,"user_admin":false,` +
				`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z",` +
				`"workspaces":[{"workspace":"awb","user":"tclaude-bot","access":"regular"},` +
				`{"workspace":"acquisition","user":"tclaude-bot","access":"admin"}]}`
		}
		return http.StatusOK, `[{"key":"awb","name":"Agent Work Board","description":"",` +
			`"active_issues":2,"created_at":"2026-08-26T09:12:03.412Z",` +
			`"updated_at":"2026-08-26T09:12:03.412Z"},` +
			`{"key":"acquisition","name":"Workspace Bluebird","description":"","active_issues":47,` +
			`"created_at":"2026-08-26T09:12:03.412Z","updated_at":"2026-08-26T09:12:03.412Z"}]`
	}

	for _, compact := range []bool{false, true} {
		res := w.post("/v1/awb/whoami", map[string]any{"compact": compact})
		out := w.outcome(res)
		body := string(out.JSON) + out.Text

		assert.Contains(t, body, "acquisition",
			"the KEY is the diagnostic: the agent has to be able to name what to ask for")
		assert.NotContains(t, body, "Workspace Bluebird",
			"an out-of-scope workspace's NAME describes it, and the gate says this caller may not read it")
		assert.NotContains(t, body, "47",
			"nor its issue count")
		assert.NotContains(t, body, "admin",
			"nor the account's access in it")
		assert.Contains(t, body, "Agent Work Board",
			"a workspace the caller MAY reach is still described in full")
	}
}

// TestAWBProxy_AttachGetRefusesAShortTransfer is the failure mode a caller
// cannot notice for itself: it writes a file and reads it as the attachment.
//
// AWB records a size and serves the content uncompressed precisely so that a
// truncated transfer is detectable, so the proxy checks rather than trusts.
func TestAWBProxy_AttachGetRefusesAShortTransfer(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(req agentd.AWBProxyRequest) (int, string) {
		if strings.HasSuffix(req.URL, "/content") {
			return http.StatusOK, "short"
		}
		return http.StatusOK, `{"issue":"awb-a3f9c1","name":"trace.txt",` +
			`"content_type":"text/plain","size":4096,"sha256":"9f86d0",` +
			`"created_at":"2026-08-26T09:12:03.412Z"}`
	}

	res := w.post("/v1/awb/attach/get", map[string]any{"id": "awb-a3f9c1", "name": "trace.txt"})
	assert.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "truncated")
	assert.NotContains(t, res.Body.String(), "short",
		"a partial transfer must not be returned as though it were the file")
}

// awbUnstubbedWorld is awbWorld without the transport stub, so the REAL
// outbound path — the one that decides what the daemon will read from a
// server — is the thing under test.
func awbUnstubbedWorld(t *testing.T, url string) *awbFlow {
	t.Helper()
	f := newFlow(t)
	f.HaveConvWithTitle(awbProxyTestConv, "ticket-worker")
	f.HaveEnrolledAgent(awbProxyTestConv)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{
		AWBProxy: &config.AWBProxyConfig{
			URL: url, Username: "tclaude-bot", AllowedWorkspaces: []string{"awb"},
		},
	}}))
	t.Setenv("AWB_PASSWORD", "hunter2")
	return &awbFlow{t: t, flow: f}
}

// TestAWBProxy_OversizedResponseIsDetectedRatherThanTruncated covers the same
// hazard one layer down: a plain io.LimitReader hands back the first N bytes
// with no error, which reads exactly like a complete short response.
func TestAWBProxy_OversizedResponseIsDetectedRatherThanTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		// Comfortably past maxAWBResponseBytes.
		_, _ = rw.Write([]byte(`{"pad":"`))
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for i := 0; i < 17; i++ {
			if _, err := rw.Write(chunk); err != nil {
				return
			}
		}
		_, _ = rw.Write([]byte(`"}`))
	}))
	defer srv.Close()

	w := awbUnstubbedWorld(t, srv.URL)
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/show", map[string]any{"id": "awb-a3f9c1"})
	assert.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "awb_unreachable",
		"an over-long response must be an error, not the first 16 MiB of one")
}

// TestAWBProxy_AnOversizedBodyIsBoundedBeforeThePermissionGate is the audit
// middleware's share of this surface.
//
// The buffering happens in middleware, ahead of both the handler's own
// MaxBytesReader and its permission gate. Attachment upload is the route that
// makes that ordering matter: it legitimately carries megabytes, so "audited
// bodies are small" — the assumption bufferAuditBody was written under — is no
// longer true here, and an ungranted caller must not be able to make the daemon
// hold whatever it sends.
func TestAWBProxy_AnOversizedBodyIsBoundedBeforeThePermissionGate(t *testing.T) {
	t.Run("an ungranted caller is still refused", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"})
		res := w.post("/v1/awb/attach/add", map[string]any{
			"id": "awb-a3f9c1", "name": "big.bin",
			"content": bytes.Repeat([]byte("x"), 2<<20),
		})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})

	t.Run("and a granted one still receives the whole body", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBWrite)
		rec.response = func(agentd.AWBProxyRequest) (int, string) {
			return http.StatusOK, `{"issue":"awb-a3f9c1","name":"big.bin",` +
				`"content_type":"application/octet-stream","size":2097152,"sha256":"9f86d0",` +
				`"created_at":"2026-08-26T09:12:03.412Z"}`
		}
		// Past the audit buffer's cap and under the handler's own, so the body
		// travels the rewound path rather than the buffered one — the case
		// where a truncating middleware would corrupt an upload silently.
		content := bytes.Repeat([]byte("x"), 2<<20)
		res := w.post("/v1/awb/attach/add", map[string]any{
			"id": "awb-a3f9c1", "name": "big.bin", "content": content,
		})
		w.outcome(res)
		assert.Equal(t, content, rec.only(t).Body,
			"rewinding for audit must hand the handler the same bytes it would have read")
	})

	t.Run("past the handler's own cap it is a bounded refusal", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBWrite)
		res := w.post("/v1/awb/attach/add", map[string]any{
			"id": "awb-a3f9c1", "name": "big.bin",
			"content": bytes.Repeat([]byte("x"), 12<<20),
		})
		assert.Equal(t, http.StatusBadRequest, res.Code, "code=%d", res.Code)
		assert.False(t, rec.sawAnyCall())
	})
}

// --- comments ---

func awbActivityJSON(id int, issue, action, body string) string {
	return `{"id":` + strconv.Itoa(id) + `,"issue":"` + issue + `","kind":"comment",` +
		`"actor":"tclaude-bot","body":"` + body + `","action":"` + action + `","changes":[],` +
		`"created_at":"2026-08-26T09:12:03.412Z"}`
}

// TestAWBProxy_CommentAddIsAWrite — a comment is published under the operator's
// account, so reading an issue must not confer the ability to write on it.
func TestAWBProxy_CommentAddIsAWrite(t *testing.T) {
	t.Run("proxy.awb.read does not confer it", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBRead)
		res := w.post("/v1/awb/comment/add",
			map[string]any{"id": "awb-a3f9c1", "body": "done"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})

	t.Run("and the operator's ceiling applies too", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}) // allow_write defaults to false
		w.grant(agentd.PermAWBWrite)
		res := w.post("/v1/awb/comment/add",
			map[string]any{"id": "awb-a3f9c1", "body": "done"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "awb_write_disabled")
		assert.False(t, rec.sawAnyCall())
	})
}

func TestAWBProxy_CommentAddSendsAWBsOwnBody(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusCreated, awbActivityJSON(42, "awb-a3f9c1", "", "Reproduced.")
	}

	res := w.post("/v1/awb/comment/add", map[string]any{
		"id": "awb-a3f9c1", "body": "Reproduced.\nOn an empty token stream.", "compact": true,
	})
	out := w.outcome(res)
	assert.Empty(t, out.Text, "awb prints nothing on a successful comment")

	call := rec.only(t)
	assert.Equal(t, http.MethodPost, call.Method)
	assert.Equal(t, "https://awb.example/api/issues/awb-a3f9c1/comments", call.URL)
	assert.Equal(t, "application/json", call.ContentType)
	assert.JSONEq(t, `{"body":"Reproduced.\nOn an empty token stream."}`, string(call.Body),
		"the body is stored byte for byte, so it must travel unaltered")
}

// TestAWBProxy_CommentAddRefusesAnEmptyBody — an empty comment is an entry in
// the timeline that says nothing, and the timeline is append-only.
func TestAWBProxy_CommentAddRefusesAnEmptyBody(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)

	for _, blank := range []string{"", "   ", "\n\t"} {
		res := w.post("/v1/awb/comment/add", map[string]any{"id": "awb-a3f9c1", "body": blank})
		assert.Equal(t, http.StatusBadRequest, res.Code, "%q: body=%s", blank, res.Body.String())
	}
	assert.False(t, rec.sawAnyCall(), "the cheap gate runs before the network")
}

// TestAWBProxy_CommentAddIsGatedByWorkspace — the same gate as every other verb,
// checked before the call and again on the issue AWB names in its answer.
func TestAWBProxy_CommentAddIsGatedByWorkspace(t *testing.T) {
	t.Run("before the call", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBWrite)
		res := w.post("/v1/awb/comment/add",
			map[string]any{"id": "secret-a3f9c1", "body": "hello"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})

	t.Run("and again on the entry AWB returned", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
		w.grant(agentd.PermAWBWrite)
		rec.response = func(agentd.AWBProxyRequest) (int, string) {
			return http.StatusCreated, awbActivityJSON(42, "secret-9", "", "landed elsewhere")
		}
		res := w.post("/v1/awb/comment/add", map[string]any{"id": "awb-a3f", "body": "hello"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.NotContains(t, res.Body.String(), "landed elsewhere")
	})
}

// TestAWBProxy_CommentListNarrowsTheTimelineToComments is what makes
// `comment list` that verb rather than the whole activity stream.
func TestAWBProxy_CommentListNarrowsTheTimelineToComments(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, "[" +
			awbActivityJSON(43, "awb-a3f9c1", "closed", "Guard against empty token stream") + "," +
			awbActivityJSON(42, "awb-a3f9c1", "", "Reproduced.") + "]"
	}

	res := w.post("/v1/awb/comment/list",
		map[string]any{"id": "awb-a3f9c1", "compact": true, "limit": 10, "offset": 5})
	out := w.outcome(res)

	call := rec.only(t)
	assert.Equal(t, http.MethodGet, call.Method)
	u, err := url.Parse(call.URL)
	require.NoError(t, err)
	assert.Equal(t, "/api/issues/awb-a3f9c1/activity", u.Path,
		"comments are a view of the one timeline, not a separate store")
	assert.Equal(t, "comment", u.Query().Get("kind"))
	assert.Equal(t, "10", u.Query().Get("limit"))
	assert.Equal(t, "5", u.Query().Get("offset"))

	assert.Equal(t,
		"43 2026-08-26T09:12:03.412Z comment @tclaude-bot closed \"Guard against empty token stream\"\n"+
			"42 2026-08-26T09:12:03.412Z comment @tclaude-bot \"Reproduced.\"\n",
		out.Text,
		"a close reason reads back here, as a comment whose action is closed")
}

// TestAWBProxy_CommentListIsBounded: awb returns every entry by default, and
// these land in an agent's context.
func TestAWBProxy_CommentListIsBounded(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) { return http.StatusOK, "[]" }

	res := w.post("/v1/awb/comment/list", map[string]any{"id": "awb-a3f9c1"})
	out := w.outcome(res)
	assert.Equal(t, "[]", string(out.JSON), "an issue with no comments renders as [], never null")
	assert.Equal(t, "50", awbQuery(t, rec.only(t)).Get("limit"))

	t.Run("and a silly offset is refused rather than spent", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		res := w.post("/v1/awb/comment/list",
			map[string]any{"id": "awb-a3f9c1", "offset": -1})
		assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnyCall())
	})
}

// TestAWBProxy_CommentListIsGatedByWorkspace — reading a discussion is reading
// the issue.
func TestAWBProxy_CommentListIsGatedByWorkspace(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	res := w.post("/v1/awb/comment/list", map[string]any{"id": "secret-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())
}

// --- activity ---

func awbChangeJSON(id int, issue, action string) string {
	return `{"id":` + strconv.Itoa(id) + `,"issue":"` + issue + `","kind":"change",` +
		`"actor":"tclaude-bot","body":"","action":"` + action + `",` +
		`"changes":[{"field":"status","from":"open","to":"closed"}],` +
		`"created_at":"2026-08-26T09:14:00.000Z"}`
}

// TestAWBProxy_ActivityReturnsTheWholeTimeline is what distinguishes `activity`
// from `comment list`: the change records are the half comments leave out.
func TestAWBProxy_ActivityReturnsTheWholeTimeline(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, "[" +
			awbChangeJSON(44, "awb-a3f9c1", "closed") + "," +
			awbActivityJSON(42, "awb-a3f9c1", "", "Reproduced.") + "]"
	}

	res := w.post("/v1/awb/activity/list", map[string]any{"id": "awb-a3f9c1", "compact": true})
	out := w.outcome(res)

	q := awbQuery(t, rec.only(t))
	assert.Empty(t, q.Get("kind"), "no kind means the whole timeline")
	assert.Equal(t, "50", q.Get("limit"))
	assert.Equal(t,
		"44 2026-08-26T09:14:00.000Z change @tclaude-bot closed "+
			`[{"field":"status","from":"open","to":"closed"}]`+"\n"+
			`42 2026-08-26T09:12:03.412Z comment @tclaude-bot "Reproduced."`+"\n",
		out.Text)
}

func TestAWBProxy_ActivityNarrowsByKind(t *testing.T) {
	for _, kind := range []string{"comment", "change"} {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		rec.response = func(agentd.AWBProxyRequest) (int, string) { return http.StatusOK, "[]" }

		res := w.post("/v1/awb/activity/list", map[string]any{"id": "awb-a3f9c1", "kind": kind})
		w.outcome(res)
		assert.Equal(t, kind, awbQuery(t, rec.only(t)).Get("kind"))
	}

	t.Run("an unknown kind is refused with the vocabulary", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		res := w.post("/v1/awb/activity/list",
			map[string]any{"id": "awb-a3f9c1", "kind": "changes"})
		assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "comment, change")
		assert.False(t, rec.sawAnyCall())
	})
}

func TestAWBProxy_ActivityIsGatedByWorkspace(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)
	res := w.post("/v1/awb/activity/list", map[string]any{"id": "secret-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall())
}

// TestAWBProxy_ActivityIsAReadNotAWrite — reading a timeline changes nothing,
// so it must not require the write slug.
func TestAWBProxy_ActivityIsAReadNotAWrite(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	res := w.post("/v1/awb/activity/list", map[string]any{"id": "awb-a3f9c1"})
	assert.Equal(t, http.StatusForbidden, res.Code, "an ungranted caller is refused")
	assert.False(t, rec.sawAnyCall())

	w.grant(agentd.PermAWBRead)
	rec.response = func(agentd.AWBProxyRequest) (int, string) { return http.StatusOK, "[]" }
	w.outcome(w.post("/v1/awb/activity/list", map[string]any{"id": "awb-a3f9c1"}))
}

// --- the review's findings ---

// TestAWBProxy_SearchTermsAreValidatedBeforeTheNetwork keeps the rule the rest
// of the listing path already follows: a malformed request must not spend a
// call on the operator's account to reach its refusal. An unfiltered listing
// resolves the server's workspace list first, so the terms have to be checked
// ahead of that.
func TestAWBProxy_SearchTermsAreValidatedBeforeTheNetwork(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"})
	w.grant(agentd.PermAWBRead)

	res := w.post("/v1/awb/issue/search", map[string]any{"terms": []string{"  "}})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnyCall(),
		"not even the /api/workspaces resolution should have happened")
}

// TestAWBProxy_AttachUploadIsNotBufferedForAudit is the second half of the
// audit-body story.
//
// Capping the buffer bounded the damage; not buffering at all removes it. The
// AWB describers name their verb from the path, so there is nothing to read —
// and the read would otherwise happen before the permission gate, for a caller
// that may be refused.
func TestAWBProxy_AttachUploadIsNotBufferedForAudit(t *testing.T) {
	w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
	w.grant(agentd.PermAWBWrite)
	rec.response = func(agentd.AWBProxyRequest) (int, string) {
		return http.StatusOK, `{"issue":"awb-a3f9c1","name":"big.bin",` +
			`"content_type":"application/octet-stream","size":2097152,"sha256":"9f86d0",` +
			`"created_at":"2026-08-26T09:12:03.412Z"}`
	}

	// Past the old audit cap, so a buffering middleware would have to rewind it.
	// The handler must still receive every byte either way.
	content := bytes.Repeat([]byte("x"), 2<<20)
	res := w.post("/v1/awb/attach/add", map[string]any{
		"id": "awb-a3f9c1", "name": "big.bin", "content": content,
	})
	w.outcome(res)
	assert.Equal(t, content, rec.only(t).Body,
		"skipping the audit buffer must not change what the handler reads")
}

// TestAWBProxy_NonSearchListingsRefuseTerms — only `search` matches text, and
// the other three have no way to.
//
// Dropping the field silently would answer a caller that asked to NARROW a
// listing with the wide one, confidently. The proxy refuses it for the same
// reason it refuses a status filter on `ready`: a filter a verb cannot honour
// is a usage error, not a field to ignore.
func TestAWBProxy_NonSearchListingsRefuseTerms(t *testing.T) {
	for _, path := range []string{
		"/v1/awb/issue/list", "/v1/awb/issue/ready", "/v1/awb/issue/blocked",
	} {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		res := w.post(path, map[string]any{"terms": []string{"credential rotation"}})
		assert.Equal(t, http.StatusBadRequest, res.Code, "%s: body=%s", path, res.Body.String())
		assert.Contains(t, res.Body.String(), "search", "the refusal names the verb that does")
		assert.False(t, rec.sawAnyCall(),
			"%s: refused before the workspace resolution spends a call", path)
	}

	t.Run("a blank term is not a term, so it is not a refusal either", func(t *testing.T) {
		w, rec := awbWorld(t, []string{"awb"})
		w.grant(agentd.PermAWBRead)
		rec.response = func(req agentd.AWBProxyRequest) (int, string) {
			if strings.Contains(req.URL, "/api/workspaces") {
				return http.StatusOK, awbWorkspacesJSON("awb")
			}
			return http.StatusOK, "[]"
		}
		res := w.post("/v1/awb/issue/list", map[string]any{"terms": []string{"", "  "}})
		w.outcome(res)
	})
}
