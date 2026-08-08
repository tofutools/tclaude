package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
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
// WHAT IT DOES NOT COVER, which matters as much as what it does: Linear
// validates the DOCUMENT before authenticating, but not the VARIABLE VALUES.
// Verified directly — an `issues` query whose $filter names a field that does
// not exist on IssueFilter comes back AUTHENTICATION_ERROR, indistinguishable
// from a correct one. So the filter maps built in linearproxy_handlers.go, and
// the input objects the mutations send, are NOT checked here. Drift in those
// surfaces at execution time as a Linear input error (reported as
// linear_failed), not as a silent wrong answer — but it will not be caught
// before it ships. Changing a filter's shape means re-reading Linear's schema
// by hand.
//
// Opt-in and network-dependent, like the dashsnap harness: it is not wired
// into CI. Run it with
//
//	TCLAUDE_LINEAR_SCHEMA_CHECK=1 go test ./pkg/claude/agentd/ -run TestLinearQueryDocuments -v
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
