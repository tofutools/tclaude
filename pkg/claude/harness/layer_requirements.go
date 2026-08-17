package harness

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Harness-agnostic declaration vocabulary for tclaude-layer filesystem
// requirements (TCL-1201).
//
// A harness answers WHAT it needs on disk for a tclaude-layer launch as a list
// of LayerPathRequirement rows. The session package owns HOW those rows are
// normalized, deduplicated, validated, prepared and rendered; it folds every
// harness's rows through one translation, so equivalent resources receive
// equivalent treatment regardless of harness.
//
// The vocabulary is deliberately smaller than CopilotBaselineEntry, which
// remains the Copilot-side authority (necessity, feature-conditionality,
// provenance, the execute-bit invariant). Rows here are the already-resolved
// output of such a catalog, not a replacement for one.

// LayerPathKind is what the path IS. The shared fold needs it because a
// directory is prepared (mkdir) and bound differently from a file or a live
// socket, and a cold path may not exist yet to stat.
type LayerPathKind string

const (
	LayerPathDirectory LayerPathKind = "directory"
	LayerPathFile      LayerPathKind = "file"
	LayerPathSocket    LayerPathKind = "socket"
)

// LayerPathAccess is the access mode the launched harness needs at the path.
// There is no execute axis: neither outer backend takes one (bubblewrap binds
// are exec-capable, Seatbelt allows process-exec for readable paths). A
// catalog that carries an execute invariant keeps it on its own rows (see
// CopilotTclaudeLayerGrantSet.ExecutablePaths).
type LayerPathAccess string

const (
	LayerPathRead  LayerPathAccess = "read"
	LayerPathWrite LayerPathAccess = "write"
)

// LayerPathRequirement is one filesystem resource a harness requires for a
// tclaude-layer launch.
type LayerPathRequirement struct {
	// Path is absolute. Canonicalization (symlink resolution, dedup) is the
	// shared fold's job, not the declarer's, so every harness gets the same
	// answer.
	Path string

	// Kind is the node type at Path.
	Kind LayerPathKind

	// Access is the required mode.
	Access LayerPathAccess

	// MayCreate marks a path launch preparation may materialize with mkdir
	// when it does not exist yet. Only directories may be created; a file or
	// socket row with MayCreate is refused by the fold, which is the
	// build-time guarantee that a non-directory can never reach directory
	// materialization.
	MayCreate bool

	// PolicyGrant marks a writable directory row that additionally composes
	// into the launch policy filesystem as a write grant, rather than riding
	// only the launch contract's phase-0 write set. Read rows always compose
	// as read grants, so the flag is meaningful for writable directories
	// only. Copilot's baseline rows are grants by design and set it; the
	// OpenCode state layout is pure launch-contract state and does not.
	PolicyGrant bool

	// Source records where the requirement came from (a catalog row id, an
	// environment variable, a documented default), for error messages and
	// disclosure.
	Source string
}

// CopilotLayerPathRequirements resolves the Copilot baseline for one launch
// and returns it in the harness-agnostic requirement vocabulary.
//
// It is a projection of CopilotTclaudeLayerGrants: the catalog keeps every
// refusal and the execute-bit check, and this function only re-attaches the
// node kind each grant lost in translation. Zipping grants with their catalog
// entries happens here, next to where both lists are produced in the same
// order, rather than at a call site that would have to trust the pairing.
func CopilotLayerPathRequirements(in CopilotBaselineInput) ([]LayerPathRequirement, error) {
	grants, err := CopilotTclaudeLayerGrants(in)
	if err != nil {
		return nil, err
	}
	if len(grants.Grants) != len(grants.Entries) {
		return nil, copilotGrantError(fmt.Sprintf(
			"copilot baseline translated %d entries into %d grants",
			len(grants.Entries), len(grants.Grants)))
	}
	out := make([]LayerPathRequirement, 0, len(grants.Grants))
	for index, grant := range grants.Grants {
		entry := grants.Entries[index]
		requirement := LayerPathRequirement{
			Path:   grant.Path,
			Source: entry.ID,
		}
		switch entry.Kind {
		case CopilotNodeDirectory:
			requirement.Kind = LayerPathDirectory
		case CopilotNodeFile:
			requirement.Kind = LayerPathFile
		case CopilotNodeSocket:
			requirement.Kind = LayerPathSocket
		default:
			return nil, copilotGrantError(fmt.Sprintf(
				"copilot baseline entry %q has unsupported node kind %q",
				entry.ID, entry.Kind))
		}
		switch grant.Access {
		case sandboxpolicy.AccessRead:
			requirement.Access = LayerPathRead
		case sandboxpolicy.AccessWrite:
			requirement.Access = LayerPathWrite
		default:
			return nil, copilotGrantError(fmt.Sprintf(
				"copilot baseline grant %q resolved unsupported access %q",
				grant.Path, grant.Access))
		}
		// Directories are the only rows the launch may materialize. The
		// executables must already exist, and a socket is a live endpoint
		// bound by its owner, never created by launch preparation.
		requirement.MayCreate = requirement.Kind == LayerPathDirectory
		// Baseline rows are grants by design (see copilot_sandbox_grants.go):
		// they compose into the launch policy filesystem, not only the
		// contract's phase-0 write set.
		requirement.PolicyGrant = true
		out = append(out, requirement)
	}
	return out, nil
}
