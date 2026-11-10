package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// tcl-aut383: a clone of an API-driven Copilot agent takes its post-init
// rename over the API drive, which connects after the clone's pane is alive.
// Clone post-init must wait for that channel, as spawn's post-init does,
// instead of sending the rename into a drive that is still bootstrapping
// (where it fails and is never retried).
func TestClone_CopilotAPIClonePostInitWaitsForTheChannel(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	haveCopilotAPIProfile(t, f)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"harness": harness.CopilotName, "name": "copilot-worker",
		"copilot_api": true, "profile": copilotAPIProfile,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	waitCalls := countingPostInitWait(t, true)
	cloned := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{"no_copy_conv": true, "follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, cloned.Code, "clone body=%s", cloned.Raw)

	require.Eventually(t, func() bool { return waitCalls.Load() > 0 }, 10*time.Second, 20*time.Millisecond,
		"clone post-init must ask whether the clone's Copilot API channel came up before renaming")
	assertNoKeystrokesTo(t, f, cloned.TmuxTarget())
}
