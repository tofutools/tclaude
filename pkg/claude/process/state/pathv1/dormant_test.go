package pathv1

import (
	"errors"
	"reflect"
	"testing"

	processstate "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestSubstrateRemainsDormantUnderActiveV6State(t *testing.T) {
	t.Parallel()
	if processstate.StateSchemaVersion != 6 {
		t.Fatalf("active schema = %d, want 6", processstate.StateSchemaVersion)
	}
	typ := reflect.TypeOf(processstate.State{})
	for _, name := range []string{"Routing", "RoutingState", "PathV1"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Fatalf("active state unexpectedly exposes %s", name)
		}
	}
}

func TestActiveV6CapabilitiesCannotRepresentPathV1Execution(t *testing.T) {
	t.Parallel()
	active := processstate.New("run", "ref", "ref", nil)
	if active.StateSchemaVersion != 6 {
		t.Fatalf("new active state schema = %d, want 6", active.StateSchemaVersion)
	}
	if err := processstate.CheckSchemaVersion(7); !errors.Is(err, processstate.ErrNewerSchemaVersion) {
		t.Fatalf("schema 7 check error = %v, want ErrNewerSchemaVersion", err)
	}
	if _, err := processstate.Decode([]byte(`{"stateSchemaVersion":7,"status":"running","originalTemplateRef":"ref","currentTemplateRef":"ref","nodes":{},"lastLogSeq":0,"logChecksum":""}`)); !errors.Is(err, processstate.ErrNewerSchemaVersion) {
		t.Fatalf("schema 7 decode error = %v, want ErrNewerSchemaVersion", err)
	}

	// These are the only path-v1 command names that could plan, claim,
	// observe, route, complete, or execute the dormant protocol. The active
	// command/reducer/executor contract rejects every one at its typed enum
	// boundary; complete_run_v1 is distinct from the legacy complete_run.
	for _, kind := range []CommandKindV1{
		CommandInitializeRouting, CommandPerformAttempt, CommandSettleAttempt,
		CommandRoutePaths, CommandActivateGeneration, CommandPropagateCandidateClosure,
		CommandSettleDetachedSink, CommandCompleteRun,
	} {
		if processstate.CommandKind(kind).IsValid() {
			t.Fatalf("active v6 command contract accepts dormant path-v1 kind %q", kind)
		}
	}

	stateType := reflect.TypeOf(processstate.State{})
	for _, name := range []string{"Routing", "RoutingState", "PathV1", "CompletionBasis"} {
		if _, ok := stateType.FieldByName(name); ok {
			t.Fatalf("active checkpoint unexpectedly exposes %s", name)
		}
	}
	eventType := reflect.TypeOf(processstate.Event{})
	for _, name := range []string{"Routing", "RoutingState", "PathV1", "CompletionBasis"} {
		if _, ok := eventType.FieldByName(name); ok {
			t.Fatalf("active reducer event unexpectedly exposes %s", name)
		}
	}
}
