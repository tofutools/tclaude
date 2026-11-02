package agentd

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// A new field in the spawn request fails here until someone decides whether an
// external steward may set it.
func TestExternalStewardSpawnFields_EveryRequestFieldIsClassified(t *testing.T) {
	kind := reflect.TypeFor[agent.SpawnRequest]()
	fields := map[string]bool{}
	for i := range kind.NumField() {
		field := kind.Field(i).Name
		fields[field] = true
		_, classified := externalStewardSpawnFields[field]
		assert.Truef(t, classified, "SpawnRequest.%s: add it to externalStewardSpawnFields, false unless the operator's pin doesn't decide it", field)
	}
	for field := range externalStewardSpawnFields {
		assert.Truef(t, fields[field], "externalStewardSpawnFields names %s, which SpawnRequest doesn't have", field)
	}
}

// Every field outside the short list is refused, by reflection rather than a
// hand-kept list of attacks.
func TestExternalStewardSpawnFields_DefaultDeny(t *testing.T) {
	kind := reflect.TypeFor[agent.SpawnRequest]()
	allowed := 0
	for i := range kind.NumField() {
		field := kind.Field(i)
		var body agent.SpawnRequest
		value := reflect.ValueOf(&body).Elem().Field(i)
		if !value.CanSet() {
			continue // Set by the decoder, alongside the exported field it marks.
		}
		switch value.Kind() {
		case reflect.Bool:
			value.SetBool(true)
		case reflect.String:
			value.SetString("x")
		case reflect.Int, reflect.Int64:
			value.SetInt(1)
		case reflect.Slice:
			value.Set(reflect.MakeSlice(field.Type, 1, 1))
		case reflect.Map:
			value.Set(reflect.MakeMap(field.Type))
		case reflect.Pointer:
			value.Set(reflect.New(field.Type.Elem()))
		default:
			require.Failf(t, "unhandled kind", "%s is a %s", field.Name, value.Kind())
		}
		refused := refuseExternalStewardSpawnFields(&body)
		if externalStewardSpawnFields[field.Name] {
			allowed++
			assert.Emptyf(t, refused, "%s", field.Name)
		} else {
			assert.NotEmptyf(t, refused, "%s", field.Name)
		}
	}
	assert.Equal(t, 5, allowed, "profile, name, initial message and a task link with its label; the group is in the path")
}

// The lineage guard refuses these too, for reasons of its own that could change.
func TestExternalStewardApprovalHolds(t *testing.T) {
	assert.True(t, externalStewardApprovalHolds(harness.DefaultName, harness.ClaudeSandboxOff), "tclaude's wall, Claude's sandbox off")
	assert.False(t, externalStewardApprovalHolds(harness.DefaultName, harness.ClaudeSandboxOn), "Claude's sandbox runs Bash without asking")
	assert.False(t, externalStewardApprovalHolds(harness.DefaultName, ""), "settings.json decides")
	assert.False(t, externalStewardApprovalHolds(harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer), "bash follows its tools policy")
	assert.True(t, externalStewardApprovalHolds(harness.CodexName, harness.SandboxManagedProfile))
}
