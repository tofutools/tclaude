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
