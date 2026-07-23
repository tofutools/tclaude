//go:build !linux && !darwin

package agentd

func openCodeProcessOwnsEndpoint(_ int, _ string) bool {
	return false
}
