package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// spawnSandboxLineageFailure prevents an agent that can spawn peers from
// minting a child with a looser launch sandbox than the caller currently has.
// Humans bypass this check: they are the trust root everywhere else in agentd.
func spawnSandboxLineageFailure(parentConvID, childHarness, childSandbox string) *spawnFailure {
	if parentConvID == "" {
		return nil
	}
	parent, err := spawnLineageParentSandbox(parentConvID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io",
			"spawn sandbox guard: " + err.Error()}
	}
	child := spawnLineageSandbox{
		Harness: harnessOrDefault(childHarness),
		Mode:    strings.TrimSpace(childSandbox),
	}
	if !spawnSandboxLineageAllowed(parent, child) {
		return &spawnFailure{http.StatusForbidden, "sandbox_restricted",
			fmt.Sprintf("agent %s was launched as %s sandbox %q and may not spawn a %s child with sandbox %q",
				short8(parentConvID), parent.Harness, parent.Mode, child.Harness, child.Mode)}
	}
	return nil
}

func sandboxProfileCapabilityFailure(harnessName, sandboxMode string, snapshot *sandboxpolicy.Snapshot) *spawnFailure {
	if snapshot == nil {
		return nil
	}
	filesystem, err := sandboxpolicy.FilesystemForLaunch(snapshot.Effective)
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem", err.Error()}
	}
	// TCL-609 capability gates run FIRST and unconditionally: a minimal read
	// baseline or an acknowledged protected grant must be refused by a harness
	// that cannot enforce it even when the profile carries no other rules.
	// Approximating either one would hand the operator a false guarantee.
	if baseline := snapshot.Effective.ReadBaseline; baseline != sandboxpolicy.ReadBaselineDefault {
		if err := harness.ValidateSandboxReadBaseline(harnessOrDefault(harnessName), sandboxMode, baseline); err != nil {
			return sandboxCapabilitySpawnFailure(err, harness.SandboxCapabilityReadBaseline)
		}
	}
	if grants := snapshot.Effective.BreakGlassFilesystem; len(grants) > 0 {
		if err := harness.ValidateSandboxBreakGlass(harnessOrDefault(harnessName), sandboxMode, grants); err != nil {
			return sandboxCapabilitySpawnFailure(err, harness.SandboxCapabilityBreakGlass)
		}
		if _, err := sandboxpolicy.BreakGlassForLaunch(snapshot.Effective); err != nil {
			return &spawnFailure{http.StatusUnprocessableEntity, harness.SandboxCapabilityBreakGlass, err.Error()}
		}
	}
	hasNetworkPolicy := snapshot.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit
	if len(filesystem) == 0 && len(snapshot.Effective.AgentDirectories) == 0 && !hasNetworkPolicy {
		return nil
	}
	switch harnessOrDefault(harnessName) {
	case harness.DefaultName:
		if hasNetworkPolicy {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network",
				"Claude launches cannot represent sandbox profile network access; use the Codex managed sandbox"}
		}
		if strings.TrimSpace(sandboxMode) != harness.ClaudeSandboxOn && filesystemHasDeny(filesystem) {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
				fmt.Sprintf("Claude filesystem deny rules require sandbox %q; sandbox %q cannot guarantee enforcement", harness.ClaudeSandboxOn, sandboxMode)}
		}
		return nil
	case harness.CodexName:
		if strings.TrimSpace(sandboxMode) == harness.SandboxManagedProfile {
			if hasNetworkPolicy {
				if err := harness.ValidateCodexAgentNetworkAccess(snapshot.Effective.NetworkAccess); err != nil {
					return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error()}
				}
			}
			return nil
		}
		if hasNetworkPolicy {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network",
				fmt.Sprintf("Codex network rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, sandboxMode)}
		}
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("Codex filesystem rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, sandboxMode)}
	default:
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("harness %q cannot represent sandbox filesystem rules", harnessName)}
	}
}

// sandboxProfilesDisabled reports launch modes whose explicit contract is to
// run without tclaude's sandbox policy. A Codex danger-full-access launch uses
// the raw --sandbox opt-out, which cannot be combined with the managed
// permission profile that renders filesystem rules. Treating the policy as
// absent also keeps environment-only profiles from being applied under a UI
// choice that says the sandbox profile is disabled.
func sandboxProfilesDisabled(harnessName, sandboxMode string) bool {
	return harnessOrDefault(harnessName) == harness.CodexName &&
		strings.TrimSpace(sandboxMode) == harness.SandboxDangerFull
}

func filesystemHasDeny(filesystem []sandboxpolicy.FilesystemGrant) bool {
	for _, grant := range filesystem {
		if grant.Access == sandboxpolicy.AccessDeny {
			return true
		}
	}
	return false
}

// sandboxCapabilitySpawnFailure converts an adapter capability refusal into
// the daemon's typed HTTP failure, preserving the adapter's stable error kind
// so the CLI and dashboard can render the specific remedy.
func sandboxCapabilitySpawnFailure(err error, fallbackKind string) *spawnFailure {
	var capErr *harness.SandboxCapabilityError
	if errors.As(err, &capErr) {
		return &spawnFailure{http.StatusUnprocessableEntity, capErr.Kind, capErr.Message}
	}
	return &spawnFailure{http.StatusUnprocessableEntity, fallbackKind, err.Error()}
}

type spawnLineageSandbox struct {
	Harness string
	Mode    string
}

func spawnLineageParentSandbox(convID string) (spawnLineageSandbox, error) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil {
		return spawnLineageSandbox{}, err
	}
	if row == nil {
		// A real daemon caller should have a live session row. Tests and very old
		// rows can lack one, so degrade to the default Claude/inherit posture
		// instead of treating "unknown" as full access.
		return spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit}, nil
	}
	h := harnessOrDefault(row.Harness)
	mode := strings.TrimSpace(row.SandboxMode)
	if h == harness.DefaultName && mode == "" {
		// Old Claude rows and the test simulator used "" for "settings.json
		// decides"; in the lineage matrix that is Claude's inherit sentinel.
		mode = harness.ClaudeSandboxInherit
	}
	return spawnLineageSandbox{Harness: h, Mode: mode}, nil
}

func spawnSandboxLineageAllowed(parent, child spawnLineageSandbox) bool {
	parent, parentOK := normalizeSpawnLineageSandbox(parent)
	child, childOK := normalizeSpawnLineageSandbox(child)
	if !parentOK || !childOK {
		return false
	}

	if parent.Harness == harness.DefaultName {
		switch parent.Mode {
		case harness.ClaudeSandboxOff:
			return true
		case harness.ClaudeSandboxInherit:
			return childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		case harness.ClaudeSandboxOn:
			return childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		}
	}

	if parent.Harness == harness.CodexName {
		switch parent.Mode {
		case harness.SandboxDangerFull:
			return true
		case harness.SandboxManagedProfile:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn)
		case harness.SandboxWorkspaceWrite:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite)
		case harness.SandboxReadOnly:
			return childIsCodex(child, harness.SandboxReadOnly)
		}
	}
	return false
}

func normalizeSpawnLineageSandbox(s spawnLineageSandbox) (spawnLineageSandbox, bool) {
	s.Harness = harnessOrDefault(s.Harness)
	s.Mode = strings.TrimSpace(s.Mode)
	switch s.Harness {
	case harness.DefaultName:
		if s.Mode == "" {
			s.Mode = harness.ClaudeSandboxInherit
		}
		switch s.Mode {
		case harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn, harness.ClaudeSandboxOff:
			return s, true
		}
	case harness.CodexName:
		switch s.Mode {
		case harness.SandboxManagedProfile, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxDangerFull:
			return s, true
		}
	}
	return spawnLineageSandbox{}, false
}

func childIsClaude(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.DefaultName && modeIn(child.Mode, modes...)
}

func childIsCodex(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.CodexName && modeIn(child.Mode, modes...)
}

func modeIn(mode string, allowed ...string) bool {
	for _, a := range allowed {
		if mode == a {
			return true
		}
	}
	return false
}
