package db

import (
	"fmt"
	"strings"
)

// SandboxDefaultImplementationPrefKey stores the operator's install-wide
// sandbox-implementation default: the lowest tier of the agent-spawn
// resolution chain, consulted only when no explicit request and no spawn
// profile (named, group default, global default) sets one.
//
// It lives in dashboard_prefs, beside the global default spawn profile, so it
// sits under ~/.tclaude/data where agents cannot write it. config.json would
// have been the obvious home for a "default", but it is not a place a sandboxed
// agent should be able to weaken the next sibling's confinement from.
//
// Three states, deliberately distinct:
//
//   - absent: UNDECIDED. Behaves exactly like SandboxDefaultHarness so an
//     existing install is never silently re-sandboxed by an upgrade, and the
//     dashboard's Diagnostics › Doctor view asks the operator to choose.
//   - SandboxDefaultTclaudeLayer: tclaude's own sandbox. `tclaude setup` writes
//     this on a fresh install.
//   - SandboxDefaultHarness: an explicit "keep each harness's own sandbox" —
//     resolution leaves the implementation unset, as before this tier existed.
const SandboxDefaultImplementationPrefKey = "tclaude.sandbox.default_implementation"

const (
	SandboxDefaultTclaudeLayer = "tclaude-layer"
	SandboxDefaultHarness      = "harness"
)

// NormalizeSandboxDefaultImplementation validates an operator-supplied value.
// "harness-builtin" is accepted as a spelling of SandboxDefaultHarness because
// that is what the operator means by it; it is NOT stored as a pin, since an
// ambient pin of harness-builtin would be refused for harnesses that own no OS
// sandbox (OpenCode, Copilot, pi).
func NormalizeSandboxDefaultImplementation(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case SandboxDefaultTclaudeLayer:
		return SandboxDefaultTclaudeLayer, nil
	case SandboxDefaultHarness, "harness-builtin":
		return SandboxDefaultHarness, nil
	default:
		return "", fmt.Errorf("invalid default sandbox implementation %q (want %s or %s)",
			raw, SandboxDefaultTclaudeLayer, SandboxDefaultHarness)
	}
}

// GetSandboxDefaultImplementation returns the stored default and whether the
// operator (or setup) has decided at all. An unreadable or malformed value is
// reported as an error; callers on the launch path treat that as undecided
// rather than making every spawn fail on a bad preference row.
func GetSandboxDefaultImplementation() (value string, decided bool, err error) {
	raw, ok, err := GetDashboardPref(SandboxDefaultImplementationPrefKey)
	if err != nil || !ok {
		return "", false, err
	}
	value, err = NormalizeSandboxDefaultImplementation(raw)
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSandboxDefaultImplementation records the operator's decision.
func SetSandboxDefaultImplementation(raw string) (string, error) {
	value, err := NormalizeSandboxDefaultImplementation(raw)
	if err != nil {
		return "", err
	}
	return value, SetDashboardPref(SandboxDefaultImplementationPrefKey, value)
}

// ClearSandboxDefaultImplementation returns the install to UNDECIDED.
func ClearSandboxDefaultImplementation() error {
	return DeleteDashboardPref(SandboxDefaultImplementationPrefKey)
}

// FreshInstallPrefKey is written by migrate when it creates the database from
// nothing (no legacy JSON import). It is the "new install" signal; databases
// created before this key existed never carry it, so they count as existing.
const FreshInstallPrefKey = "tclaude.install.created_fresh"

// SeedFreshInstallSandboxDefault writes SandboxDefaultTclaudeLayer when the
// operator has not decided AND this database was created fresh by a binary
// that knows about the install default. An existing install stays undecided, so
// an upgrade never changes how its next agent is confined; the Diagnostics › Doctor view asks
// instead. Reports whether it wrote.
func SeedFreshInstallSandboxDefault() (bool, error) {
	if _, decided, err := GetSandboxDefaultImplementation(); err != nil || decided {
		return false, err
	}
	_, fresh, err := GetDashboardPref(FreshInstallPrefKey)
	if err != nil || !fresh {
		return false, err
	}
	if _, err := SetSandboxDefaultImplementation(SandboxDefaultTclaudeLayer); err != nil {
		return false, err
	}
	return true, nil
}
