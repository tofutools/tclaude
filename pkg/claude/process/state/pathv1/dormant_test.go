package pathv1

import (
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
