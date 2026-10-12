package harness

import "testing"

// TestLaunchIdentityDeclaredForEveryHarness pins the launch-identity shape of
// every registered harness. The fields replaced name switches in the launch
// path, so this table is the record of the behaviour they must preserve, and a
// newly registered harness fails here until its shape is declared on purpose.
func TestLaunchIdentityDeclaredForEveryHarness(t *testing.T) {
	type identity struct {
		preset    PresetSessionIDKind
		mintFresh bool
		resumeID  bool
	}
	want := map[string]identity{
		DefaultName:  {preset: PresetSessionIDUUID},
		CodexName:    {preset: PresetSessionIDUnsupported},
		OpenCodeName: {preset: PresetSessionIDManagedServer},
		CopilotName:  {preset: PresetSessionIDUUID},
		GeminiName:   {preset: PresetSessionIDUUID, mintFresh: true, resumeID: true},
		ShellName:    {preset: PresetSessionIDUUID},
		PiName:       {preset: PresetSessionIDUUID, mintFresh: true, resumeID: true},
	}
	for _, name := range Names() {
		w, ok := want[name]
		if !ok {
			t.Errorf("harness %q has no declared launch identity; add it to this table deliberately", name)
			continue
		}
		h := MustGet(name)
		got := identity{
			preset:    h.PresetSessionIDKind(),
			mintFresh: h.MintsFreshLaunchSessionID(),
			resumeID:  h.ResumesFullIDWithoutStore(),
		}
		if got != w {
			t.Errorf("harness %q launch identity = %+v, want %+v", name, got, w)
		}
	}
}

func TestLaunchIdentityNilSafe(t *testing.T) {
	var h *Harness
	if h.PresetSessionIDKind() != PresetSessionIDUnsupported || h.MintsFreshLaunchSessionID() || h.ResumesFullIDWithoutStore() {
		t.Fatal("nil harness must declare no launch identity")
	}
	// MintFreshSessionID without a UUID preset is meaningless and must not mint.
	h = &Harness{MintFreshSessionID: true, PresetSessionID: PresetSessionIDManagedServer}
	if h.MintsFreshLaunchSessionID() {
		t.Fatal("minting requires the UUID preset shape")
	}
}
