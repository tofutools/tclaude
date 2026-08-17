package agentd

import (
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The spawn-time harness-config selector.
//
// The floor itself is enforced by a mount frozen at launch (see
// session/sandbox_harness_config_floor.go). This file governs only WHO MAY ASK
// for a different posture, which is the part a permission slug can honestly
// gate: agentd's permission layer is a coordination guardrail, and the OS
// sandbox is what makes it hold — never the other way round.
//
// Three rules, in the order they apply:
//
//  1. A human may select either posture outright. Humans are the trust root
//     here exactly as they are for --sandbox-profile and lineage.
//  2. An agent needs sandbox.harness-config. It is not default-granted and
//     group ownership does not confer it, because lifting the floor lets the
//     launched agent rewrite the policy that confines it.
//  3. Whoever asked, sandbox lineage still runs afterwards. A child posture
//     wider than the recorded parent's is refused by RequireContained, so the
//     slug can never be used to widen past the caller's own confinement.

// validateSpawnHarnessConfig checks the requested posture and the caller's
// authority to request it, writing the refusal itself so the slug gate can use
// the shared requirePermission path (which reports the exact matched source).
// An empty value is the ordinary case: inherit the profile chain, whose own
// default is the floor.
func validateSpawnHarnessConfig(
	w http.ResponseWriter,
	r *http.Request,
	requested string,
) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return true
	}
	if _, err := sandboxpolicy.NormalizeHarnessConfigAccess(
		sandboxpolicy.HarnessConfigAccess(requested)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_harness_config", err.Error())
		return false
	}
	if classify(peerFromContext(r.Context())) == classHuman {
		return true
	}
	if _, ok := requirePermission(w, r, PermSandboxHarnessConfig); !ok {
		return false
	}
	return true
}

// applySpawnHarnessConfig folds an explicit selection into the resolved
// snapshot. It deliberately OVERWRITES rather than composing strictest-wins
// with the profile chain: the flag is a launch contract, not a fourth scope,
// the same way --omit-sandbox-profiles overrides ambient tiers rather than
// merging with them. Composition still governs the profile chain itself, and
// lineage still governs the result.
//
// An omitted-profiles snapshot is left alone: it records "no profile tier
// applied at all" and RevalidateSnapshot refuses one carrying profile values,
// so writing a posture into it would turn a fail-closed marker into an error.
// The floor still applies to such a launch, because the floor's default is
// what an absent value means.
func applySpawnHarnessConfig(
	snapshot sandboxpolicy.Snapshot,
	requested string,
) (sandboxpolicy.Snapshot, *spawnFailure) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return snapshot, nil
	}
	access := sandboxpolicy.HarnessConfigAccess(requested)
	if snapshot.ProfilesOmitted {
		if !sandboxpolicy.HarnessConfigFloorApplies(access) {
			return snapshot, &spawnFailure{
				Status: http.StatusUnprocessableEntity,
				Kind:   "invalid_harness_config",
				Msg: "harness_config cannot be selected for a launch that omits every sandbox-profile tier; " +
					"drop --omit-sandbox-profiles or author the posture in a profile",
			}
		}
		return snapshot, nil
	}
	// A plain struct copy is enough: only scalars change, and the shared slices
	// are never mutated here. Provenance is cleared alongside the value the way
	// UnconfinedLaunchSnapshot does: it names the profile and scope a composed
	// value came from, and this value came from the launch contract instead, so
	// leaving it would make the recorded audit trail name an innocent profile.
	updated := snapshot
	updated.Effective.HarnessConfig = access
	updated.Effective.Provenance.HarnessConfig = nil
	revalidated, err := sandboxpolicy.RevalidateSnapshot(updated)
	if err != nil {
		return snapshot, &spawnFailure{
			Status: http.StatusUnprocessableEntity,
			Kind:   "invalid_harness_config",
			Msg:    err.Error(),
		}
	}
	return revalidated, nil
}
