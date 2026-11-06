package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// awbIssueVerbs are the proxy routes addressed by one issue id, each with the
// smallest body that is otherwise valid, so the id is the only thing wrong.
var awbIssueVerbs = []struct {
	path  string
	write bool
	body  map[string]any
}{
	{"/v1/awb/issue/show", false, nil},
	{"/v1/awb/dep/tree", false, nil},
	{"/v1/awb/attach/list", false, nil},
	{"/v1/awb/issue/update", true, map[string]any{"title": "renamed"}},
	{"/v1/awb/issue/claim", true, nil},
	{"/v1/awb/issue/release", true, nil},
	{"/v1/awb/issue/close", true, nil},
	{"/v1/awb/issue/reopen", true, nil},
	{"/v1/awb/issue/make-ready", true, nil},
	{"/v1/awb/issue/delete", true, map[string]any{"force": true}},
	{"/v1/awb/label/add", true, map[string]any{"label": "x"}},
	{"/v1/awb/label/rm", true, map[string]any{"label": "x"}},
	{"/v1/awb/dep/add", true, map[string]any{"type": "blocked-by", "other": "awb-b4e0d2"}},
	{"/v1/awb/dep/rm", true, map[string]any{"type": "blocked-by", "other": "awb-b4e0d2"}},
	{"/v1/awb/comment/add", true, map[string]any{"body": "progress"}},
	{"/v1/awb/attach/add", true, map[string]any{"name": "notes.md", "content": []byte("hi")}},
}

// Every verb addressed by one issue gates that id before anything reaches
// AWB: a bare hash names no workspace (400), and a workspace off the allow
// list is refused (403).
func TestAWBProxy_EveryIssueVerbGatesItsIDBeforeTheNetwork(t *testing.T) {
	for _, v := range awbIssueVerbs {
		t.Run(v.path, func(t *testing.T) {
			w, rec := awbWorld(t, []string{"awb"}, func(c *config.AWBProxyConfig) { c.AllowWrite = true })
			w.grant(agentd.PermAWBRead)
			w.grant(agentd.PermAWBWrite)
			for _, c := range []struct {
				id   string
				code int
				want string
			}{
				{"a3f9c1", http.StatusBadRequest, "A bare hash is not accepted here"},
				{"secret-a3f9c1", http.StatusForbidden, "workspace_not_allowed"},
			} {
				body := map[string]any{"id": c.id}
				for k, val := range v.body {
					body[k] = val
				}
				res := w.post(v.path, body)
				assert.Equal(t, c.code, res.Code, "%s id=%s: %s", v.path, c.id, res.Body.String())
				assert.Contains(t, res.Body.String(), c.want, "refused by the id gate itself")
			}
			assert.False(t, rec.sawAnyCall(), "the id is judged before any AWB call")
		})
	}
}
