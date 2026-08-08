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

// linearproxy_audit_test.go pins the audit classification of the Linear proxy
// routes, exactly as githubproxy_audit_test.go does for the GitHub ones and for
// exactly the same reason.
//
// Every one of these routes spends the OPERATOR's Linear credential against a
// private workspace, which is why even the reads are POSTs: the audit
// middleware records mutating methods only, so a read that was a GET would
// leave no trace of "this agent read the backlog as me".
//
// The failure mode is silent. A route missing from auditedLinearProxyVerbs has
// its verb cleared by the describer and its row dropped by recordAuditRow,
// while the handler still computes an audit detail that is then discarded — so
// the call looks audited from the handler's side and leaves nothing behind.

// linearProxyRouteLine matches a registered Linear proxy route in serve.go
// without assuming its shape. Deliberately permissive: a pattern that only
// matched today's segment count would SKIP anything else, and skipping is
// indistinguishable from passing in a guard like this one.
var linearProxyRouteLine = regexp.MustCompile(`"POST (/v1/linear/[^"]+)"`)

// TestAuditCoversEveryLinearProxyRoute reads the routes serve.go REGISTERS and
// requires each to be classifiable and named.
//
// Scanning the source is deliberate: a hand-kept table in this file would have
// exactly the gap it is meant to catch, since whoever forgets the audit map is
// equally likely to forget the table.
func TestAuditCoversEveryLinearProxyRoute(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	require.NoError(t, err, "this test derives its expectations from the route registrations")

	matches := linearProxyRouteLine.FindAllStringSubmatch(string(source), -1)
	require.NotEmpty(t, matches, "found no /v1/linear/ routes — has the registration moved?")

	var unclassifiable, unaudited []string
	for _, m := range matches {
		path := m[1]
		// Two describers are registered: {linear, {verb}} and
		// {linear, {resource}, {action}}. A path with any other shape cannot be
		// named by either, so it could never be audited whatever the map said.
		segs := strings.Split(strings.TrimPrefix(path, "/v1/linear/"), "/")
		var verb string
		switch len(segs) {
		case 1:
			verb = segs[0]
		case 2:
			verb = segs[0] + "." + segs[1]
		default:
			unclassifiable = append(unclassifiable, path)
			continue
		}
		if strings.Contains(verb, "..") || strings.HasPrefix(verb, ".") || strings.HasSuffix(verb, ".") {
			unclassifiable = append(unclassifiable, path)
			continue
		}
		if !auditedLinearProxyVerbs[verb] {
			unaudited = append(unaudited, path)
		}
	}
	sort.Strings(unclassifiable)
	sort.Strings(unaudited)

	assert.Empty(t, unclassifiable,
		"the linear describers match /v1/linear/{verb} and /v1/linear/{resource}/{action}, so these "+
			"routes can never be audited whatever auditedLinearProxyVerbs says: %q", unclassifiable)
	assert.Empty(t, unaudited,
		"these routes spend the operator's Linear credential and would write NO audit row; "+
			"add them to auditedLinearProxyVerbs: %q", unaudited)
}

// TestAuditedLinearVerbsAreAllRegistered is the converse: an entry in the map
// with no route behind it is dead weight that makes the map look more complete
// than it is.
func TestAuditedLinearVerbsAreAllRegistered(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	require.NoError(t, err)

	registered := map[string]bool{}
	for _, m := range linearProxyRouteLine.FindAllStringSubmatch(string(source), -1) {
		registered[strings.ReplaceAll(strings.TrimPrefix(m[1], "/v1/linear/"), "/", ".")] = true
	}
	var orphaned []string
	for verb := range auditedLinearProxyVerbs {
		if !registered[verb] {
			orphaned = append(orphaned, verb)
		}
	}
	sort.Strings(orphaned)
	assert.Empty(t, orphaned, "auditedLinearProxyVerbs names verbs no route serves: %q", orphaned)
}

// TestDescribeLinearProxyNamesOnlyKnownVerbs pins the clearing behaviour that
// makes the coverage test above meaningful: an unknown verb must leave the row
// unnamed (and therefore dropped), not pass through under its raw path.
func TestDescribeLinearProxyNamesOnlyKnownVerbs(t *testing.T) {
	t.Run("known one-segment verb", func(t *testing.T) {
		c := &auditCtx{vars: map[string]string{"verb": "whoami"}, fields: &auditFields{Verb: "whoami"}}
		describeLinearProxy(c)
		assert.Equal(t, "linear.whoami", c.fields.Verb)
	})

	t.Run("known two-segment verb", func(t *testing.T) {
		c := &auditCtx{
			vars:   map[string]string{"resource": "issue", "action": "comment"},
			fields: &auditFields{Verb: "issue"},
		}
		describeLinearProxyResource(c)
		assert.Equal(t, "linear.issue.comment", c.fields.Verb)
	})

	t.Run("unknown verb is cleared, not passed through", func(t *testing.T) {
		c := &auditCtx{
			vars:   map[string]string{"resource": "issue", "action": "delete"},
			fields: &auditFields{Verb: "issue"},
		}
		describeLinearProxyResource(c)
		assert.Empty(t, c.fields.Verb,
			"an unrecognised verb must be cleared so recordAuditRow drops the row rather than "+
				"recording a half-named one")
	})
}
