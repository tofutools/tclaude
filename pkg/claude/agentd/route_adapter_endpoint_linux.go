//go:build linux

package agentd

import (
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

var routeConsumerEndpoints sync.Map
var routeConsumerEndpointStatuses sync.Map

type routeConsumerEndpointStatus struct {
	state    string
	endpoint string
	err      string
}

func routeConsumerEndpointForLease(leaseID string) string {
	value, ok := routeConsumerEndpoints.Load(leaseID)
	if !ok {
		return ""
	}
	endpoint, _ := value.(string)
	return endpoint
}

func setRouteConsumerEndpoint(leaseID, endpoint string) {
	if leaseID != "" && endpoint != "" {
		routeConsumerEndpoints.Store(leaseID, endpoint)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "ready", endpoint: endpoint})
	}
}

func clearRouteConsumerEndpoint(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpoints.Delete(leaseID)
		if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
			if status, ok := value.(routeConsumerEndpointStatus); ok && status.state == "ready" {
				routeConsumerEndpointStatuses.Delete(leaseID)
			}
		}
	}
}

func routeConsumerEndpointStatusForLease(leaseID string) routeConsumerEndpointStatus {
	if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
		if status, ok := value.(routeConsumerEndpointStatus); ok {
			return status
		}
	}
	if endpoint := routeConsumerEndpointForLease(leaseID); endpoint != "" {
		return routeConsumerEndpointStatus{state: "ready", endpoint: endpoint}
	}
	return routeConsumerEndpointStatus{}
}

func setRouteConsumerEndpointReady(leaseID, endpoint string) {
	if leaseID != "" && endpoint != "" {
		routeConsumerEndpoints.Store(leaseID, endpoint)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "ready", endpoint: endpoint})
	}
}

func setRouteConsumerEndpointRefused(leaseID, detail string) {
	if leaseID != "" {
		routeConsumerEndpoints.Delete(leaseID)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "refused", err: detail})
	}
}

func setRouteConsumerEndpointPending(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpoints.Delete(leaseID)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "pending"})
	}
}

func validateRouteConsumerEndpoint(endpoint string) (string, error) {
	return routeadapter.ValidateConsumerEndpoint(endpoint)
}
