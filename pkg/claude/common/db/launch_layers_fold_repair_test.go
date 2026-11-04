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
	raw, err := encodeAgentLaunchLayers(layers)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeAgentLaunchLayers(raw)
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
	raw, err = encodeAgentLaunchLayers(layers)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = decodeAgentLaunchLayers(raw); err != nil || got.Explicit["roles"].Fields["Role"] == nil {
		t.Errorf("a role differing from the profile's stays explicit: %+v, %v", got, err)
	}
}
