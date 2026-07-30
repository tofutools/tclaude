package harness

import (
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

type EnforcementLevel int

const (
	EnforceNone EnforcementLevel = iota
	EnforcePartial
	EnforceFull
)

const (
	SandboxCapabilityNetworkAllowlist = "unsupported_sandbox_profile_network_allowlist"
	SandboxCapabilitySocketAllowlist  = "unsupported_sandbox_profile_socket_allowlist"
)

// AccessEnforcementOptions contains launch-boundary decisions that may widen
// the ordinary capability plan. Its only caller-settable field is deliberately
// specific to the exact refusal it can suppress; it is not a generic bypass
// for SandboxCapabilityError kinds.
type AccessEnforcementOptions struct {
	AllowUnenforcedNetworkClosed bool
}

// AccessEnforcement is an opaque launch-only capability token. Its fields stay
// private so callers cannot fabricate one or convert the display-only
// PredictedAccessEnforcement into it; ResolveAccessEnforcement is the only
// production constructor. Every plausible-but-unverified matrix entry remains
// EnforceNone until a pinned-harness verification is cited where it flips.
type AccessEnforcement struct {
	networkClosed           EnforcementLevel
	networkList             EnforcementLevel
	networkSelectors        []NetworkSelectorCapability
	networkPorts            EnforcementLevel
	networkDenySelectors    []NetworkSelectorCapability
	networkDenyPorts        EnforcementLevel
	networkListRefusal      string
	networkSelectorRefusal  string
	socketOpen              EnforcementLevel
	socketClosed            EnforcementLevel
	socketList              EnforcementLevel
	socketOpenRefusal       string
	socketListRefusal       string
	socketCombinationDetail string
	socketClosedRefusal     string
	constructedRoot         bool
	scope                   string
	mechanism               string
	mcpBypass               bool
}

// PredictedAccessEnforcement is display-only capability data. It is
// intentionally a distinct type with no conversion to AccessEnforcement:
// inspection has no live LaunchOSSandbox verdict and therefore cannot mint a
// value accepted by PlanAccessEnforcement.
type PredictedAccessEnforcement struct {
	NetworkClosed                EnforcementLevel
	NetworkList                  EnforcementLevel
	NetworkSelectors             []NetworkSelectorCapability
	NetworkPorts                 EnforcementLevel
	NetworkDenySelectors         []NetworkSelectorCapability
	NetworkDenyPorts             EnforcementLevel
	NetworkListRefusal           string
	NetworkListUnavailableDetail string
	NetworkSelectorRefusal       string
	NetworkListCondition         string
	SocketOpen                   EnforcementLevel
	SocketClosed                 EnforcementLevel
	SocketList                   EnforcementLevel
	SocketOpenRefusal            string
	SocketListRefusal            string
	SocketCombinationDetail      string
	SocketClosedRefusal          string
	// ConstructedRoot reports that this target builds its own filesystem root
	// rather than inheriting the host's. It is a whole-posture fact rather than
	// a per-rule verdict, and the preview needs it: the authored rules are all
	// still enforced, but everything the operator did NOT author stops being
	// visible, and that change has to be legible where they read their rules.
	ConstructedRoot bool
	Scope           string
	Mechanism       string
	MCPBypass       bool
	// NetworkEngine is the engine this policy deploys; see the table row field
	// of the same name.
	NetworkEngine sandboxpolicy.NetworkEngine
	// NetworkEngineDetail is the whole-posture engine sentence appended to the
	// network axis detail.
	NetworkEngineDetail string
}

const (
	AccessPredictionEnforced        = "enforced"
	AccessPredictionEnforcedPartial = "enforced_partial"
	AccessPredictionNotEnforced     = "not_enforced"
	AccessPredictionRefused         = "refused"
)

type PredictedAccessAxis struct {
	Tier    string `json:"tier"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

type PredictedAccessAxes struct {
	Filesystem       PredictedAccessAxis `json:"filesystem"`
	Environment      PredictedAccessAxis `json:"environment"`
	AgentDirectories PredictedAccessAxis `json:"agent_directories"`
	Network          PredictedAccessAxis `json:"network"`
	UnixSockets      PredictedAccessAxis `json:"unix_sockets"`
	// ConstructedRoot lets the preview state the filesystem consequence of a
	// built root as a RULE the operator can see, next to the rules they wrote,
	// rather than only as prose in a warning. Omitted when false so older
	// clients and inherited-root targets are unaffected.
	ConstructedRoot bool `json:"constructed_root,omitempty"`
}

// PredictedNetworkEntry projects the list-wide enforcement plan onto one
// materialized destination. Entry is included so editor clients can match the
// verdict after pack expansion without relying on positional indices.
type PredictedNetworkEntry struct {
	Entry   sandboxpolicy.NetworkAllowEntry `json:"entry"`
	Mode    string                          `json:"mode"`
	Keys    []string                        `json:"keys"`
	Outcome string                          `json:"outcome"`
	Detail  string                          `json:"detail"`
}

const PredictedNetworkDenyNotEnforcedDetail = "This deny rule is saved, but this launch target does not apply network deny entries; traffic matching this destination is not blocked by this rule. Choose Linux tclaude sandbox for enforced deny rules."

// OpenCodeFilteredExplicitProviderCaveat is the launch gate every OpenCode
// filtered row depends on. It is disclosed on the rows themselves so the
// effective-policy preview and the runtime cannot disagree about whether the
// rules will apply.
const OpenCodeFilteredExplicitProviderCaveat = "OpenCode additionally requires an explicit provider/model launch model and inline explicit-provider config; a launch without one is refused, not started with these rules dropped."

const FilteredNetworkDNSDenyDefaultAllowCaveat = "tclaude blocks addresses observed for this denied name through the sandbox DNS broker. With Allow all, another address for the same service, or encrypted DNS that bypasses the broker, can remain reachable. A blocked shared address also affects other names until the DNS lease expires."

type accessEnforcementTableRow struct {
	NetworkClosed                EnforcementLevel
	NetworkList                  EnforcementLevel
	NetworkSelectors             []NetworkSelectorCapability
	NetworkPorts                 EnforcementLevel
	NetworkDenySelectors         []NetworkSelectorCapability
	NetworkDenyPorts             EnforcementLevel
	NetworkListRefusal           string
	NetworkListUnavailableDetail string
	NetworkSelectorRefusal       string
	NetworkListCondition         string
	SocketOpen                   EnforcementLevel
	SocketClosed                 EnforcementLevel
	SocketList                   EnforcementLevel
	SocketOpenRefusal            string
	SocketListRefusal            string
	SocketCombinationDetail      string
	SocketClosedRefusal          string
	ConstructedRoot              bool
	Scope                        string
	Mechanism                    string
	MCPBypass                    bool
	// NetworkEngine is the engine this policy DEPLOYS, from
	// DeployedNetworkEngine — not the engine a layer selected. A selection on a
	// policy that needs no filtering deploys nothing and leaves this unset.
	NetworkEngine sandboxpolicy.NetworkEngine
	// NetworkEngineDetail is the whole-posture engine sentence for the network
	// axis. Empty whenever no layer selected an engine, which is what keeps an
	// engine-unset policy rendering exactly what it rendered before.
	NetworkEngineDetail string
}

// NetworkSelectorCapability is a mechanism's honest rating for one selector
// class. The stable Detail becomes the persisted per-entry disclosure whenever
// an adapter rates a class Partial or explicitly refuses a None class. An
// omitted class is also EnforceNone.
type NetworkSelectorCapability struct {
	Selector string           `json:"selector"`
	Level    EnforcementLevel `json:"level"`
	Detail   string           `json:"detail,omitempty"`
}

// BuiltinLaunchOSSandboxForValidatedMode mirrors the existing truth model for
// builtin sandboxes: their fully-resolved, validated requested mode is the
// verdict rather than a separately persisted LaunchOSSandbox record. Callers
// must pass the output of the normal sandbox-mode resolution and validation
// gates, never raw request parameters. Revisit this helper if builtin verdicts
// are ever persisted.
func BuiltinLaunchOSSandboxForValidatedMode(
	h *Harness,
	validatedMode string,
) (LaunchOSSandbox, error) {
	if err := ValidateHarnessBuiltinOSSandbox(h); err != nil {
		return LaunchOSSandbox{}, err
	}
	if h == nil || h.Sandbox == nil {
		return LaunchOSSandbox{}, fmt.Errorf("builtin sandbox has no validated mode catalog")
	}
	mode, err := h.Sandbox.ValidateMode(validatedMode)
	if err != nil {
		return LaunchOSSandbox{}, err
	}
	mode = strings.TrimSpace(mode)
	switch h.Name {
	case DefaultName:
		switch mode {
		case ClaudeSandboxOn:
			return LaunchOSSandbox{State: "on", Source: "validated builtin Claude Code sandbox mode"}, nil
		case ClaudeSandboxOff:
			return LaunchOSSandbox{State: "off", Source: "validated builtin Claude Code sandbox-off mode"}, nil
		default:
			return LaunchOSSandbox{State: "unconfigured", Source: "builtin Claude Code sandbox mode requires settings resolution"}, nil
		}
	case CodexName:
		switch mode {
		case SandboxManagedProfile, SandboxWorkspaceWrite, SandboxReadOnly:
			return LaunchOSSandbox{State: "on", Source: "validated builtin Codex sandbox mode " + mode}, nil
		case SandboxDangerFull:
			return LaunchOSSandbox{State: "off", Source: "validated builtin Codex danger-full-access mode"}, nil
		default:
			return LaunchOSSandbox{State: "unconfigured", Source: "builtin Codex sandbox mode was omitted"}, nil
		}
	default:
		return LaunchOSSandbox{}, fmt.Errorf("harness %q has no builtin access-enforcement verdict mapping", h.Name)
	}
}

// ResolveAccessEnforcement mints capability data only after rung 1 has
// produced positive OS-sandbox evidence. Outer implementations use their live
// LaunchOSSandbox verdict. Builtin implementations may derive the same evidence
// narrowly from a fully-resolved validated mode; OpenCode has no builtin OS
// sandbox and is refused by SupportsBuiltinOSSandbox before that derivation.
func ResolveAccessEnforcement(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	osSandbox LaunchOSSandbox,
	validatedBuiltinMode string,
) (AccessEnforcement, error) {
	if h == nil {
		return AccessEnforcement{}, fmt.Errorf("access enforcement requires a resolved harness")
	}
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin && osSandbox.State != "on" {
		var err error
		osSandbox, err = BuiltinLaunchOSSandboxForValidatedMode(h, validatedBuiltinMode)
		if err != nil {
			return AccessEnforcement{}, err
		}
	}
	if osSandbox.State != "on" {
		return AccessEnforcement{}, fmt.Errorf(
			"access enforcement requires a functioning OS sandbox; verdict is %q from %q",
			osSandbox.State, osSandbox.Source,
		)
	}
	row, err := accessEnforcementTable(
		h, implementation, axes, validatedBuiltinMode, runtime.GOOS,
		osSandbox.FilteredNetwork,
	)
	if err != nil {
		return AccessEnforcement{}, err
	}
	return accessEnforcementFromTable(row), nil
}

// PredictAccessEnforcement returns display-only capability data for a
// requested target. It never probes or fabricates a launch verdict.
func PredictAccessEnforcement(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	validatedBuiltinMode, platform string,
) (PredictedAccessEnforcement, error) {
	if h == nil {
		return PredictedAccessEnforcement{}, fmt.Errorf("access prediction requires a resolved harness")
	}
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin ||
		implementation.UsesNestedHarnessSandbox() {
		if err := ValidateHarnessBuiltinOSSandbox(h); err != nil {
			return PredictedAccessEnforcement{}, err
		}
	}
	row, err := accessEnforcementTable(
		h, implementation, axes, validatedBuiltinMode, platform,
		platform == "linux" ||
			(platform == "darwin" &&
				sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network)),
	)
	if err != nil {
		return PredictedAccessEnforcement{}, err
	}
	return predictedAccessEnforcementFromTable(row), nil
}

func accessEnforcementTable(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	validatedBuiltinMode, goos string,
	filteredNetworkReady bool,
) (accessEnforcementTableRow, error) {
	if implementation == sandboxpolicy.ImplementationOff {
		return accessEnforcementTableRow{
			NetworkClosed: EnforceNone,
			NetworkList:   EnforceNone,
			SocketOpen:    EnforceFull,
			SocketClosed:  EnforceNone,
			SocketList:    EnforceNone,
			Scope:         "process",
			Mechanism:     "sandbox off",
		}, nil
	}
	if implementation.UsesTclaudeLayer() {
		// One predicate, both consumers. The launch path reaches this same
		// function through ResolveAccessEnforcement, so preview and launch
		// cannot disagree about which engine a policy deploys — and therefore
		// cannot disagree about which mechanism the disclosure may name.
		deployedEngine, engineErr :=
			sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
		if engineErr != nil {
			return accessEnforcementTableRow{}, engineErr
		}
		mechanism := "tclaude-layer Seatbelt"
		if goos == "linux" {
			mechanism = "tclaude-layer bubblewrap"
		}
		if deployedEngine == sandboxpolicy.NetworkEngineProxy {
			mechanism = proxyEngineMechanism(goos)
		}
		caps := accessEnforcementTableRow{
			NetworkClosed: EnforceFull,
			NetworkList:   EnforceNone,
			// Socket capabilities are combination-aware: the closed-network
			// posture removes ambient sockets outside explicitly reopened
			// filesystem roots.
			SocketOpen:   EnforceFull,
			SocketClosed: EnforceNone,
			SocketList:   EnforceNone,
			Scope:        "process",
			Mechanism:    mechanism,
		}
		// Every packet-gateway rating below is gated on the deployed engine.
		// A policy that deploys a proxy is not enforced by pasta/nft/DNS-broker
		// machinery, so claiming those cells would describe a mechanism this
		// launch does not run. The proxy's own cells stay EnforceNone until
		// their carriage smokes land.
		packetGateway := deployedEngine != sandboxpolicy.NetworkEngineProxy
		if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
			packetGateway &&
			goos == "linux" && filteredNetworkReady &&
			(h.Name == DefaultName || h.Name == CodexName ||
				h.Name == OpenCodeName) {
			caps.NetworkList = EnforceFull
			caps.NetworkSelectors = []NetworkSelectorCapability{
				{
					Selector: string(sandboxpolicy.NetworkSelectorHost),
					Level:    EnforceFull,
					Detail:   filteredNetworkDNSCaveat(),
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorDomain),
					Level:    EnforceFull,
					Detail:   filteredNetworkDNSCaveat(),
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorCIDR),
					Level:    EnforceFull,
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorLoopback),
					Level:    EnforceFull,
					Detail:   FilteredNetworkLoopbackCaveat,
				},
			}
			caps.NetworkPorts = EnforceFull
			caps.NetworkListCondition =
				"At launch, bubblewrap, pasta, and nft must pass live checks. If any check fails, these rules are not enforced and outbound traffic is open."
			if h.Name == OpenCodeName {
				// Preview and runtime must not disagree: OpenCode reaches this
				// gateway only through an inspected explicit provider, and a
				// launch without one is refused rather than started unfiltered.
				caps.NetworkListCondition +=
					" " + OpenCodeFilteredExplicitProviderCaveat
			}
			caps.Mechanism = "tclaude-layer bubblewrap + supervised DNS/pasta/nftables gateway"
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
			packetGateway &&
			goos == "linux" && filteredNetworkReady &&
			(h.Name == DefaultName || h.Name == CodexName ||
				h.Name == OpenCodeName) {
			dnsDenyLevel := EnforceFull
			dnsDenyDetail := ""
			if axes.Network.Mode == sandboxpolicy.AccessModeOpen {
				dnsDenyLevel = EnforcePartial
				dnsDenyDetail = FilteredNetworkDNSDenyDefaultAllowCaveat
			}
			caps.NetworkDenySelectors = []NetworkSelectorCapability{
				{
					Selector: string(sandboxpolicy.NetworkSelectorHost),
					Level:    dnsDenyLevel,
					Detail:   dnsDenyDetail,
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorDomain),
					Level:    dnsDenyLevel,
					Detail:   dnsDenyDetail,
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorCIDR),
					Level:    EnforceFull,
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorLoopback),
					Level:    EnforceFull,
				},
			}
			if h.Name == OpenCodeName {
				// A deny-only profile has no allow list, so the launch-gate
				// disclosure on NetworkListCondition would never reach the
				// operator. Carry it on each deny selector instead: the row
				// says enforced, and the launch it depends on is refused rather
				// than started with the row dropped.
				for index := range caps.NetworkDenySelectors {
					caps.NetworkDenySelectors[index].Detail = strings.TrimSpace(
						caps.NetworkDenySelectors[index].Detail + " " +
							OpenCodeFilteredExplicitProviderCaveat)
				}
			}
			caps.NetworkDenyPorts = EnforceFull
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
			goos == "darwin" && filteredNetworkReady &&
			(h.Name == DefaultName || h.Name == CodexName) &&
			sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network) {
			caps.NetworkList = EnforceFull
			caps.NetworkSelectors = []NetworkSelectorCapability{{
				Selector: string(sandboxpolicy.NetworkSelectorLoopback),
				Level:    EnforceFull,
			}}
			caps.NetworkPorts = EnforceFull
			caps.Mechanism = "tclaude-layer Seatbelt native host-loopback filter"
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
			h.Name == OpenCodeName &&
			(IsLocalAccessNetworkPreset(axes.Network) ||
				IsLocalModelAPIsNetworkPreset(axes.Network)) {
			// General explicit-provider OpenCode filtering is supported on
			// Linux. These two convenience presets are narrower: they name no
			// explicit provider, and OpenCode exposes no effective-config read
			// to resolve one from, so advertising their packet capability would
			// make the rendered surface disagree with the launch-gated
			// model-transport refusal.
			caps.NetworkList = EnforceNone
			caps.NetworkSelectors = nil
			caps.NetworkPorts = EnforceNone
			caps.NetworkDenySelectors = nil
			caps.NetworkDenyPorts = EnforceNone
			caps.NetworkListCondition = ""
			caps.NetworkListRefusal =
				"missing capability unsupported_filtered_model_transport: OpenCode's local presets name no explicit provider and OpenCode exposes no effective-config read of its own loader, so their launch endpoint cannot be resolved; use an explicit-provider OpenCode config, use Claude Code or Codex with a resolvable provider, or use network open"
		}
		if axes.Network.Mode == sandboxpolicy.AccessModeClosed {
			caps.SocketClosed = EnforceFull
			// M3 materializes the resolved list at launch. Seatbelt provides
			// connect-level enforcement for the same paths.
			caps.SocketList = EnforceFull
			caps.SocketOpen = EnforceNone
			caps.SocketOpenRefusal =
				`ambient unix-socket access is not yet enforceable under closed network access on macOS tclaude-layer; ` +
					`leave unix_sockets unset (agentd only) or use open network access`
			if goos == "linux" {
				// Bubblewrap has no independent AF_UNIX connect filter.
				// Its constructed root hides sockets generally and binds listed
				// paths, but recursive readable/writable roots also expose any
				// sockets beneath them.
				caps.SocketClosed = EnforcePartial
				caps.SocketList = EnforcePartial
				// Name the posture in the mechanism itself. Every disclosure
				// that prints a mechanism — the launch degradation notice, the
				// predicted axis detail, the spawn warning — then says WHICH
				// boundary is active. An operator whose pre-existing
				// sockets+open profile starts behaving differently after an
				// upgrade learns why from the launch notes rather than from a
				// build that stopped working.
				caps.Mechanism = mechanism + " (host-network constructed root)"
				caps.SocketCombinationDetail =
					"listed Unix sockets are bound and sockets outside the sandbox's readable/writable directories remain hidden, " +
						"but sockets beneath those readable/writable directories remain reachable"
				caps.SocketOpenRefusal =
					`unix_sockets "open" cannot preserve ambient host socket visibility with closed network access on Linux tclaude-layer; ` +
						`use a socket access list or leave unix_sockets unset`
			}
		} else {
			if linuxHostOpenConstructedRootAvailable(
				h, implementation, axes, goos) {
				// TCL-798. The constructed root no longer requires giving up
				// host networking, so the filesystem half of the socket axis is
				// enforceable here. It is Partial rather than Full, and
				// permanently so: with the host network namespace shared,
				// Linux abstract-namespace sockets are not filesystem objects
				// at all and no mount plan can reach them.
				caps.SocketClosed = EnforcePartial
				caps.SocketList = EnforcePartial
				// Name the posture in the mechanism itself. Every disclosure
				// that prints a mechanism — the launch degradation notice, the
				// predicted axis detail, the spawn warning — then says WHICH
				// boundary is active. An operator whose pre-existing
				// sockets+open profile starts behaving differently after an
				// upgrade learns why from the launch notes rather than from a
				// build that stopped working.
				caps.Mechanism = mechanism + " (host-network constructed root)"
				caps.SocketCombinationDetail =
					"tclaude builds the sandbox root, so listed Unix sockets are bound and " +
						"ambient filesystem sockets are absent; sockets beneath the sandbox's own readable/writable " +
						"directories, and beneath the read-only OS surface it mounts (/usr, /bin, /sbin, /lib*, /etc, /opt), " +
						"remain reachable. Because host networking is kept, abstract-namespace Unix sockets (@…) live in the " +
						"shared network namespace rather than the filesystem and remain reachable — " +
						"close network access as well to confine those too. " +
						"Two consequences of building the root reach beyond sockets. " +
						"Host paths outside your filesystem grants and that OS surface are no longer visible at all, " +
						"where a host-open launch without this rule would have shown the whole read-only host root — " +
						"check that anything the agent needs (toolchains under your home, /var, /srv, /opt-style installs) is granted. " +
						"And the agent runs in its own PID namespace, which is required rather than incidental: " +
						"without it a host process's /proc/<pid>/root would lead straight back to the sockets the constructed root just hid. " +
						"The agent therefore cannot see or signal host processes, and tools that read the host process table stop working."
			} else if goos == "linux" {
				caps.SocketCombinationDetail =
					"Unix-socket restrictions are unenforced under host-open network on Linux tclaude-layer; " +
						"they are enforceable when network access is closed because that posture uses the constructed root"
				caps.SocketClosedRefusal =
					`unix_sockets "closed" cannot be enforced with open network access on Linux tclaude-layer; ` +
						"close network access as well, use an access list, or run without the socket restriction"
			} else {
				caps.SocketClosedRefusal =
					`unix_sockets "closed" is not yet enforceable with open network access on macOS tclaude-layer; ` +
						"close network access as well, use an access list (degrades, unenforced), or leave unix_sockets unset"
			}
			// Darwin has the required Seatbelt vocabulary, but M1's host-open
			// renderer wires no socket denies. The capability remains None
			// until that adapter consumes the authored axis.
		}
		// Whether this target builds its own root is decided once, from the
		// combination the launch will actually run, so the preview cannot
		// disagree with the applier. Linux only: Seatbelt is a path filter over
		// the host namespace and has no root to construct.
		if goos == "linux" {
			networkPosture, postureErr := sandboxpolicy.NetworkPostureForRules(
				axes.Network)
			socketTier := axes.UnixSockets.Mode
			if !linuxHostOpenConstructedRootAvailable(
				h, implementation, axes, goos) {
				// Without the mechanism the socket tier is about to be widened
				// away, so only the network posture's own implication counts.
				socketTier = sandboxpolicy.AccessModeUnset
			}
			caps.ConstructedRoot = postureErr == nil &&
				sandboxpolicy.RootPostureFor(networkPosture, socketTier) ==
					sandboxpolicy.RootConstructed
		}
		// Carried security item: an authored resolver socket restores in-sandbox
		// name-to-literal conversion and defeats the proxy engine's name
		// authority. This is the seam the invariant comment at the hosts-file
		// synthesis site names — the one place an authored engine and an
		// authored socket list meet — and it refuses through the socket axis so
		// preview and launch reach the verdict from the same row.
		if selector, resolver, conflict :=
			sandboxpolicy.NetworkEngineResolverSocketConflict(
				deployedEngine, axes.UnixSockets); conflict {
			caps.SocketList = EnforceNone
			caps.SocketListRefusal =
				sandboxpolicy.NetworkEngineResolverSocketRefusal(selector, resolver)
		}
		// The engine fields are set last so they describe the row as it ended
		// up, after every platform and harness branch above has had its say —
		// including a loopback-only policy on Darwin, which deploys no engine
		// at all and must keep its native mechanism sentence.
		caps.NetworkEngine = deployedEngine
		caps.NetworkEngineDetail = networkEngineDisclosure(
			axes.Network.Engine, deployedEngine)
		return caps, nil
	}
	switch h.Name {
	case DefaultName:
		return accessEnforcementTableRow{
			// Claude Code allowlist mechanisms are all ◑ in the design matrix
			// and therefore intentionally start disabled in M1.
			NetworkClosed: EnforceNone,
			NetworkList:   EnforceNone,
			SocketOpen:    EnforceFull,
			SocketClosed:  EnforceNone,
			SocketList:    EnforceNone,
			Scope:         "tools-only",
			Mechanism:     "Claude Code sandbox",
			MCPBypass:     true,
		}, nil
	case CodexName:
		caps := accessEnforcementTableRow{
			NetworkClosed:                EnforceNone,
			NetworkList:                  EnforceNone,
			NetworkListUnavailableDetail: CodexBuiltinFilteredNetworkDisclosure,
			SocketOpen:                   EnforceFull,
			SocketClosed:                 EnforceNone,
			SocketList:                   EnforceNone,
			Scope:                        "tools-only",
			Mechanism:                    "Codex builtin sandbox",
		}
		if strings.TrimSpace(validatedBuiltinMode) == SandboxManagedProfile {
			if goos == "darwin" {
				caps.NetworkClosed = EnforceFull
			}
			caps.SocketOpen = EnforceNone
			caps.SocketOpenRefusal =
				`ambient unix-socket access is not yet enforceable in the Codex managed profile; ` +
					`leave unix_sockets unset (agentd only) or choose a sandbox mode that preserves ambient sockets`
			caps.SocketClosed = EnforceFull
			// M3 feeds the launch-materialized profile list through Codex's
			// existing per-path filesystem read and network.unix_sockets
			// permission tables.
			caps.SocketList = EnforceFull
		}
		return caps, nil
	default:
		return accessEnforcementTableRow{}, fmt.Errorf("harness %q has no access-enforcement capability descriptor", h.Name)
	}
}

// linuxHostOpenConstructedRootAvailable reports whether this exact launch would
// get TCL-798's host-network constructed root, and therefore whether the socket
// axis may be rated above EnforceNone with the network axis left open.
//
// Three conditions, each load-bearing:
//
//   - tclaude-layer proper. The stacked implementation composes this root with
//     a harness-native inner sandbox whose interaction with a constructed
//     host-open root has no smoke evidence, and an unproven combination may not
//     raise a capability rating.
//   - Claude Code or Codex. OpenCode's boundary is its agentd-owned server with
//     a control plane of its own; its host-open arm is deliberately left on the
//     pre-TCL-798 path.
//   - A host-open network posture. An allow list or any deny renders the
//     filtered posture instead, which already constructs its root beneath a
//     private network namespace and is rated separately.
//
// The socket TIER is deliberately not part of the predicate. This function only
// answers whether the mechanism exists; the ladder applies the rating solely to
// a profile that explicitly authored a closed or list tier, which is what keeps
// the constructed root from turning itself on under profiles that never asked
// about sockets.
//
// It is exported through SupportsHostOpenConstructedRoot because the launch
// boundary has to ask the same question BEFORE the ladder runs — the capability
// verdict is one of the ladder's own inputs — and a launch that answered it
// differently from the table would probe for, and disclose, a root it is not
// going to build.
func linuxHostOpenConstructedRootAvailable(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	goos string,
) bool {
	if goos != "linux" {
		return false
	}
	if implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return false
	}
	if h == nil || (h.Name != DefaultName && h.Name != CodexName) {
		return false
	}
	posture, err := sandboxpolicy.NetworkPostureForRules(axes.Network)
	return err == nil && posture == sandboxpolicy.NetworkHostOpen
}

// SupportsHostOpenConstructedRoot reports whether this launch target would get
// TCL-798's host-network constructed root from an explicitly authored socket
// tier. Callers outside this package use it so the pre-ladder launch surfaces —
// the bubblewrap capability probe, the launch badge, the agent-socket
// environment — agree with the rating the ladder is about to compute.
//
// It answers only whether the MECHANISM applies to this target. Whether the
// profile actually asked for it is the socket tier's question, which
// sandboxpolicy.RootPostureFor answers.
func SupportsHostOpenConstructedRoot(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	goos string,
) bool {
	return linuxHostOpenConstructedRootAvailable(h, implementation, axes, goos)
}

func accessEnforcementFromTable(row accessEnforcementTableRow) AccessEnforcement {
	return AccessEnforcement{
		networkClosed: row.NetworkClosed, networkList: row.NetworkList,
		networkSelectors: cloneNetworkSelectorCapabilities(row.NetworkSelectors),
		networkPorts:     row.NetworkPorts,
		networkDenySelectors: cloneNetworkSelectorCapabilities(
			row.NetworkDenySelectors),
		networkDenyPorts:       row.NetworkDenyPorts,
		socketOpen:             row.SocketOpen,
		networkListRefusal:     row.NetworkListRefusal,
		networkSelectorRefusal: row.NetworkSelectorRefusal,
		socketClosed:           row.SocketClosed, socketList: row.SocketList,
		socketOpenRefusal:       row.SocketOpenRefusal,
		socketListRefusal:       row.SocketListRefusal,
		socketCombinationDetail: row.SocketCombinationDetail,
		socketClosedRefusal:     row.SocketClosedRefusal,
		constructedRoot:         row.ConstructedRoot,
		scope:                   row.Scope, mechanism: row.Mechanism, mcpBypass: row.MCPBypass,
	}
}

func predictedAccessEnforcementFromTable(row accessEnforcementTableRow) PredictedAccessEnforcement {
	return PredictedAccessEnforcement{
		NetworkClosed: row.NetworkClosed, NetworkList: row.NetworkList,
		NetworkSelectors: cloneNetworkSelectorCapabilities(row.NetworkSelectors),
		NetworkPorts:     row.NetworkPorts,
		NetworkDenySelectors: cloneNetworkSelectorCapabilities(
			row.NetworkDenySelectors),
		NetworkDenyPorts:             row.NetworkDenyPorts,
		SocketOpen:                   row.SocketOpen,
		NetworkListRefusal:           row.NetworkListRefusal,
		NetworkListUnavailableDetail: row.NetworkListUnavailableDetail,
		NetworkSelectorRefusal:       row.NetworkSelectorRefusal,
		NetworkListCondition:         row.NetworkListCondition,
		SocketClosed:                 row.SocketClosed, SocketList: row.SocketList,
		SocketOpenRefusal:       row.SocketOpenRefusal,
		SocketListRefusal:       row.SocketListRefusal,
		SocketCombinationDetail: row.SocketCombinationDetail,
		SocketClosedRefusal:     row.SocketClosedRefusal,
		ConstructedRoot:         row.ConstructedRoot,
		Scope:                   row.Scope, Mechanism: row.Mechanism, MCPBypass: row.MCPBypass,
		NetworkEngine:       row.NetworkEngine,
		NetworkEngineDetail: row.NetworkEngineDetail,
	}
}

// DescribePredictedAccess renders display-only outcomes. It deliberately does
// not call PlanAccessEnforcement and cannot produce an AccessEnforcement.
func DescribePredictedAccess(
	axes sandboxpolicy.ResolvedAxes,
	caps PredictedAccessEnforcement,
) PredictedAccessAxes {
	return PredictedAccessAxes{
		Network:         predictNetworkAxis(axes.Network, caps),
		UnixSockets:     predictSocketAxis(axes.UnixSockets, caps),
		ConstructedRoot: caps.ConstructedRoot,
	}
}

// DescribePredictedNetworkEntries describes the actual list plan per visible
// row. Some limitations apply to the whole list: an unsupported selector, for
// example, widens the resolved posture to open, so every row reports that it is
// not enforced rather than only marking the selector that triggered widening.
func DescribePredictedNetworkEntries(
	rules sandboxpolicy.NetworkRules,
	caps PredictedAccessEnforcement,
) []PredictedNetworkEntry {
	if rules.Mode != sandboxpolicy.AccessModeList {
		return nil
	}
	out := make([]PredictedNetworkEntry, len(rules.Allow))
	setAll := func(outcome, detail string) []PredictedNetworkEntry {
		for i, entry := range rules.Allow {
			out[i] = PredictedNetworkEntry{
				Entry: entry, Mode: "allow",
				Keys: []string{
					NetworkEntryModePredictionKey("allow", entry),
					NetworkEntryPredictionKey(entry),
				},
				Outcome: outcome, Detail: detail,
			}
		}
		return out
	}
	if caps.NetworkList == EnforceNone {
		if caps.NetworkListRefusal != "" {
			return setAll(AccessPredictionRefused, caps.NetworkListRefusal)
		}
		return setAll(AccessPredictionNotEnforced, networkListUnavailableDetail(caps))
	}
	unsupported := networkUnsupportedEntries(rules.Allow, caps.NetworkSelectors)
	if len(unsupported) > 0 {
		if caps.NetworkSelectorRefusal != "" {
			return setAll(AccessPredictionRefused, fmt.Sprintf(
				"%s; affected authored entries: %s",
				caps.NetworkSelectorRefusal, formatEntryIndices(unsupported),
			))
		}
		return setAll(AccessPredictionNotEnforced, fmt.Sprintf(
			"%s cannot express one or more destination selectors; all outbound connections are permitted; affected authored entries: %s",
			caps.Mechanism, formatEntryIndices(unsupported),
		))
	}
	for i, entry := range rules.Allow {
		capability, _ := networkSelectorCapability(
			caps.NetworkSelectors, networkSelectorForEntry(entry),
		)
		partialReasons := []string{}
		if caps.NetworkList == EnforcePartial {
			partialReasons = append(partialReasons, "the destination list is only partially enforced")
		}
		if capability.Level == EnforcePartial {
			detail := capability.Detail
			if detail == "" {
				detail = "the destination selector is only partially enforced"
			}
			partialReasons = append(partialReasons, detail)
		}
		if len(entry.Ports) > 0 && caps.NetworkPorts != EnforceFull {
			partialReasons = append(partialReasons, "the authored port restriction is not fully enforced")
		}
		// §5.3: a destination whose authored ports are not obviously HTTP-ish
		// is the one a proxy-unaware client is likely to open a socket to, and
		// under the proxy engine that traffic is blocked rather than filtered.
		if caps.NetworkEngine == sandboxpolicy.NetworkEngineProxy &&
			networkEntryNeedsProxyCarriageCaveat(entry) {
			partialReasons = append(partialReasons, ProxyEngineEntryCarriageDetail)
		}
		if caps.Scope == "tools-only" {
			partialReasons = append(partialReasons,
				"the restriction applies only to tool execution, not the harness process")
		}
		outcome := AccessPredictionEnforced
		detail := fmt.Sprintf("%s enforces this destination", caps.Mechanism)
		if len(partialReasons) > 0 {
			outcome = AccessPredictionEnforcedPartial
			detail = fmt.Sprintf("%s: %s", caps.Mechanism,
				strings.Join(partialReasons, " "))
		}
		if caps.NetworkListCondition != "" {
			detail = appendPredictionSentence(detail, caps.NetworkListCondition)
		}
		out[i] = PredictedNetworkEntry{
			Entry: entry, Mode: "allow",
			Keys: []string{
				NetworkEntryModePredictionKey("allow", entry),
				NetworkEntryPredictionKey(entry),
			},
			Outcome: outcome, Detail: detail,
		}
	}
	return out
}

// DescribePredictedNetworkDenyEntries projects the polarity-aware launch plan
// onto each authored deny row. Unsupported port-scoped rows stay whole in the
// editor but disclose omission rather than being widened into a broader block.
func DescribePredictedNetworkDenyEntries(
	entries []sandboxpolicy.NetworkAllowEntry,
	caps PredictedAccessEnforcement,
) []PredictedNetworkEntry {
	out := make([]PredictedNetworkEntry, len(entries))
	for i, entry := range entries {
		outcome := AccessPredictionNotEnforced
		detail := PredictedNetworkDenyNotEnforcedDetail
		capability, ok := networkSelectorCapability(
			caps.NetworkDenySelectors, networkSelectorForEntry(entry))
		switch {
		case !ok || capability.Level == EnforceNone:
			if ok && capability.Detail != "" {
				detail = capability.Detail
			}
		case len(entry.Ports) > 0 && caps.NetworkDenyPorts == EnforceNone:
			detail = "This port-scoped deny rule is not enforced because this target cannot express deny ports; it is omitted rather than widened into a whole-destination block."
		default:
			partialReasons := []string{}
			if capability.Level == EnforcePartial {
				reason := capability.Detail
				if reason == "" {
					reason = "the destination selector is only partially enforced"
				}
				partialReasons = append(partialReasons, reason)
			}
			if len(entry.Ports) > 0 &&
				caps.NetworkDenyPorts == EnforcePartial {
				partialReasons = append(partialReasons,
					"the deny port constraint is only partially enforced")
			}
			if caps.Scope == "tools-only" {
				partialReasons = append(partialReasons,
					"the restriction applies only to tool execution, not the harness process")
			}
			outcome = AccessPredictionEnforced
			detail = fmt.Sprintf("%s enforces this deny destination",
				caps.Mechanism)
			if len(partialReasons) > 0 {
				outcome = AccessPredictionEnforcedPartial
				detail = fmt.Sprintf("%s: %s", caps.Mechanism,
					strings.Join(partialReasons, " "))
			}
			if caps.NetworkListCondition != "" {
				detail = appendPredictionSentence(detail, caps.NetworkListCondition)
			}
		}
		out[i] = PredictedNetworkEntry{
			Entry: entry, Mode: "deny",
			Keys: []string{
				NetworkEntryModePredictionKey("deny", entry),
				NetworkEntryPredictionKey(entry),
			},
			Outcome: outcome,
			Detail:  detail,
		}
	}
	return out
}

// NetworkEntryPredictionKey is the stable editor reconciliation identity for
// one entry spelling. It preserves selector spelling (so an authored alias can
// be returned alongside the normalized key) while canonicalizing the
// order-insensitive port set.
func NetworkEntryPredictionKey(entry sandboxpolicy.NetworkAllowEntry) string {
	if len(entry.Ports) > 0 {
		ports := append([]int(nil), entry.Ports...)
		slices.Sort(ports)
		entry.Ports = slices.Compact(ports)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		panic("marshal network prediction key: " + err.Error())
	}
	return string(raw)
}

// NetworkEntryModePredictionKey prevents identical allow and deny selectors
// from aliasing in editor projections.
func NetworkEntryModePredictionKey(mode string, entry sandboxpolicy.NetworkAllowEntry) string {
	return mode + ":" + NetworkEntryPredictionKey(entry)
}

func predictNetworkAxis(
	rules sandboxpolicy.NetworkRules,
	caps PredictedAccessEnforcement,
) PredictedAccessAxis {
	return withNetworkEngineDetail(
		predictNetworkAxisWithoutEngine(rules, caps), caps)
}

// withNetworkEngineDetail appends the whole-posture engine sentence to a
// finished network axis. It runs after every other clause so the engine reads
// as a property of the posture rather than of one deny row, and it is a no-op
// for an engine-unset policy.
func withNetworkEngineDetail(
	axis PredictedAccessAxis,
	caps PredictedAccessEnforcement,
) PredictedAccessAxis {
	if caps.NetworkEngineDetail == "" {
		return axis
	}
	axis.Detail = appendPredictionSentence(axis.Detail, caps.NetworkEngineDetail)
	return axis
}

func predictNetworkAxisWithoutEngine(
	rules sandboxpolicy.NetworkRules,
	caps PredictedAccessEnforcement,
) PredictedAccessAxis {
	baseline := predictNetworkBaselineAxis(rules, caps)
	if len(rules.Deny) == 0 ||
		baseline.Outcome == AccessPredictionRefused {
		return baseline
	}
	denyRows := DescribePredictedNetworkDenyEntries(rules.Deny, caps)
	partialDetails := []string{}
	unsupportedDetails := []string{}
	for _, row := range denyRows {
		switch row.Outcome {
		case AccessPredictionEnforcedPartial:
			partialDetails = append(partialDetails, row.Detail)
		case AccessPredictionNotEnforced:
			unsupportedDetails = append(unsupportedDetails, row.Detail)
		}
	}
	switch {
	case len(unsupportedDetails) > 0:
		if baseline.Outcome == AccessPredictionEnforced {
			baseline.Outcome = AccessPredictionEnforcedPartial
		}
		baseline.Detail = appendPredictionSentence(baseline.Detail,
			"Deny limitation: "+
				joinPredictionSentences(uniquePredictionDetails(unsupportedDetails)))
	case len(partialDetails) > 0:
		if baseline.Outcome == AccessPredictionEnforced {
			baseline.Outcome = AccessPredictionEnforcedPartial
		}
		baseline.Detail = appendPredictionSentence(baseline.Detail,
			"Deny limitation: "+
				joinPredictionSentences(uniquePredictionDetails(partialDetails)))
	default:
		baseline.Detail = appendPredictionSentence(baseline.Detail,
			"Configured deny rules are enforced.")
	}
	return baseline
}

// appendPredictionSentence joins two independently-sourced disclosure clauses
// as separate sentences. Each clause is authored on its own (a baseline
// verdict, a launch-check condition, one deny row's limitation), so none of
// them can know whether it will be first, last, or in the middle. Without the
// terminator the composed line reads as one run-on sentence — "ambient
// outbound network access remains available Deny limitation: …" (TCL-864).
// Nothing is dropped or reworded; only the separator is added.
func appendPredictionSentence(base, next string) string {
	base = strings.TrimRight(base, " ")
	next = strings.TrimSpace(next)
	switch {
	case base == "":
		return next
	case next == "":
		return base
	}
	if !strings.HasSuffix(base, ".") && !strings.HasSuffix(base, "!") &&
		!strings.HasSuffix(base, "?") {
		base += "."
	}
	return base + " " + next
}

// joinPredictionSentences composes an ordered set of clauses with the same
// sentence separation appendPredictionSentence applies pairwise.
func joinPredictionSentences(values []string) string {
	out := ""
	for _, value := range values {
		out = appendPredictionSentence(out, value)
	}
	return out
}

func uniquePredictionDetails(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func predictNetworkBaselineAxis(
	rules sandboxpolicy.NetworkRules,
	caps PredictedAccessEnforcement,
) PredictedAccessAxis {
	tier := predictedTier(rules.Mode)
	switch rules.Mode {
	case sandboxpolicy.AccessModeUnset, sandboxpolicy.AccessModeOpen:
		return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionEnforced,
			Detail: "ambient outbound network access remains available"}
	case sandboxpolicy.AccessModeClosed:
		switch caps.NetworkClosed {
		case EnforceFull:
			return predictedEnforced(tier, caps.Mechanism, "closed network access")
		case EnforcePartial:
			return predictedPartial(tier, caps.Mechanism,
				"closed network access is only partially enforced; disclosed traffic remains reachable")
		default:
			return predictedRefused(tier, closedNetworkRefusal(
				caps.Mechanism, caps.Scope))
		}
	case sandboxpolicy.AccessModeList:
		if caps.NetworkList == EnforceNone {
			if caps.NetworkListRefusal != "" {
				return predictedRefused(tier, caps.NetworkListRefusal)
			}
			return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionNotEnforced,
				Detail: networkListUnavailableDetail(caps)}
		}
		unsupported := networkUnsupportedEntries(rules.Allow, caps.NetworkSelectors)
		if len(unsupported) > 0 {
			if caps.NetworkSelectorRefusal != "" {
				return predictedRefused(tier, fmt.Sprintf(
					"%s; affected authored entries: %s",
					caps.NetworkSelectorRefusal, formatEntryIndices(unsupported)))
			}
			return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionNotEnforced,
				Detail: fmt.Sprintf("%s cannot express one or more destination selectors; all outbound connections are permitted; affected authored entries: %s",
					caps.Mechanism, formatEntryIndices(unsupported))}
		}
		partialEntries, _ := networkPartialEntries(rules.Allow, caps.NetworkSelectors)
		if caps.NetworkList == EnforcePartial || len(partialEntries) > 0 ||
			(caps.NetworkPorts != EnforceFull && networkRulesHavePorts(rules)) ||
			caps.Scope == "tools-only" {
			return withNetworkListCondition(
				predictedPartial(tier, caps.Mechanism,
					"some rules allow more traffic than requested because this sandbox cannot narrow every destination, port, or process"),
				caps.NetworkListCondition,
			)
		}
		return withNetworkListCondition(
			predictedEnforced(tier, caps.Mechanism, "the network allow list"),
			caps.NetworkListCondition,
		)
	default:
		return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionRefused,
			Detail: "the network tier is invalid"}
	}
}

// CodexBuiltinFilteredNetworkDisclosure is display-only copy for the Codex
// harness-owned target. Codex's upstream proxy cannot honestly satisfy the
// ordinary TCP/UDP access-list contract, so the capability remains None and
// launch planning continues to widen to open exactly as before.
const CodexBuiltinFilteredNetworkDisclosure = "Codex has no filtered network sandbox yet. " +
	"Its upstream proxy is experimental and off by default; it admits only proxy-aware clients " +
	"and on Linux prevents access to the tclaude agentd socket, so it cannot enforce this profile's " +
	"ordinary TCP/UDP access list. Use tclaude-layer filtering on Linux, or choose network open (Allow all)."

func networkListUnavailableDetail(caps PredictedAccessEnforcement) string {
	if caps.NetworkListUnavailableDetail != "" {
		return caps.NetworkListUnavailableDetail
	}
	return fmt.Sprintf(
		"%s: no filtered-egress applier exists; all outbound connections are permitted",
		caps.Mechanism,
	)
}

func withNetworkListCondition(
	axis PredictedAccessAxis,
	condition string,
) PredictedAccessAxis {
	if condition != "" {
		axis.Detail = appendPredictionSentence(axis.Detail, condition)
	}
	return axis
}

func predictSocketAxis(
	rules sandboxpolicy.UnixSocketRules,
	caps PredictedAccessEnforcement,
) PredictedAccessAxis {
	tier := predictedTier(rules.Mode)
	switch rules.Mode {
	case sandboxpolicy.AccessModeUnset:
		return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionEnforced,
			Detail: "the sockets axis is unset; the existing sandbox posture is preserved"}
	case sandboxpolicy.AccessModeOpen:
		switch caps.SocketOpen {
		case EnforceFull:
			return predictedEnforced(tier, caps.Mechanism, "ambient host Unix-socket visibility")
		case EnforcePartial:
			return predictedPartial(tier, caps.Mechanism,
				"ambient host Unix-socket visibility is only partially preserved")
		default:
			detail := caps.SocketOpenRefusal
			if detail == "" {
				detail = fmt.Sprintf("%s cannot preserve ambient host Unix-socket visibility", caps.Mechanism)
			}
			return predictedRefused(tier, detail)
		}
	case sandboxpolicy.AccessModeClosed:
		switch caps.SocketClosed {
		case EnforceFull:
			return predictedEnforced(tier, caps.Mechanism, "closed Unix-socket access")
		case EnforcePartial:
			detail := caps.SocketCombinationDetail
			if detail == "" {
				detail = "some Unix-socket classes remain reachable"
			}
			return predictedPartial(tier, caps.Mechanism, detail)
		default:
			detail := caps.SocketClosedRefusal
			if detail == "" {
				detail = fmt.Sprintf("%s (%s scope) cannot enforce closed Unix-socket access",
					caps.Mechanism, caps.Scope)
			}
			return predictedRefused(tier, detail)
		}
	case sandboxpolicy.AccessModeList:
		if caps.SocketList == EnforceNone {
			if caps.SocketListRefusal != "" {
				return predictedRefused(tier, caps.SocketListRefusal)
			}
			detail := caps.SocketCombinationDetail
			if detail == "" {
				detail = fmt.Sprintf("%s cannot enforce the Unix-socket allow list; all filesystem-visible sockets remain reachable",
					caps.Mechanism)
			}
			return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionNotEnforced, Detail: detail}
		}
		if caps.SocketList == EnforcePartial {
			detail := caps.SocketCombinationDetail
			if detail == "" {
				detail = "the Unix-socket allow list is enforced with wider socket scope"
			}
			return predictedPartial(tier, caps.Mechanism, detail)
		}
		if caps.Scope == "tools-only" {
			return predictedPartial(tier, caps.Mechanism,
				"the Unix-socket allow list applies only to tool execution, not the harness process")
		}
		return predictedEnforced(tier, caps.Mechanism, "the Unix-socket allow list")
	default:
		return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionRefused,
			Detail: "the Unix-socket tier is invalid"}
	}
}

func predictedTier(mode sandboxpolicy.AccessMode) string {
	if mode == sandboxpolicy.AccessModeUnset {
		return "unset"
	}
	return string(mode)
}

func predictedEnforced(tier, mechanism, subject string) PredictedAccessAxis {
	return PredictedAccessAxis{
		Tier: tier, Outcome: AccessPredictionEnforced,
		Detail: fmt.Sprintf("%s enforces %s", mechanism, subject),
	}
}

func predictedPartial(tier, mechanism, detail string) PredictedAccessAxis {
	return PredictedAccessAxis{
		Tier: tier, Outcome: AccessPredictionEnforcedPartial,
		Detail: fmt.Sprintf("%s: %s", mechanism, detail),
	}
}

func predictedRefused(tier, detail string) PredictedAccessAxis {
	return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionRefused, Detail: detail}
}

func networkRulesHavePorts(rules sandboxpolicy.NetworkRules) bool {
	for _, entry := range rules.Allow {
		if len(entry.Ports) > 0 {
			return true
		}
	}
	return false
}

// PlanAccessEnforcement applies the widening-only degradation ladder. A zero
// capability literal cannot bypass rung 1: the non-optional mechanism is set
// only by ResolveAccessEnforcement after positive sandbox evidence.
func PlanAccessEnforcement(
	axes sandboxpolicy.ResolvedAxes,
	caps AccessEnforcement,
	options ...AccessEnforcementOptions,
) (sandboxpolicy.ResolvedAxes, []sandboxpolicy.AccessNotice, error) {
	if strings.TrimSpace(caps.mechanism) == "" {
		return sandboxpolicy.ResolvedAxes{}, nil, fmt.Errorf(
			"access enforcement capabilities were not resolved through the sandbox implementation gate",
		)
	}
	rendered := cloneResolvedAxes(axes)
	notices := []sandboxpolicy.AccessNotice{}

	allowUnenforcedNetworkClosed := len(options) > 0 &&
		options[0].AllowUnenforcedNetworkClosed
	if axes.Network.Mode == sandboxpolicy.AccessModeClosed && caps.networkClosed == EnforceNone {
		if !allowUnenforcedNetworkClosed {
			return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
				Kind:    SandboxCapabilityNetworkAllowlist,
				Message: closedNetworkRefusal(caps.mechanism, caps.scope),
			}
		}
		rendered.Network = sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen, Engine: axes.Network.Engine,
		}
		notices = append(notices, degradationNotice(
			"network",
			sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
			sandboxpolicy.AccessNoticeEffectNotEnforced,
			caps,
			"the human operator used the dashboard launch override; closed network access is not enforced and outbound network access remains open",
			nil,
		))
	}
	if axes.Network.Mode == sandboxpolicy.AccessModeClosed && caps.networkClosed == EnforcePartial {
		notices = append(notices, degradationNotice(
			"network", "partial_mechanism", sandboxpolicy.AccessNoticeEffectEnforcedWider,
			caps, "closed network access is partially enforced; the mechanism leaves disclosed traffic reachable", nil,
		))
	}
	if axes.UnixSockets.Mode == sandboxpolicy.AccessModeOpen && caps.socketOpen == EnforceNone {
		detail := caps.socketOpenRefusal
		if detail == "" {
			detail = fmt.Sprintf(
				"%s (%s scope) cannot preserve ambient host Unix-socket visibility",
				caps.mechanism, caps.scope,
			)
		}
		return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
			Kind:    SandboxCapabilitySocketAllowlist,
			Message: detail,
		}
	}
	if axes.UnixSockets.Mode == sandboxpolicy.AccessModeOpen && caps.socketOpen == EnforcePartial {
		notices = append(notices, degradationNotice(
			"unix_sockets", "partial_ambient_visibility", sandboxpolicy.AccessNoticeEffectEnforcedWider,
			caps, "ambient host Unix-socket visibility is only partially preserved", nil,
		))
	}
	if axes.UnixSockets.Mode == sandboxpolicy.AccessModeClosed && caps.socketClosed == EnforceNone {
		detail := caps.socketClosedRefusal
		if detail == "" {
			detail = fmt.Sprintf(
				"%s (%s scope) cannot enforce closed Unix-socket access",
				caps.mechanism, caps.scope,
			)
		}
		return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
			Kind:    SandboxCapabilitySocketAllowlist,
			Message: detail,
		}
	}
	if axes.UnixSockets.Mode == sandboxpolicy.AccessModeClosed && caps.socketClosed == EnforcePartial {
		detail := caps.socketCombinationDetail
		if detail == "" {
			detail = "closed Unix-socket access is partially enforced; some socket classes remain reachable"
		}
		notices = append(notices, degradationNotice(
			"unix_sockets", "partial_mechanism", sandboxpolicy.AccessNoticeEffectEnforcedWider,
			caps, detail, nil,
		))
	}

	if axes.Network.Mode == sandboxpolicy.AccessModeList {
		switch caps.networkList {
		case EnforceNone:
			if caps.networkListRefusal != "" {
				return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
					Kind:    SandboxCapabilityNetworkAllowlist,
					Message: caps.networkListRefusal,
				}
			}
			// Widening drops destinations, never the authored mechanism. The
			// engine has to survive every rendered form of the policy because
			// the launch decides what to deploy from these axes: a widened
			// policy that lost its engine would deploy the pre-engine default
			// while the preview named the authored one.
			rendered.Network = sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeOpen,
				Deny:   cloneNetworkEntries(axes.Network.Deny),
				Engine: axes.Network.Engine,
			}
			notices = append(notices, degradationNotice(
				"network", "no_mechanism", sandboxpolicy.AccessNoticeEffectNotEnforced,
				caps, "the network allow list is not enforced; outbound network access remains open", nil,
			))
		default:
			unsupported := networkUnsupportedEntries(axes.Network.Allow, caps.networkSelectors)
			if len(unsupported) > 0 {
				if caps.networkSelectorRefusal != "" {
					return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
						Kind: SandboxCapabilityNetworkAllowlist,
						Message: fmt.Sprintf(
							"%s; affected authored entries: %s",
							caps.networkSelectorRefusal,
							formatEntryIndices(unsupported),
						),
					}
				}
				rendered.Network = sandboxpolicy.NetworkRules{
					Mode:   sandboxpolicy.AccessModeOpen,
					Deny:   cloneNetworkEntries(axes.Network.Deny),
					Engine: axes.Network.Engine,
				}
				notices = append(notices, degradationNotice(
					"network", "selector_unsupported", sandboxpolicy.AccessNoticeEffectNotEnforced,
					caps, "the network allow list contains selectors this mechanism cannot express; outbound network access remains open",
					unsupported,
				))
			} else {
				partialEntries, partialDetail := networkPartialEntries(
					axes.Network.Allow, caps.networkSelectors)
				if len(partialEntries) > 0 {
					notices = append(notices, degradationNotice(
						"network", "selector_partial",
						sandboxpolicy.AccessNoticeEffectEnforcedWider,
						caps, partialDetail, partialEntries,
					))
				}
			}
			if len(unsupported) == 0 && caps.networkPorts != EnforceFull {
				affected := []int{}
				for i := range rendered.Network.Allow {
					if len(rendered.Network.Allow[i].Ports) == 0 {
						continue
					}
					if caps.networkPorts == EnforceNone {
						rendered.Network.Allow[i].Ports = nil
					}
					affected = append(affected, i)
				}
				if len(affected) > 0 {
					detail := "destination selectors are enforced, but port constraints are not; matching destinations are reachable on every port"
					reason := "ports_unsupported"
					if caps.networkPorts == EnforcePartial {
						detail = "destination port constraints are partially enforced; the mechanism leaves the disclosed port scope reachable"
						reason = "ports_partial"
					}
					notices = append(notices, degradationNotice(
						"network", reason, sandboxpolicy.AccessNoticeEffectEnforcedWider,
						caps, detail,
						affected,
					))
				}
			}
		}
		if caps.scope == "tools-only" && rendered.Network.Mode != sandboxpolicy.AccessModeOpen {
			notices = append(notices, degradationNotice(
				"network", "tools_only_scope", sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, "the network allow list applies only to tool execution, not the harness process", nil,
			))
		}
	}

	if len(axes.Network.Deny) > 0 {
		kept := make([]sandboxpolicy.NetworkAllowEntry, 0, len(axes.Network.Deny))
		selectorOmitted := []int{}
		portOmitted := []int{}
		selectorPartial := []int{}
		portPartial := []int{}
		for i, entry := range axes.Network.Deny {
			capability, ok := networkSelectorCapability(
				caps.networkDenySelectors, networkSelectorForEntry(entry))
			if !ok || capability.Level == EnforceNone {
				selectorOmitted = append(selectorOmitted, i)
				continue
			}
			if len(entry.Ports) > 0 && caps.networkDenyPorts == EnforceNone {
				portOmitted = append(portOmitted, i)
				continue
			}
			kept = append(kept, cloneNetworkEntry(entry))
			if capability.Level == EnforcePartial {
				selectorPartial = append(selectorPartial, i)
			}
			if len(entry.Ports) > 0 && caps.networkDenyPorts == EnforcePartial {
				portPartial = append(portPartial, i)
			}
		}
		rendered.Network.Deny = kept
		if len(selectorOmitted) > 0 {
			notices = append(notices, degradationNotice(
				"network", "deny_selector_unsupported",
				sandboxpolicy.AccessNoticeEffectNotEnforced,
				caps, "the listed deny destinations cannot be expressed and are omitted; other supported deny entries remain active",
				selectorOmitted,
			))
		}
		if len(portOmitted) > 0 {
			notices = append(notices, degradationNotice(
				"network", "deny_ports_unsupported",
				sandboxpolicy.AccessNoticeEffectNotEnforced,
				caps, "the listed port-scoped deny destinations cannot be expressed and are omitted; they are not widened into whole-destination blocks",
				portOmitted,
			))
		}
		if len(selectorPartial) > 0 {
			notices = append(notices, degradationNotice(
				"network", "deny_selector_partial",
				sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, "the listed deny destinations are only partially enforced",
				selectorPartial,
			))
		}
		if len(portPartial) > 0 {
			notices = append(notices, degradationNotice(
				"network", "deny_ports_partial",
				sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, "the listed deny port constraints are only partially enforced",
				portPartial,
			))
		}
	}

	if axes.UnixSockets.Mode == sandboxpolicy.AccessModeList {
		if caps.socketList == EnforceNone {
			if caps.socketListRefusal != "" {
				return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
					Kind:    SandboxCapabilitySocketAllowlist,
					Message: caps.socketListRefusal,
				}
			}
			rendered.UnixSockets = sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeOpen}
			detail := "the Unix-socket allow list is not enforced; all filesystem-visible sockets remain reachable"
			if caps.socketCombinationDetail != "" {
				detail = caps.socketCombinationDetail
			}
			notices = append(notices, degradationNotice(
				"unix_sockets", "no_mechanism", sandboxpolicy.AccessNoticeEffectNotEnforced,
				caps, detail, nil,
			))
		} else if caps.socketList == EnforcePartial {
			detail := caps.socketCombinationDetail
			if detail == "" {
				detail = "the Unix-socket allow list is partially enforced; some socket classes remain reachable"
			}
			notices = append(notices, degradationNotice(
				"unix_sockets", "partial_mechanism", sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, detail, nil,
			))
		} else if caps.scope == "tools-only" {
			notices = append(notices, degradationNotice(
				"unix_sockets", "tools_only_scope", sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, "the Unix-socket allow list applies only to tool execution, not the harness process", nil,
			))
		}
	}
	return rendered, notices, nil
}

func closedNetworkRefusal(mechanism, scope string) string {
	return fmt.Sprintf(
		"%s (%s scope) cannot enforce closed network access; choose a sandbox implementation that can enforce closed network access, use network open, or enable “Allow launch without enforcement” in the dashboard spawn dialog",
		mechanism, scope,
	)
}

func formatEntryIndices(indices []int) string {
	parts := make([]string, len(indices))
	for i, index := range indices {
		parts[i] = fmt.Sprintf("%d", index)
	}
	return strings.Join(parts, ", ")
}

func degradationNotice(
	axis, reason, effect string,
	caps AccessEnforcement,
	consequence string,
	entries []int,
) sandboxpolicy.AccessNotice {
	scope := caps.scope + " scope"
	if caps.mcpBypass {
		scope += "; MCP servers bypass this mechanism"
	}
	return sandboxpolicy.AccessNotice{
		Class:   sandboxpolicy.AccessNoticeClassDegradation,
		Axis:    axis,
		Reason:  reason,
		Effect:  effect,
		Detail:  fmt.Sprintf("%s (%s): %s", caps.mechanism, scope, consequence),
		Entries: append([]int(nil), entries...),
	}
}

func networkUnsupportedEntries(
	entries []sandboxpolicy.NetworkAllowEntry,
	selectors []NetworkSelectorCapability,
) []int {
	out := []int{}
	for i, entry := range entries {
		capability, ok := networkSelectorCapability(
			selectors, networkSelectorForEntry(entry))
		if !ok || capability.Level == EnforceNone {
			out = append(out, i)
		}
	}
	return out
}

func networkPartialEntries(
	entries []sandboxpolicy.NetworkAllowEntry,
	selectors []NetworkSelectorCapability,
) ([]int, string) {
	out := []int{}
	details := []string{}
	for i, entry := range entries {
		capability, ok := networkSelectorCapability(
			selectors, networkSelectorForEntry(entry))
		if !ok || capability.Level != EnforcePartial {
			continue
		}
		out = append(out, i)
		if capability.Detail != "" && !slices.Contains(details, capability.Detail) {
			details = append(details, capability.Detail)
		}
	}
	detail := "one or more destination selectors are partially enforced"
	if len(details) > 0 {
		detail = strings.Join(details, " ")
	}
	return out, detail
}

func networkSelectorForEntry(entry sandboxpolicy.NetworkAllowEntry) string {
	switch {
	case entry.Host != "":
		return string(sandboxpolicy.NetworkSelectorHost)
	case entry.Domain != "":
		return string(sandboxpolicy.NetworkSelectorDomain)
	case entry.CIDR != "":
		return string(sandboxpolicy.NetworkSelectorCIDR)
	default:
		return string(sandboxpolicy.NetworkSelectorLoopback)
	}
}

func networkSelectorCapability(
	capabilities []NetworkSelectorCapability,
	selector string,
) (NetworkSelectorCapability, bool) {
	for _, capability := range capabilities {
		if capability.Selector == selector {
			return capability, true
		}
	}
	return NetworkSelectorCapability{}, false
}

func cloneNetworkSelectorCapabilities(
	in []NetworkSelectorCapability,
) []NetworkSelectorCapability {
	return append([]NetworkSelectorCapability(nil), in...)
}

func cloneResolvedAxes(in sandboxpolicy.ResolvedAxes) sandboxpolicy.ResolvedAxes {
	out := in
	out.Network.Allow = cloneNetworkEntries(in.Network.Allow)
	out.Network.Deny = cloneNetworkEntries(in.Network.Deny)
	out.UnixSockets.Allow = append(
		[]sandboxpolicy.SocketAllowEntry(nil),
		in.UnixSockets.Allow...,
	)
	return out
}

func cloneNetworkEntries(
	in []sandboxpolicy.NetworkAllowEntry,
) []sandboxpolicy.NetworkAllowEntry {
	if in == nil {
		return nil
	}
	out := make([]sandboxpolicy.NetworkAllowEntry, len(in))
	for i, entry := range in {
		out[i] = cloneNetworkEntry(entry)
	}
	return out
}

func cloneNetworkEntry(
	in sandboxpolicy.NetworkAllowEntry,
) sandboxpolicy.NetworkAllowEntry {
	out := in
	out.Ports = append([]int(nil), in.Ports...)
	return out
}
