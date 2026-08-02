//go:build !linux

package agentd

import (
	"fmt"
	"sync"
)

var routeConsumerEndpointStatuses sync.Map
var routeConsumerEndpointStatusMu sync.Mutex

type routeConsumerEndpointStatus struct {
	state    string
	endpoint string
	err      string
}

func routeConsumerEndpointForLease(string) string { return "" }

func routeConsumerEndpointStatusForLease(leaseID string) routeConsumerEndpointStatus {
	routeConsumerEndpointStatusMu.Lock()
	defer routeConsumerEndpointStatusMu.Unlock()
	if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
		if status, ok := value.(routeConsumerEndpointStatus); ok {
			return status
		}
	}
	return routeConsumerEndpointStatus{}
}

func clearRouteConsumerEndpoint(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		routeConsumerEndpointStatuses.Delete(leaseID)
	}
}

func setRouteConsumerEndpointReady(leaseID, endpoint string) bool {
	if leaseID == "" || endpoint == "" {
		return false
	}
	routeConsumerEndpointStatusMu.Lock()
	defer routeConsumerEndpointStatusMu.Unlock()
	if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
		if status, ok := value.(routeConsumerEndpointStatus); ok && (status.state == "refused" || status.state == "closed") {
			return false
		}
	}
	routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "ready", endpoint: endpoint})
	return true
}

func setRouteConsumerEndpointRefused(leaseID, detail string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
			if status, ok := value.(routeConsumerEndpointStatus); ok && (status.state == "refused" || status.state == "closed") {
				return
			}
		}
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "refused", err: detail})
	}
}

func setRouteConsumerEndpointPending(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
			if status, ok := value.(routeConsumerEndpointStatus); ok && (status.state == "refused" || status.state == "closed") {
				return
			}
		}
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "pending"})
	}
}

func setRouteConsumerEndpointClosed(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "closed"})
	}
}

func validateRouteConsumerEndpoint(string) (string, error) {
	return "", fmt.Errorf("route consumer endpoint adapter is unsupported on this platform")
}
