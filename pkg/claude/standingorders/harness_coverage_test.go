package standingorders

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
	"testing"
)

func TestRegisteredHarnessHookCoverage(t *testing.T) {
	for _, name := range harness.Names() {
		if name == harness.ShellName {
			require.False(t, CapabilityFor(db.StandingTimingNextTurn, db.StandingTriggerSessionStart, name).Supported())
			continue
		}
		require.Contains(t, KnownHarnesses, name)
		// OpenCode's catalog is the observation stream. Every hook-based harness
		// must have a selectable baseline, matching what setup actually installs.
		require.NotEmpty(t, hookevents.BaselineEvents(name), "declare hook support for %s", name)
	}
	for _, name := range []string{harness.GeminiName, harness.CopilotName} {
		installed := harness.GeminiHookEvents
		if name == harness.CopilotName {
			installed = harness.CopilotHookEvents
		}
		require.ElementsMatch(t, installed, hookevents.BaselineEvents(name))
		for _, event := range hookevents.BaselineEvents(name) {
			c := CapabilityForOrder(&db.StandingOrder{Timing: db.StandingTimingNextTurn,
				HookSelectors: []hookevents.Selector{{Harness: name, Event: event}}}, name)
			require.Equal(t, db.StandingTransportMessage, c.Transport)
			require.True(t, c.Supported())
		}
	}
}
