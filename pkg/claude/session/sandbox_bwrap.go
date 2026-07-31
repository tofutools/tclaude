package session

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
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
	// MaterializedUnixSocketPaths freezes the exact launch-time socket
	// observation shared with disclosure. A non-nil empty list means the
	// authored selectors materialized to no live sockets.
	MaterializedUnixSocketPaths *[]string `json:"materialized_unix_socket_paths,omitempty"`
	// omitempty keeps pre-TCL-779 v2 rows byte-compatible for new readers:
	// absent means no private reopen. An older strict reader encountering the
	// field refuses the newer contract instead of silently dropping it.
	PrivateWriteDirs []TclaudeLayerPrivateWriteDir `json:"private_write_dirs,omitempty"`
	// OpenCodeControl is present only in the v4 Unix-relay contract. The
	// pathname is host-side replay authority: the server receives only an
	// already-bound descriptor and never sees this path inside its mount
	// namespace.
	OpenCodeControl *TclaudeLayerOpenCodeControl `json:"opencode_control,omitempty"`
	// NetworkEngine carries the engine a layer selected for this launch, after
	// most-explicit-wins resolution. It is launch input rather than authored
	// policy: nothing in a profile spells it yet, and an omitted value is the
	// pre-engine behavior byte for byte. Whether the selection deploys anything
	// is decided by sandboxpolicy.DeployedNetworkEngine, never here.
	NetworkEngine sandboxpolicy.NetworkEngine `json:"network_engine,omitempty"`
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
	// NetworkEngine is the resolved engine selection for this launch. See the
	// contract field of the same name; production callers leave it unset until
	// the selection surface exists.
	NetworkEngine sandboxpolicy.NetworkEngine
}

// tclaudeLayerContractNetworkEngine derives the launch contract's engine from
// the ONE place that composes it: the effective profile's resolved network
// axis. A caller that also names an engine is not merged with, it is checked
// against — the profile is the authority, and a disagreement means the caller
// is about to launch something the operator did not author.
//
// Deriving here rather than at each construction site is deliberate. A launch
// path that forgot to copy the field would otherwise silently run pre-engine
// behavior for a profile that authored an engine, which is exactly the
// disclosure-does-not-match-launch failure the selection surface exists to
// prevent; there is no site left that can forget.
func tclaudeLayerContractNetworkEngine(
	effective sandboxpolicy.EffectiveProfile,
	input TclaudeLayerLaunchInput,
) (sandboxpolicy.NetworkEngine, error) {
	if err := sandboxpolicy.ValidateNetworkEngine(input.NetworkEngine); err != nil {
		return sandboxpolicy.NetworkEngineUnset, err
	}
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return sandboxpolicy.NetworkEngineUnset, err
	}
	authored := axes.Network.Engine
	if err := sandboxpolicy.ValidateNetworkEngine(authored); err != nil {
		return sandboxpolicy.NetworkEngineUnset, err
	}
	if input.NetworkEngine != sandboxpolicy.NetworkEngineUnset &&
		input.NetworkEngine != authored {
		return sandboxpolicy.NetworkEngineUnset, fmt.Errorf(
			"tclaude-layer launch names network filtering engine %q but the effective sandbox profile authors %q",
			input.NetworkEngine, authored,
		)
	}
	return authored, nil
}

// TclaudeLayerNetworkPosture derives the launch posture after applying any
// persisted per-rule degradation notices. Access lists and default-allow
// denies intentionally have no legacy network_access spelling, so launch and
// resume consumers must not inspect that compatibility field alone.
func TclaudeLayerNetworkPosture(
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.NetworkPosture, error) {
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return sandboxpolicy.NetworkHostOpen, err
	}
	return sandboxpolicy.NetworkPostureForRules(axes.Network)
}

// TclaudeLayerNetworkEngine reports the filtering engine a resolved profile
// DEPLOYS, from the same planned axes TclaudeLayerNetworkPosture reads.
//
// It is the resume path's route to the answer the spawn and launch paths get
// from their own axes. Without it a resumed launch would re-derive the floor
// from the posture alone, probe the packet gateway's prerequisites for a launch
// that calls neither pasta nor nft, and record the packet gateway's boundary
// sentence for a session running behind a proxy.
func TclaudeLayerNetworkEngine(
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.NetworkEngine, error) {
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return sandboxpolicy.NetworkEngineUnset, err
	}
	return sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
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
	remappedGrants := remappedGrantsForEffective(effective)
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
		for _, socket := range sandboxpolicy.AgentdSocketFloor() {
			socket = CanonicalTclaudeLayerGeneratedPath(socket)
			if socket != "" {
				launchReadDirs = append(launchReadDirs, socket)
			}
		}
	}
	// GrantsFromDirs flattens the launch-composed policy back into bare paths,
	// which can only express same-path rules. Remapped grants are therefore
	// carried around it and re-attached: they occupy their own sandbox paths, so
	// they never fold with a contract dir, and dropping them here would silently
	// launch without the mount the operator authored.
	effective.Filesystem = append(
		sandboxpolicy.GrantsFromDirs(launchReadDirs, launchWriteDirs, launchDenyDirs),
		remappedGrants...)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			contractWriteDirs = append(contractWriteDirs, entry.Value)
		}
	}
	// After the agent directories are in contractWriteDirs, not before: they are
	// launch-required in exactly the same way, and a mount shadowing one would
	// otherwise slip past the named refusal and silently hide it.
	if err := validateRemappedGuestPathsAgainstContract(
		remappedGrants, append(append([]string(nil), contractWriteDirs...), launchContractReadDirs...),
	); err != nil {
		return TclaudeLayerLaunchSpec{}, err
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
	contract.NetworkEngine, err = tclaudeLayerContractNetworkEngine(effective, input)
	if err != nil {
		return TclaudeLayerLaunchSpec{}, err
	}
	if input.Snapshot != nil && input.Snapshot.UnixSocketMaterialization != nil {
		paths := append([]string(nil), input.Snapshot.UnixSocketMaterialization.Paths...)
		contract.MaterializedUnixSocketPaths = &paths
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

// CanonicalTclaudeLayerGeneratedPath freezes a daemon-owned path even when its
// leaf does not exist yet. Generated paths bypass the authored-profile
// protected root check because the daemon-final contract deliberately re-closes
// those roots; they still need the same stable parent identity on replay.
//
// Persisted-spec consumers must use this same identity function when
// recognizing generated contract rows, rather than comparing textual paths
// that can differ through a supported symlinked HOME.
func CanonicalTclaudeLayerGeneratedPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return ""
	}
	ancestor := path
	var suffix []string
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return path
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		} else if !os.IsNotExist(err) {
			return path
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return path
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

// validateTclaudeLayerFilteredPolicy proves the compiled policy is one the
// engine this plan deploys can actually enforce, before any namespace is built.
//
// Each engine is asked in its own vocabulary, and only its own: the packet
// gateway must be able to render its nftables policy, and the proxy must be
// able to compile its evaluator. Asking the proxy engine for an nft rendering
// would refuse launches over a limitation of a mechanism that is not running,
// and asking neither would let an unenforceable policy reach the floor.
func validateTclaudeLayerFilteredPolicy(plan sandboxpolicy.MountPlan) error {
	if plan.FilteredNetwork == nil {
		return fmt.Errorf("filtered network posture has no compiled gateway policy")
	}
	if tclaudeLayerPlanDeploysProxy(plan) {
		if _, err := sandboxproxy.NewEvaluatorFromRuleSet(
			*plan.FilteredNetwork); err != nil {
			return fmt.Errorf("validate filtered network proxy policy: %w", err)
		}
		return nil
	}
	if _, err := sandboxpolicy.RenderFilteredNetworkNFT(*plan.FilteredNetwork); err != nil {
		return fmt.Errorf("validate filtered network gateway policy: %w", err)
	}
	return nil
}

// TclaudeLayerFloorPosture maps a network posture and the engine deployed for
// it onto the bubblewrap floor the launch actually builds.
//
// The proxy engine's floor IS the isolated posture's construction — empty
// network namespace, PID namespace, constructed root, agentd socket allowlisted
// — and the mapping says so once, here, instead of adding a parallel case to
// every switch that builds, probes, or describes a floor. That is the whole of
// "reuse NetworkIsolatedWithAgentd unchanged": the packet posture's user
// namespace, uid-0 mapping, capability grants, nft policy, resolver reopen and
// pasta prerequisites are not skipped by a flag, they are never reached.
func TclaudeLayerFloorPosture(
	posture sandboxpolicy.NetworkPosture,
	engine sandboxpolicy.NetworkEngine,
) sandboxpolicy.NetworkPosture {
	if posture == sandboxpolicy.NetworkFiltered &&
		engine == sandboxpolicy.NetworkEngineProxy {
		return sandboxpolicy.NetworkIsolatedWithAgentd
	}
	return posture
}

// tclaudeLayerPlanFloorPosture is TclaudeLayerFloorPosture for a rendered plan.
func tclaudeLayerPlanFloorPosture(
	plan sandboxpolicy.MountPlan,
) sandboxpolicy.NetworkPosture {
	return TclaudeLayerFloorPosture(plan.NetworkPosture, plan.NetworkEngine)
}

// TclaudeLayerDeploysProxy reports whether a launch at this posture and engine
// runs the host-side filtering proxy. Both halves are required and neither is
// sufficient: a filtered posture under the packet engine runs pasta and nft
// instead, and a proxy engine on a policy that widened away from filtered runs
// no filtering at all.
//
// It is exported because a disclosure ABOUT the proxy must be emitted on
// exactly the launches that deploy one, and answering that question a second
// way is how a notice ends up describing a launch that never happened.
func TclaudeLayerDeploysProxy(
	posture sandboxpolicy.NetworkPosture,
	engine sandboxpolicy.NetworkEngine,
) bool {
	return posture == sandboxpolicy.NetworkFiltered &&
		engine == sandboxpolicy.NetworkEngineProxy
}

// tclaudeLayerPlanDeploysProxy reports whether this plan runs the host-side
// filtering proxy. It reads the engine the plan already resolved rather than
// re-deciding it, so the launch and any preview answer from one predicate.
func tclaudeLayerPlanDeploysProxy(plan sandboxpolicy.MountPlan) bool {
	return TclaudeLayerDeploysProxy(plan.NetworkPosture, plan.NetworkEngine)
}

// ResolveTclaudeLayerForEngine verifies the host capability for a launch whose
// filtering engine is already resolved. The engine can only make the floor
// LESS demanding — the proxy floor needs no user namespace, capabilities, nft
// or pasta — so probing the mapped floor is the honest prerequisite check
// rather than a relaxed one.
func ResolveTclaudeLayerForEngine(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) (string, harness.LaunchOSSandbox, error) {
	return resolveTclaudeLayerForEngine(ResolveTclaudeLayer, posture, root, engine)
}

// ResolveTclaudeLayerServerForEngine is ResolveTclaudeLayerForEngine for the
// non-interactive server boundary. It exists so a server-boundary harness
// reaches the proxy floor through the same mapping an interactive one does: a
// launch that resolved the packet floor's prerequisites here would refuse a
// proxy-engine profile on a host that will never run pasta.
func ResolveTclaudeLayerServerForEngine(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) (string, harness.LaunchOSSandbox, error) {
	return resolveTclaudeLayerForEngine(
		ResolveTclaudeLayerServer, posture, root, engine)
}

func resolveTclaudeLayerForEngine(
	resolve func(sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (
		string, harness.LaunchOSSandbox, error),
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) (string, harness.LaunchOSSandbox, error) {
	floor := TclaudeLayerFloorPosture(posture, engine)
	binary, sandbox, err := resolve(floor, root)
	if err != nil {
		return "", sandbox, err
	}
	if floor != posture {
		sandbox = tclaudeLayerProxyLaunchOSSandbox()
	}
	return binary, sandbox, nil
}

// tclaudeLayerProxyLaunchOSSandbox names the mechanism a proxy-engine launch
// actually runs. Disclosure must never inherit the isolated posture's sentence
// just because the floor is shared: the floor is the same, what runs on top of
// it is not.
func tclaudeLayerProxyLaunchOSSandbox() harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State: "on",
		Source: "tclaude-layer (bubblewrap; filtered network via supervised " +
			"loopback filtering proxy; isolated PIDs; constructed root; " +
			"agentd socket allowlisted)",
		FilteredNetwork: true,
	}
}

// ResolveTclaudeLayer verifies the host capability before a launch is
// committed. Callers record the returned verdict even when verification fails.
//
// Both postures are required because since TCL-798 they are independent: a
// constructed root under the host network namespace needs a PID namespace the
// plain host-open probe never exercised.
func ResolveTclaudeLayer(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapBinary(posture, root)
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(posture, root), nil
}

// ResolveTclaudeLayerServer verifies the host capability needed by a
// non-interactive server boundary. Unlike ResolveTclaudeLayer, it does not
// require terminal-resize relay support that the server renderer never uses.
func ResolveTclaudeLayerServer(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapServerBinary(posture, root)
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(posture, root), nil
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
	_, err := resolveBwrapBinary(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	return err
}

// TclaudeLayerServerHostAvailability reports whether this host can create the
// non-interactive server boundary, without imposing interactive relay
// capabilities on a topology that has no terminal.
func TclaudeLayerServerHostAvailability() error {
	_, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	return err
}

// FilteredNetworkPrerequisite is the live control-plane result for the Linux
// filtered gateway. Detected does not itself mean NetworkList is enforced:
// the exact launch remains gated on policy installation and gateway readiness.
type FilteredNetworkPrerequisite struct {
	Detected bool   `json:"detected"`
	Detail   string `json:"detail"`
}

// ProbeFilteredNetworkPrerequisite checks the exact host building blocks named
// by the filtered-network design. It is uncached so a resolved launch cannot
// carry a stale answer after the operator installs or removes a prerequisite.
func ProbeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	return probeFilteredNetworkPrerequisite()
}

// ValidateFilteredNetworkHarnessSupport is the harness activation seam. All
// currently registered tclaude-layer harnesses consume the Linux filtered
// gateway; provider-specific model transport remains independently fail-closed.
func ValidateFilteredNetworkHarnessSupport(
	_ *harness.Harness,
	_ sandboxpolicy.Implementation,
	_ sandboxpolicy.ResolvedAxes,
	_ FilteredNetworkPrerequisite,
) error {
	return nil
}

// LaunchWhy is persisted in the resolved snapshot and therefore reaches both
// launch notes/warnings and the dashboard badge.
func (p FilteredNetworkPrerequisite) LaunchWhy(enforcing bool) string {
	if p.Detected && enforcing {
		return "filtered-network prerequisite probe: detected (" + p.Detail +
			"); launch remains gated on atomic nft policy installation before the supervised pasta route becomes available"
	}
	if p.Detected {
		return "filtered-network prerequisite probe: detected (" + p.Detail +
			"); this launch cannot consume the filtered network rules, so they remain unenforced and outbound remains open"
	}
	return "filtered-network prerequisite probe: unavailable (" + p.Detail +
		"); the filtered network rules remain unenforced and outbound remains open"
}

// FilteredNetworkPrerequisiteNotice turns one exact live probe result into the
// durable disclosure shared by agentd's resolved-launch response and the
// session boundary's final persisted snapshot.
func FilteredNetworkPrerequisiteNotice(
	probe FilteredNetworkPrerequisite,
	enforcing bool,
) sandboxpolicy.AccessNotice {
	effect := sandboxpolicy.AccessNoticeEffectNotEnforced
	if probe.Detected && enforcing {
		effect = sandboxpolicy.AccessNoticeEffectLaunchGated
	}
	return sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Reason: sandboxpolicy.AccessNoticeReasonFilteredPrerequisite,
		Effect: effect,
		Detail: probe.LaunchWhy(enforcing),
	}
}

// ProxyEngineFloorApplies reports whether this policy's filtered posture is
// carried by the PROXY engine's floor rather than the packet gateway's.
//
// It is the one predicate every surface that must tell the two floors apart
// asks — the spawn guard's posture gate, the session boundary's posture gate,
// and the pasta/nft prerequisite disclosure below. The proxy floor reaches the
// isolated posture's plain unshare and has none of the packet gateway's
// prerequisites (§2.5): no pasta, no nft, no CAP_NET_ADMIN, no port-53 broker.
// Its own spawn-time gate is bubblewrap plus pidfd, which the posture-exact
// resolve at the launch boundary already probes, so this predicate decides
// WHICH gate runs rather than duplicating either one.
//
// An unresolvable engine answers false — the packet answer. Failing to resolve
// the engine is not evidence that a proxy is deployed, and the packet branch is
// the one that probes MORE and discloses MORE, so an unresolvable policy is
// gated and disclosed rather than quietly admitted through the cheaper floor.
func ProxyEngineFloorApplies(network sandboxpolicy.NetworkRules) bool {
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(network)
	return err == nil && engine == sandboxpolicy.NetworkEngineProxy
}

// FilteredNetworkPrerequisiteNoticeApplies reports whether the packet
// gateway's pasta/nft prerequisite disclosure describes THIS policy's launch.
//
// The probe behind that notice is the packet gateway's, so disclosing it under
// the proxy engine would name a launch gate that does not gate this launch —
// and a failing pasta on a host that will never call pasta would read as the
// reason the rules are not enforced.
//
// It is exported because the notice is appended from two launch surfaces — the
// session boundary and the daemon spawn guard — and a gate applied at only one
// of them would let the two disagree about the same profile. It is the exact
// complement of ProxyEngineFloorApplies rather than a second derivation, so the
// disclosure and the gate can never answer differently.
func FilteredNetworkPrerequisiteNoticeApplies(
	network sandboxpolicy.NetworkRules,
) bool {
	return !ProxyEngineFloorApplies(network)
}

func appendFilteredNetworkPrerequisiteNotice(
	notices []sandboxpolicy.AccessNotice,
	outerLayer bool,
	network sandboxpolicy.NetworkRules,
	enforcing bool,
	probe func() FilteredNetworkPrerequisite,
) []sandboxpolicy.AccessNotice {
	posture, err := sandboxpolicy.NetworkPostureForRules(network)
	if !outerLayer || err != nil || posture != sandboxpolicy.NetworkFiltered {
		return notices
	}
	if !FilteredNetworkPrerequisiteNoticeApplies(network) {
		return notices
	}
	return append(notices, FilteredNetworkPrerequisiteNotice(probe(), enforcing))
}

// TclaudeLayerLaunchOSSandbox records the resolved platform/posture boundary.
// Partial host-open implementations stay visibly unverified; a constructed
// isolated root can report the stronger boundary it actually enforces.
func TclaudeLayerLaunchOSSandbox(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) harness.LaunchOSSandbox {
	return tclaudeLayerLaunchOSSandbox(posture, root)
}

// TclaudeLayerLaunchOSSandboxForEngine is TclaudeLayerLaunchOSSandbox for a
// launch whose filtering engine is known. A filtered posture carried by the
// proxy engine runs no pasta, no nft and no DNS broker, so it must not be
// described with the packet gateway's sentence — the badge and the persisted
// record are read as statements about the mechanism this launch runs.
func TclaudeLayerLaunchOSSandboxForEngine(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) harness.LaunchOSSandbox {
	if TclaudeLayerFloorPosture(posture, engine) != posture {
		return tclaudeLayerProxyLaunchOSSandbox()
	}
	return tclaudeLayerLaunchOSSandbox(posture, root)
}

// TclaudeLayerLaunchOSSandboxForHarness describes the actual process boundary.
// OpenCode's attach TUI is deliberately outside the wall; its agentd-owned
// server is the process that executes tools and is the component we confine.
//
// The engine is taken rather than re-derived because the boundary sentence must
// name the mechanism this launch RUNS. A filtered posture carried by the proxy
// engine runs no pasta, no nft and no DNS broker, so inheriting the packet
// gateway's sentence for it would be a disclosure that does not match the
// rendered surface — the same failure the floor mapping exists to prevent.
func TclaudeLayerLaunchOSSandboxForHarness(
	harnessName string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) harness.LaunchOSSandbox {
	resolved := TclaudeLayerLaunchOSSandboxForEngine(posture, root, engine)
	if harnessName == harness.OpenCodeName {
		if posture == sandboxpolicy.NetworkFiltered {
			resolved.Source += "; OpenCode tool-executing server confined; " +
				"attach client outside boundary over authenticated Unix relay"
			return resolved
		}
		return tclaudeLayerOpenCodeLaunchOSSandbox()
	}
	return resolved
}

// TclaudeLayerRootPosture answers the TCL-798 question for one launch: does it
// build its own filesystem root? It exists so the surfaces that must agree
// about the boundary — the capability probe, the launch badge, the agent-socket
// environment — derive the answer from one place instead of each re-deciding
// it.
//
// The NETWORK posture is taken from the caller rather than re-derived, because
// the launch boundary may have settled on a weaker one than the profile asked
// for: a filtered launch whose live gateway prerequisites fail runs host-open.
// Re-deriving here would report a constructed root for a launch that is not
// going to build one. The SOCKET half is read from the profile, since nothing
// at the launch boundary can widen it after the capability ladder has run.
//
// Planned axes are used rather than authored ones so a socket rule the ladder
// already widened cannot keep requesting a root the launch will not enforce
// with.
func TclaudeLayerRootPosture(
	posture sandboxpolicy.NetworkPosture,
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.RootPosture, error) {
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return sandboxpolicy.RootHostInherited, err
	}
	return sandboxpolicy.RootPostureFor(posture, axes.UnixSockets.Mode), nil
}

// TclaudeLayerLaunchRootPosture is TclaudeLayerRootPosture for a launch surface
// that runs BEFORE the capability ladder, where the socket tier is still the
// authored one.
//
// The extra gate matters because the socket-driven constructed root is a Linux
// tclaude-layer mechanism for Claude Code and Codex only. Everywhere else the
// ladder is about to widen the axis away, and the applier will render an
// inherited root — so deriving a constructed root here would make the probe
// demand a namespace the launch does not need, and, worse, make
// ApplyAgentSocketEnv refuse an operator's explicit agentd socket for a launch
// that never confines sockets at all.
func TclaudeLayerLaunchRootPosture(
	h *harness.Harness,
	implementation sandboxpolicy.Implementation,
	posture sandboxpolicy.NetworkPosture,
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.RootPosture, error) {
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return sandboxpolicy.RootHostInherited, err
	}
	sockets := axes.UnixSockets.Mode
	if !harness.SupportsHostOpenConstructedRoot(
		h, implementation, axes, runtime.GOOS) {
		// Keep only the network posture's own implication.
		sockets = sandboxpolicy.AccessModeUnset
	}
	return sandboxpolicy.RootPostureFor(posture, sockets), nil
}

// ValidateTclaudeLayerNetwork refuses an isolated whole-process launch unless
// both the harness descriptor and the operator's resolved profile assert a
// model transport that functions across the selected platform's boundary
// (a network namespace on Linux, Seatbelt network denies on Darwin).
func ValidateTclaudeLayerNetwork(
	h *harness.Harness,
	effective sandboxpolicy.EffectiveProfile,
	resolvedModel harness.ResolvedModelTransport,
) ([]sandboxpolicy.AccessNotice, error) {
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return nil, err
	}
	switch axes.Network.Mode {
	case sandboxpolicy.AccessModeUnset:
		return nil, nil
	case sandboxpolicy.AccessModeOpen:
		if len(axes.Network.Deny) == 0 {
			return nil, nil
		}
		fallthrough
	case sandboxpolicy.AccessModeList:
		deployedEngine, engineErr :=
			sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
		if engineErr != nil {
			return nil, engineErr
		}
		if endpointErr := validateModelTransportLoopbackForPlatform(
			h, resolvedModel, runtime.GOOS, deployedEngine,
		); endpointErr != nil {
			return nil, endpointErr
		}
		requirement, resolveErr := harness.ResolveModelTransportRequirement(h, resolvedModel)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if coverageErr := harness.ValidateModelTransportCoverage(
			h, axes.Network, requirement); coverageErr != nil {
			return nil, coverageErr
		}
		detail := describeModelTransportRequirementForPlatform(
			axes.Network, requirement, runtime.GOOS)
		if requirement.Template != "" {
			detail += " Hosted endpoint coverage was empirically audited by the pinned M2c real-harness origin smoke (Claude Code 2.1.220; Codex CLI 0.145.0)."
		}
		if h.Name == harness.DefaultName {
			detail += " Claude Code also loads remote managed settings, which its own loader re-fetches on a background poll and applies in-process, so provider routing can move after this preflight without any local file changing. tclaude inspects the cached remote settings it can see; when the route moves anyway, the unauthored destination is denied fail-closed for new flows at the packet floor."
		}
		if h.Name == harness.CodexName {
			detail += " The provider route above is Codex's own effective config, read through its app-server, so enterprise cloud-config and MDM layers are included rather than guessed. Codex snapshots that bundle at process start and its background refresher only warms the cache for later starts, so a running session does not re-route; a refresh landing between this preflight and process start is denied fail-closed at the packet floor."
			if len(resolvedModel.Provenance) > 0 {
				detail += " Remotely delivered provider routing in effect: " +
					strings.Join(resolvedModel.Provenance, "; ") + "."
			}
		}
		if h.Name == harness.OpenCodeName {
			detail += " OpenCode filtered supports explicit-provider configs only: the launch model and frozen OPENCODE_CONFIG_CONTENT must name exactly one inspected openai-compatible provider, model without a provider override, and concrete options.baseURL. The server uses daemon-final read-only, provider-empty private XDG and HOME config directories and rechecks those directories plus persistent account/org authority before every initial exec or restart so none of those sources can replace the inspected route. OpenCode's built-in webfetch/websearch permission rules are soft tool policy; this tclaude-layer nft boundary is the packet-enforced floor."
		}
		return []sandboxpolicy.AccessNotice{{
			Class:  sandboxpolicy.AccessNoticeClassDegradation,
			Axis:   "network",
			Reason: sandboxpolicy.AccessNoticeReasonFilteredModelTraffic,
			Effect: sandboxpolicy.AccessNoticeEffectLaunchGated,
			Detail: detail,
		}}, nil
	case sandboxpolicy.AccessModeClosed:
	default:
		return nil, fmt.Errorf("unsupported tclaude-layer network mode %q", axes.Network.Mode)
	}
	if !h.SupportsOfflineModelTransport() {
		return nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: network_access none isolates the whole tclaude-layer process, but harness %q requires hosted model traffic; see docs/sandboxing.md#isolated-with-agentd-network-posture",
			h.Name,
		)
	}
	for _, entry := range effective.Environment {
		if entry.Name == sandboxpolicy.OfflineModelTransportEnv && entry.Value == "1" {
			return nil, nil
		}
	}
	return nil, fmt.Errorf(
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
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, socketPaths, plan, err :=
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
		socketPaths,
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
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, socketPaths, plan, err :=
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
		socketPaths,
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
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, socketPaths, plan, err :=
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
		socketPaths,
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
	phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, socketPaths, plan, err :=
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
		socketPaths, opaqueStateRoot, opaqueStateDirs)
	if err != nil {
		return nil, err
	}
	// Upstream bubblewrap deliberately passes non-CLOEXEC descriptors to the
	// sandbox command; it has no --preserve-fds option. ExtraFiles supplies
	// exactly fd 3 (listener) and fd 4 (relay executable).
	args = append(args, "--")
	args = append(args, serverArgv...)
	return tclaudeLayerUnixRelayServerCommandArgs(
		spec, append([]string{binary}, args...))
}

// The inherited-descriptor contract, stated once for every filtering engine.
//
// runTclaudeLayerWinchRelay installs the launcher's two preserved descriptors
// LAST in bubblewrap's ExtraFiles — after bubblewrap's own status pipe, and
// after whatever sealed inputs the deployed engine contributed. What the
// harness inside the sandbox sees is therefore determined by exactly one
// engine-specific number: how many descriptors that engine contributes. These
// constants are that number, and nothing else about the two engines differs
// here.
//
// Writing the contract this way is what let the proxy engine join it without a
// second copy of the layout. Each count is pinned at compile time against the
// engine's own fd constants, beside those constants, so a descriptor added to
// either engine is a build failure here rather than a launch that names the
// wrong fds.
const (
	// tclaudeLayerRelayStatusFD is bubblewrap's --json-status-fd, which the
	// supervisor always owns and always installs first.
	tclaudeLayerRelayStatusFD = 3
	// tclaudeLayerPacketEngineDescriptors: sealed bootstrap image, nft policy,
	// hosts and resolv.conf (fds 4-7). See filtered_network_gateway_linux.go.
	tclaudeLayerPacketEngineDescriptors = 4
	// tclaudeLayerProxyEngineDescriptors: sealed bootstrap image and the
	// loopback-only hosts file (fds 4-5). See proxy_network_bridge_linux.go.
	// The proxy engine needs neither an nft policy nor a resolver, which is
	// the whole of why its count is smaller.
	tclaudeLayerProxyEngineDescriptors = 2
)

// tclaudeLayerRelayEngineDescriptors reports how many sealed descriptors the
// engine this plan deploys hands bubblewrap ahead of the launcher's own two,
// and whether a supervisor is interposed at all.
//
// The engine question is asked through tclaudeLayerPlanDeploysProxy — the one
// deployment predicate — rather than re-derived from posture and engine here.
func tclaudeLayerRelayEngineDescriptors(
	plan sandboxpolicy.MountPlan,
) (count int, relayed bool) {
	if plan.NetworkPosture != sandboxpolicy.NetworkFiltered {
		return 0, false
	}
	if tclaudeLayerPlanDeploysProxy(plan) {
		return tclaudeLayerProxyEngineDescriptors, true
	}
	return tclaudeLayerPacketEngineDescriptors, true
}

// TclaudeLayerUnixRelayServerFDs returns the descriptors the inherited relay
// command must name, for whichever filtering engine this plan deploys: the
// packet gateway preserves the launcher's listener and executable as fds 8 and
// 9, the filtering proxy as fds 6 and 7. Without a supervisor interposed at all
// the launcher descriptors pass directly to bubblewrap as fds 3 and 4.
//
// It answers from tclaudeLayerRelayEngineDescriptors, which is also what
// tclaudeLayerUnixRelayServerCommandArgs renders against, so the fds the
// command NAMES and the fds the supervisor INSTALLS cannot disagree.
func TclaudeLayerUnixRelayServerFDs(
	spec TclaudeLayerLaunchSpec,
) (listenerFD, executableFD int, err error) {
	_, _, _, _, _, plan, err := tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return 0, 0, err
	}
	engineDescriptors, relayed := tclaudeLayerRelayEngineDescriptors(plan)
	if !relayed {
		return 3, 4, nil
	}
	base := tclaudeLayerRelayStatusFD + 1 + engineDescriptors
	return base, base + 1, nil
}

func tclaudeLayerSpecRenderInput(
	spec TclaudeLayerLaunchSpec,
) (
	[]string,
	[]TclaudeLayerPrivateWriteDir,
	[]string,
	[]TclaudeLayerReadOnlyBind,
	[]string,
	sandboxpolicy.MountPlan,
	error,
) {
	if !supportedTclaudeLayerLaunchSpecVersion(spec.Version) {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("unsupported tclaude-layer launch spec version %d", spec.Version)
	}
	if err := validateTclaudeLayerOpenCodeControl(spec); err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	plan, err := sandboxpolicy.RenderMountPlanWithEngine(
		spec.Effective, spec.Contract.NetworkEngine)
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("render mount plan: %w", err)
	}
	phase0WriteDirs, err := tclaudeLayerPhase0WriteDirs(spec.Contract, spec.Effective)
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	stateRoots := append([]string{phase0WriteDirs[0]}, spec.Contract.StateDirs...)
	stateRoots = append(stateRoots, spec.Contract.ReadOnlyStateDirs...)
	for _, stateRoot := range stateRoots {
		if err := validateTclaudeLayerHarnessStateRules(
			stateRoot,
			spec.Contract.ProfileFilesystem,
		); err != nil {
			return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
		}
	}
	privateWriteDirs, err := cleanTclaudeLayerPrivateWriteDirs(
		spec.Contract.PrivateWriteDirs,
	)
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	finalHideDirs, err := cleanTclaudeLayerAbsoluteDirs(
		"daemon-final hide", spec.Contract.FinalHideDirs)
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	readOnlyBinds, err := cleanTclaudeLayerReadOnlyBinds(spec.Contract.ReadOnlyBinds)
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
			fmt.Errorf("resolve protected paths for daemon-final read-only binds: %w", err)
	}
	for _, bind := range readOnlyBinds {
		for _, path := range []string{bind.Source, bind.Target} {
			for _, protected := range protectedRoots {
				if sandboxpolicy.PathContainsOrEqual(protected, path) {
					return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
						fmt.Errorf(
							"daemon-final read-only bind path %q is at or below protected root %q",
							path, protected)
				}
			}
			if err := validateTclaudeLayerHarnessStateRules(
				path, spec.Contract.ProfileFilesystem); err != nil {
				return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{}, err
			}
		}
	}
	socketPaths := sandboxpolicy.AgentdSocketFloor()
	if tclaudeLayerPlanUsesConstructedRoot(plan) &&
		spec.Effective.UnixSockets != nil {
		var authoredSockets []string
		if spec.Contract.MaterializedUnixSocketPaths != nil {
			authoredSockets = *spec.Contract.MaterializedUnixSocketPaths
			if socketErr := sandboxpolicy.ValidateMaterializedUnixSocketPaths(
				*spec.Effective.UnixSockets, authoredSockets); socketErr != nil {
				return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
					fmt.Errorf("validate tclaude-layer unix socket allowlist: %w", socketErr)
			}
		} else {
			var socketErr error
			authoredSockets, socketErr = sandboxpolicy.MaterializeUnixSocketPaths(
				*spec.Effective.UnixSockets)
			if socketErr != nil {
				return nil, nil, nil, nil, nil, sandboxpolicy.MountPlan{},
					fmt.Errorf("materialize tclaude-layer unix socket allowlist: %w", socketErr)
			}
		}
		for _, socket := range authoredSockets {
			socketPaths = appendUniqueDir(socketPaths, socket)
		}
	}
	return phase0WriteDirs, privateWriteDirs, finalHideDirs, readOnlyBinds, socketPaths, plan, nil
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
	posture, err := TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil {
		return err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd &&
		posture != sandboxpolicy.NetworkFiltered {
		return fmt.Errorf(
			"tclaude-layer v4 Unix relay requires the isolated or filtered network posture")
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
	_, _, _, _, _, _, err := tclaudeLayerSpecRenderInput(spec)
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
		phase0WriteDirs, plan, privateWriteDirs, nil, nil,
		sandboxpolicy.AgentdSocketFloor(), "", nil)
}

func bwrapArgsWithDaemonFinal(
	phase0WriteDirs []string,
	plan sandboxpolicy.MountPlan,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	opaqueStateRoot string,
	opaqueStateDirs []string,
) ([]string, error) {
	var hideRemounts tclaudeLayerHideRemounts
	// Set only when root construction reopens the host resolver file, so plan
	// replay can repair it beneath an ordinary ancestor deny.
	resolverTarget := ""
	if plan.NetworkPosture == sandboxpolicy.NetworkFiltered {
		if plan.FilteredNetwork == nil {
			return nil, fmt.Errorf("filtered network posture has no compiled gateway policy")
		}
		if err := validateTclaudeLayerFilteredPolicy(plan); err != nil {
			return nil, err
		}
	} else if plan.FilteredNetwork != nil {
		return nil, fmt.Errorf("non-filtered network posture unexpectedly carries a gateway policy")
	}
	args := []string{
		"--die-with-parent",
		// Give the wrapped process a new terminal session so a copied tmux
		// client or TIOCSTI-capable process cannot inject input into the host
		// pane shell outside this namespace.
		"--new-session",
	}
	// The floor, not the posture: a filtered plan under the proxy engine builds
	// the isolated posture's namespace exactly, so it takes that case rather
	// than a copy of it (see TclaudeLayerFloorPosture).
	switch tclaudeLayerPlanFloorPosture(plan) {
	case sandboxpolicy.NetworkHostOpen:
		if tclaudeLayerPlanUsesConstructedRoot(plan) {
			// TCL-798: the host network namespace is kept, but the root is
			// built rather than inherited, so ambient filesystem sockets are
			// absent unless a later bind reopens them.
			//
			// --unshare-pid is not optional here even though the network stays
			// shared. Without it the host's own processes remain visible in
			// /proc, and any of them the sandboxed user may ptrace exposes
			// /proc/<pid>/root — a live path back into the host mount namespace
			// and therefore back to every socket this posture just hid. The
			// isolated posture has always relied on the same property.
			args = append(args, "--unshare-pid")
			args = hideRemounts.appendHide(args, "/")
			break
		}
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
		// Rootless bubblewrap maps the invoking host user to namespace root.
		// That identity is required only so the sealed bootstrap can receive
		// CAP_NET_ADMIN for the namespace-local nft policy and
		// CAP_NET_BIND_SERVICE for its private DNS listener. Host file ownership
		// remains the invoking user's, and the bootstrap zeroes and verifies
		// every capability set before the harness exec.
		args = append(args,
			"--unshare-user",
			"--uid", "0",
			"--gid", "0",
			"--unshare-net",
			"--unshare-pid",
		)
		args = hideRemounts.appendHide(args, "/")
	default:
		return nil, fmt.Errorf("mount plan has invalid network posture %d", plan.NetworkPosture)
	}
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		// Never share the host's scratch directory by default.
		"--tmpfs", "/tmp",
	)
	if tclaudeLayerPlanFloorPosture(plan) == sandboxpolicy.NetworkFiltered {
		// Keep /run ambient-free while creating its private filesystem before
		// any explicitly authorized Unix sockets beneath it are rebound. The
		// filtered relay may later add only the resolver symlink target. The
		// proxy floor has no in-namespace resolver to rebind, so it never
		// reaches here.
		args = append(args, "--tmpfs", "/run")
	}
	if tclaudeLayerPlanUsesConstructedRoot(plan) {
		var err error
		args, err = appendTclaudeLayerAliases(args, plan.Aliases)
		if err != nil {
			return nil, err
		}
		args, err = appendTclaudeLayerStaticOSRoot(args)
		if err != nil {
			return nil, err
		}
		args, resolverTarget, err = appendTclaudeLayerHostResolver(args, plan)
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
	liveSocketPaths := make([]string, 0, len(socketPaths))
	if tclaudeLayerPlanUsesConstructedRoot(plan) {
		for i, socket := range socketPaths {
			if socket == "" || !filepath.IsAbs(socket) {
				return nil, fmt.Errorf("resolve agentd socket floor entry %d for isolated tclaude-layer", i)
			}
			exists, err := bwrapBindSourceExists(socket)
			if err != nil {
				return nil, fmt.Errorf("agentd socket source %q: %w", socket, err)
			}
			if !exists {
				if i == 0 {
					return nil, fmt.Errorf("isolated tclaude-layer requires the canonical agentd socket %s", socket)
				}
				if i >= len(sandboxpolicy.AgentdSocketFloor()) {
					return nil, fmt.Errorf(
						"materialized unix socket %q disappeared before the tclaude-layer adapter rendered it",
						socket)
				}
				continue
			}
			args = append(args, "--ro-bind", socket, socket)
			liveSocketPaths = append(liveSocketPaths, socket)
		}
	}
	// Class 2: replay the policy plan exactly as rendered. Repair mounts do not
	// reorder or deduplicate plan entries; they preserve the higher-precedence
	// launch contract after an ordinary ancestor hide.
	for i, entry := range plan.Entries {
		path := filepath.Clean(entry.Path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("mount plan entry %d has non-absolute path %q", i, entry.Path)
		}
		// path is the SANDBOX-side location; source is the host directory whose
		// authority the entry carries. They differ only for a mount_path grant
		// (TCL-866), and a hide is always same-path because a deny names a host
		// path rather than projecting one.
		source := filepath.Clean(entry.SourcePath())
		if source == "." || !filepath.IsAbs(source) {
			return nil, fmt.Errorf("mount plan entry %d has non-absolute source %q", i, entry.Source)
		}
		if entry.IsRemapped() && entry.Mode == sandboxpolicy.MountHide {
			return nil, fmt.Errorf(
				"mount plan entry %d hides sandbox path %q but names host source %q; a hide is always same-path",
				i, path, source)
		}
		switch entry.Mode {
		case sandboxpolicy.MountRO, sandboxpolicy.MountRW:
			exists, err := bwrapBindSourceExists(source)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, source, err)
			}
			if !exists {
				continue
			}
			if err := requireTclaudeLayerGuestMountpoint(i, entry, plan); err != nil {
				return nil, err
			}
			hideRemounts.noteReplacement(path)
			flag := "--ro-bind"
			if entry.Mode == sandboxpolicy.MountRW {
				flag = "--bind"
			}
			args = append(args, flag, source, path)
			args = appendTclaudeLayerProtectedRehides(
				args,
				path,
				protectedRoots,
				&hideRemounts,
			)
		case sandboxpolicy.MountHide:
			args = hideRemounts.appendHide(args, path)
			if tclaudeLayerPlanUsesConstructedRoot(plan) {
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
				liveSocketPaths,
				&hideRemounts,
			)
			// The resolver file is reopened during root construction, so an
			// ordinary deny on an ancestor — /run is the realistic one — would
			// otherwise shadow it and break name resolution with no notice.
			// Repaired like the agentd socket rather than left to chance.
			if resolverTarget != "" {
				args = appendTclaudeLayerSocketRepairs(
					args,
					path,
					[]string{resolverTarget},
					&hideRemounts,
				)
			}
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

// requireTclaudeLayerGuestMountpoint states the one host-side precondition a
// remapped mount has, and states it as a refusal rather than letting bubblewrap
// fail with an unattributable error.
//
// Under a CONSTRUCTED root the sandbox root is a fresh tmpfs, so bubblewrap
// creates the destination directory inside the namespace and nothing on the
// host has to exist. Under an INHERITED root the sandbox root IS the host root,
// bound read-only, so there is nowhere to create a new mountpoint: the directory
// must already exist on the host. This mirrors the existing daemon-final
// source→target bind, which has required an existing target since it was
// introduced.
//
// The condition is the root posture, not the network posture. Until TCL-798 the
// two were the same question, and this refusal was reachable for every host-open
// launch; a host-open plan that constructs its root to confine Unix sockets now
// creates the mountpoint like any other constructed root, so the refusal no
// longer applies there.
//
// tclaude deliberately does not mkdir the mountpoint itself. Creating host
// directories as a side effect of launching is the same failure the missing-
// source rule already refuses (see RenderMountPlanFromGrants): it would leave
// real directories behind on the operator's box for a rule that may never
// launch again.
func requireTclaudeLayerGuestMountpoint(
	index int,
	entry sandboxpolicy.MountEntry,
	plan sandboxpolicy.MountPlan,
) error {
	if !entry.IsRemapped() || tclaudeLayerPlanUsesConstructedRoot(plan) {
		return nil
	}
	exists, err := bwrapBindSourceExists(entry.Path)
	if err != nil {
		return fmt.Errorf("mount plan entry %d sandbox mount point %q: %w", index, entry.Path, err)
	}
	if !exists {
		return fmt.Errorf(
			"tclaude_layer_missing_mount_point: mount plan entry %d cannot mount %q at sandbox path %q because the sandbox root is the host root under the %s network posture with an inherited root, so the mount point must already exist on the host; create %q, or use a posture where tclaude constructs the root (closed or filtered network access, or an explicit unix_sockets rule)",
			index, entry.SourcePath(), entry.Path, plan.NetworkPosture, entry.Path)
	}
	return nil
}

// tclaudeLayerPlanUsesConstructedRoot is the applier's single reading of the
// TCL-798 decision. Every operation that only makes sense against a root
// tclaude built — the static OS surface, symlink aliases, socket binds, the
// mountpoint refusal above — asks this rather than re-deriving it from the
// network posture, which no longer answers the question.
func tclaudeLayerPlanUsesConstructedRoot(plan sandboxpolicy.MountPlan) bool {
	return plan.EffectiveRootPosture() == sandboxpolicy.RootConstructed
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

func describeModelTransportRequirementForPlatform(
	rules sandboxpolicy.NetworkRules,
	requirement harness.ModelTransportRequirement,
	goos string,
) string {
	detail := harness.DescribeModelTransportRequirementForRules(rules, requirement)
	// Same swap as the endpoint check above: the proxy engine installs no
	// synthetic name, so the requirement must be described in the spelling the
	// operator actually has to configure.
	if engine, err := sandboxpolicy.DeployedNetworkEngineForRules(rules); err == nil &&
		engine == sandboxpolicy.NetworkEngineProxy {
		return strings.ReplaceAll(
			detail, sandboxpolicy.FilteredNetworkHostLoopbackName, "localhost")
	}
	if goos == "darwin" {
		detail = strings.ReplaceAll(
			detail,
			sandboxpolicy.FilteredNetworkHostLoopbackName,
			"localhost",
		)
	}
	return detail
}

// validateModelTransportLoopbackForPlatform keeps the abstract loopback
// selector honest at the platform boundary. Linux's network namespace owns
// 127.0.0.1/::1 and reaches the host only through the synthetic mapping;
// Darwin Seatbelt filters the real host loopback directly and does not install
// the Linux-only synthetic hostname.
func validateModelTransportLoopbackForPlatform(
	h *harness.Harness,
	resolved harness.ResolvedModelTransport,
	goos string,
	engine sandboxpolicy.NetworkEngine,
) error {
	if strings.TrimSpace(resolved.BaseURL) == "" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(resolved.BaseURL))
	if err != nil || parsed.Hostname() == "" {
		return nil // The harness-owned resolver reports the malformed endpoint.
	}
	host := strings.ToLower(parsed.Hostname())
	harnessName := ""
	if h != nil {
		harnessName = h.Name
	}
	// The proxy engine removes the asymmetry this function exists to police.
	// It installs no synthetic host-loopback mapping at all; the loopback
	// selector means "CONNECT localhost:P through the tclaude proxy, which
	// reaches real host loopback" (§4.5), identically on Linux and Darwin. So
	// under this engine the platform-specific spellings swap: localhost is
	// correct everywhere and needs no remedy, and the packet engine's synthetic
	// name resolves to nothing on either platform.
	if engine == sandboxpolicy.NetworkEngineProxy {
		if host != sandboxpolicy.FilteredNetworkHostLoopbackName {
			return nil
		}
		return &harness.SandboxCapabilityError{
			Harness: harnessName,
			Kind:    harness.SandboxCapabilityModelTransport,
			Message: fmt.Sprintf(
				"resolved provider endpoint %q uses the synthetic host-loopback name, which only the packet filtering engine installs; under the proxy filtering engine configure the provider for localhost, 127.0.0.1, or ::1 at the same port and author a loopback rule for it, choose a resolvable non-loopback provider, or use network open",
				resolved.BaseURL,
			),
		}
	}
	if goos == "darwin" &&
		host == sandboxpolicy.FilteredNetworkHostLoopbackName {
		return &harness.SandboxCapabilityError{
			Harness: harnessName,
			Kind:    harness.SandboxCapabilityModelTransport,
			Message: fmt.Sprintf(
				"resolved provider endpoint %q uses the Linux-only synthetic host-loopback name; on macOS configure the provider for localhost, 127.0.0.1, or ::1 at the same port, choose a resolvable non-loopback provider, or use network open",
				resolved.BaseURL,
			),
		}
	}
	if goos != "linux" {
		return nil
	}
	isLoopback := host == "localhost"
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		isLoopback = address.IsLoopback()
	}
	if !isLoopback {
		return nil
	}
	port := 443
	if parsed.Scheme == "http" {
		port = 80
	}
	if rawPort := parsed.Port(); rawPort != "" {
		if parsedPort, parseErr := strconv.Atoi(rawPort); parseErr == nil {
			port = parsedPort
		}
	}
	return &harness.SandboxCapabilityError{
		Harness: harnessName,
		Kind:    harness.SandboxCapabilityModelTransport,
		Message: fmt.Sprintf(
			"resolved provider endpoint %q uses sandbox-private localhost on Linux; configure the provider for %s:%d to reach an explicitly allowed host-loopback service, choose a resolvable non-loopback provider, or use network open",
			resolved.BaseURL, sandboxpolicy.FilteredNetworkHostLoopbackName, port,
		),
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

// tclaudeLayerHostResolverRuntimeRoot is the only directory outside the static
// OS surface a resolver symlink may point into. Distributions that manage
// /etc/resolv.conf dynamically (systemd-resolved, resolvconf, NetworkManager)
// all park the real file under /run.
// These two are variables only so a test can point the traversal below at a
// fixture instead of the developer's real resolver and real /run. Neither is
// reassigned in production.
var (
	tclaudeLayerHostResolverPath        = "/etc/resolv.conf"
	tclaudeLayerHostResolverRuntimeRoot = "/run"
)

// appendTclaudeLayerHostResolver keeps DNS working for a constructed root that
// still has the HOST network namespace.
//
// The static OS surface binds /etc read-only, so /etc/resolv.conf is present
// either way. On a systemd-resolved-class host it is a SYMLINK into /run, which
// the constructed root does not have, and following it inside the namespace
// lands on nothing. The isolated posture never noticed: it has no route to
// resolve names over. The filtered posture never noticed either: it installs
// its own broker resolv.conf. A host-open constructed root is the first posture
// where the host's real resolver has to survive root construction, so the
// target file is reopened read-only and its parents created.
//
// Only the resolver FILE is bound, never its directory. /run is exactly where
// the ambient sockets this posture exists to hide tend to live, so reopening
// the surrounding directory would hand back what the constructed root just took
// away.
//
// A target outside /run is deliberately not chased. tclaude does not know what
// such a layout means, and silently binding an arbitrary host path to make DNS
// work would be a wider hole than the failure it prevents; the launch proceeds
// and name resolution behaves as it does in any other constructed root.
// It returns the reopened target so plan replay can repair it after an ordinary
// ancestor hide, the same way the agentd socket is repaired; "" means nothing
// was reopened.
func appendTclaudeLayerHostResolver(
	args []string,
	plan sandboxpolicy.MountPlan,
) ([]string, string, error) {
	if plan.NetworkPosture != sandboxpolicy.NetworkHostOpen ||
		plan.EffectiveRootPosture() != sandboxpolicy.RootConstructed {
		// An inherited root already carries the real /run, and an isolated or
		// filtered posture has no host resolver to preserve. The caller only
		// reaches here for a constructed root; this restates the precondition
		// so the reopen cannot be moved somewhere it would widen a plan.
		return args, "", nil
	}
	resolver := tclaudeLayerHostResolverPath
	info, err := os.Lstat(resolver)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return args, "", nil
	default:
		return nil, "", fmt.Errorf("inspect host resolver %q: %w", resolver, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// A regular file arrives with the read-only /etc bind.
		return args, "", nil
	}
	target, err := filepath.EvalSymlinks(resolver)
	if err != nil {
		// A dangling resolver symlink is already broken on the host. Leave the
		// namespace matching it rather than failing the launch.
		return args, "", nil
	}
	target = filepath.Clean(target)
	if tclaudeLayerStaticOSRootProvides(target) {
		return args, "", nil
	}
	if !sandboxpolicy.PathContainsOrEqual(tclaudeLayerHostResolverRuntimeRoot, target) ||
		target == tclaudeLayerHostResolverRuntimeRoot {
		return args, "", nil
	}
	// The target must be a REGULAR FILE, and this check is load-bearing rather
	// than defensive. bwrap binds whatever it is given: a resolver symlink
	// pointing at a directory would recursively expose that whole /run subtree —
	// which is exactly where the ambient sockets this posture exists to hide
	// live — and one pointing at a socket would reopen an ambient socket
	// directly. Either would make the capability row's Partial rating untrue
	// while it discloses only the abstract-socket remainder. A host whose
	// resolv.conf points at a non-file cannot resolve names anyway, so refusing
	// to follow it costs nothing real.
	targetInfo, err := os.Stat(target)
	if err != nil || !targetInfo.Mode().IsRegular() {
		return args, "", nil
	}
	parents := []string{}
	for parent := filepath.Dir(target); ; parent = filepath.Dir(parent) {
		parents = append([]string{parent}, parents...)
		if parent == tclaudeLayerHostResolverRuntimeRoot {
			break
		}
	}
	for _, parent := range parents {
		args = append(args, "--dir", parent)
	}
	return append(args, "--ro-bind", target, target), target, nil
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
		// A remapped grant has no single path that means both "the host
		// directory" and "where it appears", so it cannot ride a bare path list.
		// remappedGrantsForEffective carries it instead.
		if grant.Access == access && !grant.IsRemapped() {
			out = append(out, grant.Path)
		}
	}
	return out
}

// remappedGrantsForEffective returns the rules a bare path list cannot express.
func remappedGrantsForEffective(
	effective sandboxpolicy.EffectiveProfile,
) []sandboxpolicy.FilesystemGrant {
	var out []sandboxpolicy.FilesystemGrant
	for _, grant := range effective.Filesystem {
		if grant.IsRemapped() {
			out = append(out, grant)
		}
	}
	return out
}

// validateRemappedGuestPathsAgainstContract refuses a mount that would land on
// top of a path the launch itself requires. Launch-contract binds are applied
// BEFORE the policy plan replays, so a remapped mount covering one of them would
// shadow it — the workspace, a Git admin directory or harness state would simply
// be gone, and the harness would fail in a way that points nowhere near the rule
// that caused it.
func validateRemappedGuestPathsAgainstContract(
	remapped []sandboxpolicy.FilesystemGrant,
	contractDirs []string,
) error {
	for _, grant := range remapped {
		guest := grant.GuestPath()
		for _, dir := range contractDirs {
			dir = canonicalSandboxPath(dir)
			if dir == "" {
				continue
			}
			if sandboxpolicy.PathContainsOrEqual(guest, dir) {
				return fmt.Errorf(
					"unsupported_sandbox_profile_mount_path: mounting %q at sandbox path %q would shadow the launch-required directory %q",
					grant.Path, guest, dir)
			}
		}
	}
	return nil
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
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	args, err := bwrapArgsWithDaemonFinal(
		phase0WriteDirs, plan, privateWriteDirs, finalHideDirs, readOnlyBinds,
		socketPaths, "", nil)
	if err != nil {
		return "", err
	}
	command := clcommon.ShellQuoteArg(binary)
	for _, arg := range args {
		command += " " + clcommon.ShellQuoteArg(arg)
	}
	// Harness spawners return a safe shell command rather than argv. Keep that
	// contract intact inside the namespace instead of reparsing it here.
	command += " -- /bin/sh -c " + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}
