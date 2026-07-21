package agentd

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// breakGlassAckErrorKind is the typed error code every management, import,
// assignment, and launch-selection surface returns when a profile carrying
// protected-path authority is committed without an explicit acknowledgement.
// It is stable wire vocabulary: the dashboard and CLI both key their
// consequence warnings off it.
const breakGlassAckErrorKind = "break_glass_acknowledgement_required"

// BreakGlassRiskSummary is the operator-facing consequence text. It is
// deliberately concrete rather than a generic "this is dangerous": the ticket
// requires each surface to explain what protected access actually enables.
const BreakGlassRiskSummary = "Break-glass access reaches tclaude/harness state that is protected by default. " +
	"Read access can disclose daemon secrets, agent authorization state, and harness session transcripts and credentials. " +
	"Write access can additionally corrupt the SQLite database, harness configuration, and runtime state, " +
	"invalidate the assumptions agent authorization relies on, and break the daemon or the harness. " +
	"Grant the narrowest access that answers the question, prefer read over write, and remove the profile afterwards."

// describeBreakGlass renders the exact path/access pairs an operator is being
// asked to acknowledge. Composition must never hide the source, so callers
// pass the flattened rules rather than the authored ones.
func describeBreakGlass(grants []sandboxpolicy.BreakGlassGrant) string {
	if len(grants) == 0 {
		return ""
	}
	parts := make([]string, 0, len(grants))
	for _, grant := range grants {
		parts = append(parts, fmt.Sprintf("%s %s", grant.Access, grant.Path))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// breakGlassAckFailure builds the typed 422 for one unacknowledged surface.
// what names the operation ("save", "import", "assign globally", …) so the
// message reads as an instruction rather than a bare refusal.
func breakGlassAckFailure(what string, grants []sandboxpolicy.BreakGlassGrant) *spawnFailure {
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassAckErrorKind, fmt.Sprintf(
		"refusing to %s a sandbox profile with break-glass protected access (%s) without an explicit acknowledgement. %s "+
			"Re-send with break_glass_acknowledged: true (CLI: --i-understand-break-glass-risk).",
		what, describeBreakGlass(grants), BreakGlassRiskSummary)}
}

// requireBreakGlassAck enforces the acknowledgement for a profile payload that
// has already been normalized. The acknowledgement is deliberately transient:
// it is never persisted on the profile, so the durable danger marker is the
// break_glass_filesystem field itself and a later import, assignment, or
// machine transfer must acknowledge again.
func requireBreakGlassAck(what string, acknowledged bool, grants []sandboxpolicy.BreakGlassGrant) *spawnFailure {
	if len(grants) == 0 || acknowledged {
		return nil
	}
	return breakGlassAckFailure(what, grants)
}

// requirePayloadBreakGlassAck gates a create/edit on the FLATTENED payload, so
// an include-only edit of an innocent-looking wrapper cannot add protected
// access without an acknowledgement.
func requirePayloadBreakGlassAck(what string, acknowledged bool, p *db.SandboxProfile) *spawnFailure {
	grants, err := flattenBreakGlassForPayload(p)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "inspect sandbox profile break-glass access: " + err.Error()}
	}
	return requireBreakGlassAck(what, acknowledged, grants)
}

// postImportBreakGlassForName reports the protected access an assignment to
// name will carry AFTER the import commits. The bundle may be about to replace
// the local profile of that name (or create it), so resolving against the
// pre-import registry alone would miss or misjudge the danger. The active
// conflict policy decides which payload wins: "skip" keeps the local one.
func postImportBreakGlassForName(name string, incoming []*db.SandboxProfile, conflict string) ([]sandboxpolicy.BreakGlassGrant, error) {
	local, err := db.GetSandboxProfile(name)
	if err != nil {
		return nil, err
	}
	var effective *db.SandboxProfile
	for _, candidate := range incoming {
		if candidate.Name != name {
			continue
		}
		// "skip" leaves an existing local profile untouched, so the local
		// payload — not the bundle's — is what the assignment will point at.
		if local != nil && conflict == "skip" {
			break
		}
		effective = candidate
		break
	}
	if effective == nil {
		effective = local
	}
	if effective == nil {
		// The assignment names a profile that will not exist; the import's own
		// validation reports that. No protected access to acknowledge here.
		return nil, nil
	}
	// Resolve includes against the registry. Bundle-internal includes may not
	// be written yet, so fall back to the conservative one-level union rather
	// than reporting nothing.
	return flattenBreakGlassForPayload(effective)
}

// flattenBreakGlassForPayload resolves a not-yet-persisted profile payload
// against the CURRENT registry, so a create or edit that carries no direct
// break-glass but includes a profile that does still demands an
// acknowledgement. Gating on the direct field alone let an operator (or a
// draft-submitting agent) launder dangerous authority behind a wrapper.
//
// A dangling include cannot be resolved yet; that is the write path's error to
// report, so this reports no break-glass and lets the normal validation run.
func flattenBreakGlassForPayload(p *db.SandboxProfile) ([]sandboxpolicy.BreakGlassGrant, error) {
	if p == nil {
		return nil, nil
	}
	if len(p.Includes) == 0 {
		return p.BreakGlassFilesystem, nil
	}
	flattened, err := sandboxpolicy.Flatten(sandboxProfileToPolicy(p), registryLookupForFlatten())
	if err != nil {
		// Fail OPEN on resolution here would be a bypass, so fall back to the
		// union of the direct field and every include we can still read.
		return unresolvedIncludeBreakGlass(p), nil //nolint:nilerr // best-effort danger detection; the write path reports the real include error
	}
	return flattened.BreakGlassFilesystem, nil
}

// unresolvedIncludeBreakGlass is the conservative fallback when the include
// graph cannot be flattened: report any protected access reachable one level
// down so an unresolvable graph cannot be used to skip the acknowledgement.
func unresolvedIncludeBreakGlass(p *db.SandboxProfile) []sandboxpolicy.BreakGlassGrant {
	out := append([]sandboxpolicy.BreakGlassGrant(nil), p.BreakGlassFilesystem...)
	for _, include := range p.Includes {
		included, err := db.GetSandboxProfile(include)
		if err != nil || included == nil {
			continue
		}
		out = append(out, included.BreakGlassFilesystem...)
	}
	return out
}

func sandboxProfileToPolicy(p *db.SandboxProfile) sandboxpolicy.Profile {
	return sandboxpolicy.Profile{
		Name:                 p.Name,
		Filesystem:           p.Filesystem,
		ReadBaseline:         p.ReadBaseline,
		BreakGlassFilesystem: p.BreakGlassFilesystem,
		Environment:          p.Environment,
		AgentDirectories:     p.AgentDirectories,
		NetworkAccess:        p.NetworkAccess,
		Includes:             p.Includes,
	}
}

func registryLookupForFlatten() sandboxpolicy.LookupProfile {
	return func(include string) (*sandboxpolicy.Profile, error) {
		included, err := db.GetSandboxProfile(include)
		if err != nil || included == nil {
			return nil, err
		}
		policy := sandboxProfileToPolicy(included)
		return &policy, nil
	}
}

// flattenedBreakGlassForProfile resolves a registry profile's INCLUDES before
// reporting its protected access, so a profile that inherits break-glass from
// an included profile still demands an acknowledgement at assignment time.
// Assignment surfaces name a profile rather than posting a payload, which is
// exactly the case where composition could otherwise hide the danger.
func flattenedBreakGlassForProfile(name string) ([]sandboxpolicy.BreakGlassGrant, error) {
	profile, err := db.GetSandboxProfile(name)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return nil, nil
	}
	flattened, err := sandboxpolicy.Flatten(sandboxProfileToPolicy(profile), registryLookupForFlatten())
	if err != nil {
		return nil, err
	}
	return flattened.BreakGlassFilesystem, nil
}

// requireAssignmentBreakGlassAck gates the two persistent-risk surfaces:
// making a break-glass profile the global default, or the default for a whole
// group. Every agent launched under that scope inherits the protected access
// until the assignment is removed, so these carry the strongest wording.
func requireAssignmentBreakGlassAck(scope, name string, acknowledged bool) *spawnFailure {
	grants, err := flattenedBreakGlassForProfile(name)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "inspect sandbox profile break-glass access: " + err.Error()}
	}
	if len(grants) == 0 || acknowledged {
		return nil
	}
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassAckErrorKind, fmt.Sprintf(
		"sandbox profile %q carries break-glass protected access (%s). Assigning it as the %s default gives EVERY agent "+
			"launched under that scope this access, for as long as the assignment stands. %s "+
			"Re-send with break_glass_acknowledged: true (CLI: --i-understand-break-glass-risk).",
		name, describeBreakGlass(grants), scope, BreakGlassRiskSummary)}
}
