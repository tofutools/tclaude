//go:build !linux && !darwin

package opencodeapi

// ProcessOwnsEndpoint fails closed on unsupported platforms.
func ProcessOwnsEndpoint(_ int, _ string) bool {
	return false
}
