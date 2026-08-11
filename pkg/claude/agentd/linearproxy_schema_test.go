package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linearproxy_schema_test.go validates every GraphQL document this package
// ships against Linear's REAL schema, with no credential and no code
// generation.
//
// It works because Linear validates a document BEFORE it authenticates:
//
//	valid document, no auth  → AUTHENTICATION_ERROR
//	typo'd field,  no auth   → GRAPHQL_VALIDATION_FAILED
//
// So an unauthenticated request tells us whether our document still matches
// the schema without ever touching a workspace. That is most of what a
// codegen'd client would buy — schema-drift detection — without a 1.3 MB
// vendored SDL or a build step.
//
// It is also why the MUTATION documents are safe to send: Linear rejects them
// at the auth boundary, before execution, so nothing is created. The test
// asserts exactly that, and would fail loudly if Linear ever changed the
// ordering.
//
// A VALUE PASSED IN `variables` IS NOT CHECKED THIS WAY. Verified directly — an
// `issues` query whose $filter names a field that does not exist on IssueFilter
// comes back AUTHENTICATION_ERROR, indistinguishable from a correct one,
// because variables are coerced after authentication.
//
// TestLinearFilterShapesMatchLiveSchema below closes that gap for the filter
// maps, by moving them somewhere validation does reach: a variable's DEFAULT
// value is part of the document and is checked with it. See its own comment.
// The mutation INPUTS are still unchecked — they are assembled field by field
// across a handler rather than by one function a test could call — so changing
// one means re-reading Linear's schema by hand. Its API answers `__type`
// introspection with no credential, which is the cheapest way to do that.
//
// Opt-in and network-dependent, like the dashsnap harness: it is not wired
// into CI. Run it with
//
//	TCLAUDE_LINEAR_SCHEMA_CHECK=1 go test ./pkg/claude/agentd/ -run 'TestLinear.*MatchLiveSchema' -v
func TestLinearQueryDocumentsMatchLiveSchema(t *testing.T) {
	if os.Getenv("TCLAUDE_LINEAR_SCHEMA_CHECK") == "" {
		t.Skip("set TCLAUDE_LINEAR_SCHEMA_CHECK=1 to validate the GraphQL documents against api.linear.app")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for name, doc := range linearProxyDocuments {
		t.Run(name, func(t *testing.T) {
			// No variables. A document is validated for structure and field
			// existence before variable VALUES are considered, so omitting them
			// exercises exactly the drift we care about.
			payload, err := json.Marshal(linearRequest{Query: doc})
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				linearEndpoint, bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			require.NoError(t, err, "could not reach the Linear API")
			defer func() { _ = resp.Body.Close() }()

			var parsed linearResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
			require.NotEmpty(t, parsed.Errors,
				"an unauthenticated request must be refused; got a successful response")

			code := parsed.Errors[0].Extensions.Code
			assert.NotEqual(t, "GRAPHQL_VALIDATION_FAILED", code,
				"document %q no longer matches Linear's schema: %s", name, parsed.Errors[0].text())
			assert.Equal(t, "AUTHENTICATION_ERROR", code,
				"expected the document to validate and then be refused at auth; got %s: %s",
				code, parsed.Errors[0].text())
		})
	}
}

// TestLinearFilterShapesMatchLiveSchema validates the FILTER MAPS — the part
// the test above cannot reach.
//
// The trick is where the filter travels. A value in `variables` is coerced
// after authentication and so is never checked without a credential; a
// variable's DEFAULT value is part of the document and is validated with it. So
// each filter this package builds is inlined as the default of a `$filter`
// variable, and Linear then answers GRAPHQL_VALIDATION_FAILED for a field or
// comparator that does not exist on that filter type.
//
// The filters come from the REAL builders, so this checks what production
// sends rather than a copy of it kept in a test. That means composing a
// document from a value, which the rest of this package must never do — the
// compile-time-constant rule is what makes the proxy's GraphQL surface
// auditable. It is admissible here and nowhere else: nothing authenticates,
// nothing executes, and the assembled document is thrown away.
func TestLinearFilterShapesMatchLiveSchema(t *testing.T) {
	if os.Getenv("TCLAUDE_LINEAR_SCHEMA_CHECK") == "" {
		t.Skip("set TCLAUDE_LINEAR_SCHEMA_CHECK=1 to validate the GraphQL filters against api.linear.app")
	}
	client := &http.Client{Timeout: 30 * time.Second}

	for name, probe := range map[string]struct {
		filterType string
		selection  string
		filter     map[string]any
		wantDrift  bool
	}{
		// The control. A test that can only pass proves nothing, and this one
		// rests on a behaviour of Linear's — that a default value is validated
		// while a variable is not — which could change without notice. If this
		// subtest stops seeing GRAPHQL_VALIDATION_FAILED, every other subtest
		// here has quietly stopped checking anything.
		"control: a field that does not exist": {
			filterType: "IssueFilter",
			selection:  "issues(filter: $filter, first: 1) { nodes { identifier } }",
			filter:     map[string]any{"noSuchField": map[string]any{"eq": "x"}},
			wantDrift:  true,
		},
		// The issue filter every listing verb builds, with all three clauses
		// present so none of them goes unchecked.
		"issues": {
			filterType: "IssueFilter",
			selection:  "issues(filter: $filter, first: 1) { nodes { identifier } }",
			filter:     linearIssueFilter([]string{"TCL", "JOH"}, "In Review", true),
		},
		"project":   {filterType: "ProjectFilter", selection: "projects(filter: $filter, first: 1) { nodes { id } }"},
		"milestone": {filterType: "ProjectMilestoneFilter", selection: "projectMilestones(filter: $filter, first: 1) { nodes { id } }"},
		"label":     {filterType: "IssueLabelFilter", selection: "issueLabels(filter: $filter, first: 1) { nodes { id } }"},
		"user":      {filterType: "UserFilter", selection: "users(filter: $filter, first: 1) { nodes { id } }"},
	} {
		t.Run(name, func(t *testing.T) {
			filter := probe.filter
			if filter == nil {
				filter = liveProbeFilter(t, name)
			}
			// GraphQL default values are written in the schema language, not in
			// JSON: object keys are bare and strings stay quoted. Marshalling and
			// then unquoting the keys is the whole difference.
			encoded, err := json.Marshal(filter)
			require.NoError(t, err)
			literal := graphQLObjectKeys.ReplaceAllString(string(encoded), `$1:`)

			doc := fmt.Sprintf("query FilterShape($filter: %s = %s) { %s }",
				probe.filterType, literal, probe.selection)
			payload, err := json.Marshal(linearRequest{Query: doc})
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				linearEndpoint, bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			require.NoError(t, err, "could not reach the Linear API")
			defer func() { _ = resp.Body.Close() }()

			var parsed linearResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&parsed))
			require.NotEmpty(t, parsed.Errors,
				"an unauthenticated request must be refused; got a successful response")

			code := parsed.Errors[0].Extensions.Code
			if probe.wantDrift {
				require.Equal(t, "GRAPHQL_VALIDATION_FAILED", code,
					"a filter naming a field that does not exist must be REFUSED, or this whole test "+
						"is checking nothing; got %s: %s", code, parsed.Errors[0].text())
				return
			}
			assert.Equal(t, "AUTHENTICATION_ERROR", code,
				"filter %q no longer matches %s: %s\n%s",
				name, probe.filterType, parsed.Errors[0].text(), doc)
		})
	}
}

// liveProbeFilter builds one resolver's filter by running the resolver against
// a stub, so the shape under test is the one production assembles rather than a
// literal kept alongside it.
//
// A resolver's filter is local to it by design — it is built where the values
// that go in it are validated — so the only handle a test has on it is the
// request the resolver would have sent.
func liveProbeFilter(t *testing.T, name string) map[string]any {
	t.Helper()
	stub := &stubLinear{}
	stub.install(t)
	t.Setenv("LINEAR_API_KEY", "lin_api_testkey")
	s := testLinearSession("TCL")
	rt, fault := s.routeFor("TCL")
	require.Nil(t, fault)

	switch name {
	case "project":
		_, _ = s.resolveProjectID(t.Context(), rt, "TCL", "Some project")
	case "milestone":
		_, _ = s.resolveMilestoneID(t.Context(), rt, "8a7d5f6e-1234-4a2b-9c8d-1122334455ff", "Beta")
	case "label":
		_, _ = s.resolveLabelIDs(t.Context(), rt, "TCL", []string{"bug", "needs review"})
	case "user":
		_, _ = s.resolveAssigneeID(t.Context(), rt, "mikael")
	default:
		t.Fatalf("no probe for %q", name)
	}
	require.Len(t, stub.calls, 1, "the probe must have produced exactly one call to read a filter from")
	filter, ok := stub.calls[0].Variables["filter"].(map[string]any)
	require.True(t, ok, "the resolver must send its filter as a variable")
	return filter
}

// graphQLObjectKeys matches the quoted keys of a JSON object, which GraphQL's
// own literal syntax writes bare.
var graphQLObjectKeys = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)":`)
