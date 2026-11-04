package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The AWB and Linear proxies resolve a scoped grant into the set of keys
// (workspaces, teams) one request may reach, intersected with the operator's
// allow-list. They share one resolver, so every outcome — the two "scope
// authorizes nothing" refusals, the no-overlap refusal, the scope-only and
// narrowing successes, and the unconfigured proxy — must answer alike on both.
// Each row is run against both proxies through the daemon mux.

type scopedProxy struct {
	name string
	dim  string
	// setup builds a world allowing allowed (nil = proxy block absent), grants
	// the read slug with scopeJSON ("" = unscoped), and returns a request that
	// reads one item keyed by key.
	setup func(t *testing.T, allowed []string, scopeJSON string) func(key string) *httptest.ResponseRecorder
	// keyFor turns a lower-case row key into this proxy's spelling.
	keyFor func(string) string
	// noun is how this proxy's refusals name the dimension.
	noun       string
	disabled   string
	scopeEmpty string
	outOfScope string
}

func scopedProxies() []scopedProxy {
	return []scopedProxy{
		{
			name: "awb", dim: "awb_workspace", noun: "workspace",
			disabled: "awb_proxy_disabled", scopeEmpty: "workspace_scope_empty", outOfScope: "workspace_out_of_scope",
			keyFor: func(k string) string { return k },
			setup: func(t *testing.T, allowed []string, scopeJSON string) func(string) *httptest.ResponseRecorder {
				w, rec := awbWorld(t, allowed)
				if scopeJSON == "" {
					w.grant(agentd.PermAWBRead)
				} else {
					w.grantScoped(agentd.PermAWBRead, scopeJSON)
				}
				return func(key string) *httptest.ResponseRecorder {
					rec.response = func(agentd.AWBProxyRequest) (int, string) {
						return http.StatusOK, awbIssueJSON(key+"-a3f9c1", key)
					}
					return w.post("/v1/awb/issue/show", map[string]any{"id": key + "-a3f9c1"})
				}
			},
		},
		{
			name: "linear", dim: "linear_team", noun: "team",
			disabled: "linear_proxy_disabled", scopeEmpty: "team_scope_empty", outOfScope: "team_out_of_scope",
			keyFor: strings.ToUpper,
			setup: func(t *testing.T, allowed []string, scopeJSON string) func(string) *httptest.ResponseRecorder {
				f, rec := linearWorld(t, allowed)
				if scopeJSON == "" {
					require.NoError(t, db.GrantAgentPermission(linearProxyTestConv, agentd.PermLinearRead, "test"))
				} else {
					require.NoError(t, db.GrantAgentPermissionWithScope(
						linearProxyTestConv, agentd.PermLinearRead, scopeJSON, "test"))
				}
				return func(key string) *httptest.ResponseRecorder {
					rec.response = func(linearCall) (int, string) { return http.StatusOK, issueJSON(key+"-1", key) }
					return linearPost(t, f, "/v1/linear/issue/view", map[string]any{"identifier": key + "-1"})
				}
			},
		},
	}
}

func TestProxyScopedGrant_ResolvesAlikeOnAWBAndLinear(t *testing.T) {
	type row struct {
		name    string
		allowed []string // lower-case keys; nil = proxy not configured
		// scope builds the scope JSON from the proxy's dimension and a key
		// speller; nil = unscoped grant.
		scope    func(dim string, k func(string) string) string
		request  string
		status   int
		code     func(p scopedProxy) string
		contains []string
	}
	rows := []row{
		{
			name: "unscoped grant, no operator list: proxy disabled", allowed: []string{},
			request: "aaa", status: http.StatusServiceUnavailable,
			code: func(p scopedProxy) string { return p.disabled },
		},
		{
			name: "unscoped grant reaches the operator's list", allowed: []string{"aaa", "bbb"},
			request: "bbb", status: http.StatusOK,
		},
		{
			name: "scope only, no operator list: the scope is the whole policy", allowed: []string{},
			scope:   func(d string, k func(string) string) string { return `{"` + d + `":["` + k("aaa") + `"]}` },
			request: "aaa", status: http.StatusOK,
		},
		{
			name: "scope narrows the operator's list", allowed: []string{"aaa", "bbb"},
			scope:   func(d string, k func(string) string) string { return `{"` + d + `":["` + k("aaa") + `"]}` },
			request: "bbb", status: http.StatusForbidden,
			code: func(p scopedProxy) string { return p.outOfScope },
		},
		{
			name: "scope and operator list do not overlap", allowed: []string{"aaa"},
			scope:   func(d string, k func(string) string) string { return `{"` + d + `":["` + k("bbb") + `"]}` },
			request: "aaa", status: http.StatusForbidden,
			code:     func(p scopedProxy) string { return p.scopeEmpty },
			contains: []string{"must overlap"},
		},
		{
			name: "scope names only another dimension", allowed: []string{"aaa"},
			scope:   func(string, func(string) string) string { return `{"group":["g1"]}` },
			request: "aaa", status: http.StatusForbidden,
			code:     func(p scopedProxy) string { return p.scopeEmpty },
			contains: []string{"names no"},
		},
		{
			name: "scope names this dimension and constrains another", allowed: []string{"aaa"},
			scope: func(d string, k func(string) string) string {
				return `{"` + d + `":["` + k("aaa") + `"],"group":["g1"]}`
			},
			request: "aaa", status: http.StatusForbidden,
			code:     func(p scopedProxy) string { return p.scopeEmpty },
			contains: []string{"authorizes nothing"},
		},
	}
	for _, p := range scopedProxies() {
		for _, r := range rows {
			t.Run(p.name+"/"+r.name, func(t *testing.T) {
				allowed := make([]string, 0, len(r.allowed))
				for _, k := range r.allowed {
					allowed = append(allowed, p.keyFor(k))
				}
				scopeJSON := ""
				if r.scope != nil {
					scopeJSON = r.scope(p.dim, p.keyFor)
				}
				send := p.setup(t, allowed, scopeJSON)
				res := send(p.keyFor(r.request))
				body := res.Body.String()
				require.Equal(t, r.status, res.Code, "body=%s", body)
				if r.code != nil {
					assert.Contains(t, body, `"`+r.code(p)+`"`, "body=%s", body)
				}
				for _, s := range r.contains {
					assert.Contains(t, body, s, "body=%s", body)
				}
				if r.status != http.StatusOK && r.status != http.StatusServiceUnavailable {
					assert.Contains(t, body, p.noun, "the refusal names the dimension")
				}
			})
		}
	}
}
