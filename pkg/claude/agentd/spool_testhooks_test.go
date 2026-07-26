package agentd

import "time"

// StartSpoolConsumerForTest runs the production file-spool consumer against
// the production /v1 mux, with a test-scale rescan interval so flow tests
// don't ride on fsnotify timing. Returns the consumer's stop function.
// _test.go suffix keeps it out of production builds.
func StartSpoolConsumerForTest(root string, rescan time.Duration) func() {
	return startSpoolConsumer(buildMux(), root, rescan)
}
