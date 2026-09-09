package model

import "fmt"

// FastMode distinguishes an inherited native setting from an explicit standard tier.
type FastMode string

const (
	FastModeOn  FastMode = "on"
	FastModeOff FastMode = "off"
)

func (mode FastMode) Validate(harness string) error {
	if mode == "" {
		return nil
	}
	if mode != FastModeOn && mode != FastModeOff {
		return fmt.Errorf("unsupported fast mode %q", mode)
	}
	if harness != "codex" {
		return fmt.Errorf("fast mode is only supported by Codex")
	}
	return nil
}
