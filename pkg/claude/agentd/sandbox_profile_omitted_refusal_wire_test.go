package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSandboxProfileOmittedRefusalOmitsAnUnknownAssignment pins the wire
// contract the TCL-913 identity depends on: when the daemon does not KNOW which
// assignment an omitted refusal belongs to, the key must be ABSENT rather than
// present-and-empty.
//
// Raised by a peer reviewing the design, and the reasoning is worth keeping:
// the frontend's sandboxContextLabelFor({}) does NOT return null. It falls past
// group_name, falls past explicit, and returns "global assignment" — so a
// context arriving as {} would render a CONFIDENT WRONG NAME for an assignment
// nobody identified, which is strictly worse than the unnamed wording it
// replaced. Absent degrades to today's behaviour; empty misnames.
//
// The property holds because encoding/json's omitempty drops a map of length
// zero, not merely a nil one, so the bounds-guard-failed path and a context map
// built with no keys serialize identically. That is a property of the FIELD
// TYPE and TAG, and it stops holding silently if either changes — which is why
// it is pinned by execution here rather than reasoned about.
//
// This test is in package agentd deliberately: it marshals the PRODUCTION type.
// A mirror struct declared in the test would keep passing after production's
// tag changed, which is the defect it exists to catch.
func TestSandboxProfileOmittedRefusalOmitsAnUnknownAssignment(t *testing.T) {
	refusal := &sandboxProfileEnforcementRefusal{Kind: "k", Message: "m"}
	for name, entry := range map[string]sandboxProfileOmittedRefusal{
		"guard failed, nothing assigned": {sandboxProfileEnforcementRefusal: refusal},
		"context built with no keys": {
			sandboxProfileEnforcementRefusal: refusal,
			Context:                          map[string]string{},
		},
	} {
		encoded, err := json.Marshal(entry)
		require.NoErrorf(t, err, "%s", name)
		assert.NotContainsf(t, string(encoded), `"context"`,
			"%s: an unknown assignment must omit the key, never send an empty one", name)
	}
	// Positive control: the same shape DOES carry the key when the assignment is
	// known, so the assertions above cannot pass merely because the field never
	// serializes at all.
	known, err := json.Marshal(sandboxProfileOmittedRefusal{
		sandboxProfileEnforcementRefusal: refusal,
		Context:                          map[string]string{"group_name": "crew-10"},
	})
	require.NoError(t, err)
	assert.Contains(t, string(known), `"context":{"group_name":"crew-10"}`,
		"a known assignment must reach the wire")
	// The refusal fields stay FLAT beside it rather than nesting, which is what
	// keeps a client that predates this field reading exactly what it read
	// before.
	assert.Contains(t, string(known), `"kind":"k"`)
}
