package agentd_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// TestDashboardUsageNavChrome drives the merged Usage tab's routing in a real
// browser: the retired /costs bookmark heals to /usage/costs, each Overview /
// Quota / Costs switch is one history entry, and native Back/Forward walk them
// in order, including back into the tab from another one.
func TestDashboardUsageNavChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "usage-nav-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL + "/costs",
		OutDir:  outDir,
		States: []dashsnap.State{{
			Key:     "usage-nav-history",
			Title:   "Usage tab routing",
			Caption: "Legacy /costs lands on /usage/costs; subtab switches push history; Back/Forward restore each view.",
			JS: `return (async function(){
  async function at(path, subview, what) {
    var deadline = Date.now() + 4000;
    while (Date.now() < deadline) {
      var section = document.querySelector('#tab-usage.active');
      if (location.pathname === path && section && section.getAttribute('data-subview') === subview) return;
      await new Promise(function(resolve){ setTimeout(resolve, 25); });
    }
    var active = document.querySelector('#tab-usage');
    throw new Error(what + ': at ' + location.pathname + ' showing ' + (active && active.getAttribute('data-subview')));
  }
  function view(name) { document.querySelector('.usage-subtab[data-usage-subtab="' + name + '"]').click(); }
  await at('/usage/costs', 'costs', 'legacy /costs deep link');
  view('overview');
  await at('/usage', 'overview', 'Overview click');
  view('quota');
  await at('/usage/quota', 'quota', 'Quota click');
  history.back();
  await at('/usage', 'overview', 'Back to Overview');
  history.back();
  await at('/usage/costs', 'costs', 'Back to Costs');
  history.forward();
  await at('/usage', 'overview', 'Forward to Overview');
  document.querySelector('nav [data-tab="groups"]').click();
  var deadline = Date.now() + 4000;
  while (location.pathname !== '/' && Date.now() < deadline) await new Promise(function(resolve){ setTimeout(resolve, 25); });
  if (location.pathname !== '/') throw new Error('Groups click did not route to /');
  history.back();
  await at('/usage', 'overview', 'Back into the Usage tab');
  document.querySelector('[data-goto-tab="costs"]')?.click();
  await at('/usage/costs', 'costs', 'header cost link');
})();`,
		}},
	})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	for _, shot := range shots {
		if shot.Err != "" {
			t.Fatalf("usage nav: %s\ncontact sheet: %s", shot.Err, filepath.Join(outDir, "index.html"))
		}
	}
}
