package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type fixedMigrationAuthority struct {
	needed pathv1.UpgradeNeeded
	calls  int
}

func (a *fixedMigrationAuthority) UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error) {
	a.calls++
	return a.needed, nil
}

func TestDecideBeforePlanningUsesOnlyTypedUpgradeNeeded(t *testing.T) {
	upgrade := validUpgradeNeeded()
	drain := validUpgradeNeeded()
	drain.Reason = pathv1.UpgradeLegacyDrainRequired
	drain.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveCommand, ID: "cmd"}}
	for _, tc := range []struct {
		name   string
		needed pathv1.UpgradeNeeded
		want   PrePlanningAction
	}{
		{name: "drain", needed: drain, want: PrePlanningDrainLegacy},
		{name: "upgrade", needed: upgrade, want: PrePlanningUpgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := &fixedMigrationAuthority{needed: tc.needed}
			decision, err := DecideBeforePlanning(t.Context(), authority, "run")
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != tc.want || authority.calls != 1 {
				t.Fatalf("decision = %#v, calls = %d", decision, authority.calls)
			}
		})
	}
}

func TestDecideBeforePlanningRejectsForgedAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*pathv1.UpgradeNeeded)
	}{
		{name: "partial", mutate: func(value *pathv1.UpgradeNeeded) { value.Checkpoint.Digest = "" }},
		{name: "uppercase source digest", mutate: func(value *pathv1.UpgradeNeeded) {
			value.TemplateSourceHash = strings.ToUpper(value.TemplateSourceHash)
		}},
		{name: "forged template id", mutate: func(value *pathv1.UpgradeNeeded) {
			value.TemplateRef = "Upper@sha256:" + strings.Repeat("a", 64)
		}},
		{name: "reason mismatch", mutate: func(value *pathv1.UpgradeNeeded) { value.Reason = pathv1.UpgradeLegacyDrainRequired }},
		{name: "unsorted ids", mutate: func(value *pathv1.UpgradeNeeded) {
			value.Reason = pathv1.UpgradeLegacyDrainRequired
			value.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveWait, ID: "z"}, {Kind: pathv1.LegacyActiveCommand, ID: "a"}}
		}},
		{name: "unknown kind", mutate: func(value *pathv1.UpgradeNeeded) {
			value.Reason = pathv1.UpgradeLegacyDrainRequired
			value.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: "invented", ID: "id"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needed := validUpgradeNeeded()
			tc.mutate(&needed)
			_, err := DecideBeforePlanning(t.Context(), &fixedMigrationAuthority{needed: needed}, "run")
			if err == nil {
				t.Fatal("forged authority was accepted")
			}
		})
	}
}

func TestDecideBeforePlanningRejectsForgedCheckpointAdminProvenance(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(*pathv1.UpgradeNeeded)
		rebind           bool
		rebindResolution bool
	}{
		{
			name: "resolution omitted from active ids",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
		},
		{
			name: "cross-run record",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.RunID = "other-run"
			},
			rebind: true,
		},
		{
			name: "positive legacy event sequence rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EventSeq = 1
			},
			rebind: true,
		},
		{
			name: "missing admin type",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = ""
			},
			rebind: true,
		},
		{
			name: "missing actor",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = ""
			},
			rebind: true,
		},
		{
			name: "missing timestamp",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = ""
			},
			rebind: true,
		},
		{
			name: "block-resolution type without payload rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "zero blocked attempt rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.BlockedAttempt = 0
			},
			rebindResolution: true,
		},
		{
			name: "invalid resolution actor rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "operator"
			},
			rebindResolution: true,
		},
		{
			name: "engine resolution actor rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Actor = "engine:forged"
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution node rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.NodeID = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution reason rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Reason = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution evidence rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.EvidenceRef = ""
			},
			rebindResolution: true,
		},
		{
			name: "missing resolution timestamp rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Resolution.Timestamp = ""
			},
			rebindResolution: true,
		},
		{
			name: "wrong resolution admin type rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.AdminType = "admin_repair_recorded"
			},
			rebind: true,
		},
		{
			name: "repair type swap clears resolution authority rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_repair_recorded"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "programs-allowed type swap clears resolution authority rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "admin_programs_allowed"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "unknown nonresolution admin type rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				admin := &needed.CheckpointAdminRecords[0]
				admin.Record.AdminType = "invented"
				admin.Record.ResolutionDigest = ""
				admin.Resolution = nil
				needed.ActiveLegacyIDs = nil
				needed.Reason = pathv1.UpgradeMigrationRequired
			},
			rebind: true,
		},
		{
			name: "outer actor mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Actor = "human:other"
			},
			rebind: true,
		},
		{
			name: "outer reason mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.ReasonCode = "other"
			},
			rebind: true,
		},
		{
			name: "outer evidence mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.EvidenceRef = "ticket:other"
			},
			rebind: true,
		},
		{
			name: "outer timestamp mismatch rebound",
			mutate: func(needed *pathv1.UpgradeNeeded) {
				needed.CheckpointAdminRecords[0].Record.Timestamp = "2026-07-15T00:00:01Z"
			},
			rebind: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			needed := validUpgradeNeededWithCheckpointAdmin(t)
			test.mutate(&needed)
			if test.rebindResolution {
				rebindCheckpointAdminResolution(t, &needed)
			} else if test.rebind {
				rebindCheckpointAdminIdentity(t, &needed)
			}
			if _, err := DecideBeforePlanning(t.Context(), &fixedMigrationAuthority{needed: needed}, "run"); err == nil {
				t.Fatal("forged checkpoint admin provenance was accepted")
			}
		})
	}
}

type migrationCapableStore struct {
	store.Store
	calls int
}

func (s *migrationCapableStore) UpgradeNeeded(context.Context, string) (pathv1.UpgradeNeeded, error) {
	s.calls++
	return pathv1.UpgradeNeeded{}, nil
}

func TestLiveV6HostDoesNotCallDormantMigrationAuthority(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "terminal", Start: "end", Nodes: map[string]model.Node{"end": {Type: model.NodeTypeEnd}}}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := state.New("run", record.Ref, record.Ref, []state.NodeInit{{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusCompleted}})
	checkpoint.Status = state.RunStatusCompleted
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "run", TemplateRef: record.Ref}, checkpoint); err != nil {
		t.Fatal(err)
	}
	capable := &migrationCapableStore{Store: fs}
	host := New(capable, "test:legacy-host", nil)
	_, _ = host.Tick(t.Context())
	if capable.calls != 0 {
		t.Fatalf("live v6 host called dormant migration authority %d times", capable.calls)
	}
}

func validUpgradeNeeded() pathv1.UpgradeNeeded {
	return pathv1.UpgradeNeeded{
		Reason: pathv1.UpgradeMigrationRequired, RunID: "run", LegacyStateSchema: 6,
		Checkpoint:  pathv1.CheckpointBinding{Digest: strings.Repeat("b", 64)},
		TemplateRef: "demo@sha256:" + strings.Repeat("a", 64), TemplateSourceHash: strings.Repeat("c", 64),
	}
}

func validUpgradeNeededWithCheckpointAdmin(t *testing.T) pathv1.UpgradeNeeded {
	t.Helper()
	resolution := pathv1.BlockResolution{
		NodeID: "review", BlockedAttempt: 2, Decision: "skip", Actor: "human:operator",
		Reason: "waived", EvidenceRef: "ticket:TCL-507", Timestamp: "2026-07-15T00:00:00Z",
	}
	digest, err := pathv1.ValidateBlockResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	record := pathv1.PathV1AdminRecord{
		RunID: "run", AdminType: "block_resolution_recorded", Actor: resolution.Actor,
		ReasonCode: resolution.Reason, EvidenceRef: resolution.EvidenceRef,
		Timestamp: resolution.Timestamp, ResolutionDigest: digest,
	}
	record.ID, err = pathv1.LegacyAdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	needed := validUpgradeNeeded()
	needed.Reason = pathv1.UpgradeLegacyDrainRequired
	needed.Checkpoint.Generation = 12
	checkpointID, err := pathv1.CheckpointLegacyAdminRecordIdentity(needed.Checkpoint, record)
	if err != nil {
		t.Fatal(err)
	}
	needed.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveBlockResolution, ID: checkpointID}}
	needed.CheckpointAdminRecords = []pathv1.CheckpointLegacyAdminRecord{{
		ID: checkpointID, LegacyID: record.ID, Record: record, Resolution: &resolution,
	}}
	return needed
}

func rebindCheckpointAdminIdentity(t *testing.T, needed *pathv1.UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	oldID := admin.ID
	legacyID, err := pathv1.LegacyAdminRecordIdentity(admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	admin.Record.ID = legacyID
	admin.LegacyID = legacyID
	admin.ID, err = pathv1.CheckpointLegacyAdminRecordIdentity(needed.Checkpoint, admin.Record)
	if err != nil {
		t.Fatal(err)
	}
	for i := range needed.ActiveLegacyIDs {
		if needed.ActiveLegacyIDs[i] == (pathv1.LegacyActiveID{Kind: pathv1.LegacyActiveBlockResolution, ID: oldID}) {
			needed.ActiveLegacyIDs[i].ID = admin.ID
		}
	}
}

func rebindCheckpointAdminResolution(t *testing.T, needed *pathv1.UpgradeNeeded) {
	t.Helper()
	admin := &needed.CheckpointAdminRecords[0]
	admin.Record.Actor = admin.Resolution.Actor
	admin.Record.ReasonCode = admin.Resolution.Reason
	admin.Record.EvidenceRef = admin.Resolution.EvidenceRef
	admin.Record.Timestamp = admin.Resolution.Timestamp
	var err error
	admin.Record.ResolutionDigest, err = pathv1.BlockResolutionIdentity(*admin.Resolution)
	if err != nil {
		t.Fatal(err)
	}
	rebindCheckpointAdminIdentity(t, needed)
}
