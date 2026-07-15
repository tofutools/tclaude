package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardOperatorMessageModalPolishWired(t *testing.T) {
	js := readDashboardJS(t, "modal-operator-message.js")
	for _, needle := range []string{
		"import { makeModalResizable } from './helpers.js';",
		"import { snapshotOperatorMessageDraft } from './operator-message-model.js';",
		"import { bindBackdropDiscard } from './refresh.js';",
		"dismissGuard = bindBackdropDiscard('operator-message-modal', close, () => !pending);",
		"dismissGuard.tryDismiss()",
		"dismissGuard?.markDirty()",
		"const draft = snapshotOperatorMessageDraft({",
		"const attachmentToken = await upload(draft.files);",
		"setPending(true);",
		"el('operator-message-body').readOnly = next;",
		"'operator-message-attach-btn', 'operator-message-attach-input'",
		"'tclaude.dash.modalSize.operator-message'",
		"restoreFocus = typeof nextTarget.restoreFocus === 'function' ? nextTarget.restoreFocus : null;",
		"const focus = restoreFocus;",
		"if (!modal?.classList.contains('show')) focus?.();",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("operator composer missing %q", needle)
		}
	}
	if strings.Contains(js, "if (event.key === 'Escape')") {
		t.Error("operator composer must leave Escape to the shared dirty-dismiss guard")
	}
	if strings.Contains(js, "if (event.target === modal) close()") {
		t.Error("operator composer must leave backdrop dismissal to the shared dirty-dismiss guard")
	}

	for _, needle := range []string{
		`aria-describedby="operator-message-desc"`,
		`class="cron-create-row operator-message-body-row"`,
		`class="theme-copy-wizard">✒ Send a missive to the familiar</span>`,
		`class="theme-copy-wizard">Dispel</span>`,
		`class="theme-copy-wizard">✒ Send missive</span>`,
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("operator composer wizard markup missing %q", needle)
		}
	}
}

func TestDashboardOperatorMessageModalResizableAndWizardStyled(t *testing.T) {
	data, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(data)
	for _, needle := range []string{
		"#operator-message-modal .cron-create-modal {\n  resize: both; overflow: auto;\n}",
		"#operator-message-modal .operator-message-body-row {\n  flex: 1 1 auto; min-height: 100px; align-items: stretch;\n}",
		"#operator-message-modal .operator-message-body-row textarea {\n  align-self: stretch; min-height: 0; max-height: none; resize: none;\n}",
		"#operator-message-modal .modal-buttons { flex: 0 0 auto; }",
		"body.wizard #operator-message-modal .cron-create-modal {",
		"body.wizard #operator-message-modal .cron-create-row :is(input[type=text], textarea)",
		"body.wizard #operator-message-modal .spawn-attachments-list li",
		"body.wizard #operator-message-modal #operator-message-submit",
		"body.wizard #operator-message-modal :is(.cron-create-modal, .cron-create-row textarea)::-webkit-scrollbar-thumb",
	} {
		if !strings.Contains(css, needle) {
			t.Errorf("operator composer CSS missing %q", needle)
		}
	}
}
