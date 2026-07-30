package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// ReconcileBackground is the combined pass over both process-backed
// ledgers. These tests pin the two properties that made it one pass rather
// than two: a live process may be claimed only once ACROSS the ledgers,
// and a websocket monitor is never offered a process at all.

func TestReconcileBackground_RetiresDeadKeepsAliveAcrossBothLedgers(t *testing.T) {
	now := time.Now()
	shells := map[string]db.BgShellSeen{
		"shell-alive": {Command: "npm run dev --port 4321", Seen: now},
		"shell-dead":  {Command: "pytest tests/integration", Seen: now},
	}
	monitors := map[string]db.MonitorSeen{
		"mon-alive": {Command: "gh pr checks 123 --watch", Seen: now},
		"mon-dead":  {Command: "tail -f /var/log/deploy.log", Seen: now},
		"mon-vague": {Command: "ls", Seen: now},
	}
	cmdlines := []string{
		"/bin/bash -c ... eval 'npm run dev --port 4321' < /dev/null",
		"/bin/bash -c ... eval 'gh pr checks 123 --watch' < /dev/null",
	}

	got := ReconcileBackground(shells, monitors, cmdlines)
	assert.Equal(t, []string{"shell-alive"}, got.Shells.Alive)
	assert.Equal(t, []string{"shell-dead"}, got.Shells.Dead)
	assert.Equal(t, []string{"mon-alive"}, got.Monitors.Alive,
		"a command monitor is a descendant process like any background shell")
	assert.Equal(t, []string{"mon-dead"}, got.Monitors.Dead)
	assert.Equal(t, []string{"mon-vague"}, got.Monitors.Undecided,
		"a command too generic to match on is neither confirmed nor retired")

	// Positive evidence that NOTHING is running retires everything
	// matchable, in both ledgers.
	got = ReconcileBackground(shells, monitors, nil)
	assert.ElementsMatch(t, []string{"shell-alive", "shell-dead"}, got.Shells.Dead)
	assert.ElementsMatch(t, []string{"mon-alive", "mon-dead"}, got.Monitors.Dead)
}

// The reason the two ledgers share ONE pass: a monitor's watch script and
// a background shell are indistinguishable in the process table, so two
// independent passes would let both claim the same process and retire
// neither.
func TestReconcileBackground_OneProcessIsClaimedOnlyOnceAcrossLedgers(t *testing.T) {
	now := time.Now()
	shells := map[string]db.BgShellSeen{
		"shell-1": {Command: "npm run dev", Seen: now},
	}
	monitors := map[string]db.MonitorSeen{
		"mon-1": {Command: "npm run dev", Seen: now},
	}
	cmdlines := []string{"/bin/bash -c eval 'npm run dev'"}

	got := ReconcileBackground(shells, monitors, cmdlines)
	alive := len(got.Shells.Alive) + len(got.Monitors.Alive)
	dead := len(got.Shells.Dead) + len(got.Monitors.Dead)
	assert.Equal(t, 1, alive, "one live process supports exactly one entry")
	assert.Equal(t, 1, dead, "the other is retired rather than double-counted")

	// Deterministic across runs despite Go's randomised map iteration.
	for range 20 {
		assert.Equal(t, got, ReconcileBackground(shells, monitors, cmdlines))
	}
}

// A websocket watch runs inside the harness process. Asking the process
// table about it would retire it instantly and always.
func TestReconcileBackground_WebsocketMonitorsAreNeverRetiredByProcessEvidence(t *testing.T) {
	now := time.Now()
	monitors := map[string]db.MonitorSeen{
		"ws-1": {Label: "wss://events.example.com/stream", WS: true, Seen: now},
	}

	got := ReconcileBackground(nil, monitors, nil)
	assert.Equal(t, []string{"ws-1"}, got.Monitors.Undecided,
		"no process evidence can speak to a socket held inside the harness")
	assert.Empty(t, got.Monitors.Dead)
	assert.Empty(t, got.Monitors.Alive)

	// And it must not consume a process another entry could have claimed.
	shells := map[string]db.BgShellSeen{"shell-1": {Command: "npm run dev", Seen: now}}
	got = ReconcileBackground(shells, monitors, []string{"/bin/bash -c eval 'npm run dev'"})
	assert.Equal(t, []string{"shell-1"}, got.Shells.Alive)
	assert.Equal(t, []string{"ws-1"}, got.Monitors.Undecided)
}

// ReconcileBgShells is the shells-only view of the same pass; its existing
// callers and tests must see no behaviour change.
func TestReconcileBgShells_StillAnswersAboutShellsAlone(t *testing.T) {
	now := time.Now()
	shells := map[string]db.BgShellSeen{"shell-1": {Command: "npm run dev", Seen: now}}
	assert.Equal(t,
		ReconcileBackground(shells, nil, nil).Shells,
		ReconcileBgShells(shells, nil))
}

func TestReconcileBackground_EmptyLedgersHaveNothingToSay(t *testing.T) {
	got := ReconcileBackground(nil, nil, []string{"/bin/bash -c eval 'npm run dev'"})
	assert.Empty(t, got.Shells.Dead)
	assert.Empty(t, got.Monitors.Dead)
}
