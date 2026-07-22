package harness

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// SandboxCapabilityError is the typed, actionable refusal an adapter returns
// when a harness cannot faithfully enforce a requested sandbox policy.
//
// A harness that cannot represent the requested posture must fail loudly rather
// than approximate it: an operator who denied their home directory and reopened
// only a workspace, and then silently received today's broad read access, would
// believe in isolation that does not exist. Kind is stable wire vocabulary for
// the daemon's HTTP error code.
type SandboxCapabilityError struct {
	Harness string
	Kind    string
	Message string
}

func (e *SandboxCapabilityError) Error() string { return e.Message }

// Stable error kinds. These reach the CLI/dashboard as HTTP error codes.
const (
	SandboxCapabilityBreakGlass = "unsupported_sandbox_profile_break_glass"
)

// ValidateSandboxBreakGlass reports whether a harness/mode combination can
// represent the acknowledged protected-path rules.
//
// Both supported harnesses can, but only in their policy-rendering modes, and
// each needs its own deny suppressed (see the adapters). A harness that cannot
// must refuse: launching without the access an operator explicitly
// acknowledged would leave them debugging a sandbox that silently ignored
// their decision.
func ValidateSandboxBreakGlass(harnessName, sandboxMode string, grants []sandboxpolicy.BreakGlassGrant) error {
	if len(grants) == 0 {
		return nil
	}
	switch strings.TrimSpace(harnessName) {
	case CodexName:
		if strings.TrimSpace(sandboxMode) != SandboxManagedProfile {
			return &SandboxCapabilityError{
				Harness: CodexName,
				Kind:    SandboxCapabilityBreakGlass,
				Message: fmt.Sprintf("break-glass protected access requires Codex sandbox %q; sandbox %q cannot represent it", SandboxManagedProfile, sandboxMode),
			}
		}
		return validateCodexBreakGlassShape(grants)
	case DefaultName, "":
		// Claude re-opens a CHILD of a denied directory natively: deny
		// directories are applied shallowest-first, so an allow for a strictly
		// deeper path re-binds after the parent deny tmpfs and its unrequested
		// siblings stay masked. tclaude therefore does not suppress the parent
		// deny for a child grant — only for a grant at or above it.
		if strings.TrimSpace(sandboxMode) != ClaudeSandboxOn {
			return &SandboxCapabilityError{
				Harness: DefaultName,
				Kind:    SandboxCapabilityBreakGlass,
				Message: fmt.Sprintf("break-glass protected access requires Claude sandbox %q; sandbox %q cannot guarantee the protected denies it must re-open are even applied", ClaudeSandboxOn, sandboxMode),
			}
		}
		return nil
	default:
		return &SandboxCapabilityError{
			Harness: harnessName,
			Kind:    SandboxCapabilityBreakGlass,
			Message: fmt.Sprintf("harness %q cannot represent break-glass protected access", harnessName),
		}
	}
}

// validateCodexBreakGlassShape rejects the one break-glass shape Codex cannot
// honor: a grant strictly INSIDE tclaude's denied private-state directory.
//
// Codex resolves a deny by masking the directory with a tmpfs after every
// allow bind, so a deny always dominates a narrower grant regardless of path
// specificity or declaration order. tclaude can suppress its own deny when the
// grant sits AT or ABOVE that directory — the operator acknowledged the whole
// root, so nothing beyond what they asked for is exposed. It cannot do so for
// a child grant: suppressing the parent deny to reach one child would expose
// every unrequested sibling as well, which is exactly the overgrant this
// mechanism must not commit. Leaving the deny in place instead would silently
// discard the acknowledged access.
//
// Neither is acceptable, so the launch is refused with an actionable message.
// (Claude has the opposite precedence and handles this shape natively.)
func validateCodexBreakGlassShape(grants []sandboxpolicy.BreakGlassGrant) error {
	// Source the list from sandboxpolicy so a future protected root cannot be
	// added without this validation covering it.
	roots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return err
	}
	for _, grant := range grants {
		path := filepath.Clean(grant.Path)
		for _, root := range roots {
			root = filepath.Clean(root)
			// At or above the root is representable: tclaude suppresses its own
			// deny for exactly that path, and the operator acknowledged a grant
			// covering everything the deny protected.
			if path == root || !pathIsUnder(root, path) {
				continue
			}
			return &SandboxCapabilityError{
				Harness: CodexName,
				Kind:    SandboxCapabilityBreakGlass,
				Message: fmt.Sprintf(
					"Codex cannot grant break-glass %s access to %q: it is inside the denied protected directory %q, "+
						"and Codex denies dominate any narrower grant regardless of path specificity, so the rule would be "+
						"silently discarded. Reaching it would require dropping the deny on the whole directory, which would "+
						"also expose every sibling you did not ask for. Either request %q itself (accepting the wider access) "+
						"or run this debugging agent under Claude Code, whose allow rules re-open a child path without "+
						"widening its parent.",
					grant.Access, grant.Path, root, root),
			}
		}
	}
	return nil
}

// pathIsUnder reports whether target is at or below dir.
func pathIsUnder(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// breakGlassCoversPath reports whether an acknowledged break-glass rule sits at
// or above denyPath, meaning the adapter must NOT emit its usual protected
// deny for denyPath.
//
// This is the crux of making break-glass actually work, and the two harnesses
// need it for opposite reasons. On Codex a deny always dominates regardless of
// specificity or declaration order, so a surviving deny would silently mask the
// grant entirely. On Claude a narrower allowRead does re-open a broader
// denyRead, but deny directories are applied shallowest-first, so a deny at the
// SAME depth as the grant is order-sensitive. Suppressing the covered deny
// makes the outcome unambiguous on both.
func breakGlassCoversPath(breakGlass []string, denyPath string) bool {
	denyPath = filepath.Clean(denyPath)
	for _, grant := range breakGlass {
		grant = filepath.Clean(strings.TrimSpace(grant))
		if grant == "" || !filepath.IsAbs(grant) {
			continue
		}
		rel, err := filepath.Rel(grant, denyPath)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}
