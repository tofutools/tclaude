package agentd

import (
	"encoding/json"
	"reflect"
	"strings"
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
	"CopilotAPI":       {Harness: harness.CopilotName},
	// Codex-only toggles. Without a Codex base these would be judged against
	// Claude, refused, and fall through to the disclosure escape below — which
	// passes, but proves much less than checking them where they are legal.
	"FastMode":      {Harness: harness.CodexName},
	"SSHWorkaround": {Harness: harness.CodexName},
	"AutoReview":    {Harness: harness.CodexName},
	// OpenCode owns the tool-governance axis; ContextFeatures is Claude-only and
	// needs a base only so the GATE direction gets exercised (see the legal
	// samples below — without one it is judged where it is legal and the gate is
	// never asked about it, which is how it was missed in the first place).
	"ToolGovernance": {Harness: harness.OpenCodeName},
	// Claude-only, and listed EXPLICITLY even though Claude is the default: this
	// map is also the roster of harness-specific fields the gate-direction guard
	// walks, so a field missing from it is a field whose drop is never tested.
	// ContextFeatures was missing from the gate itself for exactly that reason.
	"ContextFeatures": {Harness: harness.DefaultName},
}

// foreignHarness names a harness that is NOT the one a field belongs to, so the
// gate-direction guard can re-trace a member onto it.
func foreignHarness(base db.SpawnProfile) string {
	if harnessOrDefault(base.Harness) == harness.DefaultName {
		return harness.CopilotName
	}
	return harness.DefaultName
}

// snapshotMergeLegalSample supplies a harness-LEGAL value for fields whose
// generic sample ("guard-sample") is rejected by every harness. Without these the
// gate refuses the sample for a reason that has nothing to do with the harness,
// the merged profile empties, and both guards take their disclosure escape —
// silently ceasing to test the very emptiness condition this file exists for.
// The two derived from the harness itself are computed rather than spelled, so
// they cannot drift from the validator that judges them.
var snapshotMergeLegalSample = map[string]func(*harness.Harness) any{
	"Approval":               func(h *harness.Harness) any { return h.Approval.DefaultPolicy() },
	"ToolGovernance":         func(h *harness.Harness) any { return h.ToolGovernance.DefaultPolicy() },
	"AskUserQuestionTimeout": func(*harness.Harness) any { return "5m" },
	"AutoCompactWindow":      func(*harness.Harness) any { return "450000" },
	"SandboxImplementation":  func(*harness.Harness) any { return "harness-builtin" },
	// A real trim slug: the gate validates the map's contents, not just its
	// presence, so "guard-sample" is refused on every harness including Claude.
	"ContextFeatures": func(*harness.Harness) any { return map[string]string{"bundled-skills": "off"} },
}

// applyLegalSample overwrites field idx with a harness-legal value when one is
// registered, so a subtest exercises "is this field COUNTED" rather than "does
// the gate reject nonsense".
func applyLegalSample(t *testing.T, p *db.SpawnProfile, idx int, base db.SpawnProfile) {
	t.Helper()
	name := reflect.TypeOf(*p).Field(idx).Name
	sample, ok := snapshotMergeLegalSample[name]
	if !ok {
		return
	}
	h, err := harness.ResolveSpawnable(harnessOrDefault(base.Harness))
	require.NoErrorf(t, err, "snapshotMergeTracedBase[%q] names a harness that does not resolve", name)
	reflect.ValueOf(p).Elem().Field(idx).Set(reflect.ValueOf(sample(h)))
}

// disclosureNamesField reports whether a drop disclosure announces the Go field
// `name` under the wire spelling an operator would recognise. Matching is done by
// squashing case and underscores rather than by generating snake_case, because
// generating it gets acronyms wrong in exactly the fields that matter here:
// CopilotAPI is `copilot_api`, not `copilot_a_p_i`, and SSHWorkaround is
// `ssh_workaround`.
func disclosureNamesField(fields []string, name string) bool {
	squash := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, "_", ""))
	}
	for _, f := range fields {
		if squash(f) == squash(name) {
			return true
		}
	}
	return false
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
		// "off" is a real ContextFeatures value.
		f.Set(reflect.ValueOf(map[string]string{"guard-sample": "off"}))
	case map[string]db.PermissionOverride:
		// Any effect other than a grant — that is the half of the override map
		// mergeSnapshotInlineProfile carries forward.
		f.Set(reflect.ValueOf(map[string]db.PermissionOverride{"guard-sample": db.Deny()}))
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
			// Judge each field against a harness that accepts it, where one is
			// named — otherwise a harness-only field is refused by the gate for a
			// reason that has nothing to do with the counting this guard is testing.
			base := snapshotMergeTracedBase[name]
			prev, traced := &db.SpawnProfile{}, &db.SpawnProfile{}
			*prev, *traced = base, base
			setSampleProfileField(t, prev, i)
			setSampleProfileField(t, traced, i)
			applyLegalSample(t, prev, i, base)
			applyLegalSample(t, traced, i, base)

			out, drop := mergeSnapshotInlineProfile(prev, traced, true)
			// No escape here, deliberately. An earlier version let a dropped field
			// pass this guard, which meant a field the gate happened to reject was
			// never checked against the emptiness condition at all — the guard stayed
			// green while the property it advertises went untested. Every field must
			// reach this subtest carrying a value its harness ACCEPTS, which is what
			// snapshotMergeTracedBase and snapshotMergeLegalSample are for.
			require.Nilf(t, drop,
				"db.SpawnProfile.%s was dropped by dropLaunchFieldsForeignToHarness (%v), so this "+
					"subtest never tested whether the field is COUNTED. Give it a harness that "+
					"accepts it in snapshotMergeTracedBase, and a legal value in "+
					"snapshotMergeLegalSample.", name, drop)
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
// either SURVIVE a re-snapshot of a member that carries nothing for it, or be
// dropped WITH A DISCLOSURE naming it. Traced is a real profile (a member that
// WAS observed) so this is the ordinary case, not the unobserved-member edge.
//
// The "or disclosed" half is the property that actually matters and it was not
// in the guard's first version (TCL-1062), which only demanded that a field
// carry. That was the easy property: it could not distinguish a field nobody had
// thought about from one deliberately dropped, so a future field could be added,
// gated, and silently lost while this test stayed green. Asserting "carried, or
// its loss is announced" closes that.
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

			out, drop := mergeSnapshotInlineProfile(prev, &traced, true)
			require.NotNil(t, out, "a traced member that carries something can never merge to an empty profile")
			if reflect.DeepEqual(reflect.ValueOf(*prev).Field(i).Interface(), reflect.ValueOf(*out).Field(i).Interface()) {
				return // carried forward intact
			}
			// Not carried. The ONLY acceptable alternative is that the merge took it
			// deliberately and said so: a field that vanishes without a word is the
			// exact defect this guard exists to prevent, one level up from the
			// fields it was originally written for.
			require.NotNilf(t, drop,
				"the template's curated db.SpawnProfile.%s did not survive a re-snapshot of a member "+
					"that carries nothing for it, AND nothing was disclosed: mergeSnapshotInlineProfile "+
					"drops %s silently. Either carry it forward, or drop it through "+
					"dropLaunchFieldsForeignToHarness so the operator is told, or add %s to "+
					"snapshotMergeTracedWinsFields with a reason why the live group may clear it.",
				name, name, name)
			assert.Truef(t, disclosureNamesField(drop.Fields, name),
				"db.SpawnProfile.%s was dropped but the disclosure names %v instead — an operator "+
					"reading it would be told the wrong field went away.", name, drop.Fields)
			assert.NotEmptyf(t, drop.Reason, "the disclosure for %s carries no reason, so it is not actionable", name)
		})
	}
}

// TestMergeSnapshotInlineProfilePreservesEveryTracedWinsFieldWhenUnobserved is
// the third structural guard, and it exists because the other two both pass
// observed=true and so cannot see the root fix at all.
//
// The fields in snapshotMergeTracedWinsFields are exactly the ones a live group
// is allowed to clear — which is why guard B exempts them. That exemption is
// also the instruction a developer adding a NEW observable field follows: it is
// the only way past guard B. So without this guard, adding a traced-wins field
// and listing it there is enough to have it blanked out of every stored template
// whenever a member cannot be observed — writing an assertion out of an absence,
// the precise defect TCL-1083's root fix removes, reintroduced silently by
// someone following the documented route.
//
// Verified to bite: deleting either half of the restore in
// mergeSnapshotInlineProfile fails this test. Before it existed, deleting
// `out.Effort, out.Sandbox = prev.Effort, prev.Sandbox` left the whole package
// green.
func TestMergeSnapshotInlineProfilePreservesEveryTracedWinsFieldWhenUnobserved(t *testing.T) {
	profile := reflect.TypeOf(db.SpawnProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		if !snapshotMergeTracedWinsFields[name] {
			continue
		}
		if name == "IsOwner" {
			// Ownership is the one traced-wins field the merge drops on purpose: it
			// rides the template agent's own is_owner flag, not its profile, so
			// there is nothing for an unobserved member to preserve.
			continue
		}
		t.Run(name, func(t *testing.T) {
			prev := &db.SpawnProfile{}
			setSampleProfileField(t, prev, i)
			applyLegalSample(t, prev, i, db.SpawnProfile{})

			// Not observed: traceMemberLaunch got past neither early return, so every
			// field it would have filled is blank. That is an absence of information,
			// and the merge must not turn it into a statement about the member.
			out, drop := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{}, false)
			require.NotNilf(t, out,
				"an unobserved member emptied a stored profile that carried db.SpawnProfile.%s", name)
			assert.Equalf(t, reflect.ValueOf(*prev).Field(i).Interface(), reflect.ValueOf(*out).Field(i).Interface(),
				"db.SpawnProfile.%s is traced-wins, and an UNOBSERVED member blanked it. \"We learned "+
					"nothing about this member\" must not be recorded as \"this member pins no %s\": that "+
					"is what manufactured the whole harness-only field family (TCL-1083). Add %s to the "+
					"!observed restore in mergeSnapshotInlineProfile.", name, name, name)
			assert.Nilf(t, drop,
				"nothing should be dropped for an unobserved member — the stored profile is "+
					"self-consistent by construction, so a drop here means the restore is incomplete "+
					"and the gate is judging %s against the wrong harness.", name)
		})
	}
}

// TestMergeSnapshotInlineProfileDropsAndDisclosesEveryForeignField is the fourth
// structural guard, and it walks the GATE direction: every harness-specific
// field, carried on a profile whose member has been re-traced onto a harness
// that does not accept it, must be taken away AND announced.
//
// The other three guards judge each field against a harness that accepts it, so
// all three stay green when a field is missing from
// dropLaunchFieldsForeignToHarness entirely. That is not hypothetical: a cold
// review found ContextFeatures missing from the gate, riding onto a re-traced
// Copilot member and 400ing it at deploy, with all three guards passing. This is
// the guard that would have caught it, so adding a harness-specific field to
// snapshotMergeTracedBase without gating it now fails here.
func TestMergeSnapshotInlineProfileDropsAndDisclosesEveryForeignField(t *testing.T) {
	profile := reflect.TypeOf(db.SpawnProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		base, harnessSpecific := snapshotMergeTracedBase[name]
		if !harnessSpecific {
			continue
		}
		t.Run(name, func(t *testing.T) {
			prev := &db.SpawnProfile{}
			*prev = base
			setSampleProfileField(t, prev, i)
			applyLegalSample(t, prev, i, base)

			// Observed, and this member is on a harness the field does not belong to.
			// A real trace, not an absence: the operator genuinely moved this agent,
			// so there is nothing to preserve and the field has to go.
			foreign := foreignHarness(base)
			out, drop := mergeSnapshotInlineProfile(prev, &db.SpawnProfile{Harness: foreign}, true)

			if out != nil {
				assert.Truef(t, reflect.ValueOf(*out).Field(i).IsZero(),
					"db.SpawnProfile.%s survived onto a member re-traced to %q, which does not accept "+
						"it. resolveInt/Bool/StringLaunchField treat this profile as a matching tier and "+
						"fail it, so the member 400s at deploy and never spawns. Add %s to "+
						"dropLaunchFieldsForeignToHarness.", name, foreign, name)
			}
			require.NotNilf(t, drop,
				"db.SpawnProfile.%s was taken away on a re-trace to %q with NOTHING disclosed. A "+
					"curated field that vanishes without a word is the defect this ticket exists to "+
					"fix; route it through dropLaunchFieldsForeignToHarness.", name, foreign)
			assert.Truef(t, disclosureNamesField(drop.Fields, name),
				"the disclosure for db.SpawnProfile.%s names %v instead", name, drop.Fields)
		})
	}
}

// TestFromGroupDropDisclosureWireShape pins the JSON keys the disclosure travels
// under. Both consumers — the CLI struct in pkg/claude/agent/templates.go and
// the dashboard's management-actions.js — read these names literally, so a
// renamed or mistyped tag does not fail anything: the field simply decodes to
// nothing and BOTH surfaces render silence. That is the same silent-loss failure
// the disclosure exists to prevent, one layer out, so the names are pinned rather
// than trusted.
func TestFromGroupDropDisclosureWireShape(t *testing.T) {
	body, err := json.Marshal(fromGroupUpdateJSON{
		Updated: true,
		Dropped: []snapshotFieldDrop{{
			Agent:  "worker",
			Fields: []string{"context_window_max", "copilot_api"},
			Reason: "template-local profile resolves to harness \"claude\", which does not accept them",
		}},
	})
	require.NoError(t, err)

	var wire struct {
		Dropped []struct {
			Agent  string   `json:"agent"`
			Fields []string `json:"fields"`
			Reason string   `json:"reason"`
		} `json:"dropped"`
	}
	require.NoError(t, json.Unmarshal(body, &wire))
	require.Len(t, wire.Dropped, 1, "the disclosure must survive the round-trip both consumers make")
	assert.Equal(t, "worker", wire.Dropped[0].Agent)
	assert.Equal(t, []string{"context_window_max", "copilot_api"}, wire.Dropped[0].Fields)
	assert.NotEmpty(t, wire.Dropped[0].Reason)

	// Omitted entirely on the common path, so an unaffected re-snapshot does not
	// grow a key both renderers would have to special-case.
	quiet, err := json.Marshal(fromGroupUpdateJSON{Updated: true})
	require.NoError(t, err)
	assert.NotContains(t, string(quiet), "dropped")
}
