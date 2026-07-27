package harness

import (
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

// AccessEnforcement is an opaque launch-only capability token. Its fields stay
// private so callers cannot fabricate one or convert the display-only
// PredictedAccessEnforcement into it; ResolveAccessEnforcement is the only
// production constructor. Every plausible-but-unverified matrix entry remains
// EnforceNone until a pinned-harness verification is cited where it flips.
type AccessEnforcement struct {
	networkClosed           EnforcementLevel
	networkList             EnforcementLevel
	networkSelectors        []string
	networkPorts            bool
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
	NetworkClosed           EnforcementLevel
	NetworkList             EnforcementLevel
	NetworkSelectors        []string
	NetworkPorts            bool
	SocketOpen              EnforcementLevel
	SocketClosed            EnforcementLevel
	SocketList              EnforcementLevel
	SocketOpenRefusal       string
	SocketListRefusal       string
	SocketCombinationDetail string
	SocketClosedRefusal     string
	Scope                   string
	Mechanism               string
	MCPBypass               bool
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
	Network     PredictedAccessAxis `json:"network"`
	UnixSockets PredictedAccessAxis `json:"unix_sockets"`
}

type accessEnforcementTableRow struct {
	NetworkClosed           EnforcementLevel
	NetworkList             EnforcementLevel
	NetworkSelectors        []string
	NetworkPorts            bool
	SocketOpen              EnforcementLevel
	SocketClosed            EnforcementLevel
	SocketList              EnforcementLevel
	SocketOpenRefusal       string
	SocketListRefusal       string
	SocketCombinationDetail string
	SocketClosedRefusal     string
	Scope                   string
	Mechanism               string
	MCPBypass               bool
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
// narrowly from a fully-resolved validated mode; OpenCode is refused by
// SupportsBuiltinOSSandbox before that derivation.
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
	row, err := accessEnforcementTable(h, implementation, axes, validatedBuiltinMode, runtime.GOOS)
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
	row, err := accessEnforcementTable(h, implementation, axes, validatedBuiltinMode, platform)
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
) (accessEnforcementTableRow, error) {
	if implementation.UsesTclaudeLayer() {
		mechanism := "tclaude-layer Seatbelt"
		if goos == "linux" {
			mechanism = "tclaude-layer bubblewrap"
		}
		caps := accessEnforcementTableRow{
			NetworkClosed: EnforceFull,
			// NetworkFiltered remains reserved and refused by every applier.
			NetworkList: EnforceNone,
			// Socket capabilities are combination-aware: on Linux and in the
			// current Seatbelt renderer, the closed-network posture is the one
			// already verified to retain only the agentd pathname sockets.
			SocketOpen:   EnforceFull,
			SocketClosed: EnforceNone,
			SocketList:   EnforceNone,
			Scope:        "process",
			Mechanism:    mechanism,
		}
		if axes.Network.Mode == sandboxpolicy.AccessModeClosed {
			caps.SocketClosed = EnforceFull
			caps.SocketOpen = EnforceNone
			caps.SocketListRefusal =
				"unix-socket access lists are not yet enforceable under closed network access on macOS tclaude-layer; " +
					"leave unix_sockets unset (agentd only) or use open network access (list degrades, unenforced)"
			caps.SocketOpenRefusal =
				`ambient unix-socket access is not yet enforceable under closed network access on macOS tclaude-layer; ` +
					`leave unix_sockets unset (agentd only) or use open network access`
			if goos == "linux" {
				// The constructed root intentionally removes ambient host
				// socket visibility. That satisfies an unset socket axis and
				// closed sockets, but cannot satisfy explicitly-authored open.
				caps.SocketOpenRefusal =
					`unix_sockets "open" cannot preserve ambient host socket visibility with closed network access on Linux tclaude-layer; ` +
						`use a socket access list or leave unix_sockets unset`
				caps.SocketListRefusal =
					"unix-socket access lists are not yet enforceable under closed network access on Linux tclaude-layer; " +
						"leave unix_sockets unset (agentd only) or use open network access (list degrades, unenforced)"
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
			NetworkClosed: EnforceNone,
			NetworkList:   EnforceNone,
			SocketOpen:    EnforceFull,
			SocketClosed:  EnforceNone,
			SocketList:    EnforceNone,
			Scope:         "tools-only",
			Mechanism:     "Codex builtin sandbox",
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
			caps.SocketListRefusal =
				"unix-socket access-list widening is not yet enforceable in the Codex managed profile; " +
					"leave unix_sockets unset (agentd only) or choose a sandbox mode that preserves ambient sockets " +
					"(list degrades, unenforced)"
			// Profile-authored socket lists are connected in M3. The Codex
			// TOML mechanism exists today, but M1 cannot truthfully claim that
			// a field no adapter consumes is enforced.
		}
		return caps, nil
	default:
		return accessEnforcementTableRow{}, fmt.Errorf("harness %q has no access-enforcement capability descriptor", h.Name)
	}
}

func accessEnforcementFromTable(row accessEnforcementTableRow) AccessEnforcement {
	return AccessEnforcement{
		networkClosed: row.NetworkClosed, networkList: row.NetworkList,
		networkSelectors: append([]string(nil), row.NetworkSelectors...),
		networkPorts:     row.NetworkPorts, socketOpen: row.SocketOpen,
		socketClosed: row.SocketClosed, socketList: row.SocketList,
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
		NetworkSelectors: append([]string(nil), row.NetworkSelectors...),
		NetworkPorts:     row.NetworkPorts, SocketOpen: row.SocketOpen,
		SocketClosed: row.SocketClosed, SocketList: row.SocketList,
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
			return predictedRefused(tier, fmt.Sprintf(
				"%s (%s scope) cannot enforce closed network access", caps.Mechanism, caps.Scope))
		}
	case sandboxpolicy.AccessModeList:
		if caps.NetworkList == EnforceNone {
			return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionNotEnforced,
				Detail: fmt.Sprintf("%s: no filtered-egress applier exists; all outbound connections are permitted",
					caps.Mechanism)}
		}
		unsupported := networkUnsupportedEntries(rules.Allow, caps.NetworkSelectors)
		if len(unsupported) > 0 {
			return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionNotEnforced,
				Detail: fmt.Sprintf("%s cannot express one or more destination selectors; all outbound connections are permitted",
					caps.Mechanism)}
		}
		if caps.NetworkList == EnforcePartial || (!caps.NetworkPorts && networkRulesHavePorts(rules)) ||
			caps.Scope == "tools-only" {
			return predictedPartial(tier, caps.Mechanism,
				"the allow list is enforced with wider destination, port, or process scope")
		}
		return predictedEnforced(tier, caps.Mechanism, "the network allow list")
	default:
		return PredictedAccessAxis{Tier: tier, Outcome: AccessPredictionRefused,
			Detail: "the network tier is invalid"}
	}
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
		if caps.SocketList == EnforcePartial || caps.Scope == "tools-only" {
			return predictedPartial(tier, caps.Mechanism,
				"the Unix-socket allow list is enforced with wider socket or process scope")
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
) (sandboxpolicy.ResolvedAxes, []sandboxpolicy.AccessNotice, error) {
	if strings.TrimSpace(caps.mechanism) == "" {
		return sandboxpolicy.ResolvedAxes{}, nil, fmt.Errorf(
			"access enforcement capabilities were not resolved through the sandbox implementation gate",
		)
	}
	rendered := cloneResolvedAxes(axes)
	notices := []sandboxpolicy.AccessNotice{}

	if axes.Network.Mode == sandboxpolicy.AccessModeClosed && caps.networkClosed == EnforceNone {
		return sandboxpolicy.ResolvedAxes{}, nil, &SandboxCapabilityError{
			Kind: SandboxCapabilityNetworkAllowlist,
			Message: fmt.Sprintf(
				"%s (%s scope) cannot enforce closed network access",
				caps.mechanism, caps.scope,
			),
		}
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
			rendered.Network = sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen}
			notices = append(notices, degradationNotice(
				"network", "no_mechanism", sandboxpolicy.AccessNoticeEffectNotEnforced,
				caps, "the network allow list is not enforced; outbound network access remains open", nil,
			))
		default:
			unsupported := networkUnsupportedEntries(axes.Network.Allow, caps.networkSelectors)
			if len(unsupported) > 0 {
				rendered.Network = sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen}
				notices = append(notices, degradationNotice(
					"network", "selector_unsupported", sandboxpolicy.AccessNoticeEffectNotEnforced,
					caps, "the network allow list contains selectors this mechanism cannot express; outbound network access remains open",
					unsupported,
				))
			} else if !caps.networkPorts {
				affected := []int{}
				for i := range rendered.Network.Allow {
					if len(rendered.Network.Allow[i].Ports) == 0 {
						continue
					}
					rendered.Network.Allow[i].Ports = nil
					affected = append(affected, i)
				}
				if len(affected) > 0 {
					notices = append(notices, degradationNotice(
						"network", "ports_unsupported", sandboxpolicy.AccessNoticeEffectEnforcedWider,
						caps, "destination selectors are enforced, but port constraints are not; matching destinations are reachable on every port",
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
		} else if caps.scope == "tools-only" {
			notices = append(notices, degradationNotice(
				"unix_sockets", "tools_only_scope", sandboxpolicy.AccessNoticeEffectEnforcedWider,
				caps, "the Unix-socket allow list applies only to tool execution, not the harness process", nil,
			))
		}
	}
	return rendered, notices, nil
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
	selectors []string,
) []int {
	out := []int{}
	for i, entry := range entries {
		selector := "loopback"
		switch {
		case entry.Host != "":
			selector = "host"
		case entry.Domain != "":
			selector = "domain"
		case entry.CIDR != "":
			selector = "cidr"
		}
		if !slices.Contains(selectors, selector) {
			out = append(out, i)
		}
	}
	return out
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
