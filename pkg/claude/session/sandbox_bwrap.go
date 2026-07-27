package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TclaudeLayerLaunchContract carries writable paths required by the launched
// harness itself rather than granted by the operator's sandbox profile.
type TclaudeLayerLaunchContract struct {
	HarnessName       string                           `json:"harness_name"`
	StateRoot         string                           `json:"state_root"`
	StateDirs         []string                         `json:"state_dirs,omitempty"`
	ReadOnlyStateDirs []string                         `json:"read_only_state_dirs,omitempty"`
	Environment       []sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
	FinalHideDirs     []string                         `json:"final_hide_dirs,omitempty"`
	ReadOnlyBinds     []TclaudeLayerReadOnlyBind       `json:"read_only_binds,omitempty"`
	WriteDirs         []string                         `json:"write_dirs"`
	ProfileFilesystem []sandboxpolicy.FilesystemGrant  `json:"profile_filesystem"`
	// omitempty keeps pre-TCL-779 v2 rows byte-compatible for new readers:
	// absent means no private reopen. An older strict reader encountering the
	// field refuses the newer contract instead of silently dropping it.
	PrivateWriteDirs []TclaudeLayerPrivateWriteDir `json:"private_write_dirs,omitempty"`
	// OpenCodeControl is present only in the v4 Unix-relay contract. The
	// pathname is host-side replay authority: the server receives only an
	// already-bound descriptor and never sees this path inside its mount
	// namespace.
	OpenCodeControl *TclaudeLayerOpenCodeControl `json:"opencode_control,omitempty"`
}

type TclaudeLayerOpenCodeControl struct {
	Transport  string `json:"transport"`
	SocketPath string `json:"socket_path"`
}

// TclaudeLayerPrivateWriteDir hides a daemon-owned shared parent and reopens
// only the current session's child. It is applied after policy replay so
// profile grants cannot expose sibling sessions.
type TclaudeLayerPrivateWriteDir struct {
	Parent  string `json:"parent"`
	Current string `json:"current"`
}

// TclaudeLayerReadOnlyBind is a daemon-final source→target bind. Unlike a
// profile read grant it cannot be weakened by a later operator-authored rule.
type TclaudeLayerReadOnlyBind struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// TclaudeLayerLaunchSpec is the complete, versioned input to the outer-layer
// renderer. It is deliberately independent of NewParams and daemon launch
// types so pane-authoritative harnesses and agentd-owned executors can render
// the same boundary.
type TclaudeLayerLaunchSpec struct {
	Version   int                            `json:"version"`
	Effective sandboxpolicy.EffectiveProfile `json:"effective"`
	Contract  TclaudeLayerLaunchContract     `json:"contract"`
}

const (
	TclaudeLayerLaunchSpecVersion       = 3
	TclaudeLayerLegacyLaunchSpecVersion = 2
	TclaudeLayerUnixRelaySpecVersion    = 4
	TclaudeLayerUnixRelayTransport      = "unix-relay"
)

// TclaudeLayerLaunchInput carries the trusted launch identities used to build
// a TclaudeLayerLaunchSpec. GitWriteDirs must already be daemon-pinned for a
// managed launch; direct human launches derive them before this seam.
type TclaudeLayerLaunchInput struct {
	HarnessName      string
	Cwd              string
	GitWriteDirs     []string
	Snapshot         *sandboxpolicy.Snapshot
	PrivateWriteDirs []TclaudeLayerPrivateWriteDir
	StateRoot        string
	StateDirs        []string
	Environment      []sandboxpolicy.EnvironmentEntry
	FinalHideDirs    []string
	ReadOnlyBinds    []TclaudeLayerReadOnlyBind
	OpenCodeControl  *TclaudeLayerOpenCodeControl
}

// BuildTclaudeLayerLaunchSpec freezes the launch-active filesystem rows, then
// constructs the exact launch contract the outer
// renderer consumes. Callers may persist the result and re-render it later
// without consulting launch-time UI or profile-registry state.
func BuildTclaudeLayerLaunchSpec(input TclaudeLayerLaunchInput) (TclaudeLayerLaunchSpec, error) {
	cwd := canonicalSandboxPath(input.Cwd)
	if cwd == "" {
		return TclaudeLayerLaunchSpec{}, fmt.Errorf("tclaude-layer launch cwd %q is not an absolute canonical path", input.Cwd)
	}
	effective := sandboxpolicy.EffectiveProfile{}
	if input.Snapshot != nil {
		effective = input.Snapshot.Effective
		filesystem, err := sandboxpolicy.FilesystemForLaunch(effective)
		if err != nil {
			return TclaudeLayerLaunchSpec{}, fmt.Errorf("freeze tclaude-layer filesystem: %w", err)
		}
		effective.Filesystem = filesystem
	}
	profileFilesystem := append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	gitWriteDirs := append([]string(nil), input.GitWriteDirs...)
	launchContractReadDirs := sandboxLaunchContractReadDirsForEffective(
		effective, append([]string{cwd}, gitWriteDirs...)...)
	launchWriteDirs := append(gitWriteDirs,
		sandboxDirsForEffective(effective, sandboxpolicy.AccessWrite)...)
	launchWriteDirs = appendUniqueDir(launchWriteDirs, cwd)
	launchReadDirs := append(
		sandboxDirsForEffective(effective, sandboxpolicy.AccessRead),
		launchContractReadDirs...,
	)
	launchDenyDirs := sandboxDirsForEffective(effective, sandboxpolicy.AccessDeny)
	var err error
	stateRoot := strings.TrimSpace(input.StateRoot)
	if stateRoot == "" {
		stateRoot, err = tclaudeLayerHarnessStateRoot(input.HarnessName)
		if err != nil {
			return TclaudeLayerLaunchSpec{}, err
		}
	}
	stateRoot, err = canonicalTclaudeLayerStatePath(stateRoot)
	if err != nil {
		return TclaudeLayerLaunchSpec{}, fmt.Errorf(
			"resolve tclaude-layer harness state root: %w", err)
	}
	contractWriteDirs := append(append([]string(nil), gitWriteDirs...), cwd)
	var stateDirs []string
	var readOnlyStateDirs []string
	if input.HarnessName == harness.OpenCodeName {
		if len(input.StateDirs) > 0 {
			stateDirs = append([]string(nil), input.StateDirs...)
		} else {
			stateDirs, err = tclaudeLayerOpenCodeStateDirs()
			if err != nil {
				return TclaudeLayerLaunchSpec{}, err
			}
		}
		contractWriteDirs = append(contractWriteDirs, stateDirs...)
		if len(input.ReadOnlyBinds) == 0 {
			// Legacy v2-compatible shape: ~/.opencode is mutable harness state,
			// while its executable subtree is reopened read-only.
			binState, stateErr := canonicalTclaudeLayerStatePath(
				filepath.Join(stateRoot, "bin"))
			if stateErr != nil {
				return TclaudeLayerLaunchSpec{}, fmt.Errorf(
					"resolve OpenCode executable state: %w", stateErr)
			}
			if !sandboxpolicy.PathContainsOrEqual(stateRoot, binState) {
				return TclaudeLayerLaunchSpec{}, fmt.Errorf(
					"OpenCode executable state %q resolves outside state root %q",
					binState, stateRoot)
			}
			readOnlyStateDirs = []string{binState}
			launchReadDirs = append(launchReadDirs, readOnlyStateDirs...)
		}
		// The executor's tool subprocesses are the managed agent. Keep their
		// authenticated coordination path reachable even when /tmp or an
		// authored Home deny hides the socket's ancestors.
		if socket := agentipc.CanonicalSocketPath(); filepath.IsAbs(socket) {
			launchReadDirs = append(launchReadDirs, socket)
		}
	}
	effective.Filesystem = sandboxpolicy.GrantsFromDirs(
		launchReadDirs, launchWriteDirs, launchDenyDirs)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			contractWriteDirs = append(contractWriteDirs, entry.Value)
		}
	}
	contract := TclaudeLayerLaunchContract{
		HarnessName:       input.HarnessName,
		StateRoot:         stateRoot,
		StateDirs:         stateDirs,
		ReadOnlyStateDirs: readOnlyStateDirs,
		Environment:       append([]sandboxpolicy.EnvironmentEntry(nil), input.Environment...),
		FinalHideDirs:     append([]string(nil), input.FinalHideDirs...),
		ReadOnlyBinds:     append([]TclaudeLayerReadOnlyBind(nil), input.ReadOnlyBinds...),
		WriteDirs:         contractWriteDirs,
		ProfileFilesystem: profileFilesystem,
		OpenCodeControl:   input.OpenCodeControl,
	}
	privateWriteDirs, err := cleanTclaudeLayerPrivateWriteDirs(input.PrivateWriteDirs)
	if err != nil {
		return TclaudeLayerLaunchSpec{}, err
	}
	contract.PrivateWriteDirs = privateWriteDirs
	phase0WriteDirs, err := tclaudeLayerPhase0WriteDirs(contract, effective)
	if err != nil {
		return TclaudeLayerLaunchSpec{}, err
	}
	contract.StateRoot = phase0WriteDirs[0]
	contract.WriteDirs = append([]string(nil), phase0WriteDirs[1:]...)
	version := TclaudeLayerLaunchSpecVersion
	if contract.OpenCodeControl != nil {
		version = TclaudeLayerUnixRelaySpecVersion
	}
	return TclaudeLayerLaunchSpec{
		Version:   version,
		Effective: effective,
		Contract:  contract,
	}, nil
}

// ResolveTclaudeLayer verifies the host capability before a launch is
// committed. Callers record the returned verdict even when verification fails.
func ResolveTclaudeLayer(posture sandboxpolicy.NetworkPosture) (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapBinary(posture)
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(posture), nil
}

// ResolveTclaudeLayerServer verifies the host capability needed by a
// non-interactive server boundary. Unlike ResolveTclaudeLayer, it does not
// require terminal-resize relay support that the server renderer never uses.
func ResolveTclaudeLayerServer(
	posture sandboxpolicy.NetworkPosture,
) (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapServerBinary(posture)
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(posture), nil
}

// TclaudeLayerHostAvailability reports whether THIS HOST can create the
// interactive tclaude-layer boundary: bubblewrap plus its terminal relay on
// Linux, or Seatbelt on macOS. nil means available. The returned error names
// the concrete missing capability. Relay-free server callers use
// TclaudeLayerServerHostAvailability instead.
//
// It shares one predicate with the launch boundary — both call
// resolveBwrapBinary — so a pre-flight answer can never disagree with the
// refusal that actually decides. What differs is only the posture, and
// deliberately: this probes the LEAST demanding posture (host-open), because
// the posture a launch really uses derives from the resolved sandbox profile's
// network_access, and isolated-with-agentd needs strictly more (network + PID
// namespaces). So a nil result here rules out an obviously-doomed launch; it
// never claims an isolated launch will succeed. The posture-exact check at the
// launch boundary remains the authority.
//
// Callers that REFUSE on this answer must call it live rather than caching:
// an operator who has just installed bwrap must not be refused by a stale
// negative. Caching is for disclosure only. See TCL-769.
func TclaudeLayerHostAvailability() error {
	_, err := resolveBwrapBinary(sandboxpolicy.NetworkHostOpen)
	return err
}

// TclaudeLayerServerHostAvailability reports whether this host can create the
// non-interactive server boundary, without imposing interactive relay
// capabilities on a topology that has no terminal.
func TclaudeLayerServerHostAvailability() error {
	_, err := resolveBwrapServerBinary(sandboxpolicy.NetworkHostOpen)
	return err
}

// TclaudeLayerLaunchOSSandbox records the resolved platform/posture boundary.
// Partial host-open implementations stay visibly unverified; a constructed
// isolated root can report the stronger boundary it actually enforces.
func TclaudeLayerLaunchOSSandbox(posture sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	return tclaudeLayerLaunchOSSandbox(posture)
}

// TclaudeLayerLaunchOSSandboxForHarness describes the actual process boundary.
// OpenCode's attach TUI is deliberately outside the wall; its agentd-owned
// server is the process that executes tools and is the component we confine.
func TclaudeLayerLaunchOSSandboxForHarness(
	harnessName string,
	posture sandboxpolicy.NetworkPosture,
) harness.LaunchOSSandbox {
	if harnessName == harness.OpenCodeName {
		return harness.LaunchOSSandbox{
			State: "on",
			// The pane, control-plane and networking caveats live in the badge's
			// partial-fidelity sentence rather than here, so each is stated once
			// (TCL-790).
			Source:     "tclaude-layer (bubblewrap; OpenCode tool-executing server confined)",
			Unverified: true,
		}
	}
	return TclaudeLayerLaunchOSSandbox(posture)
}

// ValidateTclaudeLayerNetwork refuses an isolated whole-process launch unless
// both the harness descriptor and the operator's resolved profile assert a
// model transport that functions across the selected platform's boundary
// (a network namespace on Linux, Seatbelt network denies on Darwin).
func ValidateTclaudeLayerNetwork(h *harness.Harness, effective sandboxpolicy.EffectiveProfile) error {
	posture, err := sandboxpolicy.NetworkPostureForAccess(effective.NetworkAccess)
	if err != nil {
		return err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd {
		return nil
	}
	if !h.SupportsOfflineModelTransport() {
		return fmt.Errorf(
			"unsupported_sandbox_profile_network: network_access none isolates the whole tclaude-layer process, but harness %q requires hosted model traffic; see docs/sandboxing.md#isolated-with-agentd-network-posture",
			h.Name,
		)
	}
	for _, entry := range effective.Environment {
		if entry.Name == sandboxpolicy.OfflineModelTransportEnv && entry.Value == "1" {
			return nil
		}
	}
	return fmt.Errorf(
		"unsupported_sandbox_profile_network: network_access none requires %s=1 in the resolved sandbox profile, asserting a model transport that functions across the isolated network boundary; see docs/sandboxing.md#isolated-with-agentd-network-posture",
		sandboxpolicy.OfflineModelTransportEnv,
	)
}

// WrapTclaudeLayer renders one effective profile and applies the resulting
// mount plan around the complete harness command.
func WrapTclaudeLayer(
	binary string,
	effective sandboxpolicy.EffectiveProfile,
	contract TclaudeLayerLaunchContract,
	harnessCommand string,
) (string, error) {
	return WrapTclaudeLayerSpec(binary, TclaudeLayerLaunchSpec{
		Version:   TclaudeLayerLaunchSpecVersion,
		Effective: effective,
		Contract:  contract,
	}, harnessCommand)
}

// WrapTclaudeLayerSpec renders and applies one materialized launch spec.
func WrapTclaudeLayerSpec(
	binary string,
	spec TclaudeLayerLaunchSpec,
	harnessCommand string,
) (string, error) {
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, plan, err :=
		tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return "", err
	}
	return tclaudeLayerCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		plan,
		harnessCommand,
	)
}

func cleanTclaudeLayerPrivateWriteDirs(
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
) ([]TclaudeLayerPrivateWriteDir, error) {
	if len(privateWriteDirs) == 0 {
		return nil, nil
	}
	out := make([]TclaudeLayerPrivateWriteDir, 0, len(privateWriteDirs))
	for i, privateDir := range privateWriteDirs {
		parent := filepath.Clean(strings.TrimSpace(privateDir.Parent))
		current := filepath.Clean(strings.TrimSpace(privateDir.Current))
		if parent == "." || !filepath.IsAbs(parent) {
			return nil, fmt.Errorf(
				"private write entry %d has non-absolute parent %q",
				i,
				parent,
			)
		}
		if current == "." || !filepath.IsAbs(current) {
			return nil, fmt.Errorf(
				"private write entry %d has non-absolute current path %q",
				i,
				current,
			)
		}
		relative, err := filepath.Rel(parent, current)
		if err != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			filepath.Dir(relative) != "." {
			return nil, fmt.Errorf(
				"private write entry %d current path %q must be a direct child of parent %q",
				i,
				current,
				parent,
			)
		}
		for _, item := range []struct {
			label string
			path  string
		}{
			{label: "parent", path: parent},
			{label: "current", path: current},
		} {
			label, path := item.label, item.path
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return nil, fmt.Errorf(
					"private write entry %d %s %q: %w",
					i,
					label,
					path,
					statErr,
				)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf(
					"private write entry %d %s %q is not a real directory",
					i,
					label,
					path,
				)
			}
		}
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return nil, fmt.Errorf("resolve private write entry %d parent: %w", i, err)
		}
		resolvedCurrent, err := filepath.EvalSymlinks(current)
		if err != nil {
			return nil, fmt.Errorf("resolve private write entry %d current: %w", i, err)
		}
		resolvedRelative, err := filepath.Rel(resolvedParent, resolvedCurrent)
		if err != nil || filepath.Dir(resolvedRelative) != "." ||
			resolvedRelative == "." || resolvedRelative == ".." {
			return nil, fmt.Errorf(
				"private write entry %d current path resolves outside its direct parent",
				i,
			)
		}
		out = append(out, TclaudeLayerPrivateWriteDir{
			Parent:  parent,
			Current: current,
		})
	}
	return out, nil
}

// WrapTclaudeLayerStackedSpec renders the exact outer launch boundary and
// carries a launch-owned nested-engine proof into the Linux relay. The relay
// revalidates the staged bytes into a sealed memfd immediately before
// bubblewrap, binds that immutable descriptor at the command's fixed
// executable path, and optionally consumes the staging names for the final
// launch.
func WrapTclaudeLayerStackedSpec(
	binary string,
	spec TclaudeLayerLaunchSpec,
	manifestPath, manifestSHA256, readyPath string,
	consume bool,
	harnessCommand string,
) (string, error) {
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, plan, err :=
		tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return "", err
	}
	return tclaudeLayerStackedCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		plan,
		manifestPath,
		manifestSHA256,
		readyPath,
		consume,
		harnessCommand,
	)
}

// WrapTclaudeLayerServerSpec renders a materialized launch spec for a
// non-interactive, agentd-owned server. Unlike the pane renderer it does not
// add the terminal WINCH relay: the server has no terminal to resize, and
// keeping that extra tclaude process out of its ancestry makes the persisted
// wrapper PID the actual containment boundary.
func WrapTclaudeLayerServerSpec(
	binary string,
	spec TclaudeLayerLaunchSpec,
	serverCommand string,
) (string, error) {
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, plan, err :=
		tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return "", err
	}
	return tclaudeLayerServerCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		plan,
		serverCommand,
	)
}

// TclaudeLayerUnixRelayServerExecArgs renders the v4 server boundary as argv
// so agentd can pass an already-bound Unix listener and the tclaude relay
// executable without either authority being reopened by pathname inside the
// sandbox. preserveFDs is the exact count inherited above stderr.
func TclaudeLayerUnixRelayServerExecArgs(
	binary string,
	spec TclaudeLayerLaunchSpec,
	preserveFDs int,
	serverArgv []string,
) ([]string, error) {
	if spec.Version != TclaudeLayerUnixRelaySpecVersion {
		return nil, fmt.Errorf("unix-relay server renderer requires tclaude-layer v4")
	}
	if preserveFDs != 2 || len(serverArgv) == 0 {
		return nil, fmt.Errorf("unix-relay server renderer requires inherited descriptors and a command")
	}
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, plan, err :=
		tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return nil, err
	}
	opaqueStateRoot := ""
	var opaqueStateDirs []string
	if spec.Contract.OpenCodeControl != nil &&
		filepath.Dir(spec.Contract.OpenCodeControl.SocketPath) ==
			filepath.Clean(spec.Contract.StateRoot) {
		opaqueStateRoot = spec.Contract.StateRoot
		opaqueStateDirs = spec.Contract.StateDirs
	}
	args, err := bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, privateWriteDirs, finalHideDirs, readOnlyBinds,
		opaqueStateRoot, opaqueStateDirs)
	if err != nil {
		return nil, err
	}
	// Upstream bubblewrap deliberately passes non-CLOEXEC descriptors to the
	// sandbox command; it has no --preserve-fds option. ExtraFiles supplies
	// exactly fd 3 (listener) and fd 4 (relay executable).
	args = append(args, "--")
	args = append(args, serverArgv...)
	return append([]string{binary}, args...), nil
}

func tclaudeLayerSpecRenderInput(
	spec TclaudeLayerLaunchSpec,
) (
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	sandboxpolicy.MountPlan,
	error,
) {
	if !supportedTclaudeLayerLaunchSpecVersion(spec.Version) {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("unsupported tclaude-layer launch spec version %d", spec.Version)
	}
	if err := validateTclaudeLayerOpenCodeControl(spec); err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	plan, err := sandboxpolicy.RenderMountPlan(spec.Effective)
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("render mount plan: %w", err)
	}
	phase0WriteDirs, err := tclaudeLayerPhase0WriteDirs(spec.Contract, spec.Effective)
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	stateRoots := append([]string{phase0WriteDirs[0]}, spec.Contract.StateDirs...)
	stateRoots = append(stateRoots, spec.Contract.ReadOnlyStateDirs...)
	for _, stateRoot := range stateRoots {
		if err := validateTclaudeLayerHarnessStateRules(
			stateRoot,
			spec.Contract.ProfileFilesystem,
		); err != nil {
			return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
		}
	}
	privateWriteDirs, err := cleanTclaudeLayerPrivateWriteDirs(
		spec.Contract.PrivateWriteDirs,
	)
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	finalHideDirs, err := cleanTclaudeLayerAbsoluteDirs(
		"daemon-final hide", spec.Contract.FinalHideDirs)
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	readOnlyBinds, err := cleanTclaudeLayerReadOnlyBinds(spec.Contract.ReadOnlyBinds)
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("resolve protected paths for daemon-final read-only binds: %w", err)
	}
	for _, bind := range readOnlyBinds {
		for _, path := range []string{bind.Source, bind.Target} {
			for _, protected := range protectedRoots {
				if sandboxpolicy.PathContainsOrEqual(protected, path) {
					return nil, nil, nil, nil, sandboxpolicy.MountPlan{},
						fmt.Errorf(
							"daemon-final read-only bind path %q is at or below protected root %q",
							path, protected)
				}
			}
			if err := validateTclaudeLayerHarnessStateRules(
				path, spec.Contract.ProfileFilesystem); err != nil {
				return nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
			}
		}
	}
	return phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, plan, nil
}

func validateTclaudeLayerOpenCodeControl(spec TclaudeLayerLaunchSpec) error {
	control := spec.Contract.OpenCodeControl
	if spec.Version != TclaudeLayerUnixRelaySpecVersion {
		if control != nil {
			return fmt.Errorf(
				"tclaude-layer launch spec version %d unexpectedly carries OpenCode Unix control authority",
				spec.Version)
		}
		return nil
	}
	if spec.Contract.HarnessName != harness.OpenCodeName {
		return fmt.Errorf("tclaude-layer v4 Unix control authority is OpenCode-only")
	}
	if control == nil || control.Transport != TclaudeLayerUnixRelayTransport {
		return fmt.Errorf("tclaude-layer v4 launch spec has no Unix-relay control authority")
	}
	posture, err := sandboxpolicy.NetworkPostureForAccess(spec.Effective.NetworkAccess)
	if err != nil {
		return err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd {
		return fmt.Errorf("tclaude-layer v4 Unix relay requires the isolated network posture")
	}
	rawPath := strings.TrimSpace(control.SocketPath)
	path := filepath.Clean(rawPath)
	if rawPath != control.SocketPath || path != rawPath ||
		!filepath.IsAbs(path) || filepath.Base(path) != "control.sock" {
		return fmt.Errorf("tclaude-layer v4 OpenCode control path %q is invalid", control.SocketPath)
	}
	if len(path) >= 108 {
		return fmt.Errorf("tclaude-layer v4 OpenCode control path exceeds Linux sockaddr capacity")
	}
	parent := filepath.Dir(path)
	agentID := filepath.Base(parent)
	if !strings.HasPrefix(agentID, "agt_") || len(agentID) != len("agt_")+32 {
		return fmt.Errorf("tclaude-layer v4 OpenCode control path is not under a stable agent child")
	}
	for _, r := range strings.TrimPrefix(agentID, "agt_") {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("tclaude-layer v4 OpenCode control path has invalid agent identity")
		}
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect tclaude-layer v4 OpenCode control parent: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("tclaude-layer v4 OpenCode control parent must be a real mode-0700 directory")
	}
	if resolved, err := filepath.EvalSymlinks(parent); err != nil || resolved != parent {
		return fmt.Errorf("tclaude-layer v4 OpenCode control parent is not canonical")
	}
	stateRoot := filepath.Clean(spec.Contract.StateRoot)
	privateAuthority := stateRoot == parent
	legacyAuthority := false
	for _, hidden := range spec.Contract.FinalHideDirs {
		if filepath.Clean(hidden) == filepath.Dir(parent) {
			legacyAuthority = true
			break
		}
	}
	if !privateAuthority && !legacyAuthority {
		return fmt.Errorf(
			"tclaude-layer v4 OpenCode control path is outside its private or legacy authority")
	}
	if privateAuthority && len(spec.Contract.StateDirs) == 0 {
		return fmt.Errorf(
			"tclaude-layer v4 OpenCode private control authority has no state-only reopens")
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return fmt.Errorf("resolve protected paths for OpenCode control authority: %w", err)
	}
	for _, protected := range protectedRoots {
		if sandboxpolicy.PathContainsOrEqual(protected, path) {
			return fmt.Errorf(
				"tclaude-layer v4 OpenCode control path %q is at or below protected root %q",
				path, protected)
		}
	}
	return nil
}

// ValidateTclaudeLayerLaunchSpec applies the renderer's complete structural
// validation without producing a platform command.
func ValidateTclaudeLayerLaunchSpec(spec TclaudeLayerLaunchSpec) error {
	_, _, _, _, _, err := tclaudeLayerSpecRenderInput(spec)
	return err
}

// PrepareTclaudeLayerHarnessState materializes only the harness-owned state
// roots named explicitly by a frozen launch spec. Operator-authored profile
// paths remain non-creating: a future allow path must not appear on the host
// merely because a launch mentioned it.
func PrepareTclaudeLayerHarnessState(spec TclaudeLayerLaunchSpec) error {
	if !supportedTclaudeLayerLaunchSpecVersion(spec.Version) {
		return fmt.Errorf("unsupported tclaude-layer launch spec version %d", spec.Version)
	}
	stateDirs := append([]string{spec.Contract.StateRoot}, spec.Contract.StateDirs...)
	if spec.Contract.HarnessName == harness.OpenCodeName && len(spec.Contract.StateDirs) == 0 {
		return fmt.Errorf("OpenCode tclaude-layer launch spec has no mutable state directories")
	}
	if spec.Contract.HarnessName == harness.OpenCodeName &&
		len(spec.Contract.ReadOnlyStateDirs) == 0 &&
		len(spec.Contract.ReadOnlyBinds) == 0 {
		return fmt.Errorf("OpenCode tclaude-layer launch spec does not protect its executable state")
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return fmt.Errorf("resolve protected paths before preparing harness state: %w", err)
	}
	for index, path := range stateDirs {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return fmt.Errorf("tclaude-layer harness state path %q is not absolute", path)
		}
		if err := validateTclaudeLayerHarnessStateRules(
			path, spec.Contract.ProfileFilesystem); err != nil {
			return err
		}
		for _, protected := range protectedRoots {
			if sandboxpolicy.PathContainsOrEqual(protected, path) {
				return fmt.Errorf(
					"tclaude-layer harness state path %q is at or below protected root %q",
					path, protected)
			}
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare tclaude-layer harness state %q: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect tclaude-layer harness state %q: %w", path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("tclaude-layer harness state path %q is not a directory", path)
		}
		if index > 0 {
			resolved := path
			if evaluated, err := filepath.EvalSymlinks(path); err == nil {
				resolved = filepath.Clean(evaluated)
			}
			inWriteContract := false
			for _, writeDir := range spec.Contract.WriteDirs {
				writeDir = filepath.Clean(writeDir)
				if evaluated, err := filepath.EvalSymlinks(writeDir); err == nil {
					writeDir = filepath.Clean(evaluated)
				}
				if writeDir == resolved {
					inWriteContract = true
					break
				}
			}
			if !inWriteContract {
				return fmt.Errorf(
					"tclaude-layer harness state path %q is not in the writable launch contract",
					path)
			}
		}
	}
	stateRoot := filepath.Clean(spec.Contract.StateRoot)
	for _, path := range spec.Contract.ReadOnlyStateDirs {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return fmt.Errorf("tclaude-layer read-only harness state path %q is not absolute", path)
		}
		if path == stateRoot || !sandboxpolicy.PathContainsOrEqual(stateRoot, path) {
			return fmt.Errorf(
				"tclaude-layer read-only harness state path %q is not below state root %q",
				path, stateRoot)
		}
		if err := validateTclaudeLayerHarnessStateRules(
			path, spec.Contract.ProfileFilesystem); err != nil {
			return err
		}
		for _, protected := range protectedRoots {
			if sandboxpolicy.PathContainsOrEqual(protected, path) {
				return fmt.Errorf(
					"tclaude-layer read-only harness state path %q is at or below protected root %q",
					path, protected)
			}
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare tclaude-layer read-only harness state %q: %w", path, err)
		}
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, path)
		if !covered || access != sandboxpolicy.AccessRead {
			return fmt.Errorf(
				"tclaude-layer read-only harness state path %q is not read-only in the rendered launch contract",
				path)
		}
	}
	if _, err := cleanTclaudeLayerAbsoluteDirs(
		"daemon-final hide", spec.Contract.FinalHideDirs); err != nil {
		return err
	}
	if _, err := cleanTclaudeLayerReadOnlyBinds(spec.Contract.ReadOnlyBinds); err != nil {
		return err
	}
	return nil
}

func supportedTclaudeLayerLaunchSpecVersion(version int) bool {
	return version == TclaudeLayerLaunchSpecVersion ||
		version == TclaudeLayerLegacyLaunchSpecVersion ||
		version == TclaudeLayerUnixRelaySpecVersion
}

func cleanTclaudeLayerAbsoluteDirs(label string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for i, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("%s entry %d has non-absolute path %q", label, i, path)
		}
		out = appendUniqueDir(out, path)
	}
	return out, nil
}

func cleanTclaudeLayerReadOnlyBinds(
	binds []TclaudeLayerReadOnlyBind,
) ([]TclaudeLayerReadOnlyBind, error) {
	out := make([]TclaudeLayerReadOnlyBind, 0, len(binds))
	for i, bind := range binds {
		source := filepath.Clean(strings.TrimSpace(bind.Source))
		target := filepath.Clean(strings.TrimSpace(bind.Target))
		if source == "." || !filepath.IsAbs(source) {
			return nil, fmt.Errorf("daemon-final read-only bind %d has non-absolute source %q", i, source)
		}
		if target == "." || !filepath.IsAbs(target) {
			return nil, fmt.Errorf("daemon-final read-only bind %d has non-absolute target %q", i, target)
		}
		sourceInfo, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("daemon-final read-only bind %d source %q: %w", i, source, err)
		}
		targetInfo, err := os.Lstat(target)
		if err != nil {
			return nil, fmt.Errorf("daemon-final read-only bind %d target %q: %w", i, target, err)
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 || targetInfo.Mode()&os.ModeSymlink != 0 ||
			sourceInfo.IsDir() != targetInfo.IsDir() {
			return nil, fmt.Errorf(
				"daemon-final read-only bind %d source and target must be real paths of the same type", i)
		}
		resolvedSource, err := filepath.EvalSymlinks(source)
		if err != nil {
			return nil, fmt.Errorf("resolve daemon-final read-only bind %d source: %w", i, err)
		}
		resolvedTarget, err := filepath.EvalSymlinks(target)
		if err != nil {
			return nil, fmt.Errorf("resolve daemon-final read-only bind %d target: %w", i, err)
		}
		out = append(out, TclaudeLayerReadOnlyBind{
			Source: resolvedSource,
			Target: resolvedTarget,
		})
	}
	return out, nil
}

// bwrapArgs applies the launch contract, ordered MountPlan, and fixed
// bubblewrap hygiene. Policy/profile interpretation belongs before this
// boundary.
//
// The four precedence classes are established at their normal positions in
// the argument stream, but read-only hardening of class-2 and class-3 hides is
// deferred until plan replay finishes. Bubblewrap creates missing bind
// destinations as it executes filesystem arguments, so remounting an ancestor
// hide immediately would prevent narrower launch-contract and
// most-specific-wins mounts from landing. The deferred tracker records whether
// a hide is still the topmost mount at its exact path; a later exact bind
// cancels its pending remount, while child mounts survive because
// --remount-ro is non-recursive. The isolated posture's constructed tmpfs root
// joins the same flush so unbound paths cannot accept throwaway writes. Class 4
// is materialized and hardened only after that flush, so host control remains
// the final, unshadowable phase.
func bwrapArgs(
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	privateWriteDirs ...TclaudeLayerPrivateWriteDir,
) ([]string, error) {
	return bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, privateWriteDirs, nil, nil, "", nil)
}

func bwrapArgsWithDaemonFinal(
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	opaqueStateRoot string,
	opaqueStateDirs []string,
) ([]string, error) {
	var hideRemounts tclaudeLayerHideRemounts
	args := []string{
		"--die-with-parent",
		// Give the wrapped process a new terminal session so a copied tmux
		// client or TIOCSTI-capable process cannot inject input into the host
		// pane shell outside this namespace.
		"--new-session",
	}
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen:
		// Preserve the walking skeleton: the host namespace and read-only host
		// root stay visible, including ambient pathname sockets.
		args = append(args, "--ro-bind", "/", "/")
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		// Bubblewrap brings loopback up in the newly created namespace. Start
		// from a fresh root so filesystem AF_UNIX sockets are absent unless a
		// later launch-contract or policy bind explicitly exposes them. Keep
		// bubblewrap as PID 1 (do not use --as-pid-1) so it reaps orphaned
		// harness subprocesses and host processes cannot reopen the host mount
		// namespace through /proc/<pid>/root.
		args = append(args, "--unshare-net", "--unshare-pid")
		args = hideRemounts.appendHide(args, "/")
	case sandboxpolicy.NetworkFiltered:
		return nil, fmt.Errorf("network posture %s is reserved and has no tclaude-layer applier", plan.NetworkPosture)
	default:
		return nil, fmt.Errorf("mount plan has invalid network posture %d", plan.NetworkPosture)
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		// Never share the host's scratch directory by default.
		"--tmpfs", "/tmp",
	)
	if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
		var err error
		args, err = appendTclaudeLayerAliases(args, plan.Aliases)
		if err != nil {
			return nil, err
		}
		args, err = appendTclaudeLayerStaticOSRoot(args)
		if err != nil {
			return nil, err
		}
	}
	// Class 1 baseline: the harness process itself runs inside this wall,
	// unlike the harness-native sandboxes which confine only tools. Reopen only its
	// launch-contract state/workspace paths here; protected children are hidden
	// on top next. An ordinary ancestor hide triggers a narrower repair during
	// plan replay.
	existingPhase0WriteDirs := make([]string, 0, len(phase0WriteDirs))
	for i, path := range phase0WriteDirs {
		path = filepath.Clean(path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("launch-contract write entry %d has non-absolute path %q", i, path)
		}
		exists, err := bwrapBindSourceExists(path)
		if err != nil {
			return nil, fmt.Errorf("launch-contract write entry %d source %q: %w", i, path, err)
		}
		if exists {
			args = append(args, "--bind", path, path)
			existingPhase0WriteDirs = append(existingPhase0WriteDirs, path)
		}
	}
	// Class 3 baseline: protected state is hidden after the harness root is
	// writable. EffectiveProfile deliberately excludes this baseline because
	// ordinary rules may never name these paths at all: normalizeFilesystem
	// refuses any read/write rule intersecting a protected root, and TCL-791
	// removed break-glass, the one former exception. No policy input can
	// therefore reopen a protected root: a plan entry on an ordinary ancestor
	// still lands, and appendTclaudeLayerProtectedRehides restores the hides on
	// top of it.
	//
	// The one thing mounted back inside a protected root afterwards is the
	// daemon's own spawn-attachment drop-box (class 4 below). It is not policy
	// input — its path is derived from the session identity, never named by a
	// profile or an agent — and it exposes a single daemon-created directory
	// holding this session's own attachments, not the protected state
	// underneath it.
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve protected sandbox roots: %w", err)
	}
	for _, writeDir := range phase0WriteDirs {
		for _, protected := range protectedRoots {
			if sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				return nil, fmt.Errorf(
					"tclaude-layer launch-contract path %q is at or below protected root %q",
					writeDir,
					protected,
				)
			}
		}
	}
	for _, root := range protectedRoots {
		args = hideRemounts.appendHide(args, root)
	}
	socketPaths := []string(nil)
	if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
		socket := agentipc.CanonicalSocketPath()
		if socket == "" || !filepath.IsAbs(socket) {
			return nil, fmt.Errorf("resolve canonical agentd socket for isolated tclaude-layer")
		}
		exists, err := bwrapBindSourceExists(socket)
		if err != nil {
			return nil, fmt.Errorf("agentd socket source %q: %w", socket, err)
		}
		if !exists {
			return nil, fmt.Errorf("isolated tclaude-layer requires the canonical agentd socket %s", socket)
		}
		args = append(args, "--ro-bind", socket, socket)
		socketPaths = append(socketPaths, socket)
	}
	// Class 2: replay the policy plan exactly as rendered. Repair mounts do not
	// reorder or deduplicate plan entries; they preserve the higher-precedence
	// launch contract after an ordinary ancestor hide.
	for i, entry := range plan.Entries {
		path := filepath.Clean(entry.Path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("mount plan entry %d has non-absolute path %q", i, entry.Path)
		}
		switch entry.Mode {
		case sandboxpolicy.MountRO:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			hideRemounts.noteReplacement(path)
			args = append(args, "--ro-bind", path, path)
			args = appendTclaudeLayerProtectedRehides(
				args,
				path,
				protectedRoots,
				&hideRemounts,
			)
		case sandboxpolicy.MountRW:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			hideRemounts.noteReplacement(path)
			args = append(args, "--bind", path, path)
			args = appendTclaudeLayerProtectedRehides(
				args,
				path,
				protectedRoots,
				&hideRemounts,
			)
		case sandboxpolicy.MountHide:
			args = hideRemounts.appendHide(args, path)
			if plan.NetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
				args = appendTclaudeLayerAliasRepairs(
					args,
					path,
					plan.Aliases,
					protectedRoots,
					&hideRemounts,
				)
			}
			args = appendTclaudeLayerContractRepairs(
				args,
				path,
				existingPhase0WriteDirs,
				protectedRoots,
				&hideRemounts,
			)
			args = appendTclaudeLayerSocketRepairs(
				args,
				path,
				socketPaths,
				&hideRemounts,
			)
		default:
			return nil, fmt.Errorf("mount plan entry %d has invalid mode %d", i, entry.Mode)
		}
	}
	// The attachment parent is daemon-owned shared state. Hide it after all
	// policy mounts, then reopen only this session's direct child. The parent's
	// read-only remount is non-recursive, so the child remains writable while
	// sibling names stay absent.
	//
	// This is the sole bind that lands at or below a protected root, and it is
	// deliberate: attachments have to reach the agent. It is not a policy
	// reopen. The path comes from the session identity via
	// SpawnAttachmentsPrivateDir, so neither a profile nor the agent can steer
	// it, and the class-3 tmpfs still covers everything else under the root —
	// what becomes visible is this session's own attachment area, not protected
	// state. It is daemon-created and session-writable; it holds the promoted
	// attachment batch, never the database or another session's directory.
	for _, privateDir := range privateWriteDirs {
		args = hideRemounts.appendHide(args, privateDir.Parent)
		args = append(args, "--bind", privateDir.Current, privateDir.Current)
		hideRemounts.noteReplacement(privateDir.Current)
	}
	// These invariants are daemon-owned and deliberately land after both
	// profile replay and private-child reopening. A broad profile grant can
	// therefore neither reveal legacy OpenCode roots nor make shared install
	// and global-config trees writable.
	for _, path := range finalHideDirs {
		args = hideRemounts.appendHide(args, path)
	}
	if opaqueStateRoot != "" {
		args, err = appendTclaudeLayerOpaqueState(
			args, opaqueStateRoot, opaqueStateDirs)
		if err != nil {
			return nil, err
		}
	}
	for _, bind := range readOnlyBinds {
		exists, err := bwrapBindSourceExists(bind.Source)
		if err != nil {
			return nil, fmt.Errorf("daemon-final read-only source %q: %w", bind.Source, err)
		}
		if !exists {
			return nil, fmt.Errorf("daemon-final read-only source %q does not exist", bind.Source)
		}
		targetExists, err := bwrapBindSourceExists(bind.Target)
		if err != nil {
			return nil, fmt.Errorf("daemon-final read-only target %q: %w", bind.Target, err)
		}
		if !targetExists {
			return nil, fmt.Errorf("daemon-final read-only target %q does not exist", bind.Target)
		}
		hideRemounts.noteReplacement(bind.Target)
		args = append(args, "--ro-bind", bind.Source, bind.Target)
	}
	// Class 4: host tmux control is never profile-reachable.
	// This hide must be last: unlike ProtectedPaths, an ordinary profile may
	// grant a parent of the socket directory, and a later bind would otherwise
	// reopen the host tmux server.
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return nil, fmt.Errorf("resolve tclaude tmux socket directory: %w", err)
	}
	// If an ordinary ancestor hide is pending, create the class-4 mountpoint
	// while that tmpfs is still mutable. No wrapped process runs until all
	// setup operations finish, and the final tmpfs below still supplies the
	// unshadowable host-control view.
	if hideRemounts.hasActiveAncestor(tmuxSocketDir) {
		args = append(args, "--dir", tmuxSocketDir)
	}
	args = hideRemounts.appendRemounts(args)
	args = append(args,
		"--tmpfs", tmuxSocketDir,
		"--remount-ro", tmuxSocketDir,
	)
	return args, nil
}

func appendTclaudeLayerOpaqueState(
	args []string,
	root string,
	stateDirs []string,
) ([]string, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || len(stateDirs) == 0 {
		return nil, fmt.Errorf("invalid OpenCode v4 opaque state contract")
	}
	cleanDirs := make([]string, len(stateDirs))
	for i, stateDir := range stateDirs {
		stateDir = filepath.Clean(stateDir)
		if !filepath.IsAbs(stateDir) || stateDir == root ||
			!sandboxpolicy.PathContainsOrEqual(root, stateDir) {
			return nil, fmt.Errorf("OpenCode v4 state-only reopen %q is outside %q",
				stateDir, root)
		}
		cleanDirs[i] = stateDir
	}
	// Cover the host agent child, including control.sock, then reopen only the
	// mutable XDG state directories. Bubblewrap resolves bind sources from the
	// host mount namespace before applying the operations, while destinations
	// are resolved in the new namespace; a same-path bind therefore reopens the
	// original host directory beneath the tmpfs without exposing control.sock.
	// The inherited listener fd remains the sole control authority inside the
	// server namespace.
	args = append(args, "--tmpfs", root)
	for _, stateDir := range cleanDirs {
		args = append(args,
			"--dir", filepath.Dir(stateDir),
			"--dir", stateDir,
			"--bind", stateDir, stateDir,
		)
	}
	return args, nil
}

// tclaudeLayerHideRemounts tracks the topmost exact-path operation without
// disturbing MountPlan order. The first sighting fixes deterministic flush
// order; later hide/replacement operations only update which mount currently
// owns that path.
type tclaudeLayerHideRemounts struct {
	order  []string
	active map[string]bool
}

func (r *tclaudeLayerHideRemounts) appendHide(args []string, path string) []string {
	if r.active == nil {
		r.active = make(map[string]bool)
	}
	r.noteAncestorReplacement(path)
	if _, seen := r.active[path]; !seen {
		r.order = append(r.order, path)
	}
	r.active[path] = true
	return append(args, "--tmpfs", path)
}

func (r *tclaudeLayerHideRemounts) noteReplacement(path string) {
	r.noteAncestorReplacement(path)
	if _, tracked := r.active[path]; tracked {
		r.active[path] = false
	}
}

// noteAncestorReplacement deactivates child hides shadowed by a later mount.
// A protected re-hide emitted after the replacement will reactivate its exact
// path; a child left covered only by the new ancestor is not itself a visible
// mountpoint and must not receive --remount-ro at flush time.
func (r *tclaudeLayerHideRemounts) noteAncestorReplacement(path string) {
	for candidate, active := range r.active {
		if active && candidate != path && sandboxpolicy.PathContainsOrEqual(path, candidate) {
			r.active[candidate] = false
		}
	}
}

func (r *tclaudeLayerHideRemounts) hasActiveAncestor(path string) bool {
	for candidate, active := range r.active {
		if active && sandboxpolicy.PathContainsOrEqual(candidate, path) {
			return true
		}
	}
	return false
}

func (r *tclaudeLayerHideRemounts) appendRemounts(args []string) []string {
	for _, path := range r.order {
		if r.active[path] {
			args = append(args, "--remount-ro", path)
		}
	}
	return args
}

var tclaudeLayerStaticOSPaths = []string{
	"/usr",
	"/bin",
	"/sbin",
	"/lib",
	"/lib64",
	"/lib32",
	"/libx32",
	"/etc",
	"/opt",
}

// appendTclaudeLayerStaticOSRoot constructs the fixed executable/runtime
// surface approved for the isolated posture. Merged-usr aliases remain
// symlinks instead of becoming separate recursive host binds.
func appendTclaudeLayerStaticOSRoot(args []string) ([]string, error) {
	for _, path := range tclaudeLayerStaticOSPaths {
		info, err := os.Lstat(path)
		switch {
		case err == nil:
		case os.IsNotExist(err):
			continue
		default:
			return nil, fmt.Errorf("inspect isolated-root path %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil, fmt.Errorf("read isolated-root symlink %q: %w", path, err)
			}
			args = append(args, "--symlink", target, path)
			continue
		}
		args = append(args, "--ro-bind", path, path)
	}
	return args, nil
}

func appendTclaudeLayerAliases(
	args []string,
	aliases []sandboxpolicy.MountAlias,
) ([]string, error) {
	for i, alias := range aliases {
		link := filepath.Clean(alias.Link)
		target := filepath.Clean(alias.Target)
		if link == "." || !filepath.IsAbs(link) || link == string(filepath.Separator) {
			return nil, fmt.Errorf("mount alias %d has invalid link %q", i, alias.Link)
		}
		if target == "." || !filepath.IsAbs(target) {
			return nil, fmt.Errorf("mount alias %d has non-absolute target %q", i, alias.Target)
		}
		if tclaudeLayerStaticOSRootProvides(link) {
			continue
		}
		args = append(args, "--symlink", target, link)
	}
	return args, nil
}

func tclaudeLayerStaticOSRootProvides(path string) bool {
	for _, static := range tclaudeLayerStaticOSPaths {
		if sandboxpolicy.PathContainsOrEqual(static, path) {
			return true
		}
	}
	return false
}

func appendTclaudeLayerAliasRepairs(
	args []string,
	hide string,
	aliases []sandboxpolicy.MountAlias,
	protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, alias := range aliases {
		link := filepath.Clean(alias.Link)
		if hide == link || !sandboxpolicy.PathContainsOrEqual(hide, link) {
			continue
		}
		args = append(args, "--symlink", filepath.Clean(alias.Target), link)
		args = appendTclaudeLayerProtectedRehides(
			args,
			link,
			protectedRoots,
			hideRemounts,
		)
	}
	return args
}

func appendTclaudeLayerSocketRepairs(
	args []string,
	hide string,
	socketPaths []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, socket := range socketPaths {
		if hide != socket && sandboxpolicy.PathContainsOrEqual(hide, socket) {
			hideRemounts.noteReplacement(socket)
			args = append(args, "--ro-bind", socket, socket)
		}
	}
	return args
}

func appendTclaudeLayerProtectedRehides(
	args []string,
	mountedPath string,
	protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	for _, protected := range protectedRoots {
		if sandboxpolicy.PathContainsOrEqual(mountedPath, protected) ||
			sandboxpolicy.PathContainsOrEqual(protected, mountedPath) {
			args = hideRemounts.appendHide(args, protected)
		}
	}
	return args
}

func appendTclaudeLayerContractRepairs(
	args []string,
	hide string,
	existingWriteDirs, protectedRoots []string,
	hideRemounts *tclaudeLayerHideRemounts,
) []string {
	repaired := make([]string, 0, len(existingWriteDirs))
	for _, writeDir := range existingWriteDirs {
		// An exact or narrower policy row wins. The launch seam refuses an
		// ordinary profile row at/below the harness state root separately;
		// workspace and agent-directory rows retain normal plan precedence.
		if hide != writeDir && sandboxpolicy.PathContainsOrEqual(hide, writeDir) {
			hideRemounts.noteReplacement(writeDir)
			args = append(args, "--bind", writeDir, writeDir)
			repaired = append(repaired, writeDir)
		}
	}
	// A repaired parent bind would cover an earlier protected child hide.
	// Restore every overlapping protected root after all repairs so protected
	// state still outranks launch-contract authority. Since TCL-791 these hides
	// are final: no later plan entry can reopen a protected path.
	for _, protected := range protectedRoots {
		for _, writeDir := range repaired {
			if sandboxpolicy.PathContainsOrEqual(writeDir, protected) ||
				sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				args = hideRemounts.appendHide(args, protected)
				break
			}
		}
	}
	return args
}

func validateTclaudeLayerHarnessStateRules(
	stateRoot string,
	profileFilesystem []sandboxpolicy.FilesystemGrant,
) error {
	for _, grant := range profileFilesystem {
		if sandboxpolicy.PathContainsOrEqual(stateRoot, grant.Path) {
			return fmt.Errorf(
				"tclaude-layer profile filesystem rule %q (%s) is at or below harness state root %q; refusing a launch that cannot persist harness state",
				grant.Path,
				grant.Access,
				stateRoot,
			)
		}
	}
	return nil
}

func tclaudeLayerPhase0WriteDirs(
	contract TclaudeLayerLaunchContract,
	effective sandboxpolicy.EffectiveProfile,
) ([]string, error) {
	stateRoot := strings.TrimSpace(contract.StateRoot)
	if stateRoot == "" {
		var err error
		stateRoot, err = tclaudeLayerHarnessStateRoot(contract.HarnessName)
		if err != nil {
			return nil, err
		}
	}
	candidates := append([]string{stateRoot}, contract.WriteDirs...)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			candidates = append(candidates, entry.Value)
		}
	}
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("tclaude-layer launch-contract path %q is not absolute", path)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = filepath.Clean(resolved)
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out, nil
}

func tclaudeLayerHarnessStateRoot(harnessName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for harness state: %w", err)
	}
	switch harnessName {
	case harness.DefaultName:
		return filepath.Join(home, ".claude"), nil
	case harness.CodexName:
		if root := strings.TrimSpace(os.Getenv("CODEX_HOME")); root != "" {
			return root, nil
		}
		return filepath.Join(home, ".codex"), nil
	case harness.OpenCodeName:
		return filepath.Join(home, ".opencode"), nil
	default:
		return "", fmt.Errorf("tclaude-layer has no launch-contract state root for harness %q", harnessName)
	}
}

func tclaudeLayerOpenCodeStateDirs() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for OpenCode state: %w", err)
	}
	xdgRoot := func(envName string, fallback ...string) string {
		if root := strings.TrimSpace(os.Getenv(envName)); root != "" {
			return root
		}
		return filepath.Join(append([]string{home}, fallback...)...)
	}
	dirs := []string{
		filepath.Join(xdgRoot("XDG_DATA_HOME", ".local", "share"), "opencode"),
		filepath.Join(xdgRoot("XDG_CACHE_HOME", ".cache"), "opencode"),
		filepath.Join(xdgRoot("XDG_CONFIG_HOME", ".config"), "opencode"),
		filepath.Join(xdgRoot("XDG_STATE_HOME", ".local", "state"), "opencode"),
	}
	for index, path := range dirs {
		path, err = canonicalTclaudeLayerStatePath(path)
		if err != nil {
			return nil, fmt.Errorf("resolve OpenCode state directory %q: %w", dirs[index], err)
		}
		dirs[index] = path
	}
	return dirs, nil
}

func canonicalTclaudeLayerStatePath(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	ancestor := path
	var suffix []string
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("existing ancestor %q is not a directory", ancestor)
			}
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

func sandboxDirsForEffective(
	effective sandboxpolicy.EffectiveProfile,
	access sandboxpolicy.Access,
) []string {
	out := make([]string, 0, len(effective.Filesystem))
	for _, grant := range effective.Filesystem {
		if grant.Access == access {
			out = append(out, grant.Path)
		}
	}
	return out
}

func sandboxDenyCoversEffectivePath(
	effective sandboxpolicy.EffectiveProfile,
	path string,
) bool {
	path = canonicalSandboxPath(path)
	if path == "" {
		return false
	}
	for _, grant := range effective.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny || grant.Path == path {
			continue
		}
		if sandboxpolicy.PathContainsOrEqual(grant.Path, path) {
			return true
		}
	}
	return false
}

func sandboxLaunchContractReadDirsForEffective(
	effective sandboxpolicy.EffectiveProfile,
	candidates ...string,
) []string {
	all := append([]string(nil), candidates...)
	all = append(all, sandboxDirsForEffective(effective, sandboxpolicy.AccessWrite)...)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			all = append(all, entry.Value)
		}
	}
	var out []string
	for _, candidate := range all {
		candidate = canonicalSandboxPath(candidate)
		if candidate == "" || !sandboxDenyCoversEffectivePath(effective, candidate) {
			continue
		}
		out = appendUniqueDir(out, candidate)
	}
	sort.Strings(out)
	return out
}

// ValidateTclaudeLayerHarness refuses topologies where the wrapped pane does
// not contain the process that executes tools.
func ValidateTclaudeLayerHarness(harnessName string) error {
	return validateTclaudeLayerHarness(harnessName)
}

func tclaudeLayerWrapsPane(harnessName string) bool {
	return harnessName != harness.OpenCodeName
}

// TclaudeLayerUsesServerBoundary reports whether the harness's authoritative
// tool executor is a separately managed, non-interactive server rather than
// the pane process.
func TclaudeLayerUsesServerBoundary(harnessName string) bool {
	return !tclaudeLayerWrapsPane(harnessName)
}

func bwrapBindSourceExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		// Policy authoring intentionally permits future paths. Never create one
		// on the host merely to make a bind possible.
		return false, nil
	default:
		return false, err
	}
}

func bwrapCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	args, err := bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, privateWriteDirs, finalHideDirs, readOnlyBinds,
		"", nil)
	if err != nil {
		return "", err
	}
	command := clcommon.ShellQuoteArg(binary)
	for _, arg := range args {
		command += " " + clcommon.ShellQuoteArg(arg)
	}
	// Harness spawners return a safe shell command rather than argv. Keep that
	// contract intact inside the namespace instead of reparsing it here.
	command += " -- sh -c " + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}
