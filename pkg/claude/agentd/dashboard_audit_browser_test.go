package agentd_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDashboardAuditSpawnPopoverChrome is the focused browser acceptance check
// for the audit spawn-details disclosure. It proves real pointer/keyboard
// input opens the panel, transfers focus, dismisses with Escape, restores focus
// to the [?] trigger, and dismisses when the operator clicks outside. A second
// state re-runs it under the wizard skin — self-driven, for the reason spelled
// out at wizardStateJS — to prove the disclosure is painted arcane too.
func TestDashboardAuditSpawnPopoverChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	detail, err := json.Marshal(map[string]any{
		"kind":     "tclaude.spawn.audit.v1",
		"summary":  "role: reviewer",
		"input":    map[string]any{"name": "worker", "blob": strings.Repeat("input ", 500)},
		"resolved": map[string]any{"params": map[string]any{"harness": "codex"}, "blob": strings.Repeat("resolved ", 500)},
		"response": map[string]any{"code": "invalid_profile", "error": "profile does not exist", "blob": strings.Repeat("response ", 500)},
	})
	if err != nil {
		t.Fatalf("marshal audit fixture: %v", err)
	}
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		ActorKind: db.AuditActorHuman, ActorLabel: "operator", Verb: "spawn",
		GroupName: "frontend-squad", TargetLabel: "worker", Detail: string(detail),
		Status: 400, Source: db.AuditSourceDashboard,
	}); err != nil {
		t.Fatalf("seed audit fixture: %v", err)
	}

	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()
	outDir := filepath.Join(dashSnapOutRoot(t), "audit-spawn-popover-"+time.Now().Format("20060102-150405.000"))
	openAuditTabJS := `var tab = document.querySelector('nav [data-tab="audit"]');
if (!tab) throw new Error('audit tab missing');
tab.click();
var deadline = Date.now() + 5000;
while (!document.querySelector('.audit-spawn-info-trigger') && Date.now() < deadline) await new Promise(function(resolve){setTimeout(resolve, 50);});
if (!document.querySelector('.audit-spawn-info-trigger')) throw new Error('spawn details trigger did not render');`
	// The wizard state drives the disclosure from its own JS rather than through
	// BrowserActions: no CDP-synthesized pointer or key input reaches a page
	// dashsnap loaded via ?wizard=1, so any input-driven wizard state fails.
	// That is a harness-level defect, not a skin one — it reproduces on main and
	// on other wizard states (e.g. process-editor-browser-node-click), and the
	// same skin applied client-side takes trusted input fine. Until it is fixed,
	// the real-input contract — focus transfer, Escape, outside-click dismissal
	// — stays covered by the default skin, which shares the markup; this state
	// covers the arcane paint.
	wizardStateJS := openAuditTabJS + `
document.querySelector('.audit-spawn-info-trigger').click();
await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
var panel = document.querySelector('.audit-spawn-popover');
var body = panel && panel.querySelector('pre');
if (!panel || !body) throw new Error('spawn details popover did not open under the wizard skin');
var panelStyle = getComputedStyle(panel);
if (!panelStyle.backgroundImage.includes('linear-gradient')) throw new Error('wizard spawn details kept the flat dashboard surface: ' + panelStyle.backgroundImage);
if (getComputedStyle(body).backgroundColor !== 'rgb(20, 15, 40)') throw new Error('wizard spawn details JSON body kept the default dark background: ' + getComputedStyle(body).backgroundColor);`
	makeState := func(key, title string, wizard bool) dashsnap.State {
		stateJS, actions := openAuditTabJS, []dashsnap.BrowserAction{
			{Kind: "click", Selector: ".audit-spawn-info-trigger"},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){requestAnimationFrame(function(){requestAnimationFrame(function(){try{
var panel = document.querySelector('.audit-spawn-popover');
var close = panel && panel.querySelector('.audit-spawn-popover-close');
if (!panel || !close) throw new Error('spawn details popover did not open');
if (panel.querySelectorAll('section').length !== 3) throw new Error('spawn details sections did not render');
if (getComputedStyle(panel.querySelector('section')).display === 'none') throw new Error('spawn details sections are hidden by global tab styling');
if (!panel.textContent.includes('Request input') || !panel.textContent.includes('Resolved parameters and profiles') || !panel.textContent.includes('Command response')) throw new Error('spawn details body labels did not render');
if (document.activeElement !== close) throw new Error('opening the popover did not focus close');
resolve();
}catch(error){reject(error);}});});});`},
			{Kind: "key", Key: "Escape"},
			{Kind: "eval", JS: `var trigger = document.querySelector('.audit-spawn-info-trigger');
if (document.querySelector('.audit-spawn-popover')) throw new Error('Escape did not close spawn details');
				if (trigger.getAttribute('aria-expanded') !== 'false' || document.activeElement !== trigger) throw new Error('Escape did not restore trigger focus');`},
			{Kind: "click", Selector: ".audit-spawn-info-trigger"},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){requestAnimationFrame(function(){requestAnimationFrame(function(){try{
if (!document.querySelector('.audit-spawn-popover')) throw new Error('spawn details did not reopen');
resolve();
}catch(error){reject(error);}});});});`},
			{Kind: "click", Selector: "#filter-audit"},
			{Kind: "eval", JS: `if (document.querySelector('.audit-spawn-popover')) throw new Error('outside click did not close spawn details');`},
			{Kind: "click", Selector: ".audit-spawn-info-trigger"},
			{Kind: "eval", JS: `if (!document.querySelector('.audit-spawn-popover')) throw new Error('spawn details did not reopen for the scrollbar capture');`},
		}
		if wizard {
			stateJS, actions = wizardStateJS, nil
		}
		return dashsnap.State{
			Key:     key,
			Title:   title,
			Caption: "The [?] disclosure focuses its close control, Escape restores focus to the trigger, an outside click dismisses it, and both dashboard skins are covered.",
			Wizard:  wizard,
			JS:      `return (async function(){` + stateJS + `})();`,
			Actions: actions,
		}
	}
	states := []dashsnap.State{
		makeState("audit-spawn-popover-focus-dismiss", "Audit spawn details focus and dismissal", false),
		makeState("audit-spawn-popover-wizard-skin", "Audit spawn details wizard skin", true),
	}
	shots, err := dashsnap.Capture(dashsnap.Config{BaseURL: srv.URL, OutDir: outDir, States: states})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	var failed []string
	for _, shot := range shots {
		if shot.Err != "" {
			failed = append(failed, shot.State.Key+": "+shot.Err)
		}
	}
	if len(failed) != 0 {
		t.Fatalf("audit spawn popover browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("audit spawn popover browser smoke: %s", filepath.Join(outDir, "index.html"))
}
