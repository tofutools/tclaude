package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The sandbox IMPLEMENTATION — who owns OS-level confinement — can be
// unusable for two structurally different reasons, and TCL-769 keeps them on
// two different mechanisms on purpose:
//
//   - HARNESS applicability depends on the harness a launch resolved to.
//     It belongs inside the resolveStringLaunchField validator, where the
//     existing per-field contract already does the right thing: an explicit
//     request or a profile claiming this harness fails loudly, while a lower
//     tier's profile pinned to some OTHER harness is ambient configuration
//     that is skipped, disclosed in the resolved-launch notes, and falls
//     through to the next tier.
//
//   - HOST availability depends on nothing but the host. It must NOT ride the
//     validator, because tier fallthrough would silently convert "this box has
//     no bwrap" into "resolved to harness-builtin" whenever the tier that
//     supplied the value happened to be pinned to another harness — the exact
//     silent downgrade the refuse-don't-degrade ruling forbids. It is instead a
//     separate gate applied to the ALREADY-RESOLVED value, and it never falls
//     through.
//
// The refusal payloads are correspondingly distinct, so an operator can tell
// "wrong harness" from "wrong machine" without reading the message body.

// sandboxImplementationField is the launch-field name used for provenance
// notes and invalid_<field> error kinds. It matches the JSON field on the spawn
// request and the profile wire shape, the way every other launch field's kind
// does (invalid_sandbox, invalid_auto_compact_window, …), so a client keying a
// form control off invalid_<field> resolves it. The CLI flag is spelled
// --sandbox-impl to match `session new`; the wire name is the longer one.
const sandboxImplementationField = "sandbox_implementation"

// sandboxImplementationUnavailableKind marks a refusal caused by the HOST
// lacking the capability, as opposed to invalid_sandbox_implementation, which
// marks a value that is malformed or inapplicable to the resolved harness.
const sandboxImplementationUnavailableKind = "sandbox_implementation_unavailable"

// The host-capability predicates are indirected so flow tests can drive both
// branches deterministically. Interactive harnesses require the terminal relay
// predicate; OpenCode confines its non-interactive server and therefore uses
// the server-specific predicate that omits relay-only pidfd support.
var tclaudeLayerHostAvailability = session.TclaudeLayerHostAvailability
var tclaudeLayerServerHostAvailability = session.TclaudeLayerServerHostAvailability

// validateSandboxImplementationForHarness normalizes a sandbox-implementation
// value and gates it on the harness through the capability path
// (session.ValidateTclaudeLayerHarness), never a harness-name switch here.
//
// Blank in, blank out. "" must stay unset rather than normalizing to
// harness-builtin: unset falls through to the next precedence tier, whereas an
// explicit harness-builtin PINS the legacy implementation so a lower tier
// cannot flip it. Collapsing the two would turn every profile that never
// mentioned the field into one that pins it.
//
// Host availability is deliberately NOT checked here. This function runs at
// profile-save time as well as at launch, and authoring a profile that pins
// tclaude-layer before bwrap is installed is legitimate; the host gate belongs
// on the launch.
func validateSandboxImplementationForHarness(h *harness.Harness, raw string) (string, error) {
	implementation, err := sandboxpolicy.NormalizeImplementation(raw)
	if err != nil {
		return "", err
	}
	if implementation == sandboxpolicy.ImplementationTclaudeLayer {
		if err := session.ValidateTclaudeLayerHarness(h.Name); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return string(implementation), nil
}

func resolveOpenCodeSandboxImplementationMode(
	h *harness.Harness,
	mode, rawImplementation string,
) (string, *spawnFailure) {
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_sandbox_implementation", err.Error()}
	}
	mode, err = harness.ResolveOpenCodeSandboxImplementationMode(h.Name, mode, implementation)
	if err != nil {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
	}
	return mode, nil
}

// sandboxImplementationHostFailure is the post-resolution host gate. It runs on
// the value the precedence chain settled on, whichever tier supplied it, and
// refuses the launch outright rather than degrading to harness-builtin.
//
// The probe is run LIVE, never from a cache: a cached negative would refuse a
// legitimate launch from an operator who has just installed bwrap. Caching this
// predicate is for disclosure surfaces only (see the dashboard capability
// metadata), where a stale answer costs nothing because the launch still
// refuses.
func sandboxImplementationHostFailure(harnessName, implementation string) *spawnFailure {
	if strings.TrimSpace(implementation) != string(sandboxpolicy.ImplementationTclaudeLayer) {
		return nil
	}
	availability := tclaudeLayerHostAvailability
	if session.TclaudeLayerUsesServerBoundary(harnessName) {
		availability = tclaudeLayerServerHostAvailability
	}
	if err := availability(); err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, sandboxImplementationUnavailableKind,
			fmt.Sprintf("sandbox implementation %s is not available on this host: %v; "+
				"refusing the launch rather than falling back to %s",
				sandboxpolicy.ImplementationTclaudeLayer, err,
				sandboxpolicy.ImplementationHarnessBuiltin)}
	}
	return nil
}
