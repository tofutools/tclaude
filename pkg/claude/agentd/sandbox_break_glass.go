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
	flattened, err := sandboxpolicy.Flatten(sandboxpolicy.Profile{
		Name:                 profile.Name,
		Filesystem:           profile.Filesystem,
		ReadBaseline:         profile.ReadBaseline,
		BreakGlassFilesystem: profile.BreakGlassFilesystem,
		Environment:          profile.Environment,
		AgentDirectories:     profile.AgentDirectories,
		NetworkAccess:        profile.NetworkAccess,
		Includes:             profile.Includes,
	}, func(include string) (*sandboxpolicy.Profile, error) {
		included, err := db.GetSandboxProfile(include)
		if err != nil || included == nil {
			return nil, err
		}
		return &sandboxpolicy.Profile{
			Name:                 included.Name,
			Filesystem:           included.Filesystem,
			ReadBaseline:         included.ReadBaseline,
			BreakGlassFilesystem: included.BreakGlassFilesystem,
			Environment:          included.Environment,
			AgentDirectories:     included.AgentDirectories,
			NetworkAccess:        included.NetworkAccess,
			Includes:             included.Includes,
		}, nil
	})
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
