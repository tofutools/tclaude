package agentd

import (
	"strings"
	"testing"
)

// The group attachment is a small cross-module dashboard surface: the group
// header launches the shared reference dialog, which persists through the
// group-scoped API. Pin the load-bearing wiring so a refactor cannot leave a
// decorative paperclip whose editor no longer opens or saves.
func TestDashboardAssets_GroupAttachmentWired(t *testing.T) {
	for _, c := range []struct {
		file   string
		needle string
	}{
		{"js/groups-list.js", `<${GroupAttachment} group=${group} actions=${actions} />`},
		{"js/groups-list.js", `class="group-attachment group-attachment-empty"`},
		{"js/groups-list.js", `>📎</a>`},
		{"js/groups-list.js", `class="group-attachment-invalid"`},
		{"js/groups-list.js", `parsed.protocol === 'http:' || parsed.protocol === 'https:'`},
		{"js/groups-actions.js", `openGroupAttachmentDialog({`},
		{"js/action-dialog-state.js", `kind: 'group-attachment'`},
		{"js/action-dialog-island.js", `actions.setGroupAttachment({`},
		{"js/action-dialog-actions.js", `/api/groups/${encodeURIComponent(group)}/attachment`},
		{"dashboard.css", `position: absolute; z-index: 4;`},
		{"dashboard.css", `inset-block-start: -4px; inset-inline-start: -12px;`},
		{"dashboard.css", `border: 0; background: transparent; box-shadow: none;`},
		{"dashboard.css", `@media (hover: hover) and (pointer: fine)`},
		{"dashboard.css", `.group-attachment { opacity: 0; pointer-events: none; }`},
		{"dashboard.css", `summary:hover .group-attachment`},
		{"dashboard.css", `.group-attachment:has(:focus-visible)`},
		{"dashboard.css", `.group-attachment a:focus-visible`},
		{"dashboard.css", `.group-attachment-set:hover .group-attachment-edit`},
	} {
		source := dashboardAssetFile(t, c.file)
		if !strings.Contains(source, c.needle) {
			t.Errorf("%s missing %q — group attachment wiring regressed", c.file, c.needle)
		}
	}
	css := dashboardAssetFile(t, "dashboard.css")
	if strings.Contains(css, `summary:focus-within .group-attachment`) {
		t.Error("group focus must not pin the attachment overlay open")
	}
}
