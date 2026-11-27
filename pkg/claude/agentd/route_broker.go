package agentd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// GroupRouteBroker is the daemon-owned M2 data-plane engine. On Darwin, the
// bounded route adapter attaches authenticated channels here only when the
// exact launch slot pool is explicitly configured; other platforms retain
// the platform-neutral broker seam without endpoint activation.
var groupRouteBroker = newGroupRouteBroker()

func newGroupRouteBroker() *routebroker.Broker {
	broker, err := routebroker.New(routebroker.Config{Authorizer: databaseRouteAuthority{}, OnEvent: routeAdapterBrokerEvent})
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

// routeFlowWindow is the flow-control receive window agentd's route
// endpoints and helpers use, or 0 when the operator disabled flow control.
// It is read per channel attach, so a config change applies to channels
// attached after it.
func routeFlowWindow() int {
	cfg, err := config.Load()
	if err != nil {
		return (*config.Config)(nil).RouteFlowWindow()
	}
	return cfg.RouteFlowWindow()
}

// NewGroupRouteBrokerForTest returns an isolated broker wired to the same M1
// database authority as the daemon-owned instance. It keeps lifecycle tests
// from sharing global channel state with the process-wide broker.
func NewGroupRouteBrokerForTest() *routebroker.Broker {
	broker, err := routebroker.New(routebroker.Config{
		Authorizer: databaseRouteAuthority{},
		// Keep the test authority poll quick while leaving enough room for
		// SQLite-backed checks under the race detector before failing closed.
		AuthorityCheckInterval: 50 * time.Millisecond,
		OnEvent:                routeAdapterBrokerEvent,
	})
	if err != nil {
		panic(err)
	}
	return broker
}

// RouteAuthorityTicksForTest runs a test broker's authority checks when the
// test says, not on the wall clock (NewGroupRouteBrokerManualAuthorityForTest).
type RouteAuthorityTicksForTest struct {
	mu    sync.Mutex
	ticks []chan time.Time
}

// Channels is how many channels' watchers have started: each takes one
// ticker, so a Tick after it reaches the count reaches every one of them.
func (m *RouteAuthorityTicksForTest) Channels() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ticks)
}

// Tick asks every attached channel to check its authority once. A watcher
// that has ended, or still has a tick pending, drops it, as a ticker does.
func (m *RouteAuthorityTicksForTest) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.ticks {
		select {
		case c <- time.Now():
		default:
		}
	}
}

// NewGroupRouteBrokerManualAuthorityForTest is NewGroupRouteBrokerForTest
// with authority checked only on ticks.Tick. A polled check is bounded by its
// interval, so a healthy channel whose SQLite read overran 50ms on a loaded
// CI runner failed closed mid-test. Here a check runs only where the test
// asks, and its bound is a minute: only a hung authority fails it.
func NewGroupRouteBrokerManualAuthorityForTest() (*routebroker.Broker, *RouteAuthorityTicksForTest) {
	ticks := &RouteAuthorityTicksForTest{}
	broker, err := routebroker.New(routebroker.Config{
		Authorizer:             databaseRouteAuthority{},
		AuthorityCheckInterval: time.Minute,
		AuthorityTicker: func() (<-chan time.Time, func()) {
			c := make(chan time.Time, 1)
			ticks.mu.Lock()
			ticks.ticks = append(ticks.ticks, c)
			ticks.mu.Unlock()
			return c, func() {}
		},
		OnEvent: routeAdapterBrokerEvent,
	})
	if err != nil {
		panic(err)
	}
	return broker, ticks
}

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
	if route.HostOnly {
		if !hostPublishAllowed(route.PublisherConvID) {
			return errors.New("host publishing permission was revoked")
		}
		return contextError(ctx)
	}
	group, err := db.GetAgentGroupByID(route.GroupID)
	if err != nil || group == nil || group.RouteGeneration != route.GroupGeneration {
		return errors.New("route group generation is stale")
	}
	return contextError(ctx)
}

func (databaseRouteAuthority) AuthorizeConsumer(ctx context.Context, auth routebroker.ConsumerAuth) error {
	if strings.HasPrefix(auth.LeaseID, hostRouteLeasePrefix) {
		if err := contextError(ctx); err != nil {
			return err
		}
		return authorizeHostRouteConsumer(auth)
	}
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
	if route != nil && route.HostOnly {
		return errors.New("host-only routes cannot be consumed by agents")
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
	agent, err := db.GetAgent(auth.AgentID)
	if err != nil || agent == nil || !agent.Active() || agent.CurrentConvID != auth.ConvID {
		return errors.New("consumer is no longer live")
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
