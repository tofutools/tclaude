//go:build linux

package agentd

import "github.com/tofutools/tclaude/pkg/claude/routeadapter"

func validateLinuxRoutePublishTarget(target string) error {
	_, err := routeadapter.ValidatePublisherTarget(target)
	return err
}
