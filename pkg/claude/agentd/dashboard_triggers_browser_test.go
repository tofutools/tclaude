package agentd_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TestDashboardTriggerEditorChrome covers the CSS cascade that DOM-only
// component tests cannot see. Dashboard page tabs globally hide semantic
// sections, so every section inside an open dialog must resolve to visible in
// the real production markup and stylesheet.
func TestDashboardTriggerEditorChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke - set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	if err := config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}); err != nil {
		t.Fatalf("enable triggers: %v", err)
	}
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "trigger-editor-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		States: []dashsnap.State{{
			Key:     "new-trigger-editor",
			Title:   "New trigger editor",
			Caption: "The semantic WHEN, WHERE, and THEN sections remain visible under the dashboard's production CSS cascade.",
			JS: `return (async function(){
  document.querySelector('nav [data-tab="jobs"]').click();
  var deadline = Date.now() + 5000;
  var triggerTab;
  while (!(triggerTab = Array.from(document.querySelectorAll('.jobs-subtab')).find(function(node){ return node.textContent.trim() === 'Triggers'; })) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  if (!triggerTab) throw new Error('Triggers automation view did not render');
  triggerTab.click();
  var open;
  while (!(open = document.querySelector('#trigger-create-open')) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  if (!open) throw new Error('new trigger control did not render');
  open.click();
  var dialog;
  while (!(dialog = document.querySelector('#trigger-modal .trigger-modal')) && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 25); });
  }
  if (!dialog) throw new Error('trigger dialog did not open');
  var sections = Array.from(dialog.querySelectorAll('section'));
  if (sections.length !== 3) throw new Error('expected WHEN/WHERE/THEN sections, got ' + sections.length);
  var hidden = sections.filter(function(section){ return getComputedStyle(section).display === 'none' || section.getClientRects().length === 0; });
  if (hidden.length) throw new Error('dialog semantic sections hidden by page-tab CSS: ' + hidden.map(function(section){ return section.textContent.trim().slice(0, 20); }).join(', '));
  var labels = sections.map(function(section){ return section.querySelector('.trigger-step-label').textContent.trim(); }).join(' ');
  if (labels !== 'WHEN WHERE THEN') throw new Error('trigger steps are incomplete: ' + labels);
})();`,
		}},
	})
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
		t.Fatalf("trigger editor browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("trigger editor browser smoke: %s", filepath.Join(outDir, "index.html"))
}
