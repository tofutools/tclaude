package agentd_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, "native", spawn.FocusMode,
		"a successful native open should report focus_mode:native")
	assert.Empty(t, spawn.FocusWS, "no browser fallback when the native open succeeded")
}

// Scenario: session new has written the launch-enrolled row (including its
// preset conv-id and tmux name), but the harness exits as soon as tmux starts.
// The simulator retains the dead pane with its exit evidence.
//
// Expected: the row alone must never trigger auto-focus or a successful spawn.
// A fast terminal would otherwise run `session attach` against the dead/not-yet
// live name and close — the macOS/Ghostty symptom this test guards.
func TestSpawn_DeadPaneFailsBeforeAutoFocus(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	f.World.SpawnPaneDiesAtLaunch = true

	opened := false
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opened = true
		return nil
	}))

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "auto_focus": true,
	})
	require.Equalf(t, http.StatusInternalServerError, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "managed pane exited during startup")
	assert.False(t, opened, "a session row without a live pane must not be auto-focused")
	assert.Empty(t, spawn.FocusMode, "no attach mode was actually opened")
}

// Scenario: the pane dies at launch and tmux has marked it dead but has not
// yet reaped its child, so the first observation carries neither
// pane_dead_status nor pane_dead_signal. The real exit code lands shortly
// after.
//
// Expected: the spawn failure names the real exit code. Reporting the first
// look as "unknown exit status" threw away evidence that was about to arrive,
// and left the operator with the least actionable message tclaude can produce
// for exactly the failures that need one most.
func TestSpawn_DeadPaneWaitsForTmuxToAttachExitStatus(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	f.World.SpawnPaneDiesAtLaunch = true
	// The status lands only on the second read of it, so the spawn path must
	// actually re-read to see it. Counting reads rather than sleeping means a
	// slow host cannot skip the pre-reap observation and pass vacuously.
	f.World.SpawnPaneStatusSettlesAfterReads = 2

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker"})

	require.Equalf(t, http.StatusInternalServerError, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "managed pane exited during startup (exit code 1)",
		"the settled status must be reported, not the not-yet-reaped unknown")
	assert.NotContains(t, string(spawn.Raw), "unknown exit status")
}

// Scenario: a human spawns with "auto focus" checked, but the host has
// no native terminal to pop — headless agentd (no DISPLAY/WAYLAND_DISPLAY)
// or no terminal emulator installed, modelled here by openTerminal
// returning an error.
//
// Expected: the spawn still succeeds (best-effort focus never fails the
// spawn), and the response reports focus_mode:"browser" plus a focus_ws
// path the dashboard can open an in-browser terminal against — instead
// of silently opening nothing while the toast claims "opening terminal".
func TestSpawn_AutoFocusFallsBackToBrowserWhenNoNativeWindow(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		return errors.New("no DISPLAY")
	}))

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "auto_focus": true,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn: status=%d body=%s", spawn.Code, spawn.Raw)
	}

	assert.Equal(t, "browser", spawn.FocusMode,
		"a failed native open should report focus_mode:browser")
	assert.Equal(t, "/api/spawn-focus-ws/"+spawn.Label, spawn.FocusWS,
		"focus_ws should be the label-keyed spawn-focus-ws path")
}

// Scenario: the dashboard's default-terminal preference is web, so its spawn
// request asks to auto-focus directly into an in-browser terminal.
//
// Expected: agentd never invokes the native terminal opener and returns the
// same label-keyed browser attachment handshake used by the headless fallback.
func TestSpawn_AutoFocusWebSkipsNativeWindow(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	opened := false
	t.Cleanup(agentd.SetOpenTerminalForTest(func(string) error {
		opened = true
		return nil
	}))

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "auto_focus": true, "auto_focus_web": true,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn: status=%d body=%s", spawn.Code, spawn.Raw)
	}

	assert.False(t, opened, "web auto-focus must not open a native terminal")
	assert.Equal(t, "browser", spawn.FocusMode)
	assert.Equal(t, "/api/spawn-focus-ws/"+spawn.Label, spawn.FocusWS)
}

// Scenario: a human spawns an agent without asking for auto focus —
// either the dashboard checkbox is unchecked, or a CLI / agent caller
// omits the field entirely.
//
// Expected: no terminal window is opened. Auto focus is strictly opt-in
// on the wire; only the dashboard's checkbox defaults it on.
func TestSpawn_NoAutoFocusByDefault(t *testing.T) {
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
}
