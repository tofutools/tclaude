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
	networkListRefusal      string
	networkSelectorRefusal  string
	socketOpen              EnforcementLevel
	socketClosed            EnforcementLevel
	socketList              EnforcementLevel
	socketOpenRefusal       string
	socketListRefusal       string
	socketCombinationDetail string
	socketClosedRefusal     string
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
	Scope                        string
	Mechanism                    string
	MCPBypass                    bool
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

const PredictedNetworkDenyNotEnforcedDetail = "This deny rule is authored but this release does not apply deny entries; traffic matching this destination is not blocked by this rule."

type accessEnforcementTableRow struct {
	NetworkClosed                EnforcementLevel
	NetworkList                  EnforcementLevel
	NetworkSelectors             []NetworkSelectorCapability
	NetworkPorts                 EnforcementLevel
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
	Scope                        string
	Mechanism                    string
	MCPBypass                    bool
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
	if implementation.UsesTclaudeLayer() {
		mechanism := "tclaude-layer Seatbelt"
		if goos == "linux" {
			mechanism = "tclaude-layer bubblewrap"
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
		if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
			goos == "linux" && filteredNetworkReady &&
			(h.Name == DefaultName || h.Name == CodexName ||
				h.Name == OpenCodeName) {
			caps.NetworkList = EnforceFull
			caps.NetworkSelectors = []NetworkSelectorCapability{
				{
					Selector: string(sandboxpolicy.NetworkSelectorHost),
					Level:    EnforcePartial,
					Detail:   filteredNetworkDNSCaveat(),
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorDomain),
					Level:    EnforcePartial,
					Detail:   filteredNetworkDNSCaveat(),
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorCIDR),
					Level:    EnforceFull,
				},
				{
					Selector: string(sandboxpolicy.NetworkSelectorLoopback),
					Level:    EnforcePartial,
					Detail:   FilteredNetworkLoopbackCaveat,
				},
			}
			caps.NetworkPorts = EnforceFull
			caps.NetworkListCondition =
				"Prerequisite-conditional prediction: the exact launch must pass live bubblewrap namespace, trusted pasta, and trusted nft probes; otherwise the authored allow list remains unenforced and outbound remains open."
			caps.Mechanism = "tclaude-layer bubblewrap + supervised DNS/pasta/nftables gateway"
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
			// Linux. These two convenience presets are narrower: until
			// TCL-826 resolves OpenCode's effective local-provider endpoint,
			// advertising their packet capability would make the rendered
			// surface disagree with the launch-gated model-transport refusal.
			caps.NetworkList = EnforceNone
			caps.NetworkSelectors = nil
			caps.NetworkPorts = EnforceNone
			caps.NetworkListCondition = ""
			caps.NetworkListRefusal =
				"missing capability unsupported_filtered_model_transport: OpenCode local-preset effective-config model transport resolution is tracked in TCL-826; use Claude Code or Codex with a resolvable provider, or use network open"
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
				caps.SocketCombinationDetail =
					"listed Unix sockets are bound and sockets outside the sandbox's readable/writable directories remain hidden, " +
						"but sockets beneath those readable/writable directories remain reachable"
				caps.SocketOpenRefusal =
					`unix_sockets "open" cannot preserve ambient host socket visibility with closed network access on Linux tclaude-layer; ` +
						`use a socket access list or leave unix_sockets unset`
			}
		} else {
			if goos == "linux" {
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

func accessEnforcementFromTable(row accessEnforcementTableRow) AccessEnforcement {
	return AccessEnforcement{
		networkClosed: row.NetworkClosed, networkList: row.NetworkList,
		networkSelectors: cloneNetworkSelectorCapabilities(row.NetworkSelectors),
		networkPorts:     row.NetworkPorts, socketOpen: row.SocketOpen,
		networkListRefusal:     row.NetworkListRefusal,
		networkSelectorRefusal: row.NetworkSelectorRefusal,
		socketClosed:           row.SocketClosed, socketList: row.SocketList,
		socketOpenRefusal:       row.SocketOpenRefusal,
		socketListRefusal:       row.SocketListRefusal,
		socketCombinationDetail: row.SocketCombinationDetail,
		socketClosedRefusal:     row.SocketClosedRefusal,
		scope:                   row.Scope, mechanism: row.Mechanism, mcpBypass: row.MCPBypass,
	}
}

func predictedAccessEnforcementFromTable(row accessEnforcementTableRow) PredictedAccessEnforcement {
	return PredictedAccessEnforcement{
		NetworkClosed: row.NetworkClosed, NetworkList: row.NetworkList,
		NetworkSelectors: cloneNetworkSelectorCapabilities(row.NetworkSelectors),
		NetworkPorts:     row.NetworkPorts, SocketOpen: row.SocketOpen,
		NetworkListRefusal:           row.NetworkListRefusal,
		NetworkListUnavailableDetail: row.NetworkListUnavailableDetail,
		NetworkSelectorRefusal:       row.NetworkSelectorRefusal,
		NetworkListCondition:         row.NetworkListCondition,
		SocketClosed:                 row.SocketClosed, SocketList: row.SocketList,
		SocketOpenRefusal:       row.SocketOpenRefusal,
		SocketListRefusal:       row.SocketListRefusal,
		SocketCombinationDetail: row.SocketCombinationDetail,
		SocketClosedRefusal:     row.SocketClosedRefusal,
		Scope:                   row.Scope, Mechanism: row.Mechanism, MCPBypass: row.MCPBypass,
	}
}

// DescribePredictedAccess renders display-only outcomes. It deliberately does
// not call PlanAccessEnforcement and cannot produce an AccessEnforcement.
func DescribePredictedAccess(
	axes sandboxpolicy.ResolvedAxes,
	caps PredictedAccessEnforcement,
) PredictedAccessAxes {
	return PredictedAccessAxes{
		Network:     predictNetworkAxis(axes.Network, caps),
		UnixSockets: predictSocketAxis(axes.UnixSockets, caps),
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
			detail += " " + caps.NetworkListCondition
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

// DescribePredictedNetworkDenyEntries projects authored deny rows without
// consulting capability cells. TCL-839 persists deny intent before appliers
// consume it, so every row must disclose that it does not currently block
// traffic.
func DescribePredictedNetworkDenyEntries(entries []sandboxpolicy.NetworkAllowEntry) []PredictedNetworkEntry {
	out := make([]PredictedNetworkEntry, len(entries))
	for i, entry := range entries {
		out[i] = PredictedNetworkEntry{
			Entry: entry, Mode: "deny",
			Keys: []string{
				NetworkEntryModePredictionKey("deny", entry),
				NetworkEntryPredictionKey(entry),
			},
			Outcome: AccessPredictionNotEnforced,
			Detail:  PredictedNetworkDenyNotEnforcedDetail,
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
					"the allow list is enforced with wider destination, port, or process scope"),
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
		axis.Detail += " " + condition
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
		rendered.Network = sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen}
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
			rendered.Network = sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen}
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
				rendered.Network = sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen}
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
	out.Network.Allow = make([]sandboxpolicy.NetworkAllowEntry, len(in.Network.Allow))
	for i, entry := range in.Network.Allow {
		out.Network.Allow[i] = entry
		out.Network.Allow[i].Ports = append([]int(nil), entry.Ports...)
	}
	out.UnixSockets.Allow = append(
		[]sandboxpolicy.SocketAllowEntry(nil),
		in.UnixSockets.Allow...,
	)
	return out
}
