package agentd

// SetPeerViewObserverForTest observes real receiver work without replacing it.
// Install before starting fixture traffic and restore after the traffic stops.
func SetPeerViewObserverForTest(fn func() func()) func() {
	previous := peerViewObserver.Swap(&fn)
	return func() { peerViewObserver.Store(previous) }
}
