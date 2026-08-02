//go:build !linux

package agentd

func routeConsumerEndpointForLease(string) string { return "" }
