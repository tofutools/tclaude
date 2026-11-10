package session

import (
	"fmt"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// codexAgentProfile is the tclaude-managed Codex permission profile a launch
// runs under: its name and file, the strict-Home split capability it was
// rendered for, the native permission generation it registered (activated
// only at the launch's final commit), and the app-server profile overrides.
type codexAgentProfile struct {
	name               string
	path               string
	splitCapability    *harness.CodexSplitPolicyCapability
	nativeGeneration   string
	appServerOverrides []string
}

// prepareCodexAgentProfile ensures the managed profile file exists before
// launch (self-healing —
// works even if `tclaude setup` was never run). This lives after cwd
// resolution so the profile can add a narrow write grant for the launch
// repo's Git common dir, which linked worktrees need for `git commit`.
// Only the tclaude-owned profile is auto-created; any other name must
// already be defined by the user's own config.
//
// The profile file and the native generation register on undo as soon as
// they exist, so a later launch failure removes them.
func prepareCodexAgentProfile(
	params *NewParams,
	cwd, fullConvID, sessionID, exitGeneration string,
	outerLayer bool,
	launchSandbox *sandboxpolicy.Snapshot,
	undo *launchundo.Undo,
) (codexAgentProfile, error) {
	var profile codexAgentProfile
	profileName, profilePath, splitCapability, err := ensureCodexManagedProfileWithSnapshot(params, cwd, exitGeneration, launchSandbox)
	if err != nil {
		return codexAgentProfile{}, err
	}
	profile.name = profileName
	profile.path = profilePath
	undo.Add(func() { _ = os.Remove(profilePath) })
	profile.splitCapability = splitCapability
	ownerConvID := fullConvID
	if params.SessionID != "" && ownerConvID == "" {
		ownerConvID = params.SessionID
	}
	mandatoryNative := params.CodexAppServerSocket != "" && !outerLayer
	if mandatoryNative {
		if err := RegisterCodexNativePermissionProfile(
			params.CodexAppServerGeneration, profileName, profilePath,
		); err != nil {
			return codexAgentProfile{}, fmt.Errorf("register managed Codex app-server profile: %w", err)
		}
	} else {
		generation, err := RegisterCodexLaunchNativeProfile(exitGeneration, profileName, profilePath, ownerConvID, sessionID)
		if err != nil {
			return codexAgentProfile{}, err
		}
		if generation != "" {
			undo.Add(func() { _ = UnregisterCodexNativePermissionProfile(generation) })
			profile.nativeGeneration = generation
		}
	}
	if params.CodexAppServerSocket != "" {
		if outerLayer {
			profile.appServerOverrides, err = harness.CodexAppServerProfileOverrides(profilePath)
		} else {
			profile.appServerOverrides, err = harness.CodexAppServerRegisteredProfileOverrides(profilePath)
		}
		if err != nil {
			return codexAgentProfile{}, fmt.Errorf("prepare managed Codex app-server profile: %w", err)
		}
	}
	return profile, nil
}
