package agentd_test

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDashSnapGroupsRouteMap is the bounded browser smoke for the selected
// Groups Route map surface. The fixture creates one real authority route and
// lease, then exercises graph, exact-list, and safe detail rendering through
// the production dashboard handler.
func TestDashSnapGroupsRouteMap(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	featureConfig, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if featureConfig.Features == nil {
		featureConfig.Features = &config.FeaturesConfig{}
	}
	featureConfig.Features.GroupsRouteMap = true
	if err := config.Save(featureConfig); err != nil {
		t.Fatal(err)
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		t.Fatal(err)
	}
	var group *db.AgentGroup
	for _, candidate := range groups {
		if candidate != nil && candidate.Name == "frontend-squad" {
			group = candidate
			break
		}
	}
	if group == nil {
		t.Fatal("route-map fixture group not found")
	}
	publisher, err := db.AgentIDForConv("f1000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := db.AgentIDForConv("f1000000-0000-4000-8000-000000000002")
	if err != nil {
		t.Fatal(err)
	}
	charts, err := db.AgentIDForConv("f1000000-0000-4000-8000-000000000003")
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := db.AgentIDForConv(badgesConv)
	if err != nil {
		t.Fatal(err)
	}
	makeRoute := func(name, publisherAgent, publisherConv, publisherGeneration, consumerAgent, consumerConv, consumerGeneration string, generation int64) (*db.AgentRouteLease, error) {
		route, routeErr := db.CreateAgentRoute(group.ID, publisherAgent, publisherConv, publisherGeneration, generation, name, "tcp", "tcp://127.0.0.1:9000")
		if routeErr != nil {
			return nil, routeErr
		}
		return db.OpenAgentRouteLease(route.ID, consumerAgent, consumerConv, consumerGeneration, generation)
	}
	staleLease, err := makeRoute("legacy-stale", publisher, "f1000000-0000-4000-8000-000000000001", "darwin-launch", consumer, "f1000000-0000-4000-8000-000000000002", "consumer-launch", group.RouteGeneration)
	if err != nil {
		t.Fatal(err)
	}
	// Replacing an existing membership intentionally advances the group route
	// generation, leaving the first real authority row stale.
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: group.ID, ConvID: "f1000000-0000-4000-8000-000000000001", Role: "lead"}); err != nil {
		t.Fatal(err)
	}
	group, err = db.GetAgentGroupByID(group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterDarwinRouteLaunch(publisher, "f1000000-0000-4000-8000-000000000001", "darwin-launch", []int{41001, 41002, 41003, 41004, 41005, 41006}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateDarwinRouteLaunch(publisher, "f1000000-0000-4000-8000-000000000001", "darwin-launch"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermRoutesPublish, agentd.PermRoutesConsume}, "TCL-956"); err != nil {
		t.Fatal(err)
	}
	group, err = db.GetAgentGroupByID(group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterDarwinRouteLaunch(watcher, badgesConv, "darwin-watch", []int{42001, 42002, 42003, 42004}); err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateDarwinRouteLaunch(watcher, badgesConv, "darwin-watch"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.DeleteDarwinRouteLaunch(publisher, "f1000000-0000-4000-8000-000000000001", "darwin-launch"); err != nil {
			t.Logf("delete Darwin route fixture: %v", err)
		}
		if err := db.DeleteDarwinRouteLaunch(watcher, badgesConv, "darwin-watch"); err != nil {
			t.Logf("delete Darwin watcher route fixture: %v", err)
		}
	})
	readyLease, err := makeRoute("metrics-ready", charts, "f1000000-0000-4000-8000-000000000003", "publisher-launch", watcher, badgesConv, "consumer-launch", group.RouteGeneration)
	if err != nil {
		t.Fatal(err)
	}
	refusedLease, err := makeRoute("events-refused", watcher, badgesConv, "publisher-launch", watcher, badgesConv, "darwin-watch", group.RouteGeneration)
	if err != nil {
		t.Fatal(err)
	}
	// The adapter status is the same read-only projection consumed by the
	// dashboard; refusal detail itself is deliberately never serialized.
	t.Cleanup(agentd.SetRouteConsumerEndpointStatusForTest(refusedLease.ID, "refused"))
	_ = staleLease
	_ = readyLease
	t.Cleanup(agentd.SetRouteMapPlatformForTest("darwin"))

	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()
	stamp := time.Now().Format("20060102-150405.000")
	states := []dashsnap.State{
		{
			Key:      "groups-route-map-graph",
			Title:    "Groups — Route map graph",
			Caption:  "Multiple agents and explicit ready/pending, refused, and stale authority edges render with one selected edge and safe detail.",
			JS:       routeMapGraphJS(),
			SettleMS: 400,
		},
		{
			Key:      "groups-route-map-list-detail",
			Title:    "Groups — Route map exact list/detail",
			Caption:  "Exact list selection opens safe route detail without endpoint addresses, targets, generations, or payload data.",
			JS:       routeMapListDetailJS(),
			SettleMS: 400,
		},
		{
			Key:      "groups-route-map-narrow",
			Title:    "Groups — Route map narrow",
			Caption:  "The selected graph/detail surface stacks at the narrow responsive boundary while keeping the graph bounded.",
			Width:    560,
			Height:   900,
			JS:       routeMapGraphJS(),
			SettleMS: 400,
		},
	}
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  filepath.Join(dashSnapOutRoot(t), "groups-route-map-"+stamp),
		States:  states,
	})
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, shot := range shots {
		if shot.Err != "" {
			t.Errorf("%s: %s", shot.State.Key, shot.Err)
		}
	}
}

func routeMapGraphJS() string {
	return `return (async function() {
  document.querySelector('nav [data-tab="groups"]').click();
  if (!document.querySelector('#groups-list') || document.querySelector('.route-map-table')) throw new Error('route-map graph: Members view contains route rows');
  var deadline = Date.now() + 5000;
  var routeTab;
  while (Date.now() < deadline) {
    routeTab = [...document.querySelectorAll('#groups-route-map-root [role="tab"]')].find(function(button) { return button.textContent.trim() === 'Route map'; });
    if (routeTab) break;
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  if (!routeTab) throw new Error('route-map graph: subnav did not mount');
  routeTab.click();
  deadline = Date.now() + 5000;
  while (!document.querySelector('.route-map-graph') && Date.now() < deadline) {
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  if (!document.querySelector('.route-map-graph')) throw new Error('route-map graph: graph did not render');
  if (document.querySelectorAll('.route-map-node').length < 4 || document.querySelectorAll('.route-map-edge').length < 3) throw new Error('route-map graph: multiple nodes/edges missing');
  var graphText = document.querySelector('.route-map-graph').textContent;
  for (var label of ['pending', 'refused', 'stale']) if (!graphText.includes(label)) throw new Error('route-map graph: ' + label + ' edge status missing');
  var selected = [...document.querySelectorAll('.route-map-edge')].find(function(edge) { return edge.textContent.includes('metrics-ready'); });
  if (!selected) throw new Error('route-map graph: selectable ready edge missing');
  selected.dispatchEvent(new MouseEvent('click', {bubbles: true}));
  await new Promise(function(resolve) { requestAnimationFrame(function() { requestAnimationFrame(resolve); }); });
  var detail = document.querySelector('.route-map-detail:not(.route-map-detail-empty)');
  if (!detail || !detail.textContent.includes('Route detail')) throw new Error('route-map graph: selected detail missing');
  if (!detail.textContent.includes('pending') || detail.textContent.includes('127.0.0.1') || detail.textContent.includes('publisher-launch')) throw new Error('route-map graph: unsafe or pending detail state missing');
  if (!document.querySelector('.route-map-mode button')) throw new Error('route-map graph: list toggle missing');
  var groupSelect = document.querySelector('.route-map-toolbar select');
  if (!groupSelect || [...groupSelect.options].some(function(option) { return option.value === ''; })) throw new Error('route-map graph: unscoped All groups option leaked');
  var disclosure = document.querySelector('.route-map-disclosure');
  if (!disclosure || !disclosure.textContent.includes('2/10 in use across 2 per-launch pools') || !disclosure.textContent.includes('8 available') || !disclosure.textContent.includes('Partial')) throw new Error('route-map graph: Darwin per-launch used/available Partial disclosure missing');
})();`
}

func routeMapListDetailJS() string {
	return `return (async function() {
  document.querySelector('nav [data-tab="groups"]').click();
  var deadline = Date.now() + 5000;
  var routeTab;
  while (Date.now() < deadline) {
    routeTab = [...document.querySelectorAll('#groups-route-map-root [role="tab"]')].find(function(button) { return button.textContent.trim() === 'Route map'; });
    if (routeTab) break;
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  if (!routeTab) throw new Error('route-map list: subnav did not mount');
  routeTab.click();
  var groupSelect;
  deadline = Date.now() + 5000;
  while (!(groupSelect = document.querySelector('.route-map-toolbar select')) && Date.now() < deadline) {
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  if (!groupSelect) throw new Error('route-map list: group scope control missing');
  groupSelect.value = 'infra-crew';
  groupSelect.dispatchEvent(new Event('change', {bubbles: true}));
  await new Promise(function(resolve) { requestAnimationFrame(function() { requestAnimationFrame(resolve); }); });
  if (document.querySelector('.route-map-detail:not(.route-map-detail-empty)')) throw new Error('route-map list: cross-group route detail survived group change');
  groupSelect.value = 'frontend-squad';
  groupSelect.dispatchEvent(new Event('change', {bubbles: true}));
  await new Promise(function(resolve) { requestAnimationFrame(function() { requestAnimationFrame(resolve); }); });
  var listButton;
  deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    listButton = [...document.querySelectorAll('.route-map-mode button')].find(function(button) { return button.textContent.trim() === 'Exact list'; });
    if (listButton) break;
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  if (!listButton) throw new Error('route-map list: mode control missing');
  listButton.click();
  deadline = Date.now() + 5000;
  while (!document.querySelector('.route-map-table tbody tr') && Date.now() < deadline) {
    await new Promise(function(resolve) { setTimeout(resolve, 40); });
  }
  var row = document.querySelector('.route-map-table tbody tr');
  if (!row) throw new Error('route-map list: exact route row missing');
  if (document.querySelectorAll('.route-map-table tbody tr').length < 3) throw new Error('route-map list: populated authority rows missing');
  var refused = [...document.querySelectorAll('.route-map-table tbody tr')].find(function(candidate) { return candidate.textContent.includes('events-refused'); });
  if (!refused || !refused.textContent.includes('refused')) throw new Error('route-map list: refused status missing');
  if (!document.body.textContent.includes('stale') || !document.body.textContent.includes('pending')) throw new Error('route-map list: stale/pending statuses missing');
  refused.click();
  await new Promise(function(resolve) { requestAnimationFrame(function() { requestAnimationFrame(resolve); }); });
  var detail = document.querySelector('.route-map-detail:not(.route-map-detail-empty)');
  if (!detail || !detail.textContent.includes('Stable reference') || !detail.textContent.includes('refused')) throw new Error('route-map detail: safe refused detail did not open');
  if (detail.textContent.includes('127.0.0.1') || detail.textContent.includes('publisher-launch') || detail.textContent.includes('consumer-launch')) throw new Error('route-map detail: secret route fields leaked');
  var disclosure = document.querySelector('.route-map-disclosure');
  if (!disclosure || !disclosure.textContent.includes('2/10 in use across 2 per-launch pools') || !disclosure.textContent.includes('8 available') || !disclosure.textContent.includes('Partial')) throw new Error('route-map detail: Darwin per-launch used/available Partial disclosure missing');
})();`
}
