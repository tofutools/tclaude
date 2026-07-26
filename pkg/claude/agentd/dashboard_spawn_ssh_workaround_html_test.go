package agentd

import (
	"strings"
	"testing"
)

func TestDashboardHTML_CodexSSHWorkaroundWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	must(`id="agent-spawn-ssh-workaround-row"`, "spawn dialog has an SSH workaround row")
	must(`id="agent-spawn-ssh-workaround"`, "spawn dialog has an SSH workaround checkbox")
	must("showSSHWorkaround: harness ? !!harness.can_ssh_workaround",
		"spawn row gates on the harness capability")
	must("sshWorkaround: !!harness?.can_ssh_workaround",
		"Codex-capable harnesses default the checkbox on")
	must("if (view.showSSHWorkaround) body.ssh_workaround = !!draft.sshWorkaround",
		"spawn request always carries the visible checkbox state")

	must(`id="profile-editor-ssh-workaround"`, "profile editor has an SSH workaround checkbox")
	must(`hidden=${!hEntry?.can_ssh_workaround}`, "profile checkbox is Codex-capability gated")
	must("if (h?.can_ssh_workaround) body.ssh_workaround = !!draft.ssh_workaround",
		"profile payload carries both opt-in and opt-out")
}
