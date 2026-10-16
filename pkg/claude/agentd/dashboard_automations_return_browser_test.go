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

// TestDashboardAutomationsReturnChrome proves that leaving Automations from
// its Processes subtab and clicking the Automations nav again lands back on
// /automations/processes/templates rather than the Jobs island's kind.
func TestDashboardAutomationsReturnChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "automations-return-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		Width:   1280,
		Height:  800,
		States: []dashsnap.State{{
			Key:     "automations-return-processes",
			Title:   "Automations returns to Processes",
			Caption: "Groups → Automations reopens the Processes subtab the operator left from.",
			JS: `return (async function(){
  var waitFor=async function(predicate,label){
    var deadline=Date.now()+5000;
    while(Date.now()<deadline){var value=predicate();if(value)return value;await new Promise(function(r){setTimeout(r,40);});}
    throw new Error(label);
  };
  var auto=document.querySelector('nav [data-tab="jobs"]');
  auto.click();
  (await waitFor(function(){return document.querySelector('#tab-jobs [data-automations-subtab="processes"]');},'Processes subtab missing')).click();
  await waitFor(function(){return location.pathname==='/automations/processes/templates';},'did not reach /automations/processes/templates: '+location.pathname);
  document.querySelector('nav [data-tab="groups"]').click();
  await waitFor(function(){return document.querySelector('#tab-groups.active');},'did not reach Groups: '+location.pathname);
  auto.click();
  await waitFor(function(){return location.pathname==='/automations/processes/templates'&&document.querySelector('#tab-processes.active');},'Automations did not return to Processes: '+location.pathname);
  // A history Back from Processes restores Groups exactly.
  history.back();
  await waitFor(function(){return document.querySelector('#tab-groups.active');},'Back did not restore Groups');
  // Picking a Jobs kind from outside goes to that kind, not Processes.
  auto.click();
  await waitFor(function(){return document.querySelector('#tab-processes.active');},'second return did not reach Processes');
  (await waitFor(function(){return document.querySelector('#tab-processes [data-automations-subtab="cron"]');},'Cron subtab missing')).click();
  await waitFor(function(){return location.pathname==='/automations/cron-jobs'&&document.querySelector('#tab-jobs.active');},'cron pick did not land on Jobs: '+location.pathname);
  document.querySelector('nav [data-tab="groups"]').click();
  auto.click();
  await waitFor(function(){return location.pathname==='/automations/cron-jobs'&&document.querySelector('#tab-jobs.active');},'Automations did not return to Cron jobs: '+location.pathname);
})();`,
			SettleMS: 100,
		}},
	})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	for _, shot := range shots {
		if shot.Err != "" {
			t.Fatalf("%s: %s\ncontact sheet: %s", shot.State.Key, shot.Err, filepath.Join(outDir, "index.html"))
		}
	}
}
