package sandboxpolicy

import (
	"fmt"
	"slices"
	"strings"
)

// Implementation selects which layer owns OS-level sandbox enforcement for a
// harness launch.
type Implementation string

const (
	ImplementationHarnessBuiltin Implementation = "harness-builtin"
	ImplementationTclaudeLayer   Implementation = "tclaude-layer"
	ImplementationResourceOnly   Implementation = "resource-only"
	ImplementationOff            Implementation = "off"
)

// legacyImplementationStacked is the retired "stacked" implementation: tclaude's
// outer layer with the harness's own sandbox kept active inside it. Nested
// sandboxing is no longer supported, but agents, profiles, templates and
// session rows recorded before its removal may still carry the value, so
// NormalizeImplementation reads it as tclaude-layer: those launches keep
// tclaude's own wall rather than becoming un-resumable or losing confinement.
const legacyImplementationStacked = "stacked"

// UsesTclaudeLayer reports whether tclaude owns an outer OS boundary.
func (implementation Implementation) UsesTclaudeLayer() bool {
	return implementation == ImplementationTclaudeLayer
}

// OmitsOSConfinement reports whether an implementation deliberately stands down
// every OS-level ACCESS boundary — both tclaude's own layer and the harness's
// native sandbox.
//
// `resource-only` belongs here with `off` because a cgroup bounds how much CPU
// and memory a workload may consume, never what it may read, write, or reach.
// Every gate asking "is some access boundary enforcing" must answer the same
// for both, or a limits-only launch would be credited with confinement it does
// not have. The single axis on which they differ is resource limits, which
// `off` refuses and `resource-only` exists to apply; ValidateResourceLimitTarget
// owns that distinction and is deliberately the only place that draws it.
func (implementation Implementation) OmitsOSConfinement() bool {
	return implementation == ImplementationOff ||
		implementation == ImplementationResourceOnly
}

// SupportsMountPaths reports whether an implementation on a platform can
// enforce a remapped filesystem grant (TCL-866).
//
// Projecting a host directory onto a different sandbox path requires a real
// mount namespace. Only tclaude's own outer layer on Linux has one:
//
//   - macOS Seatbelt is a path filter over the host namespace. It can allow or
//     deny a path but cannot make a directory appear anywhere else, which is
//     why the daemon-final bind path already refuses source≠target there.
//   - harness-builtin sandboxes receive path lists and confine the harness in
//     the host namespace; they have no place to express a projection at all.
//
// Callers must refuse rather than degrade. Mounting at the host path instead
// would break the authored contract in both directions: it exposes a path the
// operator did not authorize, and leaves the one they did authorize empty.
func SupportsMountPaths(implementation Implementation, goos string) bool {
	return implementation.UsesTclaudeLayer() && goos == "linux"
}

// ValidateMountPathSupport refuses a launch whose rules need a capability the
// resolved implementation does not have. The message names the missing
// capability and the exact rule, following the wording pattern the other
// unsupported_sandbox_profile_* refusals use.
func ValidateMountPathSupport(
	grants []FilesystemGrant,
	implementation Implementation,
	goos string,
) error {
	if SupportsMountPaths(implementation, goos) {
		return nil
	}
	for _, grant := range grants {
		if !grant.IsRemapped() {
			continue
		}
		return fmt.Errorf(
			"unsupported_sandbox_profile_mount_path: sandbox implementation %q on %s cannot mount %q at sandbox path %q; mounting a host directory at a different sandbox path requires a mount namespace, which only tclaude’s sandbox on Linux provides",
			implementation, goos, grant.Path, grant.GuestPath())
	}
	return nil
}

// NormalizeImplementation validates a launch's sandbox implementation.
// Empty retains the legacy harness-owned behavior. The retired "stacked" value
// maps to tclaude-layer (see legacyImplementationStacked).
func NormalizeImplementation(value string) (Implementation, error) {
	implementation := Implementation(strings.TrimSpace(value))
	if implementation == "" {
		return ImplementationHarnessBuiltin, nil
	}
	switch implementation {
	case ImplementationHarnessBuiltin, ImplementationTclaudeLayer,
		ImplementationResourceOnly, ImplementationOff:
		return implementation, nil
	case legacyImplementationStacked:
		return ImplementationTclaudeLayer, nil
	default:
		return "", fmt.Errorf("invalid sandbox implementation %q (want %s, %s, %s, or %s)",
			value, ImplementationHarnessBuiltin, ImplementationTclaudeLayer,
			ImplementationResourceOnly, ImplementationOff)
	}
}

// ValidateProtectedAncestorWriteSupport refuses a harness-native launch with a
// write rule on an ancestor of a protected root, such as a writable `~`.
//
// Hiding the protected root itself is not enough under a writable ancestor:
// the agent could rename the directory between them (`mv ~/.tclaude ~/x`) and
// reach the contents at a path no rule names. tclaude's own sandbox pins that
// chain (a mount point on Linux, an unlink deny on macOS); a harness-native
// sandbox only receives path lists and has no way to express the pin. The
// harness-native implementations refuse instead of approximating it. `off` and
// `resource-only` confine nothing, so a rule there changes nothing either way.
func ValidateProtectedAncestorWriteSupport(
	grants []FilesystemGrant,
	implementation Implementation,
	goos string,
) error {
	if implementation.UsesTclaudeLayer() || implementation.OmitsOSConfinement() ||
		!slices.ContainsFunc(grants, func(grant FilesystemGrant) bool { return grant.Access == AccessWrite }) {
		return nil
	}
	protected, err := ProtectedPaths()
	if err != nil {
		return err
	}
	for _, grant := range grants {
		if grant.Access != AccessWrite {
			continue
		}
		for _, root := range protected {
			if GuardContainsOrEqual(grant.Path, root) {
				return fmt.Errorf(
					"unsupported_sandbox_profile_protected_ancestor_write: sandbox implementation %q on %s cannot apply the write rule for %q; it contains tclaude's protected directory %q, and keeping that directory sealed under a writable parent needs tclaude’s sandbox (tclaude-layer)",
					implementation, goos, grant.Path, root)
			}
		}
	}
	return nil
}
