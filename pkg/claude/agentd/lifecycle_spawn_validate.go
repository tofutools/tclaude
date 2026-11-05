package agentd

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// normalizeSpawnLaunchPosture is executeSpawn's first validation step. It
// applies the default spawn profile, refuses what no launch path may carry
// (a shell spawn with an initial message, a harness the group's policy
// forbids), derives a name for an unnamed group spawn, and settles the
// sandbox posture: the profile omission a raw no-sandbox mode forces, the
// pinned Git common dir and worktree write grants, the revalidated sandbox
// snapshot and the capability, Codex-native registry, Copilot API loopback
// and access-plan gates. It mutates p in place.
func normalizeSpawnLaunchPosture(g *db.AgentGroup, p *spawnParams, groupName string) *spawnFailure {
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	// Trigger and template spawns bypass handleGroupSpawn. Apply the shell rule
	// here too, after their default profile has resolved the harness.
	if p.Harness == harness.ShellName && strings.TrimSpace(p.InitialMessage) != "" {
		return &spawnFailure{http.StatusBadRequest, "invalid_initial_message",
			"shell spawn requires non_interactive when initial_message is set"}
	}
	if strings.TrimSpace(p.Name) == "" && groupName != "" {
		p.Name = derivedGroupSpawnName(groupName, time.Now(), randomLabelToken())
	}
	// Defense in depth for template, wave, scribe, and process adapters that
	// call executeSpawn directly instead of passing through handleGroupSpawn.
	if fail := spawnHarnessPolicyFailure(g, p.SpawnedByConv, p.Harness); fail != nil {
		return fail
	}
	// Keep non-HTTP launch paths consistent with handleGroupSpawn. In
	// particular, template/wave callers can arrive with a previously resolved
	// global/group policy even though this agent explicitly selects Codex's raw
	// no-sandbox mode.
	if sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		omitted := sandboxpolicy.OmittedProfilesSnapshotKeepingLaunchEnvironment(p.EffectiveSandbox)
		// The mode forced this omission, unless the launch already chose one.
		omitted.ProfilesOmittedDerived = p.EffectiveSandbox == nil || !p.EffectiveSandbox.ProfilesOmittedByChoice()
		p.EffectiveSandbox = &omitted
	}
	if spawnUsesPinnedGitCommonDir(
		p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) && !p.CodexGitCommonDirPinned {
		gitCommonDir, err := spawnGitCommonDir(
			p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation, p.Cwd)
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		p.CodexGitCommonDir = gitCommonDir
		p.CodexGitCommonDirPinned = true
	}
	if p.CodexGitCommonDirPinned && !p.GitWorktreeWriteDirsPinned {
		home, err := os.UserHomeDir()
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		p.GitWorktreeWriteDirs = harness.GitWorktreeWriteDirs(p.Cwd, p.CodexGitCommonDir, home)
		p.GitWorktreeWriteDirsPinned = true
	}
	if p.GitWorktreeWriteDirsPinned {
		var fail *spawnFailure
		p.GitWorktreeWriteDirs, fail = canonicalizeRepositoryWriteDirs(
			p.GitWorktreeWriteDirs, p.DirWriteProofDirs, p.DirWriteProofToken)
		if fail != nil {
			return fail
		}
	}
	if p.EffectiveSandbox != nil {
		validated, err := sandboxpolicy.RevalidateSnapshot(*p.EffectiveSandbox)
		if err != nil {
			return &spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()}
		}
		p.EffectiveSandbox = &validated
	}
	if fail := sandboxProfileCapabilityFailure(
		p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation); fail != nil {
		return fail
	}
	if session.CodexNativeRegistryApplicable(p.CodexAppServer, harnessOrDefault(p.Harness),
		p.HarnessBuiltinMode, p.SandboxImplementation) {
		if err := codexNativeRegistryReadiness(); err != nil {
			return &spawnFailure{http.StatusPreconditionFailed,
				codexNativeRegistryErrorCode(err), err.Error()}
		}
	}
	if fail := copilotAPILoopbackFailure(
		p.CopilotAPI, p.EffectiveSandbox, p.SandboxImplementation); fail != nil {
		return fail
	}
	if _, fail := planSandboxProfileAccessForLaunch(
		p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation,
		session.ModelTransportLaunchContext{
			Model:      p.Model,
			ModelProxy: p.ModelProxy,
			Cwd:        p.Cwd,
		},
		p.AllowUnenforcedSandbox,
	); fail != nil {
		return fail
	}
	return nil
}

// settleSpawnTrust is executeSpawn's second validation step: the trust and
// posture decisions that read the settled sandbox. It reports whether the
// Codex SSH workaround applies, drops pinned write grants and the pinned Git
// common dir when no dir write proof backs them, auto-trusts a sibling
// worktree, then runs the Copilot folder-trust, caller-owned trust-dir and
// launch-posture gates against the trust value the launch will act on.
func settleSpawnTrust(p *spawnParams) (bool, *spawnFailure) {
	activeSSHWorkaround := p.SSHWorkaround && codexSSHWorkaroundApplies(
		p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation, p.EffectiveSandbox)
	if strings.TrimSpace(p.DirWriteProofToken) == "" {
		p.GitWorktreeWriteDirs = nil
		p.GitWorktreeWriteDirsPinned = false
	}
	autoTrustSiblingWorktree, err := defaultSiblingWorktreeTrust(p.Harness, p.Cwd, p.CodexGitCommonDir)
	if err != nil {
		return false, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if autoTrustSiblingWorktree {
		p.TrustDir = true
	}
	// After the sibling-worktree auto-trust has had its say, because that is
	// what makes p.TrustDir the value the launch will actually act on. Asking
	// before it would refuse a worktree spawn that is about to be seeded
	// anyway — a refusal derived from a value that had not finished being
	// decided, which is the failure shape this series keeps producing.
	if fail := copilotAPIFolderTrustFailure(*p); fail != nil {
		return false, fail
	}
	if p.SpawnedByConv != "" && p.TrustDir && !autoTrustSiblingWorktree {
		callerOwned, ownErr := callerOwnedDirTrustProved(
			p.SpawnedByConv, p.Cwd, p.DirWriteProofToken, p.DirWriteProofDirs)
		if ownErr != nil {
			return false, &spawnFailure{http.StatusInternalServerError, "io", ownErr.Error()}
		}
		if !callerOwned {
			return false, &spawnFailure{http.StatusForbidden, "trust_dir_restricted", trustDirRestrictedMessage}
		}
	}
	// Judges the LAUNCH mode for the same reason handleGroupSpawn does: the
	// guard must reason about the posture the child's row will carry, which for
	// a tclaude-layer child is the forced single-wall mode (TCL-989).
	childHarness, hErr := harness.ResolveSpawnable(harnessOrDefault(p.Harness))
	if hErr != nil {
		return false, &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", hErr.Error()}
	}
	if fail := launchPostureFailure(p.SpawnedByConv, childHarness, p.Harness, p.HarnessBuiltinMode,
		p.SandboxImplementation, p.ApprovalPolicy, p.AutoReview, p.AllowUnenforcedSandbox); fail != nil {
		return false, fail
	}
	if strings.TrimSpace(p.DirWriteProofToken) == "" {
		p.CodexGitCommonDir = ""
		p.CodexGitCommonDirPinned = false
	}
	return activeSSHWorkaround, nil
}
