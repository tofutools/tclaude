//go:build linux

package agentd

import "sync"

var routeConsumerEndpoints sync.Map

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
	}
}

func clearRouteConsumerEndpoint(leaseID string) {
	if leaseID != "" {
		routeConsumerEndpoints.Delete(leaseID)
	}
}
