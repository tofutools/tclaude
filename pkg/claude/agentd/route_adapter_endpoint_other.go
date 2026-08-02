//go:build !linux

package agentd

import (
	"fmt"
	"sync"
)

var routeConsumerEndpointStatuses sync.Map

type routeConsumerEndpointStatus struct {
	state    string
	endpoint string
	err      string
}

func routeConsumerEndpointForLease(string) string { return "" }

func routeConsumerEndpointStatusForLease(leaseID string) routeConsumerEndpointStatus {
	if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
		if status, ok := value.(routeConsumerEndpointStatus); ok {
			return status
		}
	}
	return routeConsumerEndpointStatus{}
}

func setRouteConsumerEndpoint(leaseID, endpoint string) {
	setRouteConsumerEndpointReady(leaseID, endpoint)
}

func clearRouteConsumerEndpoint(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatuses.Delete(leaseID)
	}
}

func setRouteConsumerEndpointReady(leaseID, endpoint string) {
	if leaseID != "" && endpoint != "" {
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "ready", endpoint: endpoint})
	}
}

func setRouteConsumerEndpointRefused(leaseID, detail string) {
	if leaseID != "" {
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "refused", err: detail})
	}
}

func setRouteConsumerEndpointPending(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "pending"})
	}
}

func validateRouteConsumerEndpoint(string) (string, error) {
	return "", fmt.Errorf("route consumer endpoint adapter is unsupported on this platform")
}
