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
	// The registry takes the definition as content, so the same call works
	// whether the registry is updated in this process or by agentd.
	profileTOML, err := os.ReadFile(profilePath)
	if err != nil {
		return codexAgentProfile{}, fmt.Errorf("read generated Codex permission profile: %w", err)
	}
	if mandatoryNative {
		if _, err := opCodexRegisterAppServerProfile.call(launchCodexRuntimeProfileArgs{
			launchSessionArgs: launchSession(sessionID), RuntimeGeneration: params.CodexAppServerGeneration,
			ProfileName: profileName, ProfileTOML: string(profileTOML),
		}); err != nil {
			return codexAgentProfile{}, fmt.Errorf("register managed Codex app-server profile: %w", err)
		}
	} else {
		launch := launchGenerationOf(sessionID, exitGeneration)
		unregister := func() { _, _ = opCodexUnregisterLaunchProfile.call(launch) }
		// A brokered registration can commit in agentd and still fail here,
		// when the answer is lost. Its undo is therefore registered first;
		// unregistering a generation that was never registered is a no-op.
		brokered := LaunchIsBrokered()
		if brokered {
			undo.Add(unregister)
		}
		registered, err := opCodexRegisterLaunchProfile.call(launchCodexProfileArgs{
			launchGenerationArgs: launch, ProfileName: profileName,
			ProfileTOML: string(profileTOML), OwnerConvID: ownerConvID,
		})
		if err != nil {
			return codexAgentProfile{}, err
		}
		if registered.NativeGeneration != "" {
			if !brokered {
				undo.Add(unregister)
			}
			profile.nativeGeneration = registered.NativeGeneration
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
