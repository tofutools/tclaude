package hubupdate

import "github.com/tofutools/tclaude/pkg/selfupdate"

// DiscoverServingBinary identifies the executable that actually hosts the hub.
// Updating a `tclaude hub` service must replace tclaude, not turn its executable
// into a standalone tclaude-hub whose command-line shape would differ.
func DiscoverServingBinary(path string) (selfupdate.Binary, error) {
	binary, err := selfupdate.DiscoverBinary("tclaude-hub", path)
	if err == nil {
		return binary, nil
	}
	return selfupdate.DiscoverBinary("tclaude", path)
}
