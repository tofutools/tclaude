//go:build darwin

package agentd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// The adapter is deliberately not configured from an environment variable.
// A route endpoint is admitted only after the caller's exact launch
// generation has a durable slot contract in SQLite.
var darwinRouteAdapterState struct {
	sync.Mutex
	adapter *routeadapter.Adapter
	cancel  context.CancelFunc
}

func configuredDarwinRouteAdapter() (*routeadapter.Adapter, bool, error) {
	darwinRouteAdapterState.Lock()
	defer darwinRouteAdapterState.Unlock()
	if darwinRouteAdapterState.adapter != nil {
		return darwinRouteAdapterState.adapter, true, nil
	}
	adapter, err := routeadapter.New(GroupRouteBroker(), nil)
	if err != nil {
		return nil, true, err
	}
	adapter.SetConsumerRefusalObserver(func(consumer routeadapter.Consumer, err error) {
		recordDarwinConsumerRefusal(adapter, consumer, err)
	})
	ctx, cancel := context.WithCancel(context.Background())
	darwinRouteAdapterState.adapter = adapter
	darwinRouteAdapterState.cancel = cancel
	go reconcileDarwinRouteAdapter(ctx, adapter)
	return adapter, true, nil
}

// recordDarwinConsumerRefusal moves a broker refusal of one consumer stream
// onto the durable lease state the consuming agent can read. The adapter has
// no caller to return the error to: each refused stream is an accepted local
// TCP connection, so the agent would otherwise see only a connection that
// closed, and could not tell a revoked lease from its peer hanging up.
func recordDarwinConsumerRefusal(adapter *routeadapter.Adapter, consumer routeadapter.Consumer, err error) {
	// Capacity is per-connection back-pressure, not a verdict on the lease:
	// the listener stays usable as soon as a slot frees, so this must not move
	// the endpoint to its terminal refused state or close the lease.
	if errors.Is(err, routebroker.ErrConsumerLimit) {
		slog.Warn("route adapter: consumer stream refused for capacity",
			"lease_id", consumer.LeaseID, "route_id", consumer.RouteID, "agent_id", consumer.AgentID)
		return
	}
	slog.Warn("route adapter: consumer attach refused",
		"lease_id", consumer.LeaseID, "route_id", consumer.RouteID, "agent_id", consumer.AgentID, "err", err)
	// Same treatment the Linux channel handler gives its own refusals: close
	// the durable lease, then mark the endpoint refused with the shared detail.
	_ = db.CloseAgentRouteLease(consumer.LeaseID, consumer.AgentID, consumer.ConvID)
	setRouteConsumerEndpointRefused(consumer.LeaseID, routeEndpointRefusalDetail(err))
	adapter.CloseLease(consumer.LeaseID)
}

func darwinRouteLaunch(agentID, convID, generation string) (*db.DarwinRouteLaunch, bool, error) {
	launch, err := db.GetDarwinRouteLaunch(agentID, convID, generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, errors.New("Darwin route launch contract is missing or not route-capable")
	}
	if err != nil {
		return nil, true, err
	}
	if launch.State != db.DarwinRouteLaunchActive {
		return nil, true, errors.New("Darwin route launch is stale or not active")
	}
	return launch, true, nil
}

func routeAdapterPublish(ctx context.Context, route *db.AgentRoute) (bool, error) {
	launch, enabled, err := darwinRouteLaunch(route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration)
	if err != nil || !enabled {
		return enabled, err
	}
	adapter, _, err := configuredDarwinRouteAdapter()
	if err != nil {
		return true, err
	}
	_, err = adapter.PublishWithSlots(ctx, routeadapter.Publisher{
		RouteID: route.ID, AgentID: route.PublisherAgentID, ConvID: route.PublisherConvID,
		LaunchGeneration: route.PublisherLaunchGeneration, GroupGeneration: route.GroupGeneration,
		Target: route.Target,
	}, launch.Slots)
	return true, err
}

func routeAdapterOpen(ctx context.Context, route *db.AgentRoute, lease *db.AgentRouteLease) (string, bool, error) {
	consumer, enabled, err := darwinRouteLaunch(lease.ConsumerAgentID, lease.ConsumerConvID, lease.ConsumerLaunchGeneration)
	if err != nil || !enabled {
		return "", enabled, err
	}
	// A consumer contract alone cannot create a superficially successful
	// endpoint: the publisher must have its own active route-capable contract.
	if _, publisherEnabled, publisherErr := darwinRouteLaunch(
		route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration); publisherErr != nil {
		return "", true, fmt.Errorf("publisher launch contract: %w", publisherErr)
	} else if !publisherEnabled {
		return "", true, errors.New("route publisher launch is not route-capable")
	}
	adapter, _, err := configuredDarwinRouteAdapter()
	if err != nil {
		return "", true, err
	}
	endpoint, err := adapter.OpenWithSlots(ctx, routeadapter.Consumer{
		LeaseID: lease.ID, RouteID: route.ID, AgentID: lease.ConsumerAgentID, ConvID: lease.ConsumerConvID,
		LaunchGeneration: lease.ConsumerLaunchGeneration, GroupGeneration: lease.GroupGeneration,
	}, consumer.Slots)
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
	cancel := darwinRouteAdapterState.cancel
	darwinRouteAdapterState.adapter = nil
	darwinRouteAdapterState.cancel = nil
	darwinRouteAdapterState.Unlock()
	if cancel != nil {
		cancel()
	}
	if adapter != nil {
		adapter.Close()
	}
}

func routeAdapterBrokerEvent(event routebroker.Event) {
	if event.Kind != "authority-revoked" && event.Kind != "publisher-detached" &&
		event.Kind != "consumer-rejected" {
		return
	}
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.Unlock()
	if adapter == nil {
		return
	}
	if event.Role == "publisher" &&
		(event.Kind == "authority-revoked" || event.Kind == "publisher-detached") {
		adapter.CloseRoute(event.RouteID)
	} else if event.Role == "consumer" &&
		(event.Kind == "authority-revoked" || event.Kind == "consumer-rejected") {
		if event.LeaseID != "" {
			adapter.CloseLease(event.LeaseID)
		} else {
			// Older/third-party event producers may not carry a lease. The
			// durable authority is the fallback; it reconciles each listener
			// by its exact lease instead of broad route+agent teardown.
			reconcileDarwinRouteAdapterOnce(adapter)
		}
	}
}

func reconcileDarwinRouteAdapter(ctx context.Context, adapter *routeadapter.Adapter) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileDarwinRouteAdapterOnce(adapter)
		}
	}
}

func reconcileDarwinRouteAdapterOnce(adapter *routeadapter.Adapter) {
	for _, leaseID := range adapter.LeaseIDs() {
		lease, err := db.GetAgentRouteLease(leaseID)
		if err != nil || lease == nil || lease.State != db.RouteLeaseOpen {
			adapter.CloseLease(leaseID)
		}
	}
	for _, routeID := range adapter.RouteIDs() {
		route, err := db.GetAgentRoute(routeID)
		if err != nil || route == nil || route.State != db.RouteStateReady || !routePublisherLive(route) {
			adapter.CloseRoute(routeID)
			continue
		}
		launch, enabled, launchErr := darwinRouteLaunch(route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration)
		if launchErr != nil || !enabled || launch == nil {
			adapter.CloseRoute(routeID)
		}
	}
}
