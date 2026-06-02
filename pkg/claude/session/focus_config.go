package session

import "github.com/tofutools/tclaude/pkg/claude/common/config"

// focusRaiseOnly reports whether window focus is configured raise-only —
// raise an existing terminal window but never open a fresh one as a side
// effect. Default false (open-on-focus, the historical behavior on Linux
// and macOS alike).
//
// Read live per call: focus dispatch is rare, config.Load is cheap, and
// reading live means a config edit takes effect without restarting the
// daemon (same no-cache philosophy as resolveLinuxFocusTool). config.Load
// returns a non-nil DefaultConfig even on error and RaiseOnlyFocus is
// nil-safe, so this never panics and degrades to false.
func focusRaiseOnly() bool {
	cfg, _ := config.Load()
	return cfg.RaiseOnlyFocus()
}

// focusRaiseOnlyFn is the seam tests use to pin the raise-only decision
// without writing a real config file.
var focusRaiseOnlyFn = focusRaiseOnly
