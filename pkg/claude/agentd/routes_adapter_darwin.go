//go:build darwin

package agentd

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The adapter is deliberately opt-in. A daemon with no exact route-slot
// environment remains an M1 registry/M2 broker only; enabling the Darwin
// endpoint bridge requires the same bounded pool that the launch contract
// exposed to route-capable children.
var darwinRouteAdapterState struct {
	sync.Mutex
	adapter *routeadapter.Adapter
	slots   []int
}

func configuredDarwinRouteAdapter() (*routeadapter.Adapter, bool, error) {
	raw := os.Getenv(session.DarwinRouteSlotsEnv)
	if raw == "" {
		return nil, false, nil
	}
	slots, err := session.ParseDarwinRouteSlots(raw)
	if err != nil {
		return nil, true, err
	}
	darwinRouteAdapterState.Lock()
	defer darwinRouteAdapterState.Unlock()
	if darwinRouteAdapterState.adapter != nil {
		if !sameIntSlice(darwinRouteAdapterState.slots, slots) {
			return nil, true, errors.New("Darwin route slot pool changed while agentd is running")
		}
		return darwinRouteAdapterState.adapter, true, nil
	}
	adapter, err := routeadapter.New(GroupRouteBroker(), slots)
	if err != nil {
		return nil, true, err
	}
	darwinRouteAdapterState.adapter = adapter
	darwinRouteAdapterState.slots = append([]int(nil), slots...)
	return adapter, true, nil
}

func sameIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func routeAdapterPublish(ctx context.Context, route *db.AgentRoute) (bool, error) {
	adapter, enabled, err := configuredDarwinRouteAdapter()
	if err != nil || !enabled {
		return enabled, err
	}
	_, err = adapter.Publish(ctx, routeadapter.Publisher{
		RouteID: route.ID, AgentID: route.PublisherAgentID, ConvID: route.PublisherConvID,
		LaunchGeneration: route.PublisherLaunchGeneration, GroupGeneration: route.GroupGeneration,
		Target: route.Target,
	})
	return true, err
}

func routeAdapterOpen(ctx context.Context, route *db.AgentRoute, lease *db.AgentRouteLease) (string, bool, error) {
	adapter, enabled, err := configuredDarwinRouteAdapter()
	if err != nil || !enabled {
		return "", enabled, err
	}
	endpoint, err := adapter.Open(ctx, routeadapter.Consumer{
		LeaseID: lease.ID, RouteID: route.ID, AgentID: lease.ConsumerAgentID, ConvID: lease.ConsumerConvID,
		LaunchGeneration: lease.ConsumerLaunchGeneration, GroupGeneration: lease.GroupGeneration,
	})
	return endpoint, true, err
}

func routeAdapterCloseRoute(routeID string) {
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.Unlock()
	if adapter != nil {
		adapter.CloseRoute(routeID)
	}
}

func routeAdapterCloseLease(leaseID string) {
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.Unlock()
	if adapter != nil {
		adapter.CloseLease(leaseID)
	}
}

func routeAdapterCloseAll() {
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.adapter = nil
	darwinRouteAdapterState.slots = nil
	darwinRouteAdapterState.Unlock()
	if adapter != nil {
		adapter.Close()
	}
}

func routeAdapterBrokerEvent(event routebroker.Event) {
	if event.Kind != "authority-revoked" && event.Kind != "publisher-detached" {
		return
	}
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.Unlock()
	if adapter == nil {
		return
	}
	if event.Role == "publisher" {
		adapter.CloseRoute(event.RouteID)
	} else if event.Kind == "authority-revoked" && event.Role == "consumer" {
		adapter.CloseConsumer(event.RouteID, event.AgentID)
	}
}
