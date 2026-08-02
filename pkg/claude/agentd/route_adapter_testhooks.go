package agentd

// SetRouteConsumerEndpointStatusForTest seeds the adapter status projection
// used by read-only dashboard browser fixtures. It returns a cleanup function
// so test-only refused/pending states cannot leak into another test process.
func SetRouteConsumerEndpointStatusForTest(leaseID, state string) func() {
	switch state {
	case "refused":
		setRouteConsumerEndpointRefused(leaseID, "test-only refusal detail")
	case "pending":
		setRouteConsumerEndpointPending(leaseID)
	case "closed":
		setRouteConsumerEndpointClosed(leaseID)
	default:
		panic("unsupported route consumer endpoint test state: " + state)
	}
	return func() { clearRouteConsumerEndpoint(leaseID) }
}
