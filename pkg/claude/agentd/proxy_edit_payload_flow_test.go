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

// `pr edit` (GitHub) and `mr edit` (GitLab) take a title and/or a body, and
// send only what was given: a trimmed title, the body as written. The body is
// "body" to GitHub and "description" to GitLab (awb tcl-eqwwqv).

type editProxy struct {
	name, path, bodyKey string
	// world returns the flow, and the payloads and number of upstream calls so far.
	world func(t *testing.T) (*testharness.Flow, func() []map[string]any)
}

func editProxies() []editProxy {
	return []editProxy{
		{"github", "/v1/github/pr/edit", "body", func(t *testing.T) (*testharness.Flow, func() []map[string]any) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			gh.def = ghCanned{Status: 200, Body: `{"html_url":"https://github.com/tofutools/tclaude/pull/1"}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubWrite, "test"))
			return f, func() []map[string]any {
				var out []map[string]any
				for _, c := range gh.requests() {
					out = append(out, jsonBody(t, c))
				}
				return out
			}
		}},
		{"gitlab", "/v1/gitlab/mr/edit", "description", func(t *testing.T) (*testharness.Flow, func() []map[string]any) {
			f, _, gl := glWorld(t, []string{"gitlab.contoso.test"})
			gl.def = glCanned{Status: 200, Body: `{"web_url":"https://gitlab.contoso.test/contoso/sub/player/-/merge_requests/1"}`}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))
			return f, func() []map[string]any {
				var out []map[string]any
				for _, c := range gl.requests() {
					out = append(out, glJSONBody(t, c))
				}
				return out
			}
		}},
	}
}

func TestProxyEditSendsOnlyWhatWasGiven(t *testing.T) {
	for _, p := range editProxies() {
		for _, tc := range []struct {
			name  string
			req   map[string]any
			wantT any // the title sent, nil when none
			wantB any // the body sent, nil when none
		}{
			{"title only", map[string]any{"title": "  New title  "}, "New title", nil},
			{"body only", map[string]any{"body": "  as written\n"}, nil, "  as written\n"},
			{"both", map[string]any{"title": "T", "body": "B"}, "T", "B"},
			{"blank title with a body", map[string]any{"title": "   ", "body": "B"}, nil, "B"},
		} {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				f, payloads := p.world(t)
				tc.req["number"] = 1
				res := gitProxyPost(t, f, p.path, tc.req)
				require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
				sent := payloads()
				require.Len(t, sent, 1)
				want := map[string]any{}
				if tc.wantT != nil {
					want["title"] = tc.wantT
				}
				if tc.wantB != nil {
					want[p.bodyKey] = tc.wantB
				}
				assert.Equal(t, want, sent[0])
			})
		}
	}
}

func TestProxyEditRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  map[string]any
		want string // the error message
	}{
		{"nothing", map[string]any{}, "nothing to edit: pass a title, a body, or both"},
		{"blank title and body", map[string]any{"title": " ", "body": " \n"}, "nothing to edit: pass a title, a body, or both"},
		{"dash title", map[string]any{"title": "-x"}, ""},
		{"long title", map[string]any{"title": strings.Repeat("t", 1000)}, ""},
		{"long body", map[string]any{"title": "ok", "body": strings.Repeat("b", 1<<20)}, ""},
	} {
		var bodies []string
		for _, p := range editProxies() {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				f, payloads := p.world(t)
				tc.req["number"] = 1
				res := gitProxyPost(t, f, p.path, tc.req)
				assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
				assert.Empty(t, payloads(), "a refused edit is not sent")
				var body struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}
				require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
				assert.Equal(t, "invalid_arg", body.Code)
				if tc.want != "" {
					assert.Equal(t, tc.want, body.Error)
				}
				bodies = append(bodies, res.Body.String())
			})
		}
		if len(bodies) == 2 {
			assert.Equal(t, bodies[0], bodies[1], "%s: both proxies refuse alike", tc.name)
		}
	}
}
