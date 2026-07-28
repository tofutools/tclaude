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
		{"js/groups-list.js", `snapshot?.group_attachments_mode === 'float'`},
		{"js/groups-list.js", `snapshot?.group_attachments_mode === 'fixed'`},
		{"js/groups-list.js", `placement="float"`},
		{"js/groups-list.js", `placement="fixed"`},
		{"js/groups-list.js", `class="group-attachment-label"`},
		{"js/groups-list.js", `tabindex=${fixed ? '-1' : undefined}`},
		{"js/groups-list.js", `group-attachment-empty`},
		{"js/groups-list.js", `const visibleIcon = fixed && rawURL ? null : '📎';`},
		{"js/groups-list.js", `>${visibleIcon}${visibleLabel}</a>`},
		{"js/groups-list.js", `class="group-attachment-invalid"`},
		{"js/groups-list.js", `/^https?:\/\/[^/\\?#\s]/i.test(raw)`},
		{"js/groups-list.js", `parsed.protocol === 'http:' || parsed.protocol === 'https:'`},
		{"js/groups-actions.js", `openGroupAttachmentDialog({`},
		{"js/action-dialog-state.js", `kind: 'group-attachment'`},
		{"js/action-dialog-island.js", `snapshot?.value?.group_attachments_mode`},
		{"js/action-dialog-island.js", `mode === 'float' || mode === 'fixed'`},
		{"js/action-dialog-island.js", `if (!enabled) state.close(descriptor);`},
		{"js/action-dialog-island.js", `actions.setGroupAttachment({`},
		{"js/action-dialog-actions.js", `/api/groups/${encodeURIComponent(group)}/attachment`},
		{"dashboard.css", `.group-attachment-float {`},
		{"dashboard.css", `position: absolute; z-index: 4;`},
		{"dashboard.css", `inset-block-start: -18px; inset-inline-start: 4px;`},
		{"dashboard.css", `display: inline-flex; flex-direction: row;`},
		{"dashboard.css", `border: 0; background: transparent; box-shadow: none;`},
		{"dashboard.css", `text-decoration: none; cursor: pointer;`},
		{"dashboard.css", `box-sizing: border-box; justify-content: center; cursor: pointer;`},
		{"dashboard.css", `@media (hover: hover) and (pointer: fine)`},
		{"dashboard.css", `.group-attachment-float { opacity: 0; pointer-events: none; }`},
		{"dashboard.css", `summary:hover .group-attachment-float`},
		{"dashboard.css", `.group-attachment-float:has(:focus-visible)`},
		{"dashboard.css", `.group-attachment a:focus-visible`},
		{"dashboard.css", `.group-attachment-float.group-attachment-set:hover .group-attachment-edit`},
		{"dashboard.css", `.group-attachment-fixed {`},
		{"dashboard.css", `position: static; z-index: auto;`},
		{"dashboard.css", `margin: 0 0 0 8px; padding: 1px 0;`},
		{"dashboard.css", `.group-attachment-fixed > a:hover {
  color: #58a6ff; background: transparent; text-decoration: underline;
}`},
		{"dashboard.css", `.group-attachment-fixed > button:hover {
  color: #58a6ff; background: transparent; text-decoration: none;
}`},
		{"dashboard.css", `.group-attachment-fixed.group-attachment-empty:hover {
  color: #58a6ff; border-color: #58a6ff; opacity: 1;
}`},
		{"dashboard.css", `.group-attachment-label {`},
		{"dashboard.css", `.group-attachment-fixed:hover .group-attachment-edit`},
		{"dashboard.css", `summary:hover .group-attachment-fixed`},
		{"dashboard.css", `.quick-hover > summary .group-attachment-fixed`},
		{"dashboard.css", `[open] > summary .group-attachment-fixed`},
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
