package agentd_test

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// githubproxy_audit_flow_test.go pins the thing that makes the GitHub proxy's
// reads POSTs at all.
//
// Every one of these routes spends the OPERATOR's GitHub credential against
// what may be a private repository. The design answer is that they are POSTs
// so the audit middleware records them — "this agent read the issue list as
// me" belongs in the trail beside "this agent opened a PR as me".
//
// The failure mode is silent in both directions. A route absent from
// auditedGitHubProxyVerbs has its verb cleared by describeGitHubProxy and its
// row dropped by recordAuditRow, while the handler still computes an audit
// detail that is then discarded — so the call looks audited from the handler
// and leaves nothing behind. Three consecutive verbs were added without
// noticing.

// ghProxyRoutePattern matches the route registrations in serve.go.
var ghProxyRoutePattern = regexp.MustCompile(`"POST (/v1/github/([a-z]+)/([a-z-]+))"`)

// TestAuditCoversEveryGitHubProxyRoute reads the routes serve.go REGISTERS and
// requires each to be classifiable.
//
// Scanning the source is deliberate. A table of routes maintained by hand in
// this file would have exactly the gap it is meant to catch: someone adding a
// route and forgetting the audit map is equally likely to forget the table.
// Deriving the list from the registration site means the only way to add an
// unaudited GitHub proxy route is to make this test fail.
func TestAuditCoversEveryGitHubProxyRoute(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	require.NoError(t, err, "this test derives its expectations from the route registrations")

	matches := ghProxyRoutePattern.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "found no /v1/github/ routes — has the registration moved?")

	var unaudited []string
	for _, m := range matches {
		path, resource, action := m[1], m[2], m[3]
		if !agentd.AuditedGitHubProxyVerbForTest(resource + "." + action) {
			unaudited = append(unaudited, path)
		}
	}
	sort.Strings(unaudited)
	assert.Empty(t, unaudited,
		"these routes spend the operator's GitHub credential and would write NO audit row; "+
			"add %q to auditedGitHubProxyVerbs in audit.go", unaudited)
}

// TestGHProxy_ReadsLandInTheAuditTrail is the end-to-end half: the map entry
// exists AND a row actually reaches the database, carrying the repository and
// the exit code but none of the content that was read.
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
			f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
			const secret = "PRIVATE-REPO-CONTENT-SHOULD-NOT-BE-AUDITED"
			rec.gh = agentd.ProxyResult{Stdout: secret}
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
