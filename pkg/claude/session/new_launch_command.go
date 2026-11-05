package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
		privateAttachmentDir, privateAttachmentDirCreated, prepareErr :=
			common.PrepareSpawnAttachmentsPrivateDir(in.sessionID)
		if prepareErr != nil {
			return TclaudeLayerLaunchSpec{}, fmt.Errorf(
				"prepare tclaude’s sandbox private attachment directory: %w",
				prepareErr,
			)
		}
		if privateAttachmentDirCreated {
			// Never recursively delete a root that gained an upload while
			// launch was in flight; os.Remove succeeds only while empty.
			undo.Add(func() { _ = os.Remove(privateAttachmentDir) })
		}
		privateWriteDirs = append(privateWriteDirs, TclaudeLayerPrivateWriteDir{
			Parent:  common.SpawnAttachmentsPrivateBase(),
			Current: privateAttachmentDir,
		})
		// A cross-harness morph left this agent's translated history there,
		// and the agent is pointed at it on every later launch.
		if in.convID != "" {
			if agentID, err := db.AgentIDForConv(in.convID); err == nil {
				if dir := common.ExistingMorphArtifactsDir(agentID); dir != "" {
					privateWriteDirs = append(privateWriteDirs, TclaudeLayerPrivateWriteDir{
						Parent: common.MorphArtifactsBase(), Current: dir,
					})
				}
			}
		}
	}
	var layerSpec TclaudeLayerLaunchSpec
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		spec, specErr := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
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
			return TclaudeLayerLaunchSpec{}, fmt.Errorf("build tclaude’s sandbox launch spec: %w", specErr)
		}
		layerSpec = spec
		if prepareErr := PrepareTclaudeLayerHarnessState(layerSpec); prepareErr != nil {
			return TclaudeLayerLaunchSpec{}, fmt.Errorf("prepare tclaude’s sandbox launch state: %w", prepareErr)
		}
	}
	return layerSpec, nil
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
		in.params, versionProbeExecutable, in.cwd, versionEnvironment); err != nil {
		return "", nil, err
	}
	in.timing("codex_version_checked")
	gatewayCLIPath := clcommon.SelfTclaudePath()
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		gatewayCLIPath, err = HTTPProxyCLIForLayerSpec(in.layerSpec)
		if err != nil {
			return "", nil, fmt.Errorf("resolve HTTP proxy launch CLI: %w", err)
		}
	}
	if err := db.RecordSessionModelProxy(in.sessionID, in.params.ModelProxy); err != nil {
		return "", nil, fmt.Errorf("record model gateway launch choice: %w", err)
	}
	harnessCmd := HTTPProxySpawnCommand(in.sessionID, in.h, spawnSpec, gatewayCLIPath)
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		harnessCmd, err = WrapTclaudeLayerSpecWithLoopbackBind(
			in.bwrapBinary, *in.layerSpec, codexAppServerLoopbackPort(in.params), harnessCmd)
		if err != nil {
			return "", nil, fmt.Errorf("wrap harness with tclaude’s sandbox: %w", err)
		}
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
