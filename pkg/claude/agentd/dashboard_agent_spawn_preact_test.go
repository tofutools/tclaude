package agentd

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardAgentSpawnPreactOwnership pins the migration boundary. Detailed
// field and interaction parity is exercised by jstest/agent-spawn-preact.test.mjs;
// this Go guard proves production loads that owner and cannot silently fall
// back to the retired static modal/binder.
func TestDashboardAgentSpawnPreactOwnership(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	absent := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contain %q (%s)", needle, why)
		}
	}

	htmlBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	html := string(htmlBytes)
	if !strings.Contains(html, "<div id=\"agent-spawn-root\">\n</div>") {
		t.Error("dashboard.html must expose only the empty stable agent-spawn root")
	}
	if strings.Contains(html, `id="agent-spawn-modal"`) {
		t.Error("dashboard.html still owns the retired static spawn modal")
	}
	if _, err := fs.Stat(dashboardAssetsFS, "js/modal-spawn.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retired js/modal-spawn.js is still embedded (stat err=%v)", err)
	}

	present("const agentSpawnDescriptor = createIslandDescriptor({", "the lazy feature has one stable descriptor")
	present("hosts: { root: '#agent-spawn-root' }", "the descriptor claims the stable root")
	present("mountAgentSpawnFeature({", "dashboard bootstrap mounts the feature")
	present("registerAgentSpawnController", "the island registers the stable imperative boundary")
	present("export function openAgentSpawnModal(options = {})", "all callers retain the stable open API")
	present("import { openAgentSpawnModal } from './agent-spawn-controller.js';", "palette/dock callers route through the controller")
	present("openAgentSpawnModal({groupName: group})", "group-row actions route through the controller")
	present("export function createSpawnDraft(", "plain model owns defaults")
	present("export function applySpawnProfile(", "plain model owns profile reconciliation")
	present("export function buildSpawnRequest(", "plain model owns the exact daemon request")
	present("export function createAgentSpawnState(", "generation state owns open/close lifecycle")
	present("export function createAgentSpawnActions(", "plain actions own HTTP/upload/worktree effects")
	present("export function AgentSpawnApp(", "Preact owns the rendered dialog")
	present("if (submitLock.current) return;", "duplicate submit is claimed synchronously")
	present("!state.isCurrent(current.generation)", "async returns are generation guarded")
	present("spawnDraftIsDirty(draft, baseline, attachments.length)", "dismissal reads controlled state")
	present("onPaste=${paste}", "component owns attachment paste")
	present("onDrop=${drop}", "component owns attachment drop")
	present("URL.revokeObjectURL(attachment.url)", "component disposes previews")
	present("resizeKey=\"tclaude.dash.modalSize.agent-spawn\"", "component preserves modal size")
	present("initialFocusRef=${nameRef}", "component preserves initial/restored focus")
	present("event.isComposing", "shared overlay rejects IME submit")
	absent("bindAgentSpawnModal", "the legacy binder is retired")
	absent("closeAgentSpawnModal", "the legacy close path is retired")
}
