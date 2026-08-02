package agentd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// GroupRouteBroker is the daemon-owned M2 data-plane engine. Endpoint
// adapters attach their already-authenticated channels here in a later
// milestone; this ticket intentionally does not add platform listeners or
// activation paths.
var groupRouteBroker = newGroupRouteBroker()

func newGroupRouteBroker() *routebroker.Broker {
	broker, err := routebroker.New(routebroker.Config{Authorizer: databaseRouteAuthority{}})
	if err != nil {
		// databaseRouteAuthority is a concrete, non-nil implementation. Keep
		// construction centralized so a future invalid config cannot leave a
		// partially initialized daemon broker.
		panic(err)
	}
	return broker
}

// GroupRouteBroker returns the in-process broker supervised by agentd.
func GroupRouteBroker() *routebroker.Broker { return groupRouteBroker }

// databaseRouteAuthority binds the platform-neutral channel seam to M1's
// durable route and lease records. It never receives a payload, and every
// check is repeated while a channel remains attached.
type databaseRouteAuthority struct{}

func (databaseRouteAuthority) AuthorizePublisher(ctx context.Context, auth routebroker.PublisherAuth) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(auth.RouteID) == "" || strings.TrimSpace(auth.AgentID) == "" || strings.TrimSpace(auth.ConvID) == "" || strings.TrimSpace(auth.LaunchGeneration) == "" {
		return errors.New("publisher identity is incomplete")
	}
	route, err := db.GetAgentRoute(auth.RouteID)
	if err != nil {
		return fmt.Errorf("load publisher route: %w", err)
	}
	if route == nil || route.State != db.RouteStateReady {
		return errors.New("route is not ready")
	}
	if route.PublisherAgentID != auth.AgentID || route.PublisherConvID != auth.ConvID || route.PublisherLaunchGeneration != auth.LaunchGeneration || route.GroupGeneration != auth.GroupGeneration {
		return errors.New("publisher identity or generation is stale")
	}
	if !routePublisherLive(route) {
		return errors.New("publisher is no longer live")
	}
	group, err := db.GetAgentGroupByID(route.GroupID)
	if err != nil || group == nil || group.RouteGeneration != route.GroupGeneration {
		return errors.New("route group generation is stale")
	}
	return contextError(ctx)
}

func (databaseRouteAuthority) AuthorizeConsumer(ctx context.Context, auth routebroker.ConsumerAuth) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(auth.LeaseID) == "" || strings.TrimSpace(auth.RouteID) == "" || strings.TrimSpace(auth.AgentID) == "" || strings.TrimSpace(auth.ConvID) == "" || strings.TrimSpace(auth.LaunchGeneration) == "" {
		return errors.New("consumer identity is incomplete")
	}
	lease, err := db.GetAgentRouteLease(auth.LeaseID)
	if err != nil {
		return fmt.Errorf("load consumer lease: %w", err)
	}
	if lease == nil || lease.State != db.RouteLeaseOpen {
		return errors.New("consumer lease is not open")
	}
	if lease.RouteID != auth.RouteID || lease.ConsumerAgentID != auth.AgentID || lease.ConsumerConvID != auth.ConvID || lease.ConsumerLaunchGeneration != auth.LaunchGeneration || lease.GroupGeneration != auth.GroupGeneration {
		return errors.New("consumer lease identity or generation is stale")
	}
	route, err := db.GetAgentRoute(auth.RouteID)
	if err != nil {
		return fmt.Errorf("load consumer route: %w", err)
	}
	if route == nil || route.State != db.RouteStateReady || !routePublisherLive(route) {
		return errors.New("publisher authority is no longer ready")
	}
	group, err := db.GetAgentGroupByID(route.GroupID)
	if err != nil || group == nil || group.RouteGeneration != auth.GroupGeneration {
		return errors.New("consumer group generation is stale")
	}
	member, err := db.FindAgentMemberInGroup(group.ID, auth.AgentID)
	if err != nil || member == nil {
		return errors.New("consumer is no longer a group member")
	}
	if current, known := knownRouteLaunchGeneration(auth.ConvID); known && current != auth.LaunchGeneration {
		return errors.New("consumer launch generation is stale")
	}
	return contextError(ctx)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
