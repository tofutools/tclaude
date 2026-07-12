package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_RemoteAccessWired guards the Config tab's "Remote access"
// section (JOH-227). Like Ask defaults, it is ordinary widgets of the big
// config form — populated by populateConfigForm, read by assembleConfig, and
// saved through the existing /api/config dry-run/diff/confirm flow — NOT a
// separate endpoint. The single config key `remote_access.bind` is edited as an
// interface + port pair (splitBind/joinBind), with a live status line driven by
// the snapshot's remote_access state. This pins that wiring so a change to one
// file can't silently break it.
func TestDashboardAssets_RemoteAccessWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the section heading + the three field anchors + the status line.
		"<h3>Remote access</h3>",
		`id="cfg-remote-enabled"`,
		`id="cfg-remote-host"`,
		`id="cfg-remote-port"`,
		`id="cfg-remote-status"`,
		// HTML: the setup prerequisite is spelled out (the foot-gun is that
		// enabling without generated material is a silent no-op).
		"tclaude remote-access setup",
		// JS: bind decomposition + the enable/bind assembly into cfg.remote_access.
		"function splitBind(",
		"function joinBind(",
		"function syncCfgRemoteStatus(",
		"cfg.remote_access || {}",
		"raCfg.enabled = true;",
		"raCfg.bind = raBind;",
		"cfg.remote_access = raCfg;",
		// JS: the live status reads the snapshot's material/running state.
		"latestSnapshot().remote_access",
		"ra.material_exists",
		"ra.running",
		// JS: enabling without a port falls back to the conventional 8443.
		"const REMOTE_DEFAULT_PORT = '8443';",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Remote access wiring broken", needle)
		}
	}
}
