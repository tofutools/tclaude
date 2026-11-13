package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// tclaudeLayerLaunchStepInput is what prepareTclaudeLayerLaunch builds the
// outer wall's launch spec from.
type tclaudeLayerLaunchStepInput struct {
	h                      *harness.Harness
	sessionID, cwd, convID string
	outerLayer             bool
	stateRoot              string
	gitWriteDirs           []string
	launchSandbox          *sandboxpolicy.Snapshot
	codexRuntimeDir        *TclaudeLayerPrivateWriteDir
	darwinRouteReservation *DarwinRouteSlotReservation
	routeHelper            *TclaudeLayerRouteHelper
	harnessReadPaths       []string
}

// TclaudeLayerAgentPrivateWriteDirs is the agent's own share of the private
// write dirs behind tclaude's sandbox, whose daemon-owned parents every launch
// hides: the session's attachment directory, whose paths the agent is handed,
// and any morph artifacts directory, which a cross-harness morph left for the
// agent and points it at on every later launch. The attachment directory is
// created when missing; createdAttachmentDir names it then, so a failed launch
// can remove it again while it is still empty. `session new` and the conv
// resume paths both call it, so a resumed pane sees what a fresh one does.
func TclaudeLayerAgentPrivateWriteDirs(sessionID, convID string) (dirs []TclaudeLayerPrivateWriteDir, createdAttachmentDir string, err error) {
	privateAttachmentDir, privateAttachmentDirCreated, prepareErr :=
		common.PrepareSpawnAttachmentsPrivateDir(sessionID)
	if prepareErr != nil {
		return nil, "", fmt.Errorf(
			"prepare tclaude’s sandbox private attachment directory: %w",
			prepareErr,
		)
	}
	if privateAttachmentDirCreated {
		createdAttachmentDir = privateAttachmentDir
	}
	dirs = append(dirs, TclaudeLayerPrivateWriteDir{
		Parent:  common.SpawnAttachmentsPrivateBase(),
		Current: privateAttachmentDir,
	})
	// A cross-harness morph left this agent's translated history there,
	// and the agent is pointed at it on every later launch.
	if convID != "" {
		if owner, err := opAgentIDForConv.call(launchConv(sessionID, convID)); err == nil {
			if dir := common.ExistingMorphArtifactsDir(owner.AgentID); dir != "" {
				dirs = append(dirs, TclaudeLayerPrivateWriteDir{
					Parent: common.MorphArtifactsBase(), Current: dir,
				})
			}
		} else if launchReadsLeaveThisProcess() {
			slog.Warn("could not resolve the agent of this conversation; its morph artifacts directory is not granted",
				"session_id", sessionID, "conv", convID, "error", err)
		}
	}
	return dirs, createdAttachmentDir, nil
}

// prepareTclaudeLayerLaunch is runNew's outer-wall step. Under tclaude's
// sandbox it collects the private write dirs (the Codex runtime dir, the
// session's attachment dir, whose creation undo removes again while empty,
// and any morph artifacts dir) and, when the layer wraps this harness's pane,
// builds the launch spec and prepares its harness state. Any other launch
// returns the zero spec.
func prepareTclaudeLayerLaunch(in tclaudeLayerLaunchStepInput, undo *launchundo.Undo) (TclaudeLayerLaunchSpec, error) {
	var privateWriteDirs []TclaudeLayerPrivateWriteDir
	if in.outerLayer {
		if in.codexRuntimeDir != nil {
			privateWriteDirs = append(privateWriteDirs, *in.codexRuntimeDir)
		}
		agentDirs, createdAttachmentDir, prepareErr := TclaudeLayerAgentPrivateWriteDirs(in.sessionID, in.convID)
		if prepareErr != nil {
			return TclaudeLayerLaunchSpec{}, prepareErr
		}
		if createdAttachmentDir != "" {
			// Never recursively delete a root that gained an upload while
			// launch was in flight; os.Remove succeeds only while empty.
			undo.Add(func() { _ = os.Remove(createdAttachmentDir) })
		}
		privateWriteDirs = append(privateWriteDirs, agentDirs...)
	}
	var layerSpec TclaudeLayerLaunchSpec
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		spec, specErr := PrepareTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
			HarnessName:      in.h.Name,
			StateRoot:        in.stateRoot,
			Cwd:              in.cwd,
			GitWriteDirs:     in.gitWriteDirs,
			Snapshot:         in.launchSandbox,
			PrivateWriteDirs: privateWriteDirs,
			DarwinRouteSlots: func() []int {
				if in.darwinRouteReservation == nil {
					return nil
				}
				return in.darwinRouteReservation.Slots()
			}(),
			DarwinRouteReservation: in.darwinRouteReservation,
			RouteHelper:            in.routeHelper,
			HarnessReadPaths:       in.harnessReadPaths,
		})
		if specErr != nil {
			return TclaudeLayerLaunchSpec{}, specErr
		}
		layerSpec = spec
	}
	return layerSpec, nil
}

// PrepareTclaudeLayerLaunchSpec is the launch-spec step `session new` and the
// conv resume share: it builds the frozen spec for tclaude's sandbox, then
// materializes the harness state and protected mountpoints that spec names.
// bwrap runs over a read-only root, so a hide or state target missing from the
// host cannot be created at mount time; it has to exist before the pane starts.
func PrepareTclaudeLayerLaunchSpec(input TclaudeLayerLaunchInput) (TclaudeLayerLaunchSpec, error) {
	spec, err := BuildTclaudeLayerLaunchSpec(input)
	if err != nil {
		return TclaudeLayerLaunchSpec{}, fmt.Errorf("build tclaude’s sandbox launch spec: %w", err)
	}
	if err := PrepareTclaudeLayerHarnessState(spec); err != nil {
		return TclaudeLayerLaunchSpec{}, fmt.Errorf("prepare tclaude’s sandbox launch state: %w", err)
	}
	return spec, nil
}

// launchPaneCommandInput is what assembleLaunchPaneCommand turns into the
// pane's shell command.
type launchPaneCommandInput struct {
	h                              *harness.Harness
	params                         *NewParams
	spawnSpec                      harness.SpawnSpec
	layerSpec                      *TclaudeLayerLaunchSpec
	sessionID, cwd, exitGeneration string
	outerLayer                     bool
	sandboxImplementation          sandboxpolicy.Implementation
	effectiveSandbox               *sandboxpolicy.Snapshot
	bwrapBinary                    string
	harnessReadPaths               []string
	additionalEnv                  map[string]string
	timing                         func(string, ...any)
}

// assembleLaunchPaneCommand is runNew's pane-command step. It prepares the
// harness's host-control sandbox, checks the Codex app-server version,
// records the model-gateway choice, renders the proxy spawn command (wrapped
// in tclaude's sandbox when the layer wraps the pane) and encodes the launch's
// execution boundary. A boundary that cannot be built or encoded is logged and
// returned as nil, never a launch failure.
func assembleLaunchPaneCommand(in launchPaneCommandInput) (string, []byte, error) {
	spawnSpec := in.spawnSpec
	var err error
	spawnSpec, err = in.h.PrepareHostControlSandboxLaunch(spawnSpec)
	if err != nil {
		return "", nil, fmt.Errorf("prepare %s host-control sandbox: %w", in.h.DisplayName, err)
	}
	versionProbeExecutable := spawnSpec.ExecutablePath
	var versionEnvironment []sandboxpolicy.EnvironmentEntry
	if in.effectiveSandbox != nil {
		versionEnvironment = sandboxpolicy.EnvironmentForLaunch(in.effectiveSandbox)
	}
	in.timing("sandbox_and_command_prepared")
	if err := verifyCodexAppServerLaunchVersion(
		in.params, in.sessionID, versionProbeExecutable, in.cwd, versionEnvironment); err != nil {
		return "", nil, err
	}
	in.timing("codex_version_checked")
	paneCommand := HarnessPaneCommandInput{
		SessionID: in.sessionID, Harness: in.h, Spec: spawnSpec,
	}
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		paneCommand.LayerBinary = in.bwrapBinary
		paneCommand.LayerSpec = in.layerSpec
		paneCommand.LoopbackBindPort = codexAppServerLoopbackPort(in.params)
	}
	harnessCmd, err := HarnessPaneCommand(paneCommand)
	if err != nil {
		return "", nil, err
	}
	if _, err := opRecordModelProxy.call(launchString(in.sessionID, in.params.ModelProxy)); err != nil {
		return "", nil, fmt.Errorf("record model gateway launch choice: %w", err)
	}
	boundaryInput := ExecutionBoundaryInput{
		LaunchGeneration:      in.exitGeneration,
		SandboxImplementation: string(in.sandboxImplementation),
		HarnessName:           in.h.Name,
		HarnessLookupName:     in.h.Spawn.Binary(),
		HarnessExecutable:     spawnSpec.ExecutablePath,
		HarnessRuntimeRoots:   in.harnessReadPaths,
		Cwd:                   in.cwd,
		Environment:           in.additionalEnv,
	}
	if in.effectiveSandbox != nil {
		boundaryInput.PreLaunch = in.effectiveSandbox.Effective.PreLaunch
	}
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		boundaryInput.LauncherBinary = in.bwrapBinary
		boundaryInput.LauncherBinaryResolved = true
		boundaryInput.LayerSpec = in.layerSpec
	}
	var executionBoundaryJSON []byte
	// OpenCode's authoritative tool-executing boundary belongs to its managed
	// server, not this attach-only pane. agentd records that server boundary
	// after both server and pane are live.
	if in.h.Name != harness.OpenCodeName {
		executionBoundary, boundaryErr := BuildExecutionBoundary(boundaryInput)
		if boundaryErr != nil {
			// Debug evidence must never become a launch prerequisite. A missing
			// executable or other observation race is represented by the absent
			// record and logged for the operator.
			slog.Warn("could not record launch execution boundary",
				"session_id", in.sessionID, "error", boundaryErr)
		} else {
			executionBoundaryJSON, err = json.Marshal(executionBoundary)
			if err != nil {
				slog.Warn("could not encode launch execution boundary",
					"session_id", in.sessionID, "error", err)
				executionBoundaryJSON = nil
			}
		}
	}
	return harnessCmd, executionBoundaryJSON, nil
}

// HarnessPaneCommandInput is what HarnessPaneCommand turns into the pane's
// shell command.
type HarnessPaneCommandInput struct {
	SessionID string
	Harness   *harness.Harness
	// Spec is the spawn spec with its host-control sandbox already prepared.
	Spec harness.SpawnSpec
	// LayerSpec, when set, wraps the command in tclaude's sandbox, started by
	// LayerBinary. LoopbackBindPort is the one loopback port the wrapped
	// harness may listen on (a Codex app-server's), 0 for none.
	LayerBinary      string
	LayerSpec        *TclaudeLayerLaunchSpec
	LoopbackBindPort int
}

// HarnessPaneCommand is the pane-command step `session new` and the conv
// resume share. It renders the harness's model-gateway spawn command, through
// the tclaude CLI that tclaude's sandbox can reach when the pane is wrapped,
// and wraps it in that sandbox.
func HarnessPaneCommand(in HarnessPaneCommandInput) (string, error) {
	gatewayCLIPath := clcommon.SelfTclaudePath()
	if in.LayerSpec != nil {
		var err error
		gatewayCLIPath, err = HTTPProxyCLIForLayerSpec(in.LayerSpec)
		if err != nil {
			return "", fmt.Errorf("resolve HTTP proxy launch CLI: %w", err)
		}
	}
	cmd := HTTPProxySpawnCommand(in.SessionID, in.Harness, in.Spec, gatewayCLIPath)
	if in.LayerSpec == nil {
		return cmd, nil
	}
	cmd, err := WrapTclaudeLayerSpecWithLoopbackBind(
		in.LayerBinary, *in.LayerSpec, in.LoopbackBindPort, cmd)
	if err != nil {
		return "", fmt.Errorf("wrap harness with tclaude’s sandbox: %w", err)
	}
	return cmd, nil
}
