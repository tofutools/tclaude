//go:build !linux

package agentd

func validateLinuxRoutePublishTarget(string) error { return nil }
