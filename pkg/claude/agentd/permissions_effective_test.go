package agentd

import (
	"slices"
	"strings"
	"testing"
)

// TestEffectivePermsFor_OwnerImplied covers how the effective-perm
// computation folds in the owner-conferred (owner_implied) slug set. This
// is what `tclaude agent permissions ls <owner>` renders; the calculation
// moved from the CLI into the daemon with TCL-611 so a sandboxed agent no
// longer needs local DB access to see it.
func TestEffectivePermsFor_OwnerImplied(t *testing.T) {
	const conv = "owner-aaaa-bbbb-cccc-dddd"
	ownerImplied := []string{"groups.spawn", "groups.retire", "human.notify"}

	t.Run("non-owner: ownership confers nothing", func(t *testing.T) {
		state := permissionsState{Defaults: []string{"groups.create"}}
		// A non-owner reaches effectivePermsFor with an empty owner set.
		eff, ownerAdded, source := effectivePermsFor(state, conv, nil)
		if has(eff, "groups.spawn") || has(eff, "groups.retire") {
			t.Errorf("non-owner effective must not gain owner-implied slugs: %v", eff)
		}
		if len(ownerAdded) != 0 {
			t.Errorf("ownerAdded must be empty for a non-owner, got %v", ownerAdded)
		}
		if source != "defaults" {
			t.Errorf("source = %q, want %q", source, "defaults")
		}
	})

	t.Run("owner: owner-implied slugs added and annotated", func(t *testing.T) {
		state := permissionsState{Defaults: []string{"groups.create"}}
		eff, ownerAdded, source := effectivePermsFor(state, conv, ownerImplied)
		for _, s := range ownerImplied {
			if !has(eff, s) {
				t.Errorf("owner effective missing owner-implied slug %q: %v", s, eff)
			}
			if !has(ownerAdded, s) {
				t.Errorf("ownerAdded missing %q: %v", s, ownerAdded)
			}
		}
		if !has(eff, "groups.create") {
			t.Errorf("owner effective must still carry defaults: %v", eff)
		}
		if !strings.Contains(source, "+owner") {
			t.Errorf("source = %q, want it to contain %q", source, "+owner")
		}
	})

	t.Run("owner: a slug already held via defaults isn't re-annotated as owner", func(t *testing.T) {
		// human.notify is BOTH a default here and owner-implied — it must
		// show as a normal grant, not "(via ownership)".
		state := permissionsState{Defaults: []string{"human.notify"}}
		eff, ownerAdded, _ := effectivePermsFor(state, conv, ownerImplied)
		if !has(eff, "human.notify") {
			t.Fatalf("effective missing human.notify: %v", eff)
		}
		if has(ownerAdded, "human.notify") {
			t.Errorf("human.notify is a default — must NOT be in ownerAdded: %v", ownerAdded)
		}
	})

	t.Run("owner: a deny override suppresses the owner bypass", func(t *testing.T) {
		// Deny groups.spawn for this owner: it must drop out of BOTH the
		// effective set and the owner-conferred projection — deny is
		// authoritative over the owner bypass, mirroring resolvePermission.
		state := permissionsState{
			Defaults:  []string{"groups.create"},
			Overrides: map[string]map[string]string{conv: {"groups.spawn": "deny"}},
		}
		eff, ownerAdded, source := effectivePermsFor(state, conv, ownerImplied)
		if has(eff, "groups.spawn") {
			t.Errorf("denied groups.spawn must not be effective: %v", eff)
		}
		if has(ownerAdded, "groups.spawn") {
			t.Errorf("denied groups.spawn must not be in ownerAdded: %v", ownerAdded)
		}
		// The un-denied owner slugs survive.
		if !has(eff, "groups.retire") || !has(ownerAdded, "groups.retire") {
			t.Errorf("un-denied owner slug groups.retire should survive: eff=%v ownerAdded=%v", eff, ownerAdded)
		}
		if !strings.Contains(source, "−denies") {
			t.Errorf("source = %q, want it to note the deny", source)
		}
	})

	t.Run("per-conv grants add on top of defaults", func(t *testing.T) {
		state := permissionsState{
			Defaults: []string{"groups.create"},
			Grants:   map[string][]string{conv: {"permissions.grant"}},
		}
		eff, _, source := effectivePermsFor(state, conv, nil)
		if !has(eff, "permissions.grant") || !has(eff, "groups.create") {
			t.Errorf("effective must union defaults and grants: %v", eff)
		}
		if !strings.Contains(source, "defaults+grants:") {
			t.Errorf("source = %q, want it to name the grant source", source)
		}
	})
}

func has(ss []string, want string) bool { return slices.Contains(ss, want) }
