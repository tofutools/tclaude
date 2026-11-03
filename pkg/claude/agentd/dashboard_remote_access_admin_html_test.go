package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_RemoteAccessAdminWired guards the Config tab's cert-
// management section (JOH-278): the remote-admin.js island, the endpoints it
// drives off /api/remote-access/*, and the Config form that renders it. A
// rename in one file silently breaks the panel in the browser; this pins
// the cross-file contract at `go test ./...`.
func TestDashboardAssets_RemoteAccessAdminWired(t *testing.T) {
	for _, needle := range []string{
		// The section + the four feature areas' anchors.
		`id="cfg-remote-admin"`,
		`id="ra-admin-sans"`,    // server cert SAN list (visibility)
		`id="ra-addhosts-btn"`,  // add host names (public URL support)
		`id="ra-admin-devices"`, // device list
		`id="ra-addclient-btn"`, // add a device
		`id="ra-ca-btn"`,        // CA download
		`id="ra-setup-btn"`,     // first-time setup / regenerate
		// The endpoints the island drives.
		"/api/remote-access/info",
		"/api/remote-access/add-hosts",
		"/api/remote-access/add-client",
		"/api/remote-access/client?name=",
		"/api/remote-access/ca.crt",
		"/api/remote-access/setup",
		"export function RemoteAdminPanel(",
		// The Config form renders it in the Daemon & network panel.
		"import { RemoteAdminPanel } from './remote-admin.js'",
		"<${RemoteAdminPanel} />",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — remote-access cert-management wiring broken", needle)
		}
	}
}
