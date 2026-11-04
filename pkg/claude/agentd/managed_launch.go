package agentd

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// resolveManagedLaunch resolves a launch the daemon composes itself (trigger
// and process workers, scribe and Steward summons): the default profile tiers
// through applyDefaultProfile, then the sandbox snapshot and the common
// environment, the same inputs a direct spawn into the group gets.
//
// The snapshot is the group's sandbox-profile chain (global, then group),
// unless the resolved launch mode disables sandbox profiles. When parentConv
// names a spawning agent, the snapshot must be contained in that agent's own.
//
// keepUnconfiguredBare leaves the launch with no snapshot at all when no
// sandbox profile applies and no environment resolves. A scribe has always
// launched that way, and an empty snapshot is not equivalent to none (it
// turns on the harness-config floor and the other snapshot-driven launch
// rules), so nothing configured stays exactly as it was.
//
// A profile failure comes back as applyDefaultProfile reported it; the
// sandbox steps fail with kind spawn_failed, io or permission_denied.
func resolveManagedLaunch(g *db.AgentGroup, p *spawnParams, parentConv string, keepUnconfiguredBare bool) *spawnFailure {
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	snapshot := sandboxpolicy.OmittedProfilesSnapshot()
	if !sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		resolved, err := db.ResolveEffectiveSandboxSnapshot(g.ID, "")
		if err != nil {
			return &spawnFailure{http.StatusBadRequest, "spawn_failed", err.Error()}
		}
		snapshot = resolved
	}
	if parentConv != "" {
		parent, err := db.AgentEffectiveSandboxConfigForConv(parentConv)
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if parent == nil && sandboxpolicy.HasCapabilities(snapshot) {
			return &spawnFailure{http.StatusForbidden, "permission_denied", "owning principal predates effective sandbox snapshots"}
		}
		if parent != nil {
			validated, valErr := ensureAgentDirectoriesForRelaunch(*parent)
			if valErr != nil {
				return &spawnFailure{http.StatusBadRequest, "spawn_failed", valErr.Error()}
			}
			if containErr := sandboxpolicy.RequireContained(validated, snapshot); containErr != nil {
				return &spawnFailure{http.StatusForbidden, "permission_denied", containErr.Error()}
			}
		}
	}
	p.EffectiveSandbox = &snapshot
	// A second pass fills the common environment now that there is a
	// snapshot to carry it; every other field is already resolved, so it
	// changes nothing else.
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	if keepUnconfiguredBare && len(snapshot.Applied) == 0 && len(p.EffectiveSandbox.LaunchEnvironment) == 0 {
		p.EffectiveSandbox = nil
	}
	return nil
}

// fixedWorkdirDenial reports the sandbox rule that makes dir unreachable, for
// a launch whose working directory the caller cannot choose (a scribe always
// runs in its shared workdir). Only a deny at exactly dir does: a deny on a
// strict ancestor (deny ~, deny ~/.tclaude) leaves the launch directory
// reachable, because the launch reopens its workspace under a covering deny
// (session.sandboxDenyCoversPath and the workspace reopen beside it), as it
// does for every agent's cwd. "" when dir stays reachable.
func fixedWorkdirDenial(snapshot *sandboxpolicy.Snapshot, dir string) string {
	if snapshot == nil {
		return ""
	}
	dir = canonicalLaunchDir(dir)
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny || grant.IsRemapped() || filepath.Clean(grant.Path) != dir {
			continue
		}
		var names []string
		for _, s := range snapshot.Effective.Provenance.Filesystem[grant.Path] {
			names = append(names, fmt.Sprintf("%q", s.Profile))
		}
		if len(names) == 0 {
			return fmt.Sprintf("a sandbox rule denies %s", grant.Path)
		}
		return fmt.Sprintf("sandbox profile %s denies %s", strings.Join(names, ", "), grant.Path)
	}
	return ""
}

// canonicalLaunchDir spells dir the way the snapshot spells its grant paths
// (symlinks resolved), for a directory that may not exist yet: the nearest
// existing ancestor is resolved and the missing tail kept.
func canonicalLaunchDir(dir string) string {
	dir = filepath.Clean(dir)
	var tail []string
	for cur := dir; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// uniqueNotes keeps the first occurrence of each note, in order.
func uniqueNotes(notes []string) []string {
	if len(notes) < 2 {
		return notes
	}
	seen := make(map[string]bool, len(notes))
	out := notes[:0:0]
	for _, n := range notes {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// managedProfileRefusal checks a spawn profile a managed worker's launch
// reads against ownerConv: the worker's own profile when one is given, and
// with defaults the group's default and the global default (the tiers
// applyDefaultProfile walks). Callers check the defaults after the owner's
// permission, so an owner who may not spawn at all is told that, and before
// any rate slot is spent. A disabled or operator-only profile refuses the
// worker with kind profile_disabled or profile_operator_only and a message
// naming the profile and the tier it sits on.
func managedProfileRefusal(g *db.AgentGroup, profile *db.SpawnProfile, ownerConv string, defaults bool) *spawnFailure {
	type tier struct {
		label   string
		profile *db.SpawnProfile
	}
	tiers := []tier{{"the worker's spawn profile", profile}}
	if defaults && g != nil {
		tiers = append(tiers,
			tier{fmt.Sprintf("the default spawn profile of group %s", g.Name), groupDefaultProfile(g)},
			tier{"the global default spawn profile", globalDefaultProfile()})
	}
	for _, tier := range tiers {
		if fail := profileSpawnFailure(tier.profile, ownerConv); fail != nil {
			return &spawnFailure{fail.Status, fail.Kind, fmt.Sprintf("%s (%s)", fail.Msg, tier.label)}
		}
	}
	return nil
}

// isManagedProfileRefusal reports a refusal managedProfileRefusal produces.
// It repeats on every firing until someone changes the profile, so callers
// record it as a failure and announce the first one.
func isManagedProfileRefusal(kind string) bool {
	return kind == "profile_disabled" || kind == "profile_operator_only"
}
