package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_DisconnectBanner pins the "disconnected from agentd"
// watchdog: the connection.js module ships embedded, refresh.js reports each
// poll's outcome to it, it raises the #disconnect-overlay banner and stops the
// Vegas radio (vegas.js setConnectionLost), and the HTML + CSS hooks it needs
// survive into dashboardAssets.
//
// Same playbook as TestDashboardHTML_VegasTab — the feature is purely
// client-side, so we string-search the embedded source rather than running the
// JS.
func TestDashboardHTML_DisconnectBanner(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module itself is embedded — without this the import in refresh.js
	// would 404 in the browser.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/connection.js"); err != nil {
		t.Fatalf("embedded js/connection.js missing: %v", err)
	}

	// Public surface refresh.js drives. A refactor that drops either call
	// stops the banner appearing / clearing silently.
	must("export function noteConnected", "connection.js exposes the connected signal")
	must("export function noteDisconnected", "connection.js exposes the disconnected signal")
	must("import { noteConnected, noteDisconnected } from './connection.js'",
		"refresh.js imports the watchdog")
	must("noteConnected();", "refresh.js reports a reachable agentd each poll")
	must("noteDisconnected();", "refresh.js reports an unreachable agentd on a rejected fetch")

	// The disconnect state must stop the music AND keep it stopped: connection.js
	// calls vegas.js's setConnectionLost, which stops the radio and gates
	// syncVegas from restarting it. Pin both ends of that contract.
	must("import { setConnectionLost } from './vegas.js'",
		"connection.js reaches the radio kill switch")
	must("export function setConnectionLost", "vegas.js exposes the radio kill switch")
	must("if (connectionLost) { stopMusic(); return; }",
		"syncVegas refuses to (re)start the radio while disconnected")

	// The banner DOM: the overlay, its show-hook id, and the human-readable
	// copy. connection.js toggles .show on #disconnect-overlay.
	must(`id="disconnect-overlay"`, "the banner overlay ships in the HTML")
	must(`class="disconnect-overlay"`, "connection.js toggles .show on this class")
	must("Disconnected from agentd", "the banner states the problem")
	must(`getElementById('disconnect-overlay')`, "connection.js targets the overlay")
	must(`id="disconnect-status"`, "the alert status line ships")
	must(`getElementById('disconnect-status')`,
		"connection.js rewrites the status line so the role=alert region announces")

	// CSS: hidden by default (display:none) and revealed via .show, and it must
	// NOT be a .modal-overlay (that would suspend the poll that clears it — the
	// whole reason it's a separate class).
	must(".disconnect-overlay {", "the overlay CSS ships")
	must(".disconnect-overlay.show { display: flex; }",
		"the overlay is revealed by the .show hook connection.js adds")
}
