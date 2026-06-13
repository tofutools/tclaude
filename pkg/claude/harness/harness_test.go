package harness

import (
	"testing"
)

// TestRegistry_DefaultClaude verifies the built-in claude harness is
// registered and is the default.
func TestRegistry_DefaultClaude(t *testing.T) {
	h, ok := Get(DefaultName)
	if !ok {
		t.Fatalf("claude harness not registered")
	}
	if h.Name != DefaultName || h.DisplayName != "Claude Code" {
		t.Fatalf("unexpected descriptor: name=%q display=%q", h.Name, h.DisplayName)
	}
	if Default() != h {
		t.Fatalf("Default() did not return the registered claude harness")
	}
	if h.Spawn == nil || h.Models == nil || h.Life == nil {
		t.Fatalf("claude descriptor has a nil contract: %+v", h)
	}
}

// TestResolve covers the empty→default fallback and the unknown-name
// error path (a typo must surface, not silently run Claude Code).
func TestResolve(t *testing.T) {
	got, err := Resolve("")
	if err != nil || got != Default() {
		t.Fatalf("Resolve(\"\") = (%v, %v), want (default, nil)", got, err)
	}
	if got, err := Resolve(DefaultName); err != nil || got != Default() {
		t.Fatalf("Resolve(claude) = (%v, %v), want (default, nil)", got, err)
	}
	if _, err := Resolve("nope"); err == nil {
		t.Fatalf("Resolve(nope) should error on an unknown harness")
	}
}

// TestRegister_Roundtrip exercises Register/Get/Names with a throwaway
// harness so the registry mechanics are covered without depending on a
// future real harness.
func TestRegister_Roundtrip(t *testing.T) {
	const name = "test-harness-xyz"
	Register(&Harness{Name: name, DisplayName: "Test"})
	t.Cleanup(func() {
		registryMu.Lock()
		delete(registry, name)
		registryMu.Unlock()
	})

	if h, ok := Get(name); !ok || h.Name != name {
		t.Fatalf("Get(%q) round-trip failed: %v, %v", name, h, ok)
	}

	var found bool
	for _, n := range Names() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() missing %q: %v", name, Names())
	}
}

// TestSupports_NilContracts confirms the capability helpers fold a nil
// Lifecycle (an unsupported-everything harness) into false rather than
// panicking — the safety property the injection call sites rely on.
func TestSupports_NilContracts(t *testing.T) {
	var h *Harness
	if h.SupportsRename() || h.SupportsCompact() || h.SupportsSoftExit() {
		t.Fatalf("nil harness must report no capabilities")
	}
	bare := &Harness{Name: "bare"}
	if bare.SupportsRename() || bare.SupportsCompact() || bare.SupportsSoftExit() {
		t.Fatalf("harness with nil Lifecycle must report no capabilities")
	}
}

// TestCanRenameCompact pins the deliverable-action predicates the spawn /
// row UIs gate on. The subtle case is rename: Codex has no in-pane rename
// command (SupportsRename() == false) but renames out-of-band via its
// ConvStore, so CanRename() must be true for it — gating the dashboard's
// rename control on SupportsRename alone would wrongly hide a working
// feature. Compact has no out-of-band fallback, so CanCompact() tracks
// SupportsCompact exactly: Codex (no `/compact`) is false.
func TestCanRenameCompact(t *testing.T) {
	claude := Default()
	if !claude.CanRename() || !claude.CanCompact() {
		t.Fatalf("claude must support both rename and compact: rename=%v compact=%v",
			claude.CanRename(), claude.CanCompact())
	}

	codex, ok := Get(CodexName)
	if !ok {
		t.Fatalf("codex harness not registered")
	}
	// The whole point of the broader predicate: no in-pane rename, yet
	// renameable through the ConvStore.
	if codex.SupportsRename() {
		t.Fatalf("precondition: codex is expected to have no in-pane rename command")
	}
	if !codex.CanRename() {
		t.Fatalf("codex must be renameable via its ConvStore (CanRename), even without /rename")
	}
	if codex.CanCompact() {
		t.Fatalf("codex has no compaction command, so CanCompact must be false")
	}

	// A bare descriptor (no Lifecycle, no ConvStore) and a nil receiver
	// fold to false rather than panicking.
	bare := &Harness{Name: "bare"}
	if bare.CanRename() || bare.CanCompact() {
		t.Fatalf("bare harness must report no deliverable rename/compact")
	}
	var nilH *Harness
	if nilH.CanRename() || nilH.CanCompact() {
		t.Fatalf("nil harness must report no deliverable rename/compact")
	}
}
