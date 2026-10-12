package harness

import (
	"fmt"
	"strings"
)

// pi has no sandbox. Its documentation (docs/security.md at PiMinimumVersion)
// is explicit: tools run with the operating-system permissions of the pi
// process, there is no approval prompt, and isolation is delegated to a
// container, VM or sandbox around the whole process. The catalog therefore has
// a single mode that asserts nothing is confined by pi itself; tclaude's own
// layer (--sandbox-impl tclaude-layer) is the boundary for a pi agent.
const PiSandboxOff = "off"

// PiBuiltinOSSandboxAbsenceReason is the harness-builtin refusal for pi.
const PiBuiltinOSSandboxAbsenceReason = "pi has no sandbox of its own; its documentation " +
	"delegates isolation to a container around the whole process. Use --sandbox-impl " +
	"tclaude-layer for an OS sandbox"

// piSandbox is pi's SandboxCatalog.
type piSandbox struct{}

func (piSandbox) DefaultMode() string { return PiSandboxOff }
func (piSandbox) Modes() []string     { return []string{PiSandboxOff} }

func (piSandbox) ValidateMode(mode string) (string, error) {
	switch mode = strings.TrimSpace(mode); mode {
	case "", PiSandboxOff:
		return mode, nil
	}
	return "", fmt.Errorf("invalid pi sandbox mode %q (want %s: pi has no sandbox of its own; "+
		"use --sandbox-impl tclaude-layer)", mode, PiSandboxOff)
}

func (piSandbox) ModeHelp(mode string) string {
	if strings.TrimSpace(mode) == PiSandboxOff {
		return "pi has no sandbox of its own; tclaude's sandbox (--sandbox-impl tclaude-layer) is the only boundary"
	}
	return ""
}
