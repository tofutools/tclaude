package agentd

import (
	"strings"
	"testing"
)

// The group attachment is a small cross-module dashboard surface: the group
// header launches the shared reference dialog, which persists through the
// group-scoped API. Pin the load-bearing wiring so a refactor cannot leave a
// decorative pin whose editor no longer opens or saves.
func TestDashboardAssets_GroupAttachmentWired(t *testing.T) {
	for _, c := range []struct {
		file   string
		needle string
	}{
		{"js/groups-list.js", `<${GroupAttachment} group=${group} actions=${actions} />`},
		{"js/groups-list.js", `class="group-attachment group-attachment-empty"`},
		{"js/groups-list.js", `>📌 ${label}</a>`},
		{"js/groups-list.js", `class="group-attachment-invalid muted"`},
		{"js/groups-list.js", `parsed.protocol === 'http:' || parsed.protocol === 'https:'`},
		{"js/groups-actions.js", `openGroupAttachmentDialog({`},
		{"js/action-dialog-state.js", `kind: 'group-attachment'`},
		{"js/action-dialog-island.js", `actions.setGroupAttachment({`},
		{"js/action-dialog-actions.js", `/api/groups/${encodeURIComponent(group)}/attachment`},
		{"dashboard.css", `.group-attachment-empty {`},
		{"dashboard.css", `.group-attachment-set:hover .group-attachment-edit`},
	} {
		source := dashboardAssetFile(t, c.file)
		if !strings.Contains(source, c.needle) {
			t.Errorf("%s missing %q — group attachment wiring regressed", c.file, c.needle)
		}
	}
}
