package conv

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// resumeDirsInput is what planResumeDirs derives the resume's directory
// grants from: the read, write and deny lists built from the snapshot so far.
type resumeDirsInput struct {
	h                             *harness.Harness
	harnessBuiltinMode, resumeCwd string
	outerLayer, unconfined        bool
	launchGrants                  []sandboxpolicy.FilesystemGrant
	agentDirPaths, tmpfsDirs      []string
	readDirs, writeDirs, denyDirs []string
	resumeEnv                     map[string]string
	// denyOverlay is the agent's spawn-time deny overlay, which no path this
	// resume opens may lie under.
	denyOverlay []string
}

// resumeDirs is the resume's directory plan: the read and write lists the
// harness is told about, and the Git and workspace write grants handed to
// tclaude's sandbox contract.
type resumeDirs struct {
	read, write               []string
	tclaudeLayerContractWrite []string
}

// planResumeDirs is the resume's directory step. It adds the Git worktree and
// workspace write grants, pairs read reopens for everything a deny covers that
// the agent must reach, and re-runs the reopen-under-deny and Copilot add-dir
// gates on the rules that will actually be emitted.
func planResumeDirs(in resumeDirsInput) (resumeDirs, error) {
	h, harnessBuiltinMode, resumeCwd := in.h, in.harnessBuiltinMode, in.resumeCwd
	outerLayer, unconfined := in.outerLayer, in.unconfined
	launchGrants, agentDirPaths, tmpfsDirs := in.launchGrants, in.agentDirPaths, in.tmpfsDirs
	readDirs, writeDirs, denyDirs, resumeEnv := in.readDirs, in.writeDirs, in.denyDirs, in.resumeEnv
	// A deny covering the workspace narrows the Git grants the same way the
	// spawn path does: the historical repository container would reopen every
	// sibling repo beneath the deny.
	workspaceDenied := resumeDenyCoversPath(launchGrants, resumeCwd)
	var tclaudeLayerContractWriteDirs []string
	if outerLayer ||
		(h.Name == harness.CodexName && harnessBuiltinMode == harness.SandboxManagedProfile) ||
		(h.Name == harness.DefaultName && harnessBuiltinMode != harness.ClaudeSandboxOff) {
		gitWriteDirs, err := resumeGitWorktreeWriteDirs(resumeCwd, workspaceDenied)
		if err != nil {
			return resumeDirs{}, fmt.Errorf("resolve sandboxed resume Git grants: %w", err)
		}
		writeDirs = append(gitWriteDirs, writeDirs...)
		if outerLayer {
			tclaudeLayerContractWriteDirs = append(tclaudeLayerContractWriteDirs, gitWriteDirs...)
		}
	}
	if outerLayer {
		if workspace := canonicalResumeWorkspace(resumeCwd); workspace != "" {
			writeDirs = session.AppendUniqueDir(writeDirs, workspace)
			tclaudeLayerContractWriteDirs = session.AppendUniqueDir(tclaudeLayerContractWriteDirs, workspace)
		}
	}
	if workspaceDenied {
		// A deny covering the workspace masks the harness's ordinary cwd grant,
		// and resumeGitWorktreeWriteDirs yields nothing outside a Git repository.
		// The workspace is an invariant launch requirement, so reopen it for
		// WRITE on every harness — not just Codex — or a resumed agent in a plain
		// directory silently loses write access it had at spawn.
		if workspace := canonicalResumeWorkspace(resumeCwd); workspace != "" {
			writeDirs = session.AppendUniqueDir(writeDirs, workspace)
		}
	}
	// Launch contract: pair explicit read reopens for everything tclaude
	// requires the resumed agent to reach when a deny covers it. Mirrors the
	// spawn path — see session.sandboxLaunchContractReadDirs.
	readDirs = append(readDirs, resumeLaunchContractReadDirs(launchGrants, agentDirPaths, resumeContractReadCandidates(resumeCwd, writeDirs, launchGrants, tmpfsDirs))...)
	// Re-gate on the rules that will actually be emitted: the launch contract
	// above can introduce a reopen beneath a deny that the authored profile did
	// not contain. Mirrors the spawn path.
	renderedGrants := sandboxpolicy.GrantsFromDirs(readDirs, writeDirs, denyDirs)
	// The agent's deny overlay covers a path's whole subtree, so a resume that
	// would reopen one under it (its workspace, Git or agent directories) is
	// refused rather than quietly widened.
	if err := sandboxpolicy.ValidateDenyOverlayHeld(in.denyOverlay, readDirs, writeDirs, in.agentDirPaths, in.tmpfsDirs); err != nil {
		return resumeDirs{}, fmt.Errorf("sandbox_profile_changed: %w", err)
	}
	if !outerLayer && !unconfined {
		if err := harness.ValidateSandboxReopenUnderDeny(h.Name, harnessBuiltinMode, renderedGrants); err != nil {
			return resumeDirs{}, err
		}
	}
	// The mirror-image shape for Copilot's negative-form-less path grants.
	// Mirrors the spawn path.
	// Resolved from the revalidated effective profile's environment — the same
	// entries that build resumeEnv and shellEnvironment, so the directory the
	// gate inspects is the one the resumed pane is actually granted. A profile
	// that relocates TMPDIR moves it.
	if err := harness.ValidateCopilotAddDirGrants(
		h.Name, resumeCwd, session.CopilotLaunchTempDir(resumeEnv),
		readDirs, writeDirs, denyDirs, outerLayer); err != nil {
		return resumeDirs{}, err
	}
	return resumeDirs{
		read: readDirs, write: writeDirs,
		tclaudeLayerContractWrite: tclaudeLayerContractWriteDirs,
	}, nil
}

// resumeDenyOverlay is the resumed snapshot's deny overlay, nil without one.
func resumeDenyOverlay(snapshot *sandboxpolicy.Snapshot) []string {
	if snapshot == nil {
		return nil
	}
	return snapshot.DenyOverlay
}
