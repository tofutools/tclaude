package agentd

import (
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
	if snapshot == nil || len(snapshot.Effective.Filesystem) == 0 {
		return nil
	}
	switch harnessOrDefault(harnessName) {
	case harness.DefaultName:
		return nil
	case harness.CodexName:
		if strings.TrimSpace(sandboxMode) == harness.SandboxManagedProfile {
			return nil
		}
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("Codex additive filesystem grants require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, sandboxMode)}
	default:
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("harness %q cannot represent additive sandbox filesystem grants", harnessName)}
	}
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
