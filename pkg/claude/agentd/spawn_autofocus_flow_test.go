package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Scenario: a human spawns a new agent with "auto focus" checked in the
// dashboard's spawn modal (auto_focus:true on the wire).
//
// Expected: once the spawn lands, the daemon opens a terminal window
// attached to the new agent's tclaude session — `tclaude session attach
// <label>`, routed through the tclaude wrapper (never a raw `tmux
// attach`) so the reattached session keeps its tclaude features.
func TestSpawn_AutoFocusOpensAttachTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		var gotCmd string
		t.Cleanup(agentd.SetOpenTerminalForTest(func(cmd string) error {
			gotCmd = cmd
			return nil
		}))

		spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
			"name": "worker", "auto_focus": true,
		})
		if spawn.Code != http.StatusOK {
			t.Fatalf("spawn: status=%d body=%s", spawn.Code, spawn.Raw)
		}

		assert.Contains(t, gotCmd, "session attach",
			"auto-focus should attach via the tclaude wrapper, not raw tmux")
		assert.Contains(t, gotCmd, spawn.Label,
			"auto-focus terminal should attach to the new agent's session label")
	})
}

// Scenario: a human spawns an agent without asking for auto focus —
// either the dashboard checkbox is unchecked, or a CLI / agent caller
// omits the field entirely.
//
// Expected: no terminal window is opened. Auto focus is strictly opt-in
// on the wire; only the dashboard's checkbox defaults it on.
func TestSpawn_NoAutoFocusByDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		opened := false
		t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
			opened = true
			return nil
		}))

		// auto_focus omitted entirely — the CLI / agent-API default.
		spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker"})
		if spawn.Code != http.StatusOK {
			t.Fatalf("spawn (omitted): status=%d body=%s", spawn.Code, spawn.Raw)
		}
		assert.False(t, opened, "omitted auto_focus → no terminal should open")

		// auto_focus explicitly false behaves the same.
		opened = false
		spawn = f.AsHuman().SpawnWith("alpha", map[string]any{
			"name": "worker2", "auto_focus": false,
		})
		if spawn.Code != http.StatusOK {
			t.Fatalf("spawn (false): status=%d body=%s", spawn.Code, spawn.Raw)
		}
		assert.False(t, opened, "auto_focus:false → no terminal should open")
	})
}
