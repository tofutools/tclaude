//go:build !linux && !darwin

package store

import "fmt"

func removeLegacyRunLocks(string) error {
	return fmt.Errorf("safe legacy process run-lock cleanup is unsupported on this platform")
}
