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
		"spawn row exposes launch intent for every capable harness")
	must("sshWorkaround: !!harness?.can_ssh_workaround",
		"capable harnesses default the checkbox on")
	must("checked=${draft.sshWorkaround}",
		"spawn checkbox renders the preserved intent")

	must(`id="profile-editor-ssh-workaround"`, "profile editor has an SSH workaround checkbox")
	must(`hidden=${!hEntry?.can_ssh_workaround}`, "profile checkbox is capability gated")
	must("checked=${draft.ssh_workaround}", "profile checkbox renders intent independent of its sandbox tier")
	must("body.ssh_workaround = !!draft.ssh_workaround",
		"profile payload preserves intent across independently composed tiers")
}
