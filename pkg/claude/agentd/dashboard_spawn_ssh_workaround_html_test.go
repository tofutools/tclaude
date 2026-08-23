package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_SSHWorkaroundWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	must(`id="agent-spawn-ssh-workaround-row"`, "spawn dialog has an SSH workaround row")
	must(`id="agent-spawn-ssh-workaround"`, "spawn dialog has an SSH workaround checkbox")
	must("const showSSHWorkaround = !!harness?.can_ssh_workaround",
		"spawn row gates on the harness capability")
	must("sshWorkaroundAvailable = showSSHWorkaround",
		"spawn row distinguishes managed sandbox availability")
	must("sshWorkaround: !!harness?.can_ssh_workaround",
		"capable harnesses default the checkbox on")
	must("view.sshWorkaroundAvailable && draft.sshWorkaround",
		"spawn request always carries the visible checkbox state")

	must(`id="profile-editor-ssh-workaround"`, "profile editor has an SSH workaround checkbox")
	must(`hidden=${!hEntry?.can_ssh_workaround}`, "profile checkbox is capability gated")
	must("draft.sandbox_implementation === 'tclaude-layer'",
		"profile payload supports the dedicated tclaude layer")
	must("draft.sandbox_implementation === 'stacked'",
		"profile payload supports the outer layer in stacked mode")
}
