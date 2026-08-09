package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// githubproxy_audit_flow_test.go is the end-to-end half of the GitHub proxy's
// audit contract: a row actually reaches the database, through the real daemon
// mux, carrying the repository and the exit code but none of the content read.
//
// The other half — that every registered route is classifiable at all — is a
// unit test in githubproxy_audit_test.go, which needs no mux.

// TestGHProxy_ReadsLandInTheAuditTrail covers the reads specifically. They are
// POSTs for exactly this reason: spending the operator's GitHub credential
// against a private repository is what an operator reviews later, and a read
// that leaves no trace is indistinguishable from one that never happened.
func TestGHProxy_ReadsLandInTheAuditTrail(t *testing.T) {
	for _, tc := range []struct {
		verb string
		path string
		body map[string]any
	}{
		{"github.pr.comments", "/v1/github/pr/comments", map[string]any{"number": 42}},
		{"github.run.list", "/v1/github/run/list", map[string]any{"status": "failure"}},
		{"github.run.log-failed", "/v1/github/run/log-failed", map[string]any{"run_id": 18234567890}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			f, _, gh := ghWorld(t, []string{"github.com/tofutools"})
			const secret = "PRIVATE-REPO-CONTENT-SHOULD-NOT-BE-AUDITED"
			// Every read in this table answers with the secret somewhere in
			// its payload, whichever shape that read's response takes.
			gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
				switch {
				case strings.HasSuffix(req.Path, "/actions/runs/18234567890"):
					return ghCanned{Status: 200, Body: `{"status":"completed","conclusion":"failure"}`}, true
				case strings.HasSuffix(req.Path, "/jobs"):
					return ghCanned{Status: 200, Body: `{"jobs":[]}`}, true
				case strings.HasSuffix(req.Path, "/actions/runs"):
					return ghCanned{Status: 200, Body: `{"workflow_runs":[{"id":1,"display_title":"` + secret + `"}]}`}, true
				}
				return ghCanned{Status: 200,
					Body: `[{"user":{"login":"someone"},"body":"` + secret + `"}]`}, true
			}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

			res := gitProxyPost(t, f, tc.path, tc.body)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

			row := auditRowByVerb(t, tc.verb)
			assert.Contains(t, row.Detail, "tofutools/tclaude",
				"the operator needs to know which repository their credential was spent on")
			assert.Contains(t, row.Detail, "exit=")
			// The whole point of reading these is that they return other
			// people's prose and private CI output. None of it belongs in the
			// audit log.
			assert.NotContains(t, row.Detail, secret)
			assert.NotContains(t, strings.ToLower(row.Detail), "coderabbit")
		})
	}
}
