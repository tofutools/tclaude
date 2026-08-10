package agentd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

var codexNativePaneStarts = func() ([]byte, error) {
	return clcommon.TmuxCommand("list-panes", "-a", "-F",
		"#{session_name}\t#{pane_dead}\t#{pane_start_command}").Output()
}

var registerCodexNativePermissionProfilesIfInstalled = session.RegisterCodexNativePermissionProfilesIfInstalled
var codexNativeConvOnline = isConvOnline

func init() {
	session.SetCodexNativeRegistryBeforeOrdinaryPublish(adoptLiveCodexProfilesIntoInstalledRegistry)
}

// adoptLiveCodexProfilesIntoInstalledRegistry closes the first-activation
// global-requirements hazard. Before publishing the first mandatory API
// profile, import every ordinary generated profile proven by a live Codex pane
// so the new global requirements.toml cannot block processes already running.
func adoptLiveCodexProfilesIntoInstalledRegistry() error {
	raw, err := codexNativePaneStarts()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil
		}
		return err
	}
	commands := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		name, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		dead, command, ok := strings.Cut(rest, "\t")
		if ok && dead == "0" {
			commands[name] = command
		}
	}
	dir, err := harness.CodexConfigDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing, err := db.ListCodexNativePermissionProfiles()
	if err != nil {
		return err
	}
	registeredNames := make(map[string]db.CodexNativePermissionProfile, len(existing))
	for _, profile := range existing {
		registeredNames[profile.ProfileName] = profile
	}
	queuedNames := map[string]bool{}
	var registrations []session.CodexNativePermissionProfileRegistration
	queue := func(profile db.CodexNativePermissionProfile, path string) {
		prior, alreadyRegistered := registeredNames[profile.ProfileName]
		if (alreadyRegistered && !prior.CleanupPending) || queuedNames[profile.ProfileName] {
			return
		}
		if alreadyRegistered {
			profile.Generation = prior.Generation
		}
		queuedNames[profile.ProfileName] = true
		registrations = append(registrations, session.CodexNativePermissionProfileRegistration{
			Profile: profile, ProfilePath: path,
		})
	}
	states, err := session.ListSessionStates()
	if err != nil {
		return err
	}
	for _, state := range states {
		command := commands[state.TmuxSession]
		if state.Harness != harness.CodexName || command == "" {
			continue
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() || !harness.IsCodexAgentLaunchProfilePath(path) ||
				!codexApprovalProfileOwnedByLivePane(path, []string{command}) {
				continue
			}
			profileName := strings.TrimSuffix(entry.Name(), ".config.toml")
			agentID, resolveErr := db.AgentIDForConv(state.ConvID)
			if resolveErr != nil {
				return resolveErr
			}
			queue(db.CodexNativePermissionProfile{
				Generation: "adopted:" + profileName, ProfileName: profileName, OwnerAgentID: agentID,
				OwnerConvID: state.ConvID, LaunchID: state.ID, LaunchReady: true,
			}, path)
		}
	}
	// A new pane can become visible before session-new has committed its row.
	// The generated profile marker in pane_start_command is itself the ownership
	// proof used by the approval monitor, so adopt that narrow launch now and let
	// SaveSession/SetSessionConvID bind its conversation and stable actor later.
	for tmuxSession, command := range commands {
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() || !harness.IsCodexAgentLaunchProfilePath(path) ||
				!codexApprovalProfileOwnedByLivePane(path, []string{command}) {
				continue
			}
			profileName := strings.TrimSuffix(entry.Name(), ".config.toml")
			queue(db.CodexNativePermissionProfile{
				Generation: "adopted:" + profileName, ProfileName: profileName,
				LaunchID: tmuxSession, LaunchReady: true,
			}, path)
		}
	}
	_, err = registerCodexNativePermissionProfilesIfInstalled(registrations)
	if err != nil {
		return fmt.Errorf("register live generated Codex profiles: %w", err)
	}
	return nil
}

// cleanupRetiredCodexNativeProfiles removes only profiles owned by a retired,
// offline actor. Callers may invoke it after any lifecycle observation; the
// two guards make it safe for online retirement and concurrent reinstate.
func cleanupRetiredCodexNativeProfiles(convID string) {
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	cleanupRetiredCodexNativeProfilesUnderLaunchLock(convID)
}

func cleanupRetiredCodexNativeProfilesUnderLaunchLock(convID string) {
	if codexNativeConvOnline(convID) {
		return
	}
	actor, err := db.GetAgentByConv(convID)
	if err != nil || actor == nil || actor.Active() {
		return
	}
	generations, err := db.ListCodexNativePermissionProfileGenerationsForAgent(actor.AgentID)
	if err == nil {
		err = session.CleanupCodexNativePermissionProfiles(generations)
	}
	if err != nil {
		slog.Warn("retired Codex native profile cleanup pending", "conv", convID, "error", err)
	}
}
