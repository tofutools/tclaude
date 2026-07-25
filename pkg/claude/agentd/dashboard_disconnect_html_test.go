package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_DisconnectBanner pins the "disconnected from agentd"
// watchdog: the connection.js module ships embedded, refresh.js reports each
// poll's outcome to it, it publishes connection state to the shared snapshot
// store, the Preact shell raises #disconnect-overlay, and Vegas stops the radio
// (vegas.js setConnectionLost).
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
	// The import list itself is checked by the module-graph test; here it is
	// enough that some module still pulls the watchdog in, alongside the call
	// needles below.
	must("from './connection.js'", "refresh.js imports the watchdog")
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

	// connection.js owns only the watchdog transition. The Preact shell reads
	// the connection Signal, owns the overlay DOM, and keeps the established
	// id/class/alert contracts.
	must("dashboardState.setConnection('connected')", "successful polls publish connected state")
	must("dashboardState.setConnection(", "failed polls publish retry/disconnected state")
	must("state.connection.value.status === 'disconnected'", "the Preact shell subscribes to connection state")
	must(`id="shell-disconnect-root"`, "the shell has an explicit disconnect host")
	must(`id="disconnect-overlay"`, "the banner overlay ships in the HTML")
	must("disconnect-overlay${disconnected ? ' show' : ''}", "Preact derives the show class from connection state")
	must("Disconnected from agentd", "the banner states the problem")
	must(`id="disconnect-status"`, "the alert status line ships")
	must("${disconnected ? 'Reconnecting…' : ''}",
		"the Preact alert status announces only while disconnected")
	if strings.Contains(dashboardAssets, "getElementById('disconnect-overlay')") ||
		strings.Contains(dashboardAssets, "getElementById('disconnect-status')") {
		t.Error("connection watchdog must publish Signal state, not imperatively rewrite Preact-owned disconnect DOM")
	}

	// An agentd outage also kills every web terminal's WebSocket. The recovery
	// edge is published once per outage and the terminal shell redials the dead
	// panes from it. Behaviour — including which panes are eligible and the
	// once-per-outage guarantee — is covered by jstest/terminal-outage-reconnect;
	// these needles only pin that the modules are still connected to each other,
	// because a JS unit test can inject its own wiring and would not notice the
	// production graph coming apart.
	must("export function onConnectionRestored", "connection.js publishes the recovery edge")
	must("export function noteServerIdentity",
		"connection.js reads the daemon instance id, so a restart no poll refused is still seen")
	must("noteServerIdentity(data.instance_id)", "refresh.js reports each poll's daemon identity")
	must("import { onConnectionRestored } from './connection.js'",
		"the dashboard entry subscribes the terminal shell to the recovery edge")
	must("onConnectionRestored: dependencies.onConnectionRestored",
		"the terminals island descriptor forwards the recovery edge")
	must("reconnectAfterOutage", "the terminal shell has a redial pass for the recovery edge")

	// CSS: hidden by default (display:none) and revealed via .show. It remains a
	// distinct non-dialog overlay because it is connection status, not modal UI.
	must(".disconnect-overlay {", "the overlay CSS ships")
	must(".disconnect-overlay.show { display: flex; }",
		"the overlay is revealed by the .show hook connection.js adds")
}
