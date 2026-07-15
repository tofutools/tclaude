package agentd

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TCL-454 has one DOM owner and one launcher seam for message/access dialogs.
// Interactive behavior lives in the Node component tests; this guard prevents
// a static host or legacy writer from quietly returning beside the island.
func TestDashboardMessageAccessDialogsHaveSingleOwner(t *testing.T) {
	html := string(mustReadFS(dashboardAssetsFS, "dashboard.html"))
	if got := strings.Count(html, `id="message-access-dialog-root"`); got != 1 {
		t.Fatalf("message/access island host count = %d, want 1", got)
	}
	if strings.Contains(html, `id="cron-create-target-mount"`) {
		t.Fatal("message/access still ships the retired cron-target host")
	}
	for _, id := range []string{
		"sudo-pick-agent-modal", "sudo-grant-modal", "perm-edit-modal",
		"cron-pick-target-modal", "message-create-modal", "human-reply-modal",
	} {
		if strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("dashboard.html still owns migrated dialog id %q", id)
		}
	}

	messageLegacy := string(mustReadFS(dashboardAssetsFS, "js/modal-message.js"))
	for _, forbidden := range []string{
		"openMessageCreateModal", "bindMessageModal", "bindSudoModal",
		"openPermEditModal", "openSpawnPermEditor", "openGroupPermEditor",
	} {
		if strings.Contains(messageLegacy, forbidden) {
			t.Errorf("modal-message.js still contains migrated writer %q", forbidden)
		}
	}
	if _, err := fs.Stat(dashboardAssetsFS, "js/modal-human-reply.js"); err == nil {
		t.Error("legacy modal-human-reply.js still ships")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat legacy human-reply module: %v", err)
	}
	for _, forbidden := range []string{
		"from './modal-human-reply.js'",
		"bindMessageModal, bindSudoModal",
		"pickCronTargetModal, bindTargetPicker",
		"sudoGrantBlocklist",
		"configureCronTargetPicker", "readCronTargetPicker", "setCronTargetModeListener",
		"cronTargetHost", "cronTarget.value", "configureCronTarget",
	} {
		if strings.Contains(dashboardAssets, forbidden) {
			t.Errorf("dashboard assets retain migrated legacy import/wiring %q", forbidden)
		}
	}
	for _, required := range []string{
		"registerMessageAccessDialogController(controller)",
		"mountMessageAccessDialogsFeature({",
	} {
		if !dashboardSourceContains(dashboardAssets, required) {
			t.Errorf("dashboard assets missing controller ownership seam %q", required)
		}
	}
}
