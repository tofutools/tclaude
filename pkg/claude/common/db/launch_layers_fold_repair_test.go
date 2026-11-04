package db

import (
	"encoding/json"
	"testing"
)

// Agents spawned through the CLI with --profile before it stopped folding
// the profile into the request read back without the folded values in their
// explicit layer; a value the user typed, or one that differs from the
// profile, stays.
func TestDecodeAgentLaunchLayersDropsFoldedProfileValues(t *testing.T) {
	explicit := func(fields map[string]string) ExplicitLaunchValue {
		v := ExplicitLaunchValue{Fields: map[string]json.RawMessage{}, Provenance: ExplicitAtSpawn}
		for k, raw := range fields {
			v.Fields[k] = json.RawMessage(raw)
		}
		return v
	}
	layers := &AgentLaunchLayers{
		SpawnCopy: &AgentSpawnCopy{Tiers: []SpawnCopyTier{{Tier: SpawnCopyTierNamed, ProfileName: "kit",
			Profile: json.RawMessage(`{"name":"kit","role":"qa","role_ref":"reviewer","context_window_max":200000,"include_group_default_context":false}`)}}},
		Explicit: map[string]ExplicitLaunchValue{
			"roles":                 explicit(map[string]string{"Role": `"qa"`, "RoleRef": `"reviewer"`, "RoleRefs": `["reviewer"]`}),
			"context_window":        explicit(map[string]string{"ContextWindowMax": `200000`, "AutoCompactWindow": `"50%"`}),
			"include_group_context": explicit(map[string]string{"IncludeGroupContext": `false`}),
			"model":                 explicit(map[string]string{"Model": `"sonnet"`}),
		},
	}
	got, err := decodeAgentLaunchLayers(legacyLaunchLayers(t, layers))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Explicit["roles"]; ok {
		t.Error("roles equal to the profile's must not read as explicit")
	}
	if _, ok := got.Explicit["include_group_context"]; ok {
		t.Error("include_group_context equal to the profile's must not read as explicit")
	}
	cw, ok := got.Explicit["context_window"]
	if !ok || len(cw.Fields) != 1 || cw.Fields["AutoCompactWindow"] == nil {
		t.Errorf("context_window keeps only the typed auto-compact window, got %+v", cw)
	}
	if _, ok := got.Explicit["model"]; !ok {
		t.Error("an explicit model stays explicit")
	}

	// A value that differs from the profile's was the user's.
	layers.Explicit = map[string]ExplicitLaunchValue{"roles": explicit(map[string]string{"Role": `"lead"`})}
	if got, err = decodeAgentLaunchLayers(legacyLaunchLayers(t, layers)); err != nil || got.Explicit["roles"].Fields["Role"] == nil {
		t.Errorf("a role differing from the profile's stays explicit: %+v, %v", got, err)
	}
}

// legacyLaunchLayers encodes layers as a build before the fix wrote them,
// without the Unfolded marker.
func legacyLaunchLayers(t *testing.T, layers *AgentLaunchLayers) string {
	t.Helper()
	out := *layers
	out.Version = AgentLaunchLayersVersion
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The repair is for rows written before the fix only. A row this build wrote
// keeps an explicit value equal to its copy's: after a re-applied copy, an
// explicit value the user typed can equal the new profile, and must stay
// explicit so a later edit of the profile does not replace it.
func TestDecodeAgentLaunchLayersKeepsExplicitValuesOfCurrentRows(t *testing.T) {
	layers := &AgentLaunchLayers{
		SpawnCopy: &AgentSpawnCopy{Tiers: []SpawnCopyTier{{Tier: SpawnCopyTierNamed, ProfileName: "kit",
			Profile: json.RawMessage(`{"name":"kit","context_window_max":100000}`)}}},
		Explicit: map[string]ExplicitLaunchValue{
			"context_window": {Fields: map[string]json.RawMessage{"ContextWindowMax": json.RawMessage(`100000`)}, Provenance: ExplicitAtSpawn},
		},
	}
	raw, err := encodeAgentLaunchLayers(layers)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeAgentLaunchLayers(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Explicit["context_window"]; !ok {
		t.Error("an explicit value of a current row stays explicit, even when it equals the copy")
	}

	// A repaired legacy row, written back, is not repaired again.
	legacy, err := decodeAgentLaunchLayers(legacyLaunchLayers(t, &AgentLaunchLayers{
		SpawnCopy: layers.SpawnCopy,
		Explicit: map[string]ExplicitLaunchValue{
			"model": {Fields: map[string]json.RawMessage{"Model": json.RawMessage(`"sonnet"`)}, Provenance: ExplicitAtSpawn},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Unfolded {
		t.Error("a repaired legacy row reads as unfolded, so writing it back persists the repair")
	}
}
