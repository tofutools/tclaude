//go:build linux

package agentd

import (
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

var routeConsumerEndpoints sync.Map
var routeConsumerEndpointStatuses sync.Map
var routeConsumerEndpointStatusMu sync.Mutex

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

func clearRouteConsumerEndpoint(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		routeConsumerEndpoints.Delete(leaseID)
		if value, ok := routeConsumerEndpointStatuses.Load(leaseID); ok {
			if status, ok := value.(routeConsumerEndpointStatus); ok && status.state == "ready" {
				routeConsumerEndpointStatuses.Delete(leaseID)
			}
		}
	}
}

func routeConsumerEndpointStatusForLease(leaseID string) routeConsumerEndpointStatus {
	routeConsumerEndpointStatusMu.Lock()
	defer routeConsumerEndpointStatusMu.Unlock()
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
	routeConsumerEndpoints.Store(leaseID, endpoint)
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
		routeConsumerEndpoints.Delete(leaseID)
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
		routeConsumerEndpoints.Delete(leaseID)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "pending"})
	}
}

func setRouteConsumerEndpointClosed(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpointStatusMu.Lock()
		defer routeConsumerEndpointStatusMu.Unlock()
		routeConsumerEndpoints.Delete(leaseID)
		routeConsumerEndpointStatuses.Store(leaseID, routeConsumerEndpointStatus{state: "closed"})
	}
}

func validateRouteConsumerEndpoint(endpoint string) (string, error) {
	return routeadapter.ValidateConsumerEndpoint(endpoint)
}
