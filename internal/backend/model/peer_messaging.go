package model

import "fmt"

// V1 keeps Claude peer messaging off unless the operator enables it. False remains
// portable to other providers; only a positive request requires Claude.
func ValidatePeerMessaging(enabled bool, harness string) error {
	if enabled && harness != "claude" {
		return fmt.Errorf("peer messaging is supported only by Claude")
	}
	return nil
}
