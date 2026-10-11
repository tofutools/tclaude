package agentd

import "github.com/tofutools/tclaude/pkg/selfupdate"

// SetNodeUpdateServiceForTest replaces binary discovery, leaving update jobs,
// authorization and release reads on their production paths.
func SetNodeUpdateServiceForTest(service *selfupdate.Service) func() {
	nodeUpdates.Lock()
	previous := nodeUpdates.service
	nodeUpdates.service = service
	nodeUpdates.Unlock()
	return func() {
		nodeUpdates.Lock()
		nodeUpdates.service = previous
		nodeUpdates.Unlock()
	}
}
