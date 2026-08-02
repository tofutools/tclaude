package agentd

import (
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type dashboardRouteMemberIdentity struct {
	name   string
	conv   string
	online bool
	health string
}

var dashboardRouteMapPlatform = runtime.GOOS

// SetRouteMapPlatformForTest lets browser smoke tests exercise the disclosed
// Darwin capacity boundary while rendering on a Linux CI host.
func SetRouteMapPlatformForTest(platform string) func() {
	previous := dashboardRouteMapPlatform
	dashboardRouteMapPlatform = platform
	return func() { dashboardRouteMapPlatform = previous }
}

// buildDashboardRouteMap projects the route authority into a safe operator
// view. It resolves names from the already-built roster, compares the
// generation-bound route rows with current group/member state, and intentionally
// drops endpoints, targets, launch generations, and endpoint error details.
func buildDashboardRouteMap(
	groups []*db.AgentGroup,
	groupViews []dashboardGroup,
	routesByGroup map[int64][]*db.AgentRoute,
	leasesByGroup map[int64][]*db.AgentRouteLease,
	darwinLaunchesByGroup map[int64][]*db.DarwinRouteLaunch,
) dashboardRouteMap {
	result := dashboardRouteMap{Platform: dashboardRouteMapPlatform, Routes: []dashboardRoute{}}
	if dashboardRouteMapPlatform == "darwin" {
		result.DarwinCapacity = make(map[string]dashboardDarwinCapacity, len(groups))
		for i, group := range groups {
			if group == nil || i >= len(groupViews) || !routeMapGroupEnabled(groupViews[i]) {
				continue
			}
			launches := darwinLaunchesByGroup[group.ID]
			selectors := darwinActiveSelectors(routesByGroup[group.ID], leasesByGroup[group.ID])
			capacity := dashboardDarwinCapacity{Pools: len(launches)}
			for _, launch := range launches {
				if launch == nil {
					continue
				}
				capacity.Total += len(launch.Slots)
				capacity.Used += selectors[darwinLaunchKey(launch.AgentID, launch.ConvID, launch.LaunchGeneration)]
			}
			capacity.Available = capacity.Total - capacity.Used
			result.DarwinCapacity[group.Name] = capacity
			result.DarwinSlots = maxInt(result.DarwinSlots, capacity.Total)
		}
		result.DarwinBoundary = "Partial: localhost route selectors are host-wide on Darwin"
	}

	viewByID := make(map[int64]dashboardGroup, len(groups))
	for i, group := range groups {
		if group == nil {
			continue
		}
		if i < len(groupViews) {
			viewByID[group.ID] = groupViews[i]
		}
	}

	// Build a global identity index so an endpoint that no longer belongs to
	// this route's group is disclosed as wrong-group rather than rendered as a
	// connected node. Names are friendly display text only; the agent ID remains
	// the internal identity.
	identityByAgent := map[string]dashboardRouteMemberIdentity{}
	membersByGroup := map[int64]map[string]dashboardRouteMemberIdentity{}
	for groupID, view := range viewByID {
		members := make(map[string]dashboardRouteMemberIdentity)
		for _, member := range view.Members {
			agentID := strings.TrimSpace(member.AgentID)
			if agentID == "" {
				continue
			}
			identity := dashboardRouteMemberIdentity{name: safeRouteName(member.Title), conv: member.ConvID, online: member.Online, health: member.RouteHealth}
			members[agentID] = identity
			if _, exists := identityByAgent[agentID]; !exists {
				identityByAgent[agentID] = identity
			}
		}
		membersByGroup[groupID] = members
	}

	for _, group := range groups {
		if group == nil {
			continue
		}
		groupRoutes := routesByGroup[group.ID]
		groupLeases := leasesByGroup[group.ID]
		leasesByRoute := make(map[string][]*db.AgentRouteLease)
		for _, lease := range groupLeases {
			if lease != nil {
				leasesByRoute[lease.RouteID] = append(leasesByRoute[lease.RouteID], lease)
			}
		}
		members := membersByGroup[group.ID]
		for _, route := range groupRoutes {
			if route == nil {
				continue
			}
			publisherID := strings.TrimSpace(route.PublisherAgentID)
			publisher, publisherBoundary := routeMemberState(publisherID, route.PublisherConvID, members, identityByAgent)
			generationHealth := "current"
			if route.GroupGeneration != group.RouteGeneration {
				generationHealth = "stale"
			}
			view := dashboardRoute{
				ID:                route.ID,
				GroupID:           route.GroupID,
				Group:             route.GroupName,
				StableReference:   stableRouteReferenceFor(route),
				Name:              route.Name,
				Transport:         route.Transport,
				State:             route.State,
				CreatedAt:         route.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
				GroupGeneration:   route.GroupGeneration,
				GenerationHealth:  generationHealth,
				PublisherAgentID:  publisherID,
				PublisherName:     publisher.name,
				PublisherHealth:   publisher.health,
				PublisherBoundary: publisherBoundary,
				Consumers:         []dashboardRouteConsumer{},
			}
			for _, lease := range leasesByRoute[route.ID] {
				if lease == nil {
					continue
				}
				consumerID := strings.TrimSpace(lease.ConsumerAgentID)
				consumer, boundary := routeMemberState(consumerID, lease.ConsumerConvID, members, identityByAgent)
				endpointState := routeConsumerEndpointStatusForLease(lease.ID).state
				if lease.State != db.RouteLeaseOpen {
					if endpointState != "refused" {
						endpointState = "closed"
					}
				} else if endpointState == "" {
					endpointState = "pending"
				}
				leaseGeneration := "current"
				if lease.GroupGeneration != group.RouteGeneration {
					leaseGeneration = "stale"
				}
				view.Consumers = append(view.Consumers, dashboardRouteConsumer{
					ID:               lease.ID,
					ConsumerAgentID:  consumerID,
					ConsumerName:     consumer.name,
					State:            lease.State,
					EndpointState:    endpointState,
					GenerationHealth: leaseGeneration,
					ConsumerHealth:   consumer.health,
					Boundary:         boundary,
				})
			}
			result.Routes = append(result.Routes, view)
		}
	}
	return result
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func routeMapGroupEnabled(view dashboardGroup) bool {
	return routePermissionsEnabled(view.Permissions)
}

func routePermissionsEnabled(permissions []string) bool {
	hasPublish, hasConsume := false, false
	for _, permission := range permissions {
		hasPublish = hasPublish || permission == PermRoutesPublish
		hasConsume = hasConsume || permission == PermRoutesConsume
	}
	return hasPublish && hasConsume
}

// dashboardRouteHealth checks the launch capability itself. A route or lease
// row is not evidence that the current online generation can serve routes;
// this status is also emitted for members with no route rows yet.
func dashboardRouteHealth(group *db.AgentGroup, permissions []string, member *convRowBundle, darwinLaunches []*db.DarwinRouteLaunch) string {
	if group == nil || member == nil || !member.Online || !routePermissionsEnabled(permissions) {
		return ""
	}
	capable := false
	if dashboardRouteMapPlatform == "darwin" {
		for _, launch := range darwinLaunches {
			if launch != nil && launch.State == db.DarwinRouteLaunchActive &&
				launch.AgentID == member.AgentID && launch.ConvID == member.ConvID &&
				launch.LaunchGeneration == member.LaunchGeneration {
				capable = true
				break
			}
		}
	} else {
		capable = routeHelperCapabilityActive(member.AgentID, member.ConvID, member.LaunchGeneration)
	}
	if capable {
		return "current"
	}
	return "restart-needed"
}

func darwinLaunchKey(agentID, convID, generation string) string {
	return agentID + "\x00" + convID + "\x00" + generation
}

// darwinActiveSelectors is the read-only adapter seam for occupancy. A ready
// or draining route consumes one publisher selector; every open lease consumes
// one consumer selector. Matching is exact on agent, conversation, and launch
// generation, so stale launches cannot inflate a current pool.
func darwinActiveSelectors(routes []*db.AgentRoute, leases []*db.AgentRouteLease) map[string]int {
	selectors := make(map[string]int)
	for _, route := range routes {
		if route == nil || (route.State != db.RouteStateReady && route.State != db.RouteStateDraining) {
			continue
		}
		selectors[darwinLaunchKey(route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration)]++
	}
	for _, lease := range leases {
		if lease == nil || lease.State != db.RouteLeaseOpen {
			continue
		}
		selectors[darwinLaunchKey(lease.ConsumerAgentID, lease.ConsumerConvID, lease.ConsumerLaunchGeneration)]++
	}
	return selectors
}

type routeMemberView struct {
	name   string
	health string
}

func routeMemberState(agentID, convID string, members map[string]dashboardRouteMemberIdentity, all map[string]dashboardRouteMemberIdentity) (routeMemberView, string) {
	if member, ok := members[agentID]; ok {
		health := "current"
		if member.conv != convID {
			health = "restart-needed"
		} else if member.health != "" && member.health != "current" {
			health = member.health
		} else if !member.online {
			health = "offline"
		}
		return routeMemberView{name: member.name, health: health}, "in-group"
	}
	if _, known := all[agentID]; known {
		return routeMemberView{name: "", health: "wrong-group"}, "wrong-group"
	}
	return routeMemberView{name: "", health: "hidden"}, "hidden"
}

func safeRouteName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(unnamed agent)"
	}
	return value
}
