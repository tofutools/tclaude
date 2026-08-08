package agentd

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The two guards in this file are STRUCTURAL, not value tests. They are the
// sibling of db.TestInlineProfileJSONCoversEveryLaunchField, one layer up: that
// one pins the STORAGE of a template-local spawn profile, these pin the
// update-mode re-snapshot MERGE of one.
//
// mergeSnapshotInlineProfile hand-enumerates db.SpawnProfile twice — once to
// carry a curated field forward from the stored template, and once again in the
// trailing "is any of this non-empty" condition that decides whether the merged
// profile survives at all. A field missing from the first enumeration is
// silently reset to the traced value; a field missing from the SECOND is worse,
// because a profile carrying only that field collapses to nil and the whole
// template-local profile disappears with it. Either way the re-snapshot reports
// 200 and the operator sees a template that saved cleanly.
//
// `context_window_max` fell into exactly that gap (TCL-1062) — it was carried by
// the snapshot builder and by storage, and then dropped by the merge — which is
// what these exist to stop repeating. The field-count condition is a PROXY for
// "is this profile empty": it was correct when written and stops answering the
// question the moment a field it does not count arrives.

// snapshotMergeExemptFields are the db.SpawnProfile fields that cannot appear on
// a template-local profile at all, so the merge has nothing to do with them.
// This is deliberately the same list as the exempt list in
// db.TestInlineProfileJSONCoversEveryLaunchField, and for the same reason: a
// field absent from templateInlineProfileJSON can never be read back off a
// stored template, and snapshotGroupTemplate never traces one onto a fresh
// inline profile either. Adding a field here is a decision someone has to write
// down, which is the point.
var snapshotMergeExemptFields = map[string]bool{
	// Identity + storage: owned by the template agent / the profiles table.
	"ID": true, "Name": true, "Aliases": true,
	"CreatedAt": true, "UpdatedAt": true,
	"Disabled": true, "DisabledReason": true, "OperatorOnly": true,
	"AgentName": true, "Role": true, "Descr": true, "InitialMessage": true,
	// Spawn-dialog-only toggles: meaningless for a template deploy.
	"SyncWorktree": true, "AutoFocus": true, "IncludeGroupDefaultContext": true,
}

// snapshotMergeTracedWinsFields are the fields the live group owns outright: the
// traced value replaces the stored one INCLUDING when it is blank, because
// re-snapshotting a member that no longer pins them is how a template's stale
// pin gets cleared. They are exempt from the carry-forward guard only — they are
// still subject to the emptiness count.
var snapshotMergeTracedWinsFields = map[string]bool{
	// The four fields every session row records, so a blank traced answer is a
	// real observation of "this member pins nothing" rather than a failure to look.
	"Harness": true, "Model": true, "Effort": true, "Sandbox": true,
	// Ownership is observable too, and rides the template agent's own is_owner
	// flag rather than its profile — the merge drops prev's on purpose.
	"IsOwner": true,
}

// snapshotMergeTracedBase overrides the traced profile a field's carry-forward is
// checked against. The default is an ordinary observed member that carries
// nothing but a model. A field whose carry-forward is deliberately GATED on the
// harness the field belongs to names a traced member of that harness here, so
// the guard still demands a carry-forward rather than being switched off for
// that field.
var snapshotMergeTracedBase = map[string]db.SpawnProfile{
	// A Copilot cap is invalid on any other harness, and Harness is traced-wins,
	// so the merge only carries the cap onto a profile that still resolves to
	// Copilot — see the gate in mergeSnapshotInlineProfile (TCL-1062). Check the
	// carry-forward against a member that is still Copilot.
	"ContextWindowMax": {Harness: harness.CopilotName},
}

// setSampleProfileField writes a distinctive non-zero value into field idx of p.
// The switch is exhaustive on purpose: a db.SpawnProfile field of a type not
// listed fails the guard rather than being skipped, because a silently skipped
// field is precisely the hole these tests exist to close.
func setSampleProfileField(t *testing.T, p *db.SpawnProfile, idx int) {
	t.Helper()
	f := reflect.ValueOf(p).Elem().Field(idx)
	switch f.Interface().(type) {
	case string:
		f.SetString("guard-sample")
	case int64:
		f.SetInt(123456)
	case *bool:
		v := true
		f.Set(reflect.ValueOf(&v))
	case map[string]string:
		// "off" is a real ContextFeatures value and, for PermissionOverrides, any
		// effect other than a grant — which is the half of that map the merge
		// carries forward.
		f.Set(reflect.ValueOf(map[string]string{"guard-sample": "off"}))
	default:
		t.Fatalf("db.SpawnProfile.%s is a %s, which this guard cannot populate: "+
			"teach setSampleProfileField the new type so the field is actually covered",
			reflect.TypeOf(*p).Field(idx).Name, f.Type())
	}
}

// TestMergeSnapshotInlineProfileCountsEveryStoredField pins the emptiness
// condition: a merged profile carrying exactly one field must survive the merge
// with that field intact. Every field is set on BOTH sides so the guard does not
// have to know which side wins — whichever the merge picks, the result is
// non-empty and the trailing condition has to say so.
func TestMergeSnapshotInlineProfileCountsEveryStoredField(t *testing.T) {
	profile := reflect.TypeOf(db.SpawnProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		if snapshotMergeExemptFields[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			prev, traced := &db.SpawnProfile{}, &db.SpawnProfile{}
			setSampleProfileField(t, prev, i)
			setSampleProfileField(t, traced, i)

			out := mergeSnapshotInlineProfile(prev, traced)
			require.NotNilf(t, out,
				"a template-local profile carrying only db.SpawnProfile.%s collapsed to nil: "+
					"mergeSnapshotInlineProfile's trailing \"is this empty\" condition does not "+
					"count %s, so a re-snapshot silently discards the whole inline profile. Add "+
					"%s to that condition (and to the carry-forward above it), or to "+
					"snapshotMergeExemptFields with a reason.", name, name, name)
			assert.Falsef(t, reflect.ValueOf(*out).Field(i).IsZero(),
				"db.SpawnProfile.%s was set on both the stored and the traced profile and came "+
					"back zero: the merge drops it outright.", name)
		})
	}
}

// TestMergeSnapshotInlineProfileCarriesEveryCuratedField pins the other half:
// for every field the live group does NOT own, a stored template value must
// survive a re-snapshot of a member that carries nothing for it. Traced is a
// real profile (a member that WAS observed) so this is the ordinary case, not
// the untraceable-member edge.
func TestMergeSnapshotInlineProfileCarriesEveryCuratedField(t *testing.T) {
	profile := reflect.TypeOf(db.SpawnProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		if snapshotMergeExemptFields[name] || snapshotMergeTracedWinsFields[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			prev := &db.SpawnProfile{}
			setSampleProfileField(t, prev, i)
			traced := db.SpawnProfile{Model: "sonnet"}
			if base, ok := snapshotMergeTracedBase[name]; ok {
				traced = base
			}

			out := mergeSnapshotInlineProfile(prev, &traced)
			require.NotNil(t, out, "a traced member that carries something can never merge to an empty profile")
			assert.Equalf(t, reflect.ValueOf(*prev).Field(i).Interface(), reflect.ValueOf(*out).Field(i).Interface(),
				"the template's curated db.SpawnProfile.%s did not survive a re-snapshot of a member "+
					"that carries nothing for it: mergeSnapshotInlineProfile has no carry-forward for "+
					"%s. Add one, or add %s to snapshotMergeTracedWinsFields with a reason why the "+
					"live group is allowed to clear it.", name, name, name)
		})
	}
}
