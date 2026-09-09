package model

import "fmt"

// V1 keeps Claude auto-memory off unless the operator enables it. False remains
// portable to other providers; only a positive request requires Claude.
func ValidateAutoMemory(enabled bool, harness string) error {
	if enabled && harness != "claude" {
		return fmt.Errorf("auto-memory is supported only by Claude")
	}
	return nil
}
