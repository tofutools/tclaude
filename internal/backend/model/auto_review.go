package model

import "fmt"

func ValidateAutoReview(enabled bool, harness string) error {
	if enabled && harness != "codex" {
		return fmt.Errorf("automatic approval review is only supported by Codex")
	}
	return nil
}

// Never produces no approval request for a classifier to decide. Preserve that
// v1 distinction even when the saved native reviewer flag remains enabled.
func (d DesiredConfiguration) UsesAutoReview() bool {
	return d.Harness == "codex" && d.AutoReview && d.Approval != ApprovalNever && d.Approval != ApprovalAutomatic
}
