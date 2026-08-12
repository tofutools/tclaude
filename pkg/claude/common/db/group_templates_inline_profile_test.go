package db

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInlineProfileJSONCoversEveryLaunchField is a STRUCTURAL guard, not a
// value test. The template-local (inline) spawn profile has its own hand-written
// serialization pair, so every field added to db.SpawnProfile has to be added to
// three more places by hand — the JSON struct, the marshal, and the unmarshal.
// Miss one and the daemon accepts the field, validates it, reports 200, and then
// throws it away at the DB boundary: a silent data-loss path with no error
// anywhere. `copilot_api` and `context_window_max` both fell into exactly that
// gap, which is what this guard exists to stop repeating.
//
// The exempt list below is the deliberate part of the design and is what makes
// the guard readable: identity belongs to the template agent, and the three
// spawn-dialog toggles have no meaning for a template deploy. Anything NOT
// listed must round-trip. Adding a field to the exempt list is a decision
// someone has to write down here, which is the point.
func TestInlineProfileJSONCoversEveryLaunchField(t *testing.T) {
	exempt := map[string]bool{
		// Identity + storage: owned by the template agent / the profiles table.
		"ID": true, "Name": true, "Aliases": true,
		"CreatedAt": true, "UpdatedAt": true,
		"Disabled": true, "DisabledReason": true, "OperatorOnly": true,
		"AgentName": true, "Role": true, "RoleRef": true, "RoleRefs": true, "Descr": true, "InitialMessage": true,
		// Spawn-dialog-only toggles: meaningless for a template deploy.
		"SyncWorktree": true, "AutoFocus": true, "IncludeGroupDefaultContext": true,
	}

	inline := reflect.TypeOf(templateInlineProfileJSON{})
	carried := map[string]bool{}
	for i := range inline.NumField() {
		carried[inline.Field(i).Name] = true
	}

	profile := reflect.TypeOf(SpawnProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		if exempt[name] {
			continue
		}
		assert.Truef(t, carried[name],
			"db.SpawnProfile.%s has no templateInlineProfileJSON field: a template-local "+
				"profile carrying it would be accepted, validated, then silently dropped when "+
				"stored. Add it to the struct AND to inlineProfileToJSON/inlineProfileFromJSON, "+
				"or add it to this test's exempt list with a reason.", name)
	}
}

// TestInlineProfileRoundTripsCopilotDrive pins the tri-state specifically: the
// structural guard above catches a MISSING field, but a field present in the
// struct and forgotten in one of the two conversion funcs would still pass it.
// All three states have to survive, because "unset" and "explicitly send-keys"
// are different instructions to the tier stack.
func TestInlineProfileRoundTripsCopilotDrive(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		want *bool
	}{
		{"unset", nil},
		{"api", &on},
		{"send-keys", &off},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := inlineProfileToJSON(&SpawnProfile{Harness: "copilot", CopilotAPI: tc.want})
			require.NotEmpty(t, encoded)
			decoded := inlineProfileFromJSON(encoded)
			require.NotNil(t, decoded)
			if tc.want == nil {
				assert.Nil(t, decoded.CopilotAPI)
				return
			}
			require.NotNil(t, decoded.CopilotAPI, "the drive must survive storage")
			assert.Equal(t, *tc.want, *decoded.CopilotAPI)
		})
	}
}
