package agentd

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// githubproxy_audit_test.go pins the audit classification of the GitHub proxy
// routes. It touches no daemon mux, so it is a unit test rather than a flow
// test; the end-to-end half (a row actually reaching the database) lives in
// githubproxy_audit_flow_test.go.
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

// ghProxyRouteLine matches a registered GitHub proxy route in serve.go without
// assuming its shape. Deliberately permissive: a pattern that only matched the
// segment shape the routes happen to have today would SKIP anything else, and
// skipping is indistinguishable from passing in a guard like this one. Whatever
// it captures must then survive the checks below.
var ghProxyRouteLine = regexp.MustCompile(`"POST (/v1/github/[^"]+)"`)

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

	matches := ghProxyRouteLine.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "found no /v1/github/ routes — has the registration moved?")

	var unclassifiable, unaudited []string
	for _, m := range matches {
		path := m[1]
		// describeGitHubProxy is registered for {github, {resource}, {action}}
		// and looks up "resource.action". A route with any other shape is not
		// merely unlisted below — the describer cannot name it at all, so it
		// could never be audited whatever the map said.
		segs := strings.Split(strings.TrimPrefix(path, "/v1/github/"), "/")
		if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
			unclassifiable = append(unclassifiable, path)
			continue
		}
		if !auditedGitHubProxyVerbs[segs[0]+"."+segs[1]] {
			unaudited = append(unaudited, path)
		}
	}
	sort.Strings(unclassifiable)
	sort.Strings(unaudited)

	assert.Empty(t, unclassifiable,
		"describeGitHubProxy matches /v1/github/{resource}/{action}, so these routes can never be "+
			"audited whatever auditedGitHubProxyVerbs says: %q", unclassifiable)
	assert.Empty(t, unaudited,
		"these routes spend the operator's GitHub credential and would write NO audit row; "+
			"add them to auditedGitHubProxyVerbs in audit.go: %q", unaudited)
}

// TestGHProxyRouteScanSeesHyphenatedSegments guards the guard.
//
// The pattern used to be `([a-z]+)/([a-z-]+)`, which matched every route that
// existed and would have silently skipped a `pull-request/view`. A scanner that
// cannot see a route reports no problem with it, which is the one failure this
// whole test must not have.
func TestGHProxyRouteScanSeesHyphenatedSegments(t *testing.T) {
	const registration = `mux.HandleFunc("POST /v1/github/pull-request/log-failed", h)`
	m := ghProxyRouteLine.FindStringSubmatch(registration)
	require.NotNil(t, m, "a hyphenated resource must not slip past the scan unseen")
	assert.Equal(t, "/v1/github/pull-request/log-failed", m[1])
}
