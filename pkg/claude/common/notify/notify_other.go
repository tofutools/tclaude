//go:build !linux && !darwin

package notify

import "fmt"

// platformSend returns an error on unsupported platforms, triggering the stderr fallback.
func platformSend(_, _, _ string) error {
	return fmt.Errorf("notifications not supported on this platform")
}
