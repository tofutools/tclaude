package agentd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestBuildDashboardRouteMapSafeProjection(t *testing.T) {
	created := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	groups := []*db.AgentGroup{
		{ID: 1, Name: "alpha", RouteGeneration: 2},
		{ID: 2, Name: "beta", RouteGeneration: 1},
	}
	views := []dashboardGroup{
		{Name: "alpha", RouteGeneration: 2, Members: []dashboardMember{
			{AgentID: "agt_pub", ConvID: "conv_pub", Title: "Publisher", Online: true},
			{AgentID: "agt_cons", ConvID: "conv_cons", Title: "Consumer", Online: false},
		}},
		{Name: "beta", RouteGeneration: 1, Members: []dashboardMember{
			{AgentID: "agt_other", ConvID: "conv_other", Title: "Other", Online: true},
		}},
	}
	route := &db.AgentRoute{
		ID: "rte_alpha", GroupID: 1, GroupName: "alpha", PublisherAgentID: "agt_pub",
		PublisherConvID: "conv_pub", PublisherLaunchGeneration: "launch-secret",
		GroupGeneration: 2, Name: "metrics", Transport: "tcp", Target: "tcp://127.0.0.1:9000",
		State: db.RouteStateWithdrawn, CreatedAt: created,
	}
	lease := &db.AgentRouteLease{
		ID: "rlease_alpha", RouteID: route.ID, ConsumerAgentID: "agt_cons",
		ConsumerConvID: "conv_cons", ConsumerLaunchGeneration: "consumer-secret",
		GroupGeneration: 2, State: db.RouteLeaseOpen, OpenedAt: created,
	}
	setRouteConsumerEndpointRefused(lease.ID, "secret endpoint detail")
	t.Cleanup(func() { clearRouteConsumerEndpoint(lease.ID) })

	got := buildDashboardRouteMap(groups, views,
		map[int64][]*db.AgentRoute{1: {route}},
		map[int64][]*db.AgentRouteLease{1: {lease}},
	)
	if len(got.Routes) != 1 {
		t.Fatalf("route count = %d, want 1", len(got.Routes))
	}
	view := got.Routes[0]
	if view.State != db.RouteStateWithdrawn || view.GenerationHealth != "current" {
		t.Fatalf("route lifecycle projection = %q/%q", view.State, view.GenerationHealth)
	}
	if view.PublisherName != "Publisher" || view.PublisherHealth != "current" {
		t.Fatalf("publisher projection = %#v", view)
	}
	if len(view.Consumers) != 1 || view.Consumers[0].EndpointState != "refused" {
		t.Fatalf("consumer endpoint projection = %#v", view.Consumers)
	}
	if view.Consumers[0].ConsumerHealth != "offline" {
		t.Fatalf("consumer health = %q, want offline", view.Consumers[0].ConsumerHealth)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, forbidden := range []string{"tcp://127.0.0.1:9000", "launch-secret", "consumer-secret", "secret endpoint detail", "target"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("route map exposed forbidden %q: %s", forbidden, text)
		}
	}
}

func TestBuildDashboardRouteMapDisclosesBoundariesAndRestartHealth(t *testing.T) {
	groups := []*db.AgentGroup{{ID: 1, Name: "alpha", RouteGeneration: 3}, {ID: 2, Name: "beta", RouteGeneration: 1}}
	views := []dashboardGroup{
		{Name: "alpha", Members: []dashboardMember{{AgentID: "agt_current", ConvID: "new-generation", Title: "Current", Online: true}}},
		{Name: "beta", Members: []dashboardMember{{AgentID: "agt_wrong", ConvID: "other", Title: "Wrong Group", Online: true}}},
	}
	route := &db.AgentRoute{ID: "rte_stale", GroupID: 1, GroupName: "alpha", PublisherAgentID: "agt_wrong", PublisherConvID: "old", PublisherLaunchGeneration: "old", GroupGeneration: 2, Name: "stale", Transport: "tcp", State: db.RouteStatePublisherLost}
	lease := &db.AgentRouteLease{ID: "rlease_hidden", RouteID: route.ID, ConsumerAgentID: "agt_missing", ConsumerConvID: "missing", GroupGeneration: 2, State: db.RouteLeaseClosed}
	got := buildDashboardRouteMap(groups, views, map[int64][]*db.AgentRoute{1: {route}}, map[int64][]*db.AgentRouteLease{1: {lease}})
	view := got.Routes[0]
	if view.GenerationHealth != "stale" || view.PublisherBoundary != "wrong-group" || view.PublisherHealth != "wrong-group" {
		t.Fatalf("wrong-group publisher projection = %#v", view)
	}
	if view.Consumers[0].Boundary != "hidden" || view.Consumers[0].ConsumerHealth != "hidden" || view.Consumers[0].EndpointState != "closed" {
		t.Fatalf("hidden consumer projection = %#v", view.Consumers[0])
	}
}
