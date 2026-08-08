package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon resolves the spawn dialog's identity and birth-time access fields
// down the profile tier stack, and reads PRESENCE on the wire as "this caller
// has spoken". Every one of those fields carries `omitempty`, so a Go client
// that merely assigns a zero value produces a request with the key MISSING —
// which is the opposite instruction. These pin the State* / MarshalJSON pair
// that lets a Go client say "false" and "empty" out loud.

// fieldsOf marshals a request and returns its top-level keys' raw values, so a
// test can tell an absent key from one carrying a zero value.
func fieldsOf(t *testing.T, r SpawnRequest) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(r)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	return fields
}

// An untouched request keeps its historical shape: zero values stay off the
// wire, which is exactly what invites the daemon's profile tiers to answer.
func TestSpawnRequestOmitsUnstatedZeroValues(t *testing.T) {
	fields := fieldsOf(t, SpawnRequest{Profile: "reviewer-kit"})
	for _, key := range []string{"name", "role", "descr", "initial_message", "auto_focus", "is_owner", "permission_overrides"} {
		assert.NotContains(t, fields, key, "an unstated %s must stay off the wire", key)
	}
	assert.Equal(t, json.RawMessage(`"reviewer-kit"`), fields["profile"])
}

// A stated zero value survives the omitempty tags — without this a client whose
// dialog shows the field could never say "no", only "you decide".
func TestSpawnRequestStatedZeroValuesReachTheWire(t *testing.T) {
	var req SpawnRequest
	req.StateName("")
	req.StateInitialMessage("")
	req.StateIsOwner(false)

	fields := fieldsOf(t, req)
	assert.Equal(t, json.RawMessage(`""`), fields["name"], "an emptied name box is an answer")
	assert.Equal(t, json.RawMessage(`""`), fields["initial_message"], "an emptied brief is an answer")
	assert.Equal(t, json.RawMessage(`false`), fields["is_owner"], "an unticked owner box is an answer")
}

// The bits survive a wire round-trip in both directions: what a client states,
// the daemon reads back as stated.
func TestSpawnRequestPresenceSurvivesRoundTrip(t *testing.T) {
	var sent SpawnRequest
	sent.StateName("reviewer")
	sent.StateIsOwner(true)

	data, err := json.Marshal(sent)
	require.NoError(t, err)
	var got SpawnRequest
	require.NoError(t, json.Unmarshal(data, &got))

	assert.True(t, got.NameSpecified())
	assert.Equal(t, "reviewer", got.Name)
	assert.True(t, got.IsOwnerSpecified())
	assert.True(t, got.IsOwner)
	assert.False(t, got.RoleSpecified(), "a field nobody stated stays unstated")
	assert.False(t, got.PermissionOverridesSpecified())
}

// The pre-existing auto_review / trust_dir bits still work: this change
// generalised their mechanism rather than replacing it.
func TestSpawnRequestKeepsExplicitAutoReviewAndTrustDirFalse(t *testing.T) {
	var decoded SpawnRequest
	require.NoError(t, json.Unmarshal([]byte(`{"auto_review":false,"trust_dir":false}`), &decoded))
	require.True(t, decoded.AutoReviewSpecified())
	require.True(t, decoded.TrustDirSpecified())

	fields := fieldsOf(t, decoded)
	assert.Equal(t, json.RawMessage(`false`), fields["auto_review"])
	assert.Equal(t, json.RawMessage(`false`), fields["trust_dir"])
}

// A nil overrides map has to encode as {} rather than null: null decodes back
// to "absent" on the daemon's presence check, so it would silently become the
// invitation it was meant to refuse.
func TestSpawnRequestNilStatedOverridesEncodeAsEmptyObject(t *testing.T) {
	var req SpawnRequest
	req.PermissionOverrides = nil
	req.permissionOverridesSpecified = true

	fields := fieldsOf(t, req)
	assert.Equal(t, json.RawMessage(`{}`), fields["permission_overrides"])

	var got SpawnRequest
	data, err := json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &got))
	assert.True(t, got.PermissionOverridesSpecified(), "{} reads back as stated; null would not")
}
